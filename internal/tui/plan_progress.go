package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/specialist"
	"github.com/Twigpine/zero/internal/streamjson"
)

// PlanProgressBridge turns plan lifecycle events into TUI messages.
//
// LIFETIME, and it is the PostureGate problem again: the registry is built once
// per session and the orchestrate tool holds this recorder for the process's
// life, while the run id changes on every run. So this is a POINTER with
// mutable state that the model re-attaches per run, not a closure over a
// value-typed model that would freeze the first run's id forever.
//
// THREAD SAFETY: every method here is called from the tool's goroutine, never
// from the Bubble Tea event loop. It takes a mutex, builds a message, and hands
// it to the sink — which is the same asynchronous path OnToolProgress already
// uses. Nothing here renders, blocks, or touches model state.
//
// BEST EFFORT, like every other recorder on this path: a nil bridge, a nil sink
// or an unattached run is a silent no-op. Recording must never be the thing
// that fails a plan (execSessionRecorder.append's contract).
type PlanProgressBridge struct {
	mu    sync.Mutex
	sink  func(tea.Msg)
	runID int
	// store/sessionID make the plan DURABLE. The TUI drove the panel and wrote
	// nothing: a plan that ran here left no record at all, while the same plan
	// under `zero exec` wrote five events per task. Resume is a reduction over
	// those events, so a plan recorded by only one of the two surfaces is a
	// plan only one of them can ever resume.
	//
	// Written from THIS goroutine — the tool's — never the event loop. Appending
	// is file I/O, and the Bubble Tea loop must not block on it.
	store     *sessions.Store
	sessionID string
	// planStore/planSessionID are captured when a plan starts and never rebound
	// by a later run. Display state may attach to another run in the same
	// session; durable ownership may not move underneath an in-flight plan.
	planStore     *sessions.Store
	planSessionID string
	// recordErr latches the first append failure. Recording is best-effort and
	// must never fail a plan, but a silent drop would let a user believe a plan
	// was persisted when it was not.
	recordErr error
	// background marks the running plan as one that outlives the run that
	// launched it. Every message it posts carries the flag, because the panel's
	// stale-run guard drops anything whose runID is not the active one — which
	// is right for a foreground plan's leftovers and wrong for a background
	// plan that is still working.
	background bool
	// completedBySession collects finished background plans for the originating
	// conversation to be told about on a later turn. Drained by the agent loop,
	// never by the event loop.
	completedBySession map[string][]string
	// cancelPlan stops THIS PLAN without stopping the turn. Held only while a
	// plan is running and dropped in PlanCompleted, so a stop arriving after the
	// plan ended cannot cancel a context that has since been reused.
	cancelPlan context.CancelFunc
	// paused / resume implement the task-boundary pause. resume is closed on
	// resume rather than signalled, so a waiter that arrives after the resume
	// still proceeds instead of blocking forever on a send nobody makes.
	paused bool
	resume chan struct{}
	// lastPlans holds the ARGUMENTS of the most recent plan admitted per session —
	// what /plans save writes. Kept here rather than in the panel because the
	// panel holds a RENDERING of a plan (ids, statuses, depths) and saving that
	// would produce something that merely resembles what ran. The bridge is
	// handed the real Plan, so it keeps the one thing that can be run again.
	lastPlans map[string]lastPlanRecord
	// cardByTask maps a task id to the card it opened, so a child's stream event
	// can be routed to the right row. The recorder is the only thing that knows
	// this pairing — it created it — which is why per-task progress travels
	// through here rather than being guessed at the display end.
	//
	// KEYED BY TASK ID, WHICH IS UNIQUE WITHIN A PLAN AND NOT BETWEEN TWO. That
	// is sound only because a surface carries one plan at a time, which
	// specialist.PlanSurfaceBusy enforces at the tool. Were two plans ever to
	// report here at once, an id as ordinary as "tests" would have one plan's
	// dispatch overwrite the other's entry: the first completion would close the
	// wrong row, and the card left behind could never be closed by anything —
	// it would spin in AGENTS for the rest of the session. If that gate is ever
	// relaxed, this map needs a plan discriminator BEFORE it is.
	cardByTask map[string]string
	// dispatched counts tasks so each gets a unique temporary card key. The
	// child's real session id is not known until the child process creates it,
	// so the card is keyed by this and reconciled on completion — exactly how
	// the Task tool's card works.
	dispatched int
	// planFinalized makes every background exit converge on one terminal
	// transition. The launcher may observe a panic or an early return after the
	// executor already reported completion; only the first finalizer wins.
	planFinalized bool
}

type lastPlanRecord struct {
	args map[string]any
	name string
}

// The optional halves the bridge implements, asserted at COMPILE TIME. Each is
// consulted through a type assertion, so a signature that drifts out of shape
// does not fail to build — it silently stops being found, and the behaviour it
// carries disappears with no error anywhere. This branch has shipped that exact
// defect often enough to pay for four lines.
var (
	_ specialist.PlanRecorder             = (*PlanProgressBridge)(nil)
	_ specialist.PlanLifecycleRecorder    = (*PlanProgressBridge)(nil)
	_ specialist.PlanController           = (*PlanProgressBridge)(nil)
	_ specialist.PlanSurfaceBusy          = (*PlanProgressBridge)(nil)
	_ specialist.PlanTaskProgressRecorder = (*PlanProgressBridge)(nil)
	_ specialist.PlanPreflightReporter    = (*PlanProgressBridge)(nil)
)

// NewPlanProgressBridge returns a bridge that is inert until Attach is called.
func NewPlanProgressBridge() *PlanProgressBridge { return &PlanProgressBridge{} }

// Attach binds the bridge to the run that is about to start. Called on every
// run so a plan's cards belong to the run that produced them; the stale-run
// guard in the message handlers does the rest.
func (bridge *PlanProgressBridge) Attach(sink func(tea.Msg), runID int, store *sessions.Store, sessionID string) {
	if bridge == nil {
		return
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	bridge.sink = sink
	bridge.runID = runID
	// dispatched is deliberately NOT reset. It was, and that was safe only while
	// every plan lived inside one run: a background plan still dispatching when
	// the next run attaches would restart the counter and hand the new run's
	// tasks card keys the old plan is still using — one card overwriting
	// another, which is the specialist-card collision defect in a new costume.
	// A counter monotonic for the bridge's life costs nothing and cannot collide.
	bridge.store = store
	bridge.sessionID = sessionID
}

// SelectSession updates the conversational scope before another run starts.
// Session switches are refused while a background plan is live, so this never
// changes an active plan's captured durable owner.
func (bridge *PlanProgressBridge) SelectSession(sessionID string) {
	if bridge == nil {
		return
	}
	bridge.mu.Lock()
	bridge.sessionID = sessionID
	bridge.mu.Unlock()
}

// record appends one plan event to the session log.
//
// BEST EFFORT at every level: no store, no session, or a latched earlier
// failure and it does nothing. It mirrors execSessionRecorder.append's
// contract exactly, because the two are the same record written from two
// surfaces.
func (bridge *PlanProgressBridge) record(eventType sessions.EventType, payload map[string]any) {
	if bridge == nil {
		return
	}
	bridge.mu.Lock()
	store, sessionID := bridge.store, bridge.sessionID
	if bridge.planSessionID != "" {
		store, sessionID = bridge.planStore, bridge.planSessionID
	}
	failed := bridge.recordErr != nil
	bridge.mu.Unlock()
	if store == nil || sessionID == "" || failed {
		return
	}
	_, err := store.AppendEvent(sessionID, sessions.AppendEventInput{Type: eventType, Payload: payload})
	if err != nil {
		bridge.mu.Lock()
		bridge.recordErr = err
		bridge.mu.Unlock()
	}
}

// PlanRunning takes the cancel scoped to the plan that is starting. Any pause
// left over from a previous plan is cleared here: a new plan must never begin
// life suspended by a key the user pressed during the last one.
// PlanPreflight surfaces what auto-assignment is doing before a plan exists.
func (bridge *PlanProgressBridge) PlanPreflight(status string) {
	if bridge == nil {
		return
	}
	bridge.send(func(runID int) tea.Msg {
		return planPreflightMsg{runID: runID, status: status}
	})
}

func (bridge *PlanProgressBridge) PlanRunning(cancel context.CancelFunc) {
	if bridge == nil {
		return
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	bridge.capturePlanOwnerLocked()
	bridge.cancelPlan = cancel
	bridge.clearPauseLocked()
}

// SetBackground marks the plan about to run as a background one. Called by the
// launcher before the plan starts, and cleared when it ends.
func (bridge *PlanProgressBridge) SetBackground(background bool) {
	if bridge == nil {
		return
	}
	bridge.mu.Lock()
	if background {
		bridge.capturePlanOwnerLocked()
		bridge.recordErr = nil
		bridge.planFinalized = false
	}
	bridge.background = background
	bridge.mu.Unlock()
}

// DrainCompletedPlans returns and clears what finished in the background since
// the last drain, for the agent loop to append to the conversation.
//
// Drained by the AGENT loop, on its own goroutine, at the same point the
// post-edit diagnostics nudge is drained — that is the existing channel for
// background work reporting into a later turn, and reusing it means a plan
// completion is budgeted, compacted and ordered exactly like every other
// tail message.
func (bridge *PlanProgressBridge) DrainCompletedPlans() string {
	if bridge == nil {
		return ""
	}
	bridge.mu.Lock()
	done := append([]string(nil), bridge.completedBySession[bridge.sessionID]...)
	delete(bridge.completedBySession, bridge.sessionID)
	bridge.mu.Unlock()
	if len(done) == 0 {
		return ""
	}
	return strings.Join(done, "\n\n")
}

// WaitWhilePaused blocks the TOOL's goroutine — never the event loop — until
// the user resumes or the plan is cancelled.
func (bridge *PlanProgressBridge) WaitWhilePaused(ctx context.Context) {
	if bridge == nil {
		return
	}
	for {
		bridge.mu.Lock()
		paused, resume := bridge.paused, bridge.resume
		bridge.mu.Unlock()
		if !paused || resume == nil {
			return
		}
		select {
		case <-resume:
			// Loop rather than return: a resume followed immediately by another
			// pause must be honoured, and re-reading the state is what makes
			// the two orderings equivalent.
		case <-ctx.Done():
			return
		}
	}
}

// StopPlan cancels the running plan and reports whether there was one.
//
// Called from the Bubble Tea event loop, so it does exactly two cheap things:
// it reads a pointer and calls a cancel func.
//
// It also clears the pause, and it is worth being precise about WHY, because
// the first version of this comment claimed the wrong mechanism. Releasing the
// parked executor is NOT what the clear does — WaitWhilePaused selects on ctx,
// so the cancel below frees it on its own. What the clear does is fix the
// reported STATE: without it the bridge still says "paused" between the stop
// and the plan's terminal event, so the surface would offer "/plans resume" for
// a plan that is being abandoned.
func (bridge *PlanProgressBridge) StopPlan() bool {
	if bridge == nil {
		return false
	}
	bridge.mu.Lock()
	cancel := bridge.cancelPlan
	bridge.clearPauseLocked()
	bridge.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// SetPlanPaused pauses or resumes at the next task boundary. Reports whether
// there was a running plan to act on, so the caller can say "no plan is
// running" rather than silently doing nothing.
func (bridge *PlanProgressBridge) SetPlanPaused(paused bool) bool {
	if bridge == nil {
		return false
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.cancelPlan == nil {
		return false
	}
	if !paused {
		bridge.clearPauseLocked()
		return true
	}
	if !bridge.paused {
		bridge.paused = true
		bridge.resume = make(chan struct{})
	}
	return true
}

// PlanPaused reports the pause state, for the status line.
func (bridge *PlanProgressBridge) PlanPaused() bool {
	if bridge == nil {
		return false
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return bridge.paused
}

// PlanRunningNow reports whether a plan is in flight, so a control command can
// refuse with a reason instead of appearing to work.
func (bridge *PlanProgressBridge) PlanRunningNow() bool {
	if bridge == nil {
		return false
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return bridge.cancelPlan != nil
}

// RunningPlanName answers specialist.PlanSurfaceBusy: this surface carries one
// plan, and it says which.
//
// background is consulted ALONGSIDE cancelPlan, not instead of it, because the
// two are set at different moments. The launcher sets background synchronously
// before it starts the goroutine; the goroutine sets cancelPlan when the
// executor reaches its first task. Between those two points the plan is
// unquestionably running, and a gate reading only cancelPlan would wave the
// next one straight through the window.
func (bridge *PlanProgressBridge) RunningPlanName() (string, bool) {
	if bridge == nil {
		return "", false
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.cancelPlan == nil && !bridge.background {
		return "", false
	}
	name := ""
	if last, ok := bridge.lastPlans[bridge.planSessionID]; ok {
		name = last.name
	} else if last, ok := bridge.lastPlans[bridge.sessionID]; ok {
		name = last.name
	}
	if strings.TrimSpace(name) == "" {
		// A plan launched but not yet admitted has no name recorded. Refusing
		// without one is still the right answer; "a plan" is honest.
		name = "a plan"
	}
	return name, true
}

// BackgroundPlanLive reports whether a BACKGROUND plan is still in flight.
//
// beginRun wipes the orchestrate panel so a previous turn's plan cannot bleed
// into the new one, which is right for a foreground plan and wrong for a
// background one: those are built to outlive the run that launched them, and
// every message they post carries the background flag precisely to survive the
// stale-run guard. Wiping anyway leaves the guard passing messages that then
// no-op against an empty byID, so the PLAN surface vanishes for the rest of the
// plan's life while it keeps running.
//
// background is consulted alongside cancelPlan for the same reason
// RunningPlanName does it: the launcher sets background synchronously before
// starting the goroutine, and cancelPlan only appears once the executor reaches
// its first task. Reading either alone leaves a window.
func (bridge *PlanProgressBridge) BackgroundPlanLive() bool {
	if bridge == nil {
		return false
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	// SetBackground is synchronous at launch, before the goroutine records
	// admission or installs its cancel function. The flag alone must close that
	// window or /new and /resume can switch sessions between launch and admission.
	return bridge.background
}

// clearPauseLocked releases any waiter. Closing the channel rather than sending
// on it means every waiter wakes and a late waiter never blocks.
func (bridge *PlanProgressBridge) clearPauseLocked() {
	bridge.paused = false
	if bridge.resume != nil {
		close(bridge.resume)
		bridge.resume = nil
	}
}

// LastPlan returns the arguments of the most recent plan admitted this session,
// and its name. Reports false when no plan has run — the caller says so rather
// than saving an empty file.
func (bridge *PlanProgressBridge) LastPlan() (map[string]any, string, bool) {
	if bridge == nil {
		return nil, "", false
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	last, ok := bridge.lastPlans[bridge.sessionID]
	if !ok || len(last.args) == 0 {
		return nil, "", false
	}
	return clonePlanArgs(last.args), last.name, true
}

// TaskProgress routes one of a task's child events to that task's card.
//
// Called from the TASK's goroutine — several at once when a plan runs
// concurrently — so it takes the lock, resolves the card, and hands a message
// to the sink. Nothing here renders or blocks, which is the same contract every
// other method on this bridge keeps.
func (bridge *PlanProgressBridge) TaskProgress(taskID string, event streamjson.Event) {
	if bridge == nil || event.Type != streamjson.EventToolCall {
		// Only tool calls: the card counts tool calls and names the current one,
		// and forwarding every token would be a message per token on the event
		// loop for a display that shows neither.
		return
	}
	bridge.mu.Lock()
	card := bridge.cardByTask[taskID]
	background := bridge.background
	bridge.mu.Unlock()
	if card == "" {
		// No card: the task was never dispatched through this bridge. Silently
		// dropping is right — inventing one would put a row on screen for work
		// the panel never admitted.
		return
	}
	name, detail := event.Name, toolCallSummary(event)
	bridge.send(func(runID int) tea.Msg {
		return planTaskProgressMsg{
			runID: runID, taskID: taskID, cardKey: card,
			toolName: name, detail: detail, background: background,
		}
	})
}

// RecordingError reports the first append failure, so a surface can say once
// that the plan was not fully persisted rather than leaving it silent.
func (bridge *PlanProgressBridge) RecordingError() error {
	if bridge == nil {
		return nil
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return bridge.recordErr
}

// send posts a message if the bridge is attached. Nil-safe at every level.
func (bridge *PlanProgressBridge) send(build func(runID int) tea.Msg) {
	if bridge == nil {
		return
	}
	bridge.mu.Lock()
	sink, runID := bridge.sink, bridge.runID
	bridge.mu.Unlock()
	if sink == nil {
		return
	}
	sink(build(runID))
}

// planTaskKey is the temporary card key for the nth dispatched task. Namespaced
// so it can never collide with a tool call id.
func planTaskKey(n int) string { return fmt.Sprintf("plantask_%d", n) }

// planTaskOutputLimit bounds what a task's result contributes to the model.
//
// TaskResult.Output is the child's FULL answer and is deliberately not
// truncated at the tool boundary — the report task in a nine-task plan returned
// a hundred thousand tokens of it. The sidebar shows a few lines; carrying the
// rest through the event loop and holding it per task for the life of the
// session would be paying megabytes for text nothing reads. The whole answer is
// still in the child's session, which the row's drill-in opens.
const planTaskOutputLimit = 2000

func boundTaskOutput(output string) string {
	output = strings.TrimSpace(output)
	if len(output) <= planTaskOutputLimit {
		return output
	}
	// Cut on a rune boundary: a half-written multibyte character renders as a
	// replacement glyph, which reads as corruption rather than truncation.
	cut := planTaskOutputLimit
	for cut > 0 && !utf8.RuneStart(output[cut]) {
		cut--
	}
	return strings.TrimSpace(output[:cut]) + "…"
}

// PlanAdmitted announces the plan so the transcript can show that N tasks are
// about to run rather than going silent until the first one finishes.
func (bridge *PlanProgressBridge) PlanAdmitted(plan specialist.Plan) {
	if bridge != nil {
		// Args, not the Plan: what gets saved has to be re-admitted through
		// ParsePlan on the way back in, so it is stored in the shape ParsePlan
		// accepts and never as an object that could reach execution unvalidated.
		bridge.mu.Lock()
		bridge.capturePlanOwnerLocked()
		// Admission starts the durable lifecycle. Clear an error from a previous
		// finished plan before this plan's first append, never later in
		// PlanRunning where it would erase an admission failure.
		bridge.recordErr = nil
		bridge.planFinalized = false
		if bridge.lastPlans == nil {
			bridge.lastPlans = map[string]lastPlanRecord{}
		}
		bridge.lastPlans[bridge.planSessionID] = lastPlanRecord{args: clonePlanArgs(plan.Args()), name: plan.Name()}
		bridge.mu.Unlock()
	}
	bridge.record(specialist.PlanAdmittedEvent(plan))
	name := plan.Name()
	count := plan.TaskCount()
	limit := plan.Budget().MaxTokens

	// Copied into the message in EXECUTION ORDER, with the dependency edges, so
	// the panel can draw the graph without reaching back into the plan — which
	// it could not do anyway: Plan's fields are unexported and it lives in
	// another package on the tool's goroutine.
	byID := map[string]specialist.Task{}
	for _, task := range plan.Tasks() {
		byID[task.ID] = task
	}
	graph := make([]planGraphTask, 0, count)
	for _, id := range plan.Order() {
		task := byID[id]
		graph = append(graph, planGraphTask{
			id:        id,
			dependsOn: append([]string(nil), task.DependsOn...),
			phase:     task.Phase,
		})
	}

	bridge.send(func(runID int) tea.Msg {
		// REMOVED: a workers count rode this message for planRunningCardKey, which
		// attributed a plan's child progress to "whichever task was dispatched
		// last". That was deleted when per-task progress arrived carrying its own
		// task id — and the field outlived its only consumer, still pointing
		// readers at a function that no longer exists. The panel gets the number
		// it displays from the plan report.
		return planAdmittedMsg{runID: runID, name: name, taskCount: count, tasks: graph, tokenLimit: limit,
			background: bridge.isBackground()}
	})
}

// TaskDispatched opens a card for the task that is about to run.
func (bridge *PlanProgressBridge) TaskDispatched(task specialist.Task) {
	if bridge == nil {
		return
	}
	bridge.record(specialist.TaskDispatchedEvent(task))
	bridge.mu.Lock()
	bridge.dispatched++
	key := planTaskKey(bridge.dispatched)
	if bridge.cardByTask == nil {
		bridge.cardByTask = map[string]string{}
	}
	bridge.cardByTask[task.ID] = key
	bridge.mu.Unlock()

	id, summary, model := task.ID, planTaskSummary(task), task.Model
	bridge.send(func(runID int) tea.Msg {
		return planTaskStartMsg{runID: runID, taskID: id, summary: summary, cardKey: key,
			model: model, background: bridge.isBackground()}
	})
}

// TaskCompleted closes the card and reconciles it to the child's real session
// id so the user can drill into it.
func (bridge *PlanProgressBridge) TaskCompleted(result specialist.TaskResult) {
	bridge.record(specialist.TaskCompletedEvent(result))
	bridge.finish(result, specialistCompleted)
}

// TaskFailed closes the card with the outcome's own status. A cancelled or
// skipped task is NOT rendered as an error: nothing broke.
func (bridge *PlanProgressBridge) TaskFailed(result specialist.TaskResult) {
	bridge.record(specialist.TaskFailedEvent(result))
	bridge.finish(result, planOutcomeStatus(result.Outcome))
}

func (bridge *PlanProgressBridge) finish(result specialist.TaskResult, status specialistStatus) {
	if bridge == nil {
		return
	}
	// BY TASK ID, never by the dispatch counter. With one worker the last
	// dispatched card was always the finishing task's card, so a counter was
	// indistinguishable from a lookup — and wrong the moment max_workers > 1
	// opened six cards at once. Under fan-out a completion arrives in whatever
	// order the task finished, so closing planTaskKey(dispatched) marked a task
	// that was still running as done and left the finished one spinning for the
	// rest of the session.
	//
	// The map is also the AUTHORITY on whether the task was dispatched. The
	// outcome cannot answer that: TaskCancelled is emitted both for a task
	// stopped mid-flight (it has a card) and for one cancelled before it ever
	// ran (it does not), and treating the first as undispatched opened a second
	// card and left the real one running forever — the same bug wearing the
	// cancel path's clothes.
	//
	// Read-and-delete: an entry exists exactly while its task is in flight, so a
	// later plan reusing a task id can never resolve against a stale card.
	bridge.mu.Lock()
	key, dispatched := bridge.cardByTask[result.ID]
	delete(bridge.cardByTask, result.ID)
	bridge.mu.Unlock()

	// A task that was never dispatched (dependency-skipped, budget-skipped,
	// cancelled before it ran) has no card. Reporting it against another task's
	// key would close the wrong card, so those carry their own key and the
	// handler creates the card on demand.
	taskID, sessionID, reason := result.ID, result.SessionID, result.Err
	outcome, tokens, attempts := result.Outcome, result.Tokens, result.Attempts
	// WHAT IT RAN ON, not what it was dispatched with. The card's model is set
	// once at dispatch from the ASSIGNED model; a task whose model the provider
	// refused then runs on the session's, and without this the row goes on
	// naming a model that never executed it.
	ranOn, fellBackFrom := result.Model, result.RetriedOnParentModel
	output := boundTaskOutput(result.Output)
	bridge.send(func(runID int) tea.Msg {
		return planTaskDoneMsg{
			runID:        runID,
			taskID:       taskID,
			cardKey:      key,
			dispatched:   dispatched,
			sessionID:    sessionID,
			status:       status,
			outcome:      string(outcome),
			reason:       reason,
			tokens:       tokens,
			attempts:     attempts,
			output:       output,
			model:        ranOn,
			fellBackFrom: fellBackFrom,
			background:   bridge.isBackground(),
		}
	})
}

// PlanCompleted reports the plan's terminal state.
func (bridge *PlanProgressBridge) PlanCompleted(plan specialist.Plan, report specialist.PlanReport) {
	bridge.record(specialist.PlanCompletedEvent(plan, report))
	bridge.finalizePlan(planCompletedMsg{
		name: plan.Name(), status: string(report.Status),
		succeeded: report.Succeeded, failed: report.Failed, skipped: report.Skipped, cancelled: report.Cancelled,
		tokensUsed: report.TokensUsed, tokenLimit: plan.Budget().MaxTokens, maxSpeedup: report.MaxSpeedup,
	}, backgroundPlanReport(plan, report))
}

// FinalizeBackgroundExit is the launcher's final safety net. Normal execution
// reaches PlanCompleted first and this becomes an idempotent no-op. A panic,
// cancellation, or early return that skipped the executor's terminal callback
// still records and publishes one explicit terminal transition.
func (bridge *PlanProgressBridge) FinalizeBackgroundExit(panicked, cancelled bool) {
	if bridge == nil {
		return
	}
	bridge.mu.Lock()
	if bridge.planFinalized {
		bridge.mu.Unlock()
		return
	}
	name := bridge.lastPlans[bridge.planSessionID].name
	bridge.mu.Unlock()
	if name == "" {
		name = "(unnamed)"
	}
	status, failed, cancelledCount, reason := "failed", 1, 0, "background plan exited without a terminal report"
	if panicked {
		reason = "background plan panicked"
	} else if cancelled {
		status, failed, cancelledCount, reason = "cancelled", 0, 1, "background plan was cancelled"
	}
	bridge.record(sessions.EventPlanCompleted, map[string]any{
		"name": name, "status": status, "failed": failed, "cancelled": cancelledCount, "reason": reason,
	})
	bridge.finalizePlan(planCompletedMsg{name: name, status: status, failed: failed, cancelled: cancelledCount},
		"The background plan "+name+" has finished with status "+status+": "+reason+".")
}

func (bridge *PlanProgressBridge) finalizePlan(message planCompletedMsg, completion string) {
	if bridge == nil {
		return
	}
	bridge.mu.Lock()
	if bridge.planFinalized {
		bridge.mu.Unlock()
		return
	}
	bridge.planFinalized = true
	wasBackground := bridge.background
	originSessionID := bridge.planSessionID
	durabilityError := ""
	if bridge.recordErr != nil {
		durabilityError = sanitizeCardText(bridge.recordErr.Error())
	}
	bridge.cancelPlan = nil
	bridge.background = false
	bridge.clearPauseLocked()
	if wasBackground {
		if bridge.completedBySession == nil {
			bridge.completedBySession = map[string][]string{}
		}
		if durabilityError != "" {
			completion += "\n\nWarning: plan progress was not fully persisted, so this result is not safely resumable: " + durabilityError
		}
		bridge.completedBySession[originSessionID] = append(bridge.completedBySession[originSessionID], completion)
	}
	bridge.planStore = nil
	bridge.planSessionID = ""
	bridge.mu.Unlock()

	message.background = wasBackground
	message.durabilityError = durabilityError
	bridge.send(func(runID int) tea.Msg {
		message.runID = runID
		return message
	})
}

func (bridge *PlanProgressBridge) capturePlanOwnerLocked() {
	if bridge.planSessionID != "" {
		return
	}
	bridge.planStore = bridge.store
	bridge.planSessionID = bridge.sessionID
}

func clonePlanArgs(input map[string]any) map[string]any {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

// PlanIsBackground reports whether the running plan outlives its run, for a
// surface that wants to say so.
func (bridge *PlanProgressBridge) PlanIsBackground() bool { return bridge.isBackground() }

// isBackground reports whether the running plan outlives its run.
func (bridge *PlanProgressBridge) isBackground() bool {
	if bridge == nil {
		return false
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return bridge.background
}

// backgroundPlanReport is what the model is told when a background plan ends.
// The SAME summary a foreground plan returns, prefixed with which plan it was —
// by the time it arrives the conversation has moved on, and "Plan partial: 1
// succeeded" with no name attached is a result the model cannot place.
func backgroundPlanReport(plan specialist.Plan, report specialist.PlanReport) string {
	name := plan.Name()
	if name == "" {
		name = "(unnamed)"
	}
	return "The background plan " + name + " has finished. This is its result:\n\n" + report.Summary()
}

// planOutcomeStatus maps a task outcome onto the card status. Cancelled and
// skipped are deliberately NOT specialistError: a user who stopped a plan did
// not break it, and a task skipped because its dependency failed is not itself
// a failure.
func planOutcomeStatus(outcome specialist.TaskOutcome) specialistStatus {
	switch outcome {
	case specialist.TaskSucceeded:
		return specialistCompleted
	case specialist.TaskFailed:
		return specialistError
	default:
		// Cancelled, dependency-skipped, budget-skipped: ended without running
		// to completion, but not an error.
		return specialistCancelled
	}
}

// planTaskSummary is a SHORT label for the card. The full prompt stays in the
// tool output; a display surface never becomes the data path.
func planTaskSummary(task specialist.Task) string {
	// Sanitized, not just newline-cut. Task prompts are model-authored and can
	// carry whatever a poisoned file or web page fed into the orchestrate args,
	// and this string is painted into the inline panel, the sidebar rows and the
	// detail pane. Cutting at the first newline leaves every other control byte
	// intact, so an ESC/OSC sequence survives and can repaint the terminal.
	// sanitizeCardText already drops exactly these for the permission cards.
	summary := sanitizeCardText(task.Prompt)
	if summary == "" {
		return ""
	}
	return truncateRunes(summary, planTaskSummaryWidth)
}

// planTaskSummaryWidth bounds the card label. truncateRunes (view.go) does the
// cut on RUNE boundaries — reusing it rather than reimplementing keeps one
// truncation rule, since slicing by byte index produces mojibake.
const planTaskSummaryWidth = 60

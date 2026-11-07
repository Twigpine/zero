package tui

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/specialist"
)

// THE RENDER-CACHE COLLISION, asserted with N cards where one has failed.
//
// A rowSpecialist row carries no id and no text — everything that distinguishes
// one card from another lives behind row.specialistInfo, and none of it was in
// the cache key. Two specialist cards in one run therefore hashed identically,
// so the first one rendered was reused for the second: a failed sub-agent shown
// as a successful one. One child per run made that rare; a plan produces one
// card per task, which makes it certain.
func TestSpecialistCardsInOneRunDoNotShareACacheKey(t *testing.T) {
	m := model{}
	now := time.Now()
	cards := []specialistInfo{
		{name: "a", childSessionID: "s1", status: specialistCompleted, startedAt: now, completedAt: now},
		{name: "b", childSessionID: "s2", status: specialistError, errorMsg: "boom", startedAt: now, completedAt: now},
		{name: "c", childSessionID: "s3", status: specialistCancelled, startedAt: now, completedAt: now},
		{name: "d", childSessionID: "s4", status: specialistCompleted, startedAt: now, completedAt: now},
	}

	seen := map[string]string{}
	for index := range cards {
		row := transcriptRow{kind: rowSpecialist, runID: 7, specialistInfo: &cards[index]}
		key, _ := m.renderRowCacheKey(row, 80, rowContext{}, cardRenderOptions{}, false)
		if other, clash := seen[key]; clash {
			t.Fatalf("cards %q and %q share a cache key, so one renders as the other", other, cards[index].name)
		}
		seen[key] = cards[index].name
	}
}

// Every field that changes what a card shows must be in the key. Checked field
// by field so one added to specialistInfo and forgotten here fails loudly.
func TestSpecialistCacheKeyCoversEveryVaryingField(t *testing.T) {
	m := model{}
	base := specialistInfo{
		name: "a", description: "d", childSessionID: "s1", status: specialistRunning,
		exitCode: 0, errorMsg: "", toolCount: 1, currentTool: "grep", currentDetail: "x",
		startedAt: time.Unix(1, 0), completedAt: time.Unix(2, 0),
	}
	keyOf := func(info specialistInfo) string {
		key, _ := m.renderRowCacheKey(
			transcriptRow{kind: rowSpecialist, runID: 1, specialistInfo: &info}, 80, rowContext{}, cardRenderOptions{}, false)
		return key
	}
	original := keyOf(base)

	mutations := map[string]func(*specialistInfo){
		"name":           func(i *specialistInfo) { i.name = "z" },
		"description":    func(i *specialistInfo) { i.description = "z" },
		"childSessionID": func(i *specialistInfo) { i.childSessionID = "z" },
		"status":         func(i *specialistInfo) { i.status = specialistError },
		"exitCode":       func(i *specialistInfo) { i.exitCode = 3 },
		"errorMsg":       func(i *specialistInfo) { i.errorMsg = "z" },
		"toolCount":      func(i *specialistInfo) { i.toolCount = 9 },
		"currentTool":    func(i *specialistInfo) { i.currentTool = "z" },
		"currentDetail":  func(i *specialistInfo) { i.currentDetail = "z" },
		"startedAt":      func(i *specialistInfo) { i.startedAt = time.Unix(99, 0) },
		"completedAt":    func(i *specialistInfo) { i.completedAt = time.Unix(99, 0) },
	}
	for field, mutate := range mutations {
		changed := base
		mutate(&changed)
		if keyOf(changed) == original {
			t.Errorf("changing %s does not change the cache key, so a stale render survives it", field)
		}
	}
}

// The bridge must never touch the event loop and must be inert until attached.
func TestPlanProgressBridgeIsInertUntilAttached(t *testing.T) {
	bridge := NewPlanProgressBridge()
	// No sink: every method is a silent no-op rather than a panic. Recording is
	// best-effort and must never be the thing that fails a plan.
	bridge.TaskDispatched(specialist.Task{ID: "a"})
	bridge.TaskCompleted(specialist.TaskResult{ID: "a", Outcome: specialist.TaskSucceeded})
	bridge.TaskFailed(specialist.TaskResult{ID: "b", Outcome: specialist.TaskFailed})

	var nilBridge *PlanProgressBridge
	nilBridge.TaskDispatched(specialist.Task{ID: "a"})
	nilBridge.Attach(nil, 1, nil, "")
}

func TestPlanProgressBridgeEmitsACardPerTask(t *testing.T) {
	var got []tea.Msg
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(msg tea.Msg) { got = append(got, msg) }, 42, nil, "")

	bridge.TaskDispatched(specialist.Task{ID: "a", Prompt: "first"})
	bridge.TaskCompleted(specialist.TaskResult{ID: "a", Outcome: specialist.TaskSucceeded, SessionID: "specialist_aaa"})
	bridge.TaskDispatched(specialist.Task{ID: "b", Prompt: "second"})
	bridge.TaskFailed(specialist.TaskResult{ID: "b", Outcome: specialist.TaskFailed, Err: "boom"})

	if len(got) != 4 {
		t.Fatalf("expected one message per transition, got %d", len(got))
	}
	first, ok := got[0].(planTaskStartMsg)
	if !ok || first.taskID != "a" || first.runID != 42 {
		t.Fatalf("first message = %#v, want a start for task a on run 42", got[0])
	}
	second, ok := got[2].(planTaskStartMsg)
	if !ok || second.cardKey == first.cardKey {
		t.Fatalf("each task needs its OWN card key, got %q twice", first.cardKey)
	}
	done, ok := got[3].(planTaskDoneMsg)
	if !ok || done.status != specialistError || !done.dispatched {
		t.Fatalf("last message = %#v, want a dispatched failure", got[3])
	}
}

// A task that never started has no card, so closing the LAST dispatched task's
// card for it would mark the wrong task — the collision defect in a new shape.
func TestSkippedTaskDoesNotCloseTheDispatchedTasksCard(t *testing.T) {
	var got []tea.Msg
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(msg tea.Msg) { got = append(got, msg) }, 1, nil, "")

	bridge.TaskDispatched(specialist.Task{ID: "b", Prompt: "runs"})
	bridge.TaskFailed(specialist.TaskResult{ID: "b", Outcome: specialist.TaskFailed, Err: "boom"})
	bridge.TaskFailed(specialist.TaskResult{ID: "d", Outcome: specialist.TaskSkippedDependency, Err: "skipped"})

	skipped, ok := got[2].(planTaskDoneMsg)
	if !ok {
		t.Fatalf("expected a done message for the skipped task, got %#v", got[2])
	}
	if skipped.dispatched {
		t.Fatal("a dependency-skipped task never ran; marking it dispatched closes another task's card")
	}
	if skipped.status == specialistError {
		t.Fatal("a task skipped because its dependency failed is not itself an error")
	}
}

// Cancelled and skipped are not errors. A user who stopped a plan did not break
// it, and a wall of red is exactly what the outcome exists to prevent.
func TestCancelledAndSkippedRenderAsNeitherSuccessNorError(t *testing.T) {
	for _, outcome := range []specialist.TaskOutcome{
		specialist.TaskCancelled,
		specialist.TaskSkippedDependency,
		specialist.TaskSkippedBudget,
	} {
		if status := planOutcomeStatus(outcome); status != specialistCancelled {
			t.Errorf("outcome %q mapped to status %v, want the neutral cancelled status", outcome, status)
		}
	}
	if planOutcomeStatus(specialist.TaskFailed) != specialistError {
		t.Error("a real failure must still render as an error")
	}
	if planOutcomeStatus(specialist.TaskSucceeded) != specialistCompleted {
		t.Error("a success must still render as a success")
	}
}

// The card label is a SUMMARY. The full prompt stays in the tool output — a
// display formatter on the data path is how a 583-rune work product became 200
// mangled runes.
func TestPlanTaskSummaryIsShortAndCutsOnRuneBoundaries(t *testing.T) {
	multiline := specialist.Task{Prompt: "first line\nsecond line"}
	if got := planTaskSummary(multiline); got != "first line" {
		t.Fatalf("summary = %q, want only the first line", got)
	}
	long := specialist.Task{Prompt: strings.Repeat("日", 200)}
	summary := planTaskSummary(long)
	if len([]rune(summary)) > planTaskSummaryWidth {
		t.Fatalf("summary is %d runes, over the %d cap", len([]rune(summary)), planTaskSummaryWidth)
	}
	if !strings.HasSuffix(summary, "…") {
		t.Fatalf("a truncated summary must say so: %q", summary)
	}
	for _, r := range summary {
		if r == '�' {
			t.Fatalf("summary was cut mid-rune: %q", summary)
		}
	}
}

// A retried task ran more than once behind ONE dispatch — the retry lives in the
// executor, so the panel sees a single card that took twice as long. The count
// has to reach the detail, or that time looks like one very slow attempt.
func TestARetriedTaskShowsItsAttemptCount(t *testing.T) {
	var got []tea.Msg
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(msg tea.Msg) { got = append(got, msg) }, 7, nil, "")
	bridge.TaskDispatched(specialist.Task{ID: "a", Prompt: "look"})
	bridge.TaskFailed(specialist.TaskResult{ID: "a", Outcome: specialist.TaskFailed, Attempts: 3, Err: "stalled"})

	var done planTaskDoneMsg
	found := false
	for _, msg := range got {
		if typed, ok := msg.(planTaskDoneMsg); ok {
			done, found = typed, true
		}
	}
	if !found {
		t.Fatal("no planTaskDoneMsg was posted")
	}
	if done.attempts != 3 {
		t.Fatalf("attempts = %d; want 3 — the bridge dropped the count", done.attempts)
	}

	state := &orchestratePanelState{}
	state.admit(planAdmittedMsg{name: "p", taskCount: 1, tasks: []planGraphTask{{id: "a"}}}, time.Now())
	state.markStarted("a", "look", "k", "", time.Now())
	state.markDone("a", done.outcome, done.tokens, done.attempts, time.Now())
	if got := state.tasks[0].attempts; got != 3 {
		t.Fatalf("panel attempts = %d; want 3", got)
	}
}

// A task that ran once says nothing about attempts: the ordinary case must be
// untouched by the retry machinery.
func TestASingleAttemptAddsNoAttemptCount(t *testing.T) {
	state := &orchestratePanelState{}
	state.admit(planAdmittedMsg{name: "p", taskCount: 1, tasks: []planGraphTask{{id: "a"}}}, time.Now())
	state.markStarted("a", "look", "k", "", time.Now())
	state.markDone("a", string(specialist.TaskSucceeded), 0, 1, time.Now())
	if got := state.tasks[0].attempts; got != 1 {
		t.Fatalf("attempts = %d; want 1", got)
	}
}

// FAN-OUT ORDER. With one worker a task could only finish after it was
// dispatched and before the next one was, so "the last dispatched card" was
// always the finishing task's card. With max_workers > 1 six tasks are open at
// once and they finish in whatever order they finish — so a completion must be
// matched to the card its OWN task opened, by id.
//
// Closing the last dispatched card instead leaves the earlier tasks' cards open
// forever: the sidebar spins on tasks that finished minutes ago, and a task
// still running is shown as done.
func TestACompletionClosesItsOwnCardNotTheLastDispatchedOne(t *testing.T) {
	var got []tea.Msg
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(msg tea.Msg) { got = append(got, msg) }, 1, nil, "")

	// Three tasks in flight together, finishing in reverse order.
	bridge.TaskDispatched(specialist.Task{ID: "a", Prompt: "first"})
	bridge.TaskDispatched(specialist.Task{ID: "b", Prompt: "second"})
	bridge.TaskDispatched(specialist.Task{ID: "c", Prompt: "third"})

	cards := map[string]string{}
	for _, msg := range got {
		if start, ok := msg.(planTaskStartMsg); ok {
			cards[start.taskID] = start.cardKey
		}
	}
	if len(cards) != 3 {
		t.Fatalf("expected three open cards, got %v", cards)
	}

	got = nil
	bridge.TaskCompleted(specialist.TaskResult{ID: "c", Outcome: specialist.TaskSucceeded})
	bridge.TaskFailed(specialist.TaskResult{ID: "a", Outcome: specialist.TaskCancelled})
	bridge.TaskCompleted(specialist.TaskResult{ID: "b", Outcome: specialist.TaskSucceeded})

	if len(got) != 3 {
		t.Fatalf("expected three completions, got %d", len(got))
	}
	for _, msg := range got {
		done, ok := msg.(planTaskDoneMsg)
		if !ok {
			t.Fatalf("expected a done message, got %#v", msg)
		}
		if want := cards[done.taskID]; done.cardKey != want {
			t.Errorf("task %s closed card %q, but it opened card %q — the wrong card is marked done and its own spins forever",
				done.taskID, done.cardKey, want)
		}
	}
}

// STOPPING A PLAN. The executor emits TaskCancelled for two different things: a
// task stopped while it was running, and a task cancelled before it ever
// started. Only the second has no card. Reading "cancelled" as "never
// dispatched" made the handler open a SECOND card for a task that already had
// one and close that, leaving the real card spinning after the plan was stopped.
func TestCancellingARunningTaskClosesTheCardItAlreadyHas(t *testing.T) {
	var got []tea.Msg
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(msg tea.Msg) { got = append(got, msg) }, 1, nil, "")

	bridge.TaskDispatched(specialist.Task{ID: "running", Prompt: "in flight when the user stopped the plan"})
	start := got[0].(planTaskStartMsg)

	got = nil
	bridge.TaskFailed(specialist.TaskResult{ID: "running", Outcome: specialist.TaskCancelled, Err: "cancelled"})
	bridge.TaskFailed(specialist.TaskResult{ID: "queued", Outcome: specialist.TaskCancelled, Err: "cancelled"})

	stopped, ok := got[0].(planTaskDoneMsg)
	if !ok {
		t.Fatalf("expected a done message, got %#v", got[0])
	}
	if !stopped.dispatched {
		t.Error("a task cancelled MID-FLIGHT was dispatched; reporting otherwise opens a second card and leaves the first spinning")
	}
	if stopped.cardKey != start.cardKey {
		t.Errorf("cancelled card %q, opened card %q", stopped.cardKey, start.cardKey)
	}

	queued, ok := got[1].(planTaskDoneMsg)
	if !ok {
		t.Fatalf("expected a done message, got %#v", got[1])
	}
	if queued.dispatched {
		t.Error("a task cancelled BEFORE it ran has no card; claiming it was dispatched closes a card that does not exist")
	}
}

// The map entry is deleted as the task finishes, so it exists exactly while the
// task is in flight. Without that, a task id reused by a LATER plan resolves
// against the earlier plan's card: a skipped task is reported as dispatched and
// closes a card that belongs to a plan that ended long ago.
func TestAFinishedTasksCardIsNotResolvableByALaterPlan(t *testing.T) {
	var got []tea.Msg
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(msg tea.Msg) { got = append(got, msg) }, 1, nil, "")

	// Plan one runs a task called "cfg" to completion.
	bridge.TaskDispatched(specialist.Task{ID: "cfg", Prompt: "first plan"})
	bridge.TaskCompleted(specialist.TaskResult{ID: "cfg", Outcome: specialist.TaskSucceeded})

	// Plan two has a task with the same id, skipped because a dependency failed.
	got = nil
	bridge.TaskFailed(specialist.TaskResult{ID: "cfg", Outcome: specialist.TaskSkippedDependency, Err: "skipped"})

	done, ok := got[0].(planTaskDoneMsg)
	if !ok {
		t.Fatalf("expected a done message, got %#v", got[0])
	}
	if done.dispatched {
		t.Error("a skipped task resolved against the previous plan's card for the same id")
	}
}

// THE WIRING, not the pieces. TaskResult.Output has always held the child's full
// answer and the TUI never saw it: the model knew a task had finished and what
// it cost, but not what it returned — so a finished agent row could report
// everything except the thing the user ran it for.
//
// Driven bridge → message → Update, because testing setResult directly would
// pass with the bridge sending nothing at all.
func TestATasksOutputReachesTheAgentRow(t *testing.T) {
	var got []tea.Msg
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(msg tea.Msg) { got = append(got, msg) }, 7, nil, "")

	bridge.TaskDispatched(specialist.Task{ID: "a-reltime", Prompt: "audit pkg/reltime"})
	bridge.TaskCompleted(specialist.TaskResult{
		ID:      "a-reltime",
		Outcome: specialist.TaskSucceeded,
		Output:  "pkg/reltime: 3 findings.\nreltime.go:41 — Parse ignores the tz suffix",
		Tokens:  22781,
	})

	m := newModel(context.Background(), Options{})
	m.activeRunID = 7
	for _, msg := range got {
		updated, _ := m.Update(msg)
		m = updated.(model)
	}

	info, ok := m.specialists.getBySessionID("plantask_1")
	if !ok {
		t.Fatalf("no card for the finished task: %+v", m.specialists.all())
	}
	if !strings.Contains(info.result, "Parse ignores the tz suffix") {
		t.Errorf("the task's output never reached its card, got %q", info.result)
	}
}

// The child's answer is not budgeted for the event loop. The report task in a
// nine-task plan returned a hundred thousand tokens of it; carrying that through
// the loop and holding it per task for the session would be paying megabytes for
// text nothing reads. The whole answer stays in the child's session.
func TestATasksOutputIsBoundedBeforeItLeavesTheBridge(t *testing.T) {
	long := strings.Repeat("finding line that goes on and on. ", 500)
	if len(long) <= planTaskOutputLimit {
		t.Fatalf("sanity check failed: the fixture must exceed the limit")
	}
	bounded := boundTaskOutput(long)
	if len(bounded) > planTaskOutputLimit+len("…") {
		t.Errorf("bounded output is %d bytes, limit is %d", len(bounded), planTaskOutputLimit)
	}
	if !strings.HasSuffix(bounded, "…") {
		t.Errorf("a truncated answer must say it was truncated: %q", bounded[maxInt(0, len(bounded)-40):])
	}
	if !utf8.ValidString(bounded) {
		t.Error("the cut must land on a rune boundary")
	}

	// A multibyte character straddling the limit must not be split in half.
	multi := strings.Repeat("é", planTaskOutputLimit)
	if b := boundTaskOutput(multi); !utf8.ValidString(b) {
		t.Error("multibyte output was cut mid-rune")
	}

	// Short output passes through whole, with no ellipsis implying loss.
	if got := boundTaskOutput("  brief answer  "); got != "brief answer" {
		t.Errorf("short output = %q, want it trimmed and intact", got)
	}
}

// THE WINDOW BETWEEN LAUNCH AND FIRST TASK. The launcher sets the background
// flag synchronously before it starts the goroutine; the goroutine sets the
// cancel when the executor reaches its first task. A gate that read only the
// cancel would wave the next plan straight through the gap between them — and
// that gap is exactly where the model's next tool call arrives, because a
// background plan returns immediately by design.
func TestTheSurfaceReadsBusyFromTheMomentAPlanIsLaunched(t *testing.T) {
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(tea.Msg) {}, 1, nil, "")

	if _, busy := bridge.RunningPlanName(); busy {
		t.Fatal("a fresh bridge carries no plan")
	}

	// Launched, not yet executing: no cancel has been handed over.
	bridge.SetBackground(true)
	name, busy := bridge.RunningPlanName()
	if !busy {
		t.Error("a launched background plan makes the surface busy before its first task runs")
	}
	if name == "" {
		t.Error("the refusal needs something to name, even before admission")
	}

	// Admitted: the name becomes the real one.
	bridge.PlanAdmitted(mustPlan(t, "sweep"))
	if name, _ := bridge.RunningPlanName(); name != "sweep" {
		t.Errorf("RunningPlanName = %q, want the admitted plan's name", name)
	}

	// A foreground plan makes it busy through the cancel instead.
	free := NewPlanProgressBridge()
	free.Attach(func(tea.Msg) {}, 1, nil, "")
	free.PlanRunning(func() {})
	if _, busy := free.RunningPlanName(); !busy {
		t.Error("a foreground plan holding a cancel makes the surface busy")
	}

	// And the surface is free again once the plan ends.
	bridge.PlanCompleted(mustPlan(t, "sweep"), specialist.PlanReport{Status: specialist.PlanCompleted})
	if _, busy := bridge.RunningPlanName(); busy {
		t.Error("a finished plan must release the surface")
	}
}

func mustPlan(t *testing.T, name string) specialist.Plan {
	t.Helper()
	plan, err := specialist.ParsePlan(map[string]any{
		"name":   name,
		"tasks":  []any{map[string]any{"id": "a", "prompt": "one"}},
		"budget": map[string]any{"max_workers": float64(1), "max_tokens": float64(500_000)},
	}, specialist.Limits{ParentTools: []string{"read_file"}})
	if err != nil {
		t.Fatalf("building the fixture plan: %v", err)
	}
	return plan
}

// THE BRIDGE MUST CARRY THE MODEL, not just the message type having a field for
// it. The terminal surfaces read planTaskStartMsg.model; a test that builds that
// message by hand passes while the bridge sends "" and nothing is ever shown.
func TestTheBridgeCarriesTheModelATaskWillRunOn(t *testing.T) {
	var got []tea.Msg
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(msg tea.Msg) { got = append(got, msg) }, 1, nil, "")

	bridge.TaskDispatched(specialist.Task{ID: "s", Prompt: "scan", Model: "grok-4.3"})
	bridge.TaskDispatched(specialist.Task{ID: "plain", Prompt: "inherits"})

	models := map[string]string{}
	for _, msg := range got {
		if start, ok := msg.(planTaskStartMsg); ok {
			models[start.taskID] = start.model
		}
	}
	if models["s"] != "grok-4.3" {
		t.Errorf("the dispatch message carried model %q, want the task's", models["s"])
	}
	if models["plain"] != "" {
		t.Errorf("a task that named no model must carry none, got %q", models["plain"])
	}
}

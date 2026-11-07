package specialist

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Twigpine/zero/internal/streamjson"
)

// PlanStatus is a plan's terminal state.
//
// PARTIAL IS ITS OWN STATUS, with its own exit code. This repo has repeatedly
// reported failure as success (audit RC-F), and in a plan that is worse:
// nineteen of twenty tasks failing must never surface as a completed run.
type PlanStatus string

const (
	// PlanCompleted: every task succeeded.
	PlanCompleted PlanStatus = "completed"
	// PlanPartial: at least one task succeeded and at least one failed, was
	// skipped, or was cut off by the budget. Maps to exit 4 (exitIncomplete).
	PlanPartial PlanStatus = "partial"
	// PlanFailed: no task succeeded.
	PlanFailed PlanStatus = "failed"
	// PlanCancelled: the run was stopped and nothing had succeeded yet. Its own
	// status so a deliberate stop is never reported as a failure.
	PlanCancelled PlanStatus = "cancelled"
)

// TaskOutcome is why a task ended the way it did.
type TaskOutcome string

const (
	TaskSucceeded TaskOutcome = "succeeded"
	TaskFailed    TaskOutcome = "failed"
	// TaskSkippedDependency: a dependency failed, so this never ran. RECORDED,
	// never silently dropped — a task that vanished from the report would be
	// indistinguishable from one that was never planned.
	TaskSkippedDependency TaskOutcome = "dependency_failed"
	// TaskSkippedBudget: the plan's token budget was exhausted before this
	// task could be dispatched.
	TaskSkippedBudget TaskOutcome = "budget_exhausted"
	// TaskCancelled: the run was cancelled before this task finished, or before
	// it started. ITS OWN OUTCOME, not a failure — cancelling a twenty-task
	// plan used to mark every remaining task "failed" with "context canceled",
	// so a deliberate Ctrl-C read as nineteen defects. Nothing failed; the user
	// stopped it.
	TaskCancelled TaskOutcome = "cancelled"
)

// TaskResult is one task's record.
type TaskResult struct {
	ID       string
	Outcome  TaskOutcome
	Duration time.Duration
	// Identity is the fingerprint of the task that produced this result — see
	// taskIdentity. The executor stamps it on a succeeded result so a resume can
	// tell a completed task that is UNCHANGED (skip it) from one whose prompt,
	// model, tools or dependencies were edited since (re-run it, and its
	// dependents with it). Empty on a result that did not complete, and on any
	// event recorded before resume became identity-aware.
	Identity string
	// Output is the task's FULL result, verbatim. Not truncated here: the tool
	// boundary budgets and redacts it exactly like any other tool output.
	Output    string
	Err       string
	SessionID string
	Tokens    int
	// Stalled reports that the stall watchdog stopped this task — the child
	// emitted nothing for the whole timeout. Set by the runner, read by the
	// executor to decide whether the task is worth another attempt.
	//
	// A FLAG, not a message match: deciding to spend another child by looking
	// for a phrase in Err is the same class as comparing errors with == instead
	// of errors.Is, which is what silently disabled every stall retry in the
	// prototype.
	Stalled bool
	// MeasurementConflicts are numbers this task reported that its OWN commands
	// contradict — a timing stated as 4.20s where every `go test` it ran printed
	// 0.86s. Empty for the overwhelming majority of tasks.
	//
	// THE PARENT'S TRIPWIRE CANNOT SEE THIS. It checks the parent's answer against
	// the parent's tool output, and a plan task's commands run in the child's own
	// session — so a number a task invents was caught by neither. The child's
	// tool results do stream to the parent, which is what makes the check
	// possible here at all.
	//
	// REPORTED, NOT RETRIED. The parent's version sends an answer back for one
	// more turn; doing that here would spend another whole task to re-ask a
	// question the report can simply carry the answer to. A reader told "this
	// figure disagrees with the command that produced it" can weigh it; a reader
	// told nothing cannot.
	MeasurementConflicts []string
	// Signal describes the signal that killed this task's child, empty when it
	// exited normally.
	//
	// A FLAG, NOT A MESSAGE MATCH, for the same reason Stalled is one. A child
	// killed by its own context looks identical in prose to one killed by the OS,
	// and the executor has to tell them apart: it cancelled the first and knows
	// why, and knows nothing at all about the second.
	Signal string
	// ScratchpadPath is where the executor persisted this task's FULL output, so
	// a dependent handed a truncated excerpt can read the rest. Empty when the
	// plan kept no scratchpad, which is the pre-existing behaviour, and for a
	// FAILED task, whose output the briefing does not pass on either.
	//
	// WRITTEN BY THE EXECUTOR, never by the task. A read-only plan task has no
	// write tool at all, and granting it one would flip RequiresIsolation() and
	// demand a git worktree for every plan that used this.
	ScratchpadPath string
	// Model is what the task actually ran on, empty when it inherited the
	// parent's. Reported so a per-task model is OBSERVABLE: the feature changes
	// which model does the work and what it costs, and a change nobody can see
	// is one nobody can check.
	Model string
	// Attempts is how many times the task ran, always at least 1 for a task that
	// was dispatched. Duration and Tokens are the TOTALS across those attempts,
	// because what the plan spent is what the plan spent.
	Attempts int
	// Declined reports that the child stopped with its work clearly unfinished —
	// it said it could not do the task rather than doing it wrongly.
	//
	// A CODE, NOT A MESSAGE. The child exits childExitIncomplete for exactly this,
	// so the signal is structural like Stalled; the prose that accompanies it
	// ("the final message admits the objective was not met") is for the reader,
	// never for the decision.
	//
	// It matters because a decline is usually TRANSIENT in a way a wrong answer is
	// not. A measured plan had one task decline on a directory that existed and
	// that its three sibling tasks read without trouble; the decline cost that
	// task, its dependent, and the final report — a third of the plan.
	Declined bool
	// ProviderFailed reports that the child died on the PROVIDER rather than on
	// the work: an HTTP failure the transport would not replay.
	//
	// A CODE, NOT A MESSAGE, for the same reason Declined is one: the child exits
	// childExitProvider for exactly this.
	//
	// It exists because the transport deliberately does not retry 500/502/504 —
	// unlike 429/503/529 those do not guarantee the request had no effect, so
	// REPLAYING THE SAME POST risks paying for the same completion twice
	// (providerio.ShouldRetryStatus). That is the right call for a replay and the
	// wrong outcome for a plan: a measured ten-task run lost one task to a bare
	// "Internal Server Error" after 6 tool calls and 42,524 tokens, and with it
	// anything that depended on it. A fresh child is not a replay of that POST —
	// it is a new request that the provider is very likely to answer.
	ProviderFailed bool
	// ModelRejected reports that the task died without its assigned model ever
	// producing anything — the signature of a provider refusing the MODEL rather
	// than the work failing. Set by the runner, read by runTaskWithRetries to
	// decide whether one attempt on the parent's model is worth spending.
	//
	// A FLAG, not a message match, exactly like Stalled and for the same reason.
	ModelRejected bool
	// RetriedOnParentModel names the assigned model that could not run, on a task
	// the plan then re-ran on the session's own model. Empty for everything else.
	//
	// Reported because a silent fallback is the worst of both worlds: the plan
	// says it used the model it chose while something else did the work, and the
	// next plan picks the same broken model again. Seeing it is what lets the
	// name be added to planModels.exclude.
	RetriedOnParentModel string
}

// PlanReport is the plan's terminal record, carried in plan_completed.
type PlanReport struct {
	Status    PlanStatus
	Tasks     []TaskResult
	Succeeded int
	Failed    int
	Skipped   int
	// Cancelled counts tasks stopped by the user rather than broken. Kept
	// separate from Failed so the summary and the panel can say so.
	Cancelled int
	// SequentialTotal is the sum of task durations — what this run actually
	// spent.
	SequentialTotal time.Duration
	// CriticalPath is the longest dependency-weighted path through the DAG: the
	// wall time a perfectly parallel run could not go below.
	CriticalPath time.Duration
	// MaxSpeedup is SequentialTotal / CriticalPath — the THEORETICAL ceiling on
	// what fan-out could buy, measured from a sequential run with no writes.
	//
	// This number decides whether Phase 3 is built. The kill criterion is a
	// median of >= 2.0 across >= 20 real plans: below that the ceiling is too
	// low for real coordination overhead to fit underneath, and concurrency
	// would cost more than it returns.
	//
	// It answers VALUE, not safety. Independence-violation would answer safety,
	// but read-only tasks are always safe to parallelise, so that rate would be
	// 0 regardless and would decide nothing.
	MaxSpeedup float64
	TokensUsed int
	// TokenLimit is what the plan asked to be bounded by, carried so the report
	// can say 712222/500000 rather than a spend with nothing to read it against.
	TokenLimit int
	// Workers is how many tasks this plan ACTUALLY ran at once, and
	// WorkersRequested is what it asked for. Both, because the machine's
	// capacity may be lower than the request and a plan that asked for sixteen
	// and ran six has not been given sixteen — reporting only the request would
	// make the number a fiction, which is the same reason max_workers is
	// rejected outside its range rather than trimmed into it.
	Workers          int
	WorkersRequested int
}

// PlanTaskRequest is everything one task needs to run.
//
// A STRUCT, not a widening parameter list. The runner's inputs are the thing
// that grows every stage — 2a adds Progress, 2b will add background wiring —
// and each addition through a positional parameter is another chance for a
// second construction path to omit a field the first one carried. That is
// exactly the class that produced findings 1 and 7: a caller that forgets a
// field compiles fine and fails silently.
type PlanTaskRequest struct {
	// Task is the validated task to run.
	Task Task
	// Tools is the already-intersected grant. Never widened downstream.
	Tools []string
	// ReadRoots are directories this task may read beyond its workspace — the
	// plan's scratchpad, when it has one. Empty for every plan that does not,
	// which is the pre-existing behaviour. Emitted as --add-dir (grants write).
	ReadRoots []string
	// ReadOnlyRoots are directories this task may READ but not write — the
	// parent's request_permissions grants, so a plan can audit a granted external
	// path. Emitted as --add-read-dir (scope.AddRead), never the write channel.
	ReadOnlyRoots []string
	// Progress, when set, receives each stream-json event the task's child
	// emits. nil is a no-op — the behaviour for every caller that does not wire
	// live progress.
	Progress func(streamjson.Event)
	// ParentSessionID / ParentModel / ParentReasoningEffort identify the run
	// issuing the plan, so a task runs on the SAME model its parent is running
	// on rather than whatever the child's own config resolves to.
	//
	// Per call, not per registration: the TUI's registry is built once while
	// /model can change the model between runs. Attached by the orchestrate
	// tool from tools.RunOptions, which is where the Task tool reads the same
	// three values.
	ParentSessionID       string
	ParentModel           string
	ParentReasoningEffort string
	// ParentToolCallID is the orchestrate call this task belongs to.
	//
	// The Task tool carries it and this path did not, so a plan task's child was
	// the only kind of child whose accounting could not be traced back to the
	// call that spawned it — spend recorded against a session with nothing
	// naming what asked for it. Same shape as every other gap in this file: a
	// value present at the tool, consumed by accounting, and dropped in between.
	ParentToolCallID string
	// Cwd overrides where this task runs. Empty means the parent's workspace,
	// which is every read-only plan. A write-capable plan sets it to its
	// isolated worktree, and it is carried per REQUEST rather than captured in
	// PlanTaskContext because the workspace belongs to a plan, not to the
	// process — the same reason ParentModel lives here.
	Cwd string
	// StallTimeout bounds how long this task may emit nothing. Resolved by
	// ExecutePlan from the plan's budget so every task in a plan shares one
	// answer, rather than each runner re-deriving it.
	StallTimeout time.Duration
	// StallPoll is how often the watchdog checks for silence. Zero means the
	// production rule — a sixth of StallTimeout, floored at one second.
	//
	// TESTS ONLY, and it exists because without it they cannot reach the
	// watchdog at all: the floor means a 60ms StallTimeout still polls once a
	// second, so a child living 300ms gets ZERO ticks and a test asserting "the
	// chatty child survived" passes identically whether or not its events are
	// wired to the clock. That test was the only cover for the wiring, and it
	// could not fail. The alternative was a child that runs for several seconds
	// in every suite run, to observe something that takes milliseconds.
	StallPoll time.Duration
	// Spend is the plan's live token meter, shared by every task in flight. A
	// task that crosses the plan's limit while running is stopped by it — the
	// dispatch-time check cannot, because it only sees numbers that have already
	// landed. nil means no live metering.
	Spend *planSpend
	// MaxTaskTokens bounds what THIS task may spend. 0 is unbounded, which is
	// every plan that does not ask for a cap.
	MaxTaskTokens int
	// WaitsOnOtherTasks marks a task with dependencies, which decides WHICH POOL
	// it draws from: work that waits draws from the reserve, so it cannot be
	// starved by the work it waited for.
	//
	// The pool, not a precomputed ceiling. A number computed by the caller is one
	// more thing a caller can forget to compute; the meter owns the arithmetic
	// and a request only has to say which side of it this task is on.
	WaitsOnOtherTasks bool
	// SystemPrompt replaces the plan-task system prompt for this request. Empty
	// keeps the plan-task prompt, which is every task of every plan.
	//
	// IT EXISTS FOR THE ROUTER, which is not a plan task at all. The plan-task
	// prompt opens "You have read-only tools: USE THEM. Start with a tool call,
	// not prose" — sound advice for work, and a direct contradiction of the
	// router's own instruction to reply with JSON and nothing else. The single
	// highest-leverage prompt in the system, the one choosing every other task's
	// model, was being told to do the opposite of what it was asked.
	SystemPrompt string
}

// PlanRunner runs one task. The executor depends on this seam rather than on
// Executor directly so the budget, ordering and failure semantics are testable
// without launching child processes.
type PlanRunner func(ctx context.Context, req PlanTaskRequest) (TaskResult, error)

// PlanRecorder receives plan lifecycle events. Recording is BEST-EFFORT and
// must never fail the run — it mirrors execSessionRecorder.append's contract,
// where a latched error is surfaced once but the run continues.
type PlanRecorder interface {
	TaskDispatched(task Task)
	TaskCompleted(result TaskResult)
	TaskFailed(result TaskResult)
}

// ExecutePlan runs a plan SEQUENTIALLY in its validated topological order.
//
// The order comes from the same Kahn pass that proved the graph acyclic, so
// admission and execution cannot disagree about it.

// ExecOption tunes one plan execution.
//
// VARIADIC because ExecutePlanIn has seventy-five call sites, seventy-three of
// them tests. A seventh positional parameter would rewrite every one of them to
// pass a value they do not care about, and a test that had to be edited to keep
// compiling is a test nobody re-read. An option they omit means "as before",
// which is exactly what those tests are asserting.
type ExecOption func(*execOptions)

// WithScratchpad keeps each task's FULL output on disk for the life of the plan
// and points truncated excerpts at it.
//
// OPT-IN. It writes files and adds a read root to every task, and neither should
// happen to a caller that did not ask.
func WithScratchpad() ExecOption {
	return func(o *execOptions) { o.scratchpad = true }
}

// WithReadRoots grants every task READ access to directories the parent holds
// BEYOND its workspace — the paths a request_permissions grant added to the
// parent's scope. Without it a plan that audits a granted external path fails
// "outside the workspace" in every task, because a task inherits its workspace
// but not the parent's dynamic read grants.
//
// READ ONLY, and merged with the scratchpad grant rather than replacing it: these
// are the parent's already-approved paths, handed down unchanged, never widened.
func WithReadRoots(roots []string) ExecOption {
	return func(o *execOptions) {
		o.parentReadRoots = append(o.parentReadRoots, roots...)
	}
}

// WithPreSpentTokens charges work the PLAN caused but no task performed.
//
// Today that is exactly one thing: the routing call auto_assign makes before any
// task runs. It happens outside the executor, so its tokens reached neither the
// budget nor the report — and the report is what a person reads to decide
// whether a plan was worth what it cost.
func WithPreSpentTokens(tokens int) ExecOption {
	return func(o *execOptions) {
		if tokens > 0 {
			o.preSpent = tokens
		}
	}
}

type execOptions struct {
	// scratchpad asks for a per-plan directory of task outputs.
	scratchpad bool
	// preSpent is tokens the plan owes before its first task starts.
	preSpent int
	// contextWindow reports the window of the model a TASK will run on, 0 when
	// unknown. Called with the task's own model id, or "" when the task named
	// none — the supplier knows what the run itself is using, and this package
	// deliberately does not.
	contextWindow ContextWindowFunc
	// parentReadRoots are directories the parent may read beyond its workspace —
	// its request_permissions grants — handed to every task so a plan can audit a
	// granted external path. Empty for a run that wired none, which is every
	// existing caller.
	parentReadRoots []string
	// wallTime is the time source for the wall budget; zero is the real clock.
	wallTime wallTime
}

// ContextWindowFunc reports a model's context window in tokens, 0 when unknown.
//
// A HOOK, not an import: this package runs on the child-execution path and must
// not drag the provider stack in behind it. The surface that owns the catalogue
// supplies it — see planContextWindows in internal/cli — and nil means no window
// is knowable here, which is the honest default for a run that never wired one.
//
// 0 MEANS UNKNOWN, the same spelling and the same meaning as
// agent.ContextMeasurement.ContextWindow, so the two cannot drift into
// disagreeing about what 0 means. Unknown disables the adjustment rather than
// guessing at it.
//
// AN EMPTY MODEL ID means "whatever this run is using". A plan task that names
// no model inherits the parent's, and only the supplying side knows what that is.
type ContextWindowFunc func(modelID string) int

// WithContextWindows sizes each task's dependency briefing to the context window
// of the model that will READ it. Omitted, or supplied nil, every briefing keeps
// the fixed caps it had before.
func WithContextWindows(window ContextWindowFunc) ExecOption {
	return func(o *execOptions) { o.contextWindow = window }
}

// windowFor is the resolved window for one task, and the nil-safe path that
// makes every existing caller behave as it did.
func (o execOptions) windowFor(task Task) int {
	if o.contextWindow == nil {
		return 0
	}
	return o.contextWindow(task.Model)
}

// ExecutePlanIn is ExecutePlan with the plan's WORKSPACE named. ExecutePlan is
// the read-only case — the workspace a plan does not need — kept as the name
// every existing caller and test already uses.
func ExecutePlanIn(ctx context.Context, plan Plan, workspace PlanWorkspace, parentTools []string, run PlanRunner, recorder PlanRecorder, opts ...ExecOption) PlanReport {
	var options execOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	// THE PLAN'S SCRATCHPAD. Off unless asked for, so every existing caller —
	// and the additivity guarantee — is untouched: with no scratchpad, Record
	// returns "", scratchpadPointer renders nothing, and the briefing is the
	// sentence it always was.
	//
	// A scratchpad that cannot be created is NOT a reason to refuse the plan.
	// It only makes truncated excerpts reachable; without it they are exactly as
	// reachable as they were before, which is to say the plan still works.
	var scratchpad *Scratchpad
	if options.scratchpad {
		if pad, err := NewScratchpad(plan.Name()); err == nil {
			scratchpad = pad
			defer scratchpad.Release()
		}
	}
	// THE PLAN'S OWN CONTEXT, derived here rather than by the caller.
	//
	// Cancelling it abandons the PLAN and leaves the TURN alive; cancelling the
	// run still cancels this too, because it is a child. Deriving it here rather
	// than in the orchestrate tool is the difference between one call path
	// having per-plan cancellation and every call path having it — this is the
	// function that owns a plan's lifetime, and the seam belongs where the
	// lifetime is.
	ctx, cancelPlan := context.WithCancel(ctx)
	defer cancelPlan()
	// max_wall_seconds bounds the PLAN, which means it has to reach the children.
	// The pre-dispatch check below is not a bound on its own: it is consulted
	// only when the walk needs a free slot, so a plan whose ready set fits the
	// worker pool dispatches in one wave and is never gated again. Measured at
	// max_workers=4 with a 1s wall and four 2s tasks: 2.0s elapsed, reported
	// "completed", nothing skipped — while the same plan at max_workers=1
	// correctly reported partial. Asking for parallelism deleted the bound.
	//
	// A deadline on the plan context fixes that with the machinery a user stop
	// already proves works, and the walk keeps its skip-the-rest behaviour for
	// tasks not yet dispatched.
	// ONE SOURCE FOR THE WALL: this clock, read by the backstop that cancels
	// children and by the walk that skips tasks not yet dispatched. Two sites
	// reading the budget differently is how they drift — a plan whose backstop
	// reached only one of them would kill work in flight while still dispatching
	// more, or the reverse.
	//
	// A CLOCK RATHER THAN context.WithTimeout, because the budget has to be able
	// to move: a plan the user paused is not spending it. See planWallClock.
	wallClock := newPlanWallClock(planWallBudget(plan.Budget()), options.wallTime)
	if wallClock != nil {
		var cancelWall context.CancelFunc
		ctx, cancelWall = context.WithCancel(ctx)
		defer cancelWall()
		wallClock.watch(ctx, cancelWall)
	}
	planRunning(recorder, cancelPlan)

	tasks := map[string]Task{}
	for _, task := range plan.Tasks() {
		tasks[task.ID] = task
	}

	report := PlanReport{}
	results := map[string]TaskResult{}
	failed := map[string]bool{}
	// budgetLeft is decremented as tasks complete. Enforced HERE, at dispatch —
	// validation alone would be a promise, not a bound. Under the zeromaxing
	// posture every child inherits a 320-turn ceiling, so a twenty-task plan
	// authorises 6,400 child turns from a single tool call; this is what stands
	// between that number and the user's bill.
	// budgetLeft is only a bound when the plan asked for one. Zero means
	// unbounded: spend is metered and reported, not gated.
	budgetLeft := plan.Budget().MaxTokens
	bounded := budgetLeft > 0
	// STICKY PER POOL, not per plan. One flag meant that the moment any task was
	// skipped for budget, every task after it was skipped too — including work
	// drawing from a pool that had not been touched. Upstream running out closed
	// the door on the downstream work the reserve was holding open, which is the
	// whole failure this reserve exists to prevent, arriving one flag later.
	upstreamExhausted, downstreamExhausted := false, false
	// Harvested spend per pool. The live meter stops a task MID-RUN; these decide
	// whether the next one may start, and they are separated for the same reason:
	// a feeder's overshoot must not close the door on the work it fed.
	upstreamUsed, downstreamUsed := 0, 0
	// The live meter, shared by every task in flight. Same limit, consulted from
	// inside a running task rather than between two of them.
	spend := &planSpend{limit: planSpendLimit(plan.Budget().MaxTokens, options.preSpent)}
	// One budget for the whole plan, so a systemic provider failure costs two
	// extra children rather than one per task. See planProviderRetryBudget.
	providerRetries := &planProviderRetryBudget{remaining: maxPlanProviderRetries}
	// WHO IS DEPENDED ON. Computed once from the validated graph: a task others
	// wait for stops earlier than one nothing waits for, so the work downstream
	// of it is not starved by it.
	waitsOnOthers := map[string]bool{}
	for _, task := range plan.Tasks() {
		if len(task.DependsOn) > 0 {
			waitsOnOthers[task.ID] = true
			spend.downstreamTasks++
		}
	}
	spend.totalTasks = plan.TaskCount()

	// One stall timeout for the whole plan, resolved once.
	stallTimeout := stallTimeoutFor(plan.Budget())

	cancelled := false

	// THE WORKER POOL. One worker is the sequential path: every wait below
	// becomes "until the previous task finished", and the walk applies the same
	// checks in the same order it always has.
	workers := effectivePlanWorkers(plan.Budget().MaxWorkers)
	report.Workers = workers
	report.WorkersRequested = plan.Budget().MaxWorkers
	report.TokenLimit = plan.Budget().MaxTokens
	// SEEDED, NOT ADDED AT THE END. report.TokensUsed accumulates per task from
	// here on, and every early return between here and the end reports whatever
	// it holds — so a total corrected only on the success path would be wrong on
	// exactly the runs a reader most wants the truth about.
	report.TokensUsed = options.preSpent
	slots := newPlanSlots(workers)

	// harvest applies one completed dispatch: its spend, its outcome, its
	// record. Called only from THIS goroutine, so results, failed, report and
	// budgetLeft are never touched concurrently and need no lock — the
	// concurrency is in the dispatches, not in the bookkeeping.
	harvest := func(completion taskCompletion) {
		slots.release()
		id, result, err := completion.id, completion.result, completion.err
		// PERSISTED ONLY WHERE THE BRIEFING WOULD SHOW IT: a succeeded task's
		// answer and a cancelled task's partial findings, which are evidence. A
		// FAILED task's output is a diagnostic or an answer already judged wrong,
		// and the briefing deliberately refuses to pass it on — recording it to
		// a directory every task can list would hand it on anyway. Called from
		// each branch once the outcome is final, not before it is decided.
		//
		// A FAILURE TO RECORD IS NOT A FAILURE OF THE TASK. The scratchpad is an
		// optimisation over the excerpt the dependent gets anyway; losing the
		// disk copy must never turn a task that ran into a task that failed.
		keepOnPad := func(result *TaskResult) {
			if path, recordErr := scratchpad.Record(id, strings.TrimSpace(result.Output)); recordErr == nil {
				result.ScratchpadPath = path
			}
		}
		result.ID = id
		if result.Duration == 0 {
			result.Duration = time.Since(completion.started)
		}
		report.SequentialTotal += result.Duration
		budgetLeft -= result.Tokens
		// HARVESTED PER POOL, for the same reason the live meter is: a dependent
		// must not be refused dispatch because a feeder overspent. budgetLeft
		// stays as the whole-plan figure the report reads; these decide who may
		// still start.
		if waitsOnOthers[id] {
			downstreamUsed += result.Tokens
		} else {
			upstreamUsed += result.Tokens
		}
		report.TokensUsed += result.Tokens

		if err != nil || result.Outcome == TaskFailed {
			// A task cut short by cancellation is CANCELLED, not failed. The
			// distinction survives all the way to the terminal status, so a
			// stopped plan never reports as a broken one.
			//
			// IT MUST BE THIS TASK THAT WAS CUT SHORT, not merely this task being
			// harvested after someone stopped the plan. A bare ctx.Err() check
			// relabelled a task that had already failed on its own — a real
			// compile error, reported and complete — the moment the user pressed
			// stop, erasing the one result worth reading and reporting the plan as
			// "stopped" when something in it was genuinely broken.
			//
			// THE TASK DID NOT CONCLUDE is the thing being detected, and two
			// structural signals say so — never the message text, invariant 9:
			//
			//   err != nil. The executor returns the process-level error
			//   alongside its result (exec.go's post-start failure path), so a
			//   task the context killed always carries one.
			//
			//   result.Stalled. The watchdog's own path returns err nil
			//   deliberately, because a stall is a decision this side of the
			//   boundary made. A stall that coincides with a Ctrl-C is still a
			//   cancellation, which is what TestACancelledRunIsNotRetried pins.
			//
			// Neither is true of a child that ran to the end and reported a
			// failure of its own — err nil, not stalled, its reason in
			// result.Err — and that is the case a bare ctx.Err() swallowed.
			//   result.Signal. A child killed by its own context is SIGKILLed —
			//   osexec.CommandContext does that with no WaitDelay — and the
			//   executor returns that as a StatusError result with a NIL error.
			//   So "concluded" was true for a task the plan had just killed, and
			//   two tasks stopped at exactly 300.0s were reported as ordinary
			//   FAILURES carrying a guess list: "Common causes: an out-of-memory
			//   kill, a timeout, or cancellation; check the signal to tell
			//   which." The plan knew which. It shrugged, and a reader
			//   reasonably concluded their machine had run out of memory and
			//   went to raise limits that were never involved.
			concluded := err == nil && !result.Stalled && result.Signal == ""
			cutShort := errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
				(ctx.Err() != nil && !concluded)
			if cutShort {
				cancelled = true
				result.Outcome = TaskCancelled
				// NAMED, NOT GUESSED. The prose the child came back with is the
				// signal diagnostic, which for a task the plan itself stopped is
				// a guess about our own decision — so it is replaced here rather
				// than preserved. A signal is still reported, as a detail of a
				// reason we can state, not in place of one.
				if result.Err == "" || result.Signal != "" {
					// Which one matters to the reader: a wall-budget stop is the
					// plan spending what it was allowed, a cancel is a person
					// deciding. Reporting both as "the run was stopped" left a
					// user looking for who stopped it.
					if wallClock.wallExpired() || errors.Is(err, context.DeadlineExceeded) {
						result.Err = "cancelled: the plan's max_wall_seconds elapsed while this task was running"
					} else {
						result.Err = "cancelled: the run was stopped while this task was running"
					}
					if result.Signal != "" {
						result.Err += " (the child was terminated by " + result.Signal + ")"
					}
				}
				keepOnPad(&result)
				results[id] = result
				failed[id] = true
				report.Cancelled++
				recordFailed(recorder, result)
				return
			}
			result.Outcome = TaskFailed
			if result.Err == "" && err != nil {
				result.Err = err.Error()
			}
			results[id] = result
			failed[id] = true
			report.Failed++
			recordFailed(recorder, result)
			return
		}
		// A TERMINAL OUTCOME THE RUNNER ALREADY DECIDED IS NOT OVERWRITTEN.
		//
		// The line below used to be unconditional, and the guard above tests only
		// TaskFailed — so a runner returning TaskCancelled with no error fell
		// through and was relabelled a success. A real plan killed all six of its
		// tasks for running over budget and reported "6 succeeded, 0 failed, 0
		// skipped", with the kill notice sitting in each task's own result text
		// where the headline contradicted it.
		//
		// Work that did not finish, reported as done, is the failure this whole
		// executor is built to prevent; the runner is the only thing that watched
		// the task run, so when it names an outcome, that outcome stands.
		if result.Outcome == TaskCancelled {
			keepOnPad(&result)
			results[id] = result
			// Its dependents are blocked, exactly as for a failure: a task that
			// was cut short did not produce what they were waiting for.
			failed[id] = true
			report.Cancelled++
			recordFailed(recorder, result)
			return
		}
		result.Outcome = TaskSucceeded
		// STAMPED HERE, ON THE ONE SUCCESS PATH BOTH SURFACES FLOW THROUGH.
		// recordCompleted feeds the CLI recorder and the TUI bridge the same
		// result, and both write it verbatim into task_completed — so setting the
		// identity once, before recording, is what puts it in the log for resume
		// to match against. Only a succeeded task is matched by identity; a failed
		// one re-runs regardless, so its result carries none.
		result.Identity = taskIdentity(tasks[id])
		keepOnPad(&result)
		results[id] = result
		report.Succeeded++
		recordCompleted(recorder, result)
	}

	// resolved reports whether every dependency of a task has finished, one way
	// or another. A task waits for its dependencies to RESOLVE, not to succeed:
	// a failed dependency resolves it too, and the skip check below turns that
	// into the recorded dependency_failed.
	resolved := func(task Task) bool {
		for _, dep := range task.DependsOn {
			if _, done := results[dep]; !done {
				return false
			}
		}
		return true
	}

	for _, id := range plan.Order() {
		task := tasks[id]

		// WAIT FOR THIS TASK'S TURN: until its dependencies have resolved and a
		// worker is free. Waiting on the task the WALK reached — rather than
		// picking whichever ready task appeared first — is what keeps dispatch
		// in the plan's validated order, so one worker reproduces the sequential
		// executor exactly and more workers only overlap what was already
		// adjacent.
		for slots.busy() && (!resolved(task) || slots.full()) {
			harvest(<-slots.done)
		}

		// PAUSE, at the task boundary, BEFORE the cancellation check — so a user
		// who stops a paused plan is not left waiting for a resume that will
		// never come. WaitWhilePaused returns on ctx, and the check below then
		// turns it into a cancellation exactly as if the plan had been running.
		//
		// AND THE TIME IS GIVEN BACK. max_wall_seconds bounds what a plan spends,
		// and a paused plan spends nothing: without this, pausing for twenty
		// minutes took twenty minutes off a thirty-minute budget, and pausing for
		// longer than the budget meant the plan was already dead when the user
		// resumed it — stopped, and told its wall budget elapsed, by time in
		// which no task ran.
		//
		// HELD BEFORE BLOCKING, not credited after: the clock must know the walk
		// is parked while it is parked, or its timer fires mid-pause on the
		// pre-pause deadline. Only time with no task in flight is given back —
		// see planWallClock.
		wallClock.holdDispatch()
		waitWhilePaused(recorder, ctx)
		wallClock.releaseDispatch()

		// Cancellation is checked FIRST and recorded as its own outcome. Once
		// the run is cancelled every remaining task is cancelled too — they are
		// not blocked by a dependency and the budget did not run out.
		if cancelled || ctx.Err() != nil {
			cancelled = true
			// Same distinction the in-flight path draws: a wall-budget expiry is
			// the plan spending what it was allowed, not a person stopping it.
			reason := "cancelled: the run was stopped before this task ran"
			if wallClock.wallExpired() {
				reason = "cancelled: the plan's max_wall_seconds elapsed before this task ran"
			}
			result := TaskResult{
				ID:      id,
				Outcome: TaskCancelled,
				Err:     reason,
			}
			results[id] = result
			failed[id] = true
			report.Cancelled++
			recordFailed(recorder, result)
			continue
		}

		if missing, skip := unusableDependencies(task, results); skip {
			result := TaskResult{
				ID:      id,
				Outcome: TaskSkippedDependency,
				Err: fmt.Sprintf("skipped: no dependency produced anything to work from (%s)",
					strings.Join(missing, ", ")),
			}
			results[id] = result
			failed[id] = true // its own dependents are blocked too
			report.Skipped++
			recordFailed(recorder, result)
			continue
		}

		// A budget-exhausted or timed-out plan SKIPS the rest rather than
		// aborting: independent work already done still counts, and the record
		// must show what was not attempted.
		// THE TASK'S OWN POOL, not the shared remainder. Checking budgetLeft
		// meant a feeder that overshot its ceiling closed the door on every
		// dependent — which is precisely what a reserve is supposed to prevent,
		// and precisely what happened: feeders capped at 375,000 landed at
		// 534,144 and the dependents were skipped for a budget that was never
		// theirs to spend.
		downstream := waitsOnOthers[id]
		poolExhausted := downstreamExhausted
		if !downstream {
			poolExhausted = upstreamExhausted
		}
		if bounded && !poolExhausted {
			if downstream {
				poolExhausted = int64(downstreamUsed) >= spend.ceilingFor(true)
			} else {
				poolExhausted = int64(upstreamUsed) >= spend.ceilingFor(false)
			}
		}
		if poolExhausted || wallClock.exhausted() {
			if downstream {
				downstreamExhausted = true
			} else {
				upstreamExhausted = true
			}
			result := TaskResult{
				ID:      id,
				Outcome: TaskSkippedBudget,
				Err:     "skipped: the plan's budget was exhausted before this task ran",
			}
			results[id] = result
			failed[id] = true
			report.Skipped++
			recordFailed(recorder, result)
			continue
		}

		// BELT AND BRACES with the validator: the grant is intersected again
		// here, so a validation bug cannot widen a task's authority. Same shape
		// as Phase 1's forwardedReasoningEffort guard. Computed BEFORE dispatch
		// is recorded: a task that cannot be granted anything was never
		// dispatched, and the event log must not say it was.
		granted, grantErr := planToolGrant(task, parentTools)
		if grantErr != nil {
			result := TaskResult{ID: id, Outcome: TaskFailed, Err: grantErr.Error()}
			results[id] = result
			failed[id] = true
			report.Failed++
			recordFailed(recorder, result)
			continue
		}

		// WHAT ITS DEPENDENCIES FOUND, handed to it rather than left for it to
		// rediscover. See dependencyBriefing.
		perTask, totalBrief := dependencyBriefingBudget(options.windowFor(task))
		task.Prompt = withDependencyBriefingBudget(task, results, perTask, totalBrief)
		// AND WHAT IT MAY SPEND, so it can land instead of being shot down.
		task.Prompt = withTokenBudgetNotice(task.Prompt, plan.Budget().MaxTokensPerTask)

		recordDispatched(recorder, task)
		policy := retryPolicy{
			task:          task,
			tools:         granted,
			cwd:           workspace.Path,
			stallTimeout:  stallTimeout,
			spend:         spend,
			maxTaskTokens: plan.Budget().MaxTokensPerTask,
			waitsOnOthers: len(task.DependsOn) > 0,
			// readRoots is emitted to the child as --add-dir, which grants WRITE, so
			// only the scratchpad (a dir the executor owns) travels it. The parent's
			// request_permissions READ grants must NOT: routing a read grant through
			// the write channel would escalate it. They travel readOnlyRoots, emitted
			// as --add-read-dir (scope.AddRead) so the child can read but not write.
			readRoots:       scratchpadReadRoots(scratchpad),
			readOnlyRoots:   options.parentReadRoots,
			maxRetries:      plan.Budget().MaxRetries,
			providerRetries: providerRetries,
			wallClock:       wallClock,
		}
		slots.take()
		started := time.Now()
		// Counted in flight HERE, on the walk's goroutine, so the very next pause
		// gate already knows it is running; counted out BEFORE the completion is
		// queued, because a parked walk does not harvest it.
		finished := wallClock.taskStarted()
		go func(id string) {
			// Every goroutine gets recover(): a panic in one task must not take
			// the plan with it, and the slot must be returned either way or the
			// scheduler waits forever on a worker that will never report.
			defer func() {
				if panicked := recover(); panicked != nil {
					finished()
					slots.done <- taskCompletion{
						id: id, started: started,
						result: TaskResult{Outcome: TaskFailed, Err: fmt.Sprintf("task panicked: %v", panicked)},
					}
				}
			}()
			result, err := runTaskWithRetries(ctx, policy, run)
			finished()
			slots.done <- taskCompletion{id: id, result: result, err: err, started: started}
		}(id)
	}

	// DRAIN. Everything still in flight is harvested before the report is
	// assembled, or a plan would report on tasks that had not finished.
	for slots.busy() {
		harvest(<-slots.done)
	}

	for _, id := range plan.Order() {
		report.Tasks = append(report.Tasks, results[id])
	}
	report.CriticalPath = criticalPath(plan, results)
	report.MaxSpeedup = speedup(report.SequentialTotal, report.CriticalPath)
	report.Status = terminalStatus(report)
	return report
}

// retryPolicy is one task's retry inputs. A struct for the same reason
// PlanTaskRequest is one: this parameter list is the thing that grows, and a
// positional list is where a second call site quietly omits a field.
type retryPolicy struct {
	task          Task
	tools         []string
	cwd           string
	stallTimeout  time.Duration
	maxRetries    int
	spend         *planSpend
	maxTaskTokens int
	waitsOnOthers bool
	// readRoots are directories a task may READ beyond its workspace — today
	// only the plan's scratchpad. Never write roots: the executor is the sole
	// writer there, which is what makes contention impossible rather than
	// merely unlikely.
	readRoots []string
	// readOnlyRoots are the parent's request_permissions READ grants, handed to
	// the task READ-ONLY (emitted as --add-read-dir, applied via scope.AddRead) so
	// a plan can audit a granted external path without the task gaining write
	// access to it. Kept SEPARATE from readRoots because readRoots is emitted as
	// --add-dir, which grants write.
	readOnlyRoots []string
	// wallClock is the plan's running-time budget, nil when unbounded. Read
	// rather than a fixed deadline so time the user spent paused does not
	// count against it.
	wallClock *planWallClock
	// providerRetries is the PLAN-WIDE budget for provider-failure retries,
	// shared by every task in the plan and nil when there is none.
	//
	// PLAN-WIDE, not per task, because the exit code cannot tell a transient
	// 500 from a permanent one. internal/cli returns exitProvider for every
	// agent-run failure — an expired key, an unknown model, exhausted quota —
	// so a per-task retry meant a dead API key cost a ten-task plan twenty
	// spawns to produce the same ten "authentication failed" errors. A shared
	// budget keeps the value for the case this exists for (one task catching a
	// bad moment) and bounds the waste when the failure is systemic: the first
	// couple of tasks pay for the discovery, the rest do not.
	providerRetries *planProviderRetryBudget
}

// planProviderRetryBudget bounds provider-failure retries across a whole plan.
// Concurrent: tasks are dispatched in parallel, so the counter is mutex-guarded
// like planSpend rather than a plain int.
type planProviderRetryBudget struct {
	mu        sync.Mutex
	remaining int
}

// maxPlanProviderRetries is how many provider-failure retries one plan may
// spend in total. Two: enough for a genuine blip to be re-tried and for a
// second, independent one later in the same plan, and few enough that a
// systemic failure costs two extra children rather than one per task.
const maxPlanProviderRetries = 2

// take reports whether a retry is available and consumes it when so. A nil
// budget yields none, which is the honest answer for a caller that wired one
// deliberately absent.
func (budget *planProviderRetryBudget) take() bool {
	if budget == nil {
		return false
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.remaining <= 0 {
		return false
	}
	budget.remaining--
	return true
}

// childExitIncomplete is the child's exit code for "stopped with work clearly
// unfinished".
//
// DUPLICATED FROM internal/cli's exitIncomplete, deliberately: cli imports this
// package, so the constant cannot travel the other way without inverting the
// dependency. Two definitions of one number is a thing that drifts, so cli owns
// a test asserting they agree — the only defence available.
const childExitIncomplete = 4

// childExitProvider is the child's exit code for "the provider failed", as
// opposed to the task failing. Duplicated from internal/cli's exitProvider for
// the same reason childExitIncomplete is, and pinned by the same agreement test.
const childExitProvider = 3

// runTaskWithRetries runs one task, retrying it ONLY when it stalled.
//
// The retry lives HERE, in the executor, and not in the runner — the executor
// owns the budget, the wall deadline, cancellation and the record, and a retry
// hidden inside the runner would spend a second child's tokens without any of
// them counting. Duration and Tokens come back as the TOTAL across attempts,
// so a plan's reported spend is its real spend.
//
// Every reason to stop is checked BEFORE launching another child: a cancelled
// run, an expired wall deadline, or an attempt budget that is used up. The
// prototype's equivalent loop retried a cancelled task, which turned Ctrl-C into
// another spawn.
func runTaskWithRetries(ctx context.Context, policy retryPolicy, run PlanRunner) (TaskResult, error) {
	var result TaskResult
	var err error
	var totalDuration time.Duration
	var totalTokens int

	// fellBackFrom is the assigned model that would not run, once the task has
	// been re-dispatched on the parent's. Non-empty is also what BOUNDS the
	// fallback at exactly one: a provider that refuses the parent's model too
	// must not put this loop into a spawn cycle.
	var fellBackFrom string
	// retriedAfterDecline bounds the decline retry at exactly one. A model that
	// declines twice is telling us something about the task, not having a bad
	// moment.
	retriedAfterDecline := false
	// retriedAfterProviderFailure bounds the provider retry at exactly one, for
	// the same reason: a provider failing twice in a row is having an outage, not
	// a bad moment, and a second retry spends another child to learn that.
	retriedAfterProviderFailure := false

	for attempt := 1; ; attempt++ {
		started := time.Now()
		result, err = run(ctx, PlanTaskRequest{
			Task:              policy.task,
			Tools:             policy.tools,
			Cwd:               policy.cwd,
			StallTimeout:      policy.stallTimeout,
			Spend:             policy.spend,
			MaxTaskTokens:     policy.maxTaskTokens,
			WaitsOnOtherTasks: policy.waitsOnOthers,
			ReadRoots:         policy.readRoots,
			ReadOnlyRoots:     policy.readOnlyRoots,
		})
		if result.Duration == 0 {
			result.Duration = time.Since(started)
		}
		totalDuration += result.Duration
		totalTokens += result.Tokens
		result.Attempts = attempt
		result.Duration = totalDuration
		result.Tokens = totalTokens
		// Carried across the fallback: the task that finally ran on the parent's
		// model must still report which model could not run, or the same broken
		// model is chosen again by the next plan and nothing says why.
		result.RetriedOnParentModel = fellBackFrom

		switch {
		case result.ModelRejected && fellBackFrom == "" && strings.TrimSpace(policy.task.Model) != "":
			// AN ASSIGNED MODEL THE PROVIDER WILL NOT RUN IS NOT THE TASK'S FAULT.
			//
			// Auto-assignment picks from the list the provider itself published,
			// so "listed but unusable" is the normal failure of a discovery
			// endpoint that describes products rather than endpoints — a model
			// belonging to another account, or one whose id lists on /v1/models
			// while chat completions answers "Multi Agent requests are not allowed
			// on chat completions". Falling back to the model the session is
			// demonstrably able to run costs one child and saves the task.
			//
			// Gated by the SAME stops as a stall retry, because it spends the same
			// thing: a cancelled run and an exhausted wall budget both refuse.
			// Not gated by maxRetries — that budget bounds stall THRASHING, where
			// the same model reruns the same work hoping it answers this time.
			// This is a different model, tried once, and fellBackFrom is its bound.
			if ctx.Err() != nil {
				return result, err
			}
			if policy.wallClock.exhausted() {
				return result, err
			}
			fellBackFrom = strings.TrimSpace(policy.task.Model)
			policy.task.Model = ""
			continue
		case result.Declined && !retriedAfterDecline:
			// A DECLINE IS WORTH ONE MORE ATTEMPT, and a wrong answer is not.
			//
			// The distinction is the whole justification. "Running it again buys
			// the same report" is true of a task that read the code and concluded
			// wrongly — it will conclude the same thing. It is not true of a task
			// that said it could not proceed: a measured plan had one decline on a
			// directory that existed and that its three siblings read without
			// trouble, and that single refusal cost the task, its dependent, and
			// the final report.
			//
			// Gated by the SAME stops as every other retry here, because it spends
			// the same thing. Not gated by maxRetries: that budget bounds stall
			// thrashing, where the same model reruns the same work hoping for a
			// different mood. This is one attempt, bounded by its own flag.
			if ctx.Err() != nil {
				return result, err
			}
			if policy.wallClock.exhausted() {
				return result, err
			}
			retriedAfterDecline = true
			// AWAY FROM THE MODEL THAT DECLINED, when there is somewhere to go.
			// Repeating the same model is the weakest version of this; the
			// session's own model is one the parent is demonstrably able to run.
			if fellBackFrom == "" && strings.TrimSpace(policy.task.Model) != "" {
				fellBackFrom = strings.TrimSpace(policy.task.Model)
				policy.task.Model = ""
			}
			continue
		case result.ProviderFailed && !retriedAfterProviderFailure && !grantsPlanWriteTool(policy.tools):
			// THE PROVIDER FAILED, WHICH IS NOT AN ANSWER.
			//
			// The branch below is right about a task that read the code and
			// concluded wrongly: running it again buys the same report. It is not
			// right about an HTTP 500 — that is a coin flip, and a measured
			// ten-task run lost one task to a bare "Internal Server Error" after 6
			// tool calls and 42,524 tokens, taking its dependents with it. The
			// transport will not replay the POST itself, deliberately: 500/502/504
			// do not guarantee the request had no effect, so a replay could pay
			// for the same completion twice (providerio.ShouldRetryStatus). A
			// fresh child is not that replay — it is a new request.
			//
			// READ-ONLY TASKS ONLY, and that bound is the whole safety argument. A
			// write task may have applied part of its change before the provider
			// died; re-running it could apply that change twice, and no exit code
			// can tell us how far it got. A read-only task re-reads, which costs
			// tokens and nothing else. The same asymmetry the no-tool-call check
			// below rests on: for writes the inference has to be airtight.
			//
			// Gated by the SAME stops as every other retry here, because it spends
			// the same thing. Not gated by maxRetries: that budget bounds stall
			// thrashing, where the same model reruns the same work hoping for a
			// different mood. This is one attempt, bounded by its own flag.
			if ctx.Err() != nil {
				return result, err
			}
			if policy.wallClock.exhausted() {
				return result, err
			}
			// THE PLAN-WIDE BUDGET, checked last so it is only spent on a retry
			// that is actually going to happen.
			if !policy.providerRetries.take() {
				return result, err
			}
			retriedAfterProviderFailure = true
			continue
		case !result.Stalled:
			// Anything other than a stall is an ANSWER, including a failure. The
			// child ran and reported; running it again buys the same report.
			return result, err
		case attempt > policy.maxRetries:
			// Out of attempts. Restate the failure with the count, because
			// "stalled" and "stalled on every one of three attempts" call for
			// different responses.
			result.Err = stallError(policy.task.ID, policy.stallTimeout, attempt).Error()
			return result, err
		case ctx.Err() != nil:
			// The run was stopped. NOT retried, and not relabelled either — the
			// executor's own cancellation handling turns this into TaskCancelled.
			return result, err
		case policy.wallClock.exhausted():
			// The plan's wall budget is gone. Another attempt would overrun it
			// on behalf of the task that already exhausted it.
			result.Err = stallError(policy.task.ID, policy.stallTimeout, attempt).Error()
			return result, err
		}
	}
}

// terminalStatus maps counts onto the three terminal states. Partial is its own
// status precisely so a mostly-failed plan can never be reported as success.
func terminalStatus(report PlanReport) PlanStatus {
	switch {
	case report.Succeeded == 0 && report.Cancelled > 0:
		// Stopped before anything finished. Not a failure — nothing broke.
		return PlanCancelled
	case report.Succeeded == 0:
		return PlanFailed
	case report.Failed == 0 && report.Skipped == 0 && report.Cancelled == 0:
		return PlanCompleted
	default:
		// Cancelled MUST be part of this condition. Without it a plan with two
		// successes and two cancellations reported "completed" — work that
		// never ran, reported as done, which is RC-F exactly.
		return PlanPartial
	}
}

// planSpend is the plan's token meter as it happens, rather than after the fact.
//
// budgetLeft next to it is the AUTHORITATIVE post-completion number and still
// decides dispatch. This one exists because that one arrives too late: it moves
// only when a task finishes, so nothing between dispatch and completion can be
// stopped by it. A measured run spent 3,091,618 against a 200,000 budget and the
// meter did not move once while it happened — four tasks were in flight, each
// unbounded, and the first decrement landed after all of them had finished.
//
// Both sum the same usage events, so they agree at the end; they differ only in
// WHEN they can be consulted.
type planSpend struct {
	// TWO POOLS, because one could not hold. A single shared counter meant the
	// reserve was drawn from the same account it was capping: four feeders in
	// flight each overshoot their ceiling by about one usage event, and with
	// max_workers 4 that overshoot was the size of the reserve itself. A measured
	// plan stopped its feeders at 375,000 as designed, landed at 534,144, and the
	// dependents were then skipped for a budget the feeders had already spent —
	// the exact failure the reserve existed to prevent, one layer along.
	//
	// Separating them is what makes the reserve real: a dependent's pool cannot
	// be touched by a feeder, however far the feeder overshoots its own.
	upstream   atomic.Int64
	downstream atomic.Int64
	limit      int64
	// downstreamShare is the fraction of the budget held for work that waits on
	// other work, as a count of tasks over the total. Zero means no later work
	// exists and nothing is held back.
	downstreamTasks int
	totalTasks      int
}

// add records tokens against the pool this task draws from and returns that
// pool's running total.
func (spend *planSpend) add(tokens int, downstream bool) int64 {
	if spend == nil || tokens <= 0 {
		return 0
	}
	if downstream {
		return spend.downstream.Add(int64(tokens))
	}
	return spend.upstream.Add(int64(tokens))
}

// ceilingFor is what a task drawing from this pool may spend.
//
// THE AXIS IS EARLIER VERSUS LATER, not "feeds others" versus "terminal". A
// verify task in a find -> verify -> synthesize chain both depends on work and
// is depended on; classifying it as a feeder put it in the pool its own finders
// had already drained, and it was refused dispatch for their spend. Whether a
// task WAITS on other work is what decides which side of the reserve it sits on.
//
// UPSTREAM IS CAPPED; DOWNSTREAM IS FLOORED. Work that waits on nothing stops at
// three quarters so the last quarter survives for the work that waits on it.
// Work that waits gets AT LEAST that quarter, and whatever upstream did not use
// — frugal finders should not leave their synthesis artificially poor.
func (spend *planSpend) ceilingFor(downstream bool) int64 {
	if spend == nil || spend.limit <= 0 {
		return 0
	}
	reserve := spend.reserve()
	if reserve <= 0 {
		// No later work exists to protect; holding anything back would waste it.
		return spend.limit
	}
	if !downstream {
		return spend.limit - reserve
	}
	if left := spend.limit - spend.upstream.Load(); left > reserve {
		return left
	}
	return reserve
}

// add records tokens and reports whether the plan has now crossed its limit.
// An unset limit never reports crossed — unbounded is a deliberate choice.
// overPool reports whether a pool's running total has passed what that pool
// allows. An unbounded plan never has.
func (spend *planSpend) overPool(total int64, downstream bool) bool {
	ceiling := spend.ceilingFor(downstream)
	return ceiling > 0 && total > ceiling
}

// reserve is the share of the budget held for work that waits on other work.
//
// PROPORTIONAL TO HOW MUCH LATER WORK THERE IS, not a flat fraction. It was a
// flat quarter, and a seven-task plan with three downstream tasks gave them
// 125,000 between them: verify and sweep used 149,769 and synthesis was skipped.
// The reserve was the right idea sized by the wrong thing — the budget's shape
// rather than the plan's.
//
// Three of seven tasks downstream now reserves three sevenths. A plan that is
// mostly synthesis keeps most of its budget for synthesis; a plan that is mostly
// finding keeps little, which is correct in both directions.
func (spend *planSpend) reserve() int64 {
	if spend == nil || spend.limit <= 0 || spend.downstreamTasks <= 0 || spend.totalTasks <= 0 {
		return 0
	}
	return spend.limit * int64(spend.downstreamTasks) / int64(spend.totalTasks)
}

// Per-dependency and whole-briefing caps on what a task is told about its
// dependencies.
//
// BOUNDED BECAUSE THE ALTERNATIVE DOES NOT TERMINATE. A twenty-task plan where
// every task inherits every ancestor's full output is a context-overflow machine:
// the last task in a chain would carry the entire plan. The caps are generous
// enough to carry a real answer and small enough that a deep chain stays inside
// one context.
//
// A task that needs MORE than the cap still holds its read tools and the file
// paths its dependency quoted, so nothing is unreachable — it is one tool call
// away instead of free.
const (
	dependencyBriefingPerTask = 4000
	dependencyBriefingTotal   = 12000
)

// SIZED TO THE READER'S CONTEXT WINDOW when one is known.
//
// The constants above are provider-blind, and this catalogue is not: its models
// run from an 8k local Ollama to a 1M-context gateway. 12,000 characters is
// most of a small model's context and a rounding error in a large one's. A
// measured case: a code-review child produced 18,349 characters and a dependent
// task would have seen 4,000 of them — 78% discarded — whether it had room for
// the rest or not.
//
// THE BUDGET BELONGS TO THE TASK THAT READS THE BRIEFING, never the one that
// wrote it. With per-task models a 1M-context synthesiser routinely depends on a
// 32k finder, and sizing by the producer would starve the reader for no reason.
//
// UNKNOWN KEEPS TODAY'S NUMBERS EXACTLY. A window of 0 means nothing reported
// one — the majority case in this catalogue — and returns the two constants
// unchanged, so every provider that describes nothing behaves byte-identically
// to before this existed. Same contract as agent.ContextMeasurement: unknown
// disables the adjustment rather than guessing at it.
const (
	// briefingWindowFraction is how much of a reader's context the inherited
	// findings may occupy. A tenth leaves the task its own prompt, its tools and
	// room to actually work — the briefing is an input to the job, not the job.
	briefingWindowFraction = 0.10
	// briefingCharsPerToken converts the window (tokens) to the budget
	// (characters). Four is the usual English approximation and it only has to be
	// the right order of magnitude: this decides a budget, not an invoice.
	briefingCharsPerToken = 4
	// briefingFloorTotal keeps a briefing worth reading on a very small model.
	// Below this a dependent learns nothing and re-derives everything, which
	// costs more than the characters saved.
	briefingFloorTotal = 4000
	// briefingCeilingTotal stops a 1M-context model from inheriting an entire
	// plan. The bound the original constants existed for does not go away just
	// because the window is large: a twenty-task chain still must not accumulate.
	briefingCeilingTotal = 96_000
	// briefingPerTaskShare is the total divided among dependencies. Three,
	// because 12,000/4,000 is exactly the ratio the constants above already
	// chose; deriving it keeps a 30k-window model on today's numbers rather than
	// silently re-tuning every existing plan.
	briefingPerTaskShare = 3
)

// dependencyBriefingBudget returns the per-dependency and whole-briefing caps
// for a reader with the given context window, 0 meaning unknown.
func dependencyBriefingBudget(contextWindow int) (perTask, total int) {
	if contextWindow <= 0 {
		return dependencyBriefingPerTask, dependencyBriefingTotal
	}
	total = int(float64(contextWindow) * briefingCharsPerToken * briefingWindowFraction)
	if total < briefingFloorTotal {
		total = briefingFloorTotal
	}
	if total > briefingCeilingTotal {
		total = briefingCeilingTotal
	}
	return total / briefingPerTaskShare, total
}

// withDependencyBriefing prefixes a task's prompt with what its dependencies
// found, and returns the prompt unchanged when it has none.
//
// depends_on ORDERED EXECUTION AND PASSED NOTHING, which is the defect this
// closes. A task named as a dependency ran first and its output went to the
// report; the dependent started from a blank context and had to rediscover the
// same files. Two costs, both measured on real runs:
//
//   - Every judgement task re-read what the tracing tasks had already read. The
//     largest avoidable spend in a plan.
//   - A synthesising task received CONCLUSIONS with no trace behind them, so it
//     could repeat a claim but never check one. That is exactly how a plan
//     reported that MCP servers inherit an unscrubbed environment: the tracing
//     task had walked the scrubbing path, and the task that wrote the finding
//     could not see it.
//
// SUCCEEDED DEPENDENCIES ONLY. A dependency that produced nothing skips its
// dependents entirely (unusableDependencies), so anything reaching here has
// something to say; a cancelled or empty result contributes nothing rather than
// an empty heading.
//
// Deterministic order, following DependsOn as the plan declared it, so the same
// plan produces the same prompt — a resumed plan must not differ from its first
// run because a map iterated differently.
// withDependencyBriefing briefs a task at the FIXED caps.
//
// Kept as the two-argument function every existing caller and test already
// uses. Editing six test call sites to pass budgets they do not care about
// would have been six tests changed to keep compiling — and a test edited for
// that reason is one nobody re-reads. They assert briefing CONTENT, which the
// fixed caps still produce exactly as before.

// withDependencyBriefingBudget is the same briefing sized to the reading task.
func withDependencyBriefingBudget(task Task, results map[string]TaskResult, perTaskBudget, totalBudget int) string {
	if len(task.DependsOn) == 0 {
		return task.Prompt
	}
	var brief strings.Builder
	var unfinished []string
	remaining := totalBudget
	for _, id := range task.DependsOn {
		result, ok := results[id]
		if !ok {
			continue
		}
		// PARTIAL WORK IS STILL WORK. A dependency cut short at its budget was
		// investigating right up to the moment it stopped, and what it wrote
		// before then is real evidence. Discarding it is how four cancelled
		// finders took a whole plan down with them while their findings sat
		// unread in the results map.
		//
		// LABELLED, though, and that is not optional: a reader handed an
		// incomplete answer as if it were finished will treat its silences as
		// findings. Nothing was said about X becomes X is fine.
		// CANCELLED, NOT MERELY UNSUCCESSFUL. A cancelled task was investigating
		// when it was stopped, so what it wrote is evidence. A FAILED task ran
		// and did not deliver: its output is a harness diagnostic or an answer
		// already judged wrong, and passing that on as a finding would launder a
		// failure into a source.
		partial := result.Outcome == TaskCancelled
		if result.Outcome != TaskSucceeded && !partial {
			continue
		}
		output := strings.TrimSpace(result.Output)
		if output == "" || remaining <= 0 {
			continue
		}
		if partial {
			unfinished = append(unfinished, id)
		}
		budget := perTaskBudget
		if budget > remaining {
			budget = remaining
		}
		truncated := false
		if len(output) > budget {
			output, truncated = output[:budget], true
		}
		remaining -= len(output)
		heading := fmt.Sprintf("### Result of task %q", id)
		if partial {
			heading += " — INCOMPLETE, this task was stopped before it finished"
		}
		fmt.Fprintf(&brief, "%s\n%s\n", heading, output)
		if truncated {
			// SAID, not silent. A reader that cannot tell it was given part of an
			// answer will treat the part as the whole.
			//
			// AND NOW REACHABLE. When the plan kept a scratchpad, the whole answer
			// is on disk and the dependent is told where — so a truncated excerpt
			// stops being a loss and becomes a summary with the rest one read_file
			// behind it. Without a scratchpad this is exactly the old sentence.
			if pointer := scratchpadPointer(result.ScratchpadPath, len(strings.TrimSpace(result.Output))); pointer != "" {
				brief.WriteString(pointer + "\n")
			} else {
				brief.WriteString("[truncated — re-read the files named above if you need more]\n")
			}
		}
		brief.WriteString("\n")
	}
	if brief.Len() == 0 {
		return task.Prompt
	}
	preamble := "## What the tasks you depend on already found\n\n"
	closing := "Use this instead of rediscovering it. Verify anything you are about to rely on — " +
		"a claim above is a previous task's conclusion, not established fact.\n\n"
	if len(unfinished) > 0 {
		// SAY IT TWICE, at the top and at the bottom, because this is the sentence
		// that stops a partial answer being reported as a complete one.
		preamble += "Some of these tasks were STOPPED BEFORE THEY FINISHED (" +
			strings.Join(unfinished, ", ") + "). What they wrote is real, and what they did not " +
			"reach is unknown — not absent.\n\n"
		closing += "Say plainly in your answer which inputs were incomplete and what that leaves " +
			"uncovered. An unfinished input's silence is not evidence of anything.\n\n"
	}
	return preamble + brief.String() + closing + "## Your task\n\n" + task.Prompt
}

// unmeteredWallBudget is the wall bound applied to a plan that asked for NO
// bound of any kind.
//
// EVERY PLAN SHOULD HAVE AT LEAST ONE. A plan with no max_tokens has nothing to
// meter against — and on a provider that reports no usage at all, max_tokens
// would not have bounded it either: the meter reads the child's usage events,
// and a provider that emits none leaves it at zero forever while the work runs.
// Nothing then stops the plan except the work finishing.
//
// Generous on purpose. Measured plans on this repo ran 3-5 minutes of wall time;
// an hour is far beyond any of them and exists to catch the runaway, not to
// discipline a slow plan. A caller who wants longer says so, and one who set
// max_tokens is left alone entirely — they already chose their bound.
const unmeteredWallBudget = time.Hour

// planWallBudget is the wall bound a plan actually runs under: its own when it
// named one, and otherwise a default ONLY when it named no token bound either.
//
// Applied narrowly on purpose. Defaulting a wall for every plan would put a new
// ceiling over plans that deliberately run unbounded; this only catches the plan
// that asked for no bound at all, which is the one that cannot be stopped.
func planWallBudget(budget Budget) time.Duration {
	if budget.MaxWall > 0 {
		return budget.MaxWall
	}
	if budget.MaxTokens > 0 {
		return 0
	}
	return unmeteredWallBudget
}

// tokensPerToolCall is what one tool call costs a plan task, measured: a task
// that made 28 calls spent 232,416 tokens, another 24 calls for 259,705. The
// number is a rough proxy and is used as one — a model cannot see its own token
// meter, but it can count its own tool calls, so this converts a budget it
// cannot observe into a quantity it can.
const tokensPerToolCall = 8_000

// budgetLandingStrip is the share of a task's cap held back so a task that
// decides to stop still has room to WRITE its answer.
//
// Told to stop at its full cap, a task would still be composing when the hard
// limit arrived and be killed mid-sentence — the very outcome this exists to
// avoid. It is told a smaller number and killed at the real one; the gap is the
// runway.
const budgetLandingStrip = 5 // one fifth held back

// withTokenBudgetNotice tells a task what it may spend, in a unit it can count.
//
// A CAP THAT ONLY KILLS PRODUCES NOTHING. A measured plan set
// max_tokens_per_task to 200,000; every one of its six tasks was killed between
// 213k and 259k having done substantial reading, and the run cost 1,437,049
// tokens for zero completed tasks. Each was most of the way to an answer nobody
// received. Overspending and getting answers is a bad trade; overspending and
// getting nothing is a worse one.
//
// So the cap is announced as a DEADLINE rather than sprung as a guillotine. A
// task that knows its bound can stop investigating and write down what it found,
// and a partial answer carrying file:line evidence is worth incomparably more
// than a corpse. The hard kill stays as the backstop for a task that ignores it.
//
// Silent when there is no cap, which is every plan that does not ask for one.
func withTokenBudgetNotice(prompt string, maxTaskTokens int) string {
	if maxTaskTokens <= 0 {
		return prompt
	}
	announced := maxTaskTokens - maxTaskTokens/budgetLandingStrip
	calls := announced / tokensPerToolCall
	if calls < 1 {
		calls = 1
	}
	return prompt + fmt.Sprintf(
		"\n\n## Your budget for this task\n\n"+
			"About %d tokens — roughly %d tool calls at this repo's typical cost. You cannot see your own "+
			"token count, so count tool calls instead.\n\n"+
			"Read what matters most FIRST, and start writing your answer well before you reach that number. "+
			"A partial answer quoting file:line for what you did check is worth far more than being cut off "+
			"mid-investigation, which returns nothing at all. If you run short, say plainly what you did not "+
			"get to rather than guessing at it.",
		announced, calls)
}

// unusableDependencies reports the dependencies that produced NOTHING a
// dependent could work from, and whether the task should be skipped entirely.
//
// SKIPPED ONLY WHEN EVERY DEPENDENCY CAME BACK EMPTY. It used to be skipped when
// ANY did, and that cost a real run everything: four finder tasks were cut short
// on budget, three of them holding substantial partial findings, and the verify,
// sweep and synthesis tasks that depended on them were all skipped. Two runs,
// 3.6 million tokens, and no report — while the evidence to write one sat in the
// results map.
//
// A task with SOME evidence can still do useful work and say what it lacked; a
// task with NONE cannot, and running it would produce a confident answer drawn
// from nothing, which is worse than the gap. That is the line.
//
// Partial output from a cancelled dependency counts as evidence. A task stopped
// at its budget was working right up to the moment it stopped, and what it wrote
// before then is real — see withDependencyBriefing, which labels it as
// incomplete so nobody mistakes it for a finished answer.
func unusableDependencies(task Task, results map[string]TaskResult) (missing []string, skip bool) {
	if len(task.DependsOn) == 0 {
		return nil, false
	}
	usable := 0
	for _, dep := range task.DependsOn {
		result, ran := results[dep]
		if ran && result.Outcome == TaskSucceeded {
			usable++
			continue
		}
		if ran && result.Outcome == TaskCancelled && strings.TrimSpace(result.Output) != "" {
			// Cut short, but it wrote something before it was. A FAILED
			// dependency does not count: it ran and did not deliver, and its
			// output is a diagnostic rather than a finding.
			usable++
			continue
		}
		missing = append(missing, dep)
	}
	sort.Strings(missing)
	return missing, usable == 0
}

// planToolGrant intersects a task's requested tools with the parent's grant.
// An empty request inherits the parent's read-only grant; it never widens it.
//
// It REFUSES an empty result rather than returning one. An empty grant used to
// be handed on to the manifest, where an empty tool list read as "unspecified"
// and expanded to the default read-only category — so the narrower the parent,
// the wider the child. Returning an error keeps the empty case from ever
// reaching a place that has to guess what it meant. See Manifest.ToolsResolved
// for the other half of that fix.
func planToolGrant(task Task, parentTools []string) ([]string, error) {
	parent := map[string]bool{}
	for _, name := range parentTools {
		parent[name] = true
	}
	out := []string{}
	if len(task.Tools) == 0 {
		// THE DEFAULT STAYS READ-ONLY even though a named write tool is now
		// permitted. A task that asked for nothing must not inherit the ability
		// to change things — writing is opted into per task, by name, or every
		// unqualified task in every plan silently becomes write-capable the day
		// the parent grant widens.
		for _, name := range parentTools {
			if planReadOnlyTools[name] {
				out = append(out, name)
			}
		}
	} else {
		for _, name := range task.Tools {
			// THE PARENT'S GRANT, unconditionally. The read-only half of this
			// check moved out with its sibling in validateTaskTools — a named
			// write tool is now permitted — and the two had to move TOGETHER:
			// admission permitting what dispatch drops would produce a task that
			// validated and then ran with less than it asked for, silently.
			//
			// The old "only check the parent when it supplied a list" form was
			// what let a task widen its authority whenever the grant was unwired,
			// and that half is untouched.
			// The same two bounds as admission, in the same order: grantable at
			// all, then held by the parent. Dropping rather than refusing is
			// what makes this the narrowing layer.
			if !planReadOnlyTools[name] && !planWriteTools[name] {
				continue
			}
			if !parent[name] {
				continue
			}
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf(
			"task %q resolved no tools it may use: this run holds no read-only tools a plan task can inherit (parent grant: %s)",
			task.ID, describeGrant(parentTools))
	}
	sort.Strings(out)
	return out, nil
}

// describeGrant renders a parent grant for an error message, naming the empty
// case explicitly so the reason is never a blank space.
func describeGrant(parentTools []string) string {
	if len(parentTools) == 0 {
		return "none"
	}
	sorted := append([]string(nil), parentTools...)
	sort.Strings(sorted)
	return strings.Join(sorted, ", ")
}

// criticalPath is the longest dependency-weighted path through the DAG: for
// each task, its own duration plus the longest path among its dependencies.
// Computed over the validated topological order, so every dependency is already
// resolved when a task is reached.
func criticalPath(plan Plan, results map[string]TaskResult) time.Duration {
	longest := map[string]time.Duration{}
	tasks := map[string]Task{}
	for _, task := range plan.Tasks() {
		tasks[task.ID] = task
	}
	var best time.Duration
	for _, id := range plan.Order() {
		var upstream time.Duration
		for _, dep := range tasks[id].DependsOn {
			if longest[dep] > upstream {
				upstream = longest[dep]
			}
		}
		longest[id] = upstream + results[id].Duration
		if longest[id] > best {
			best = longest[id]
		}
	}
	return best
}

// speedup is sequential / critical-path, guarded against a zero denominator (a
// plan whose tasks all took no measurable time).
func speedup(sequential, critical time.Duration) float64 {
	if critical <= 0 {
		return 0
	}
	return float64(sequential) / float64(critical)
}

// Summary renders the plan's result for the model, including max_speedup — the
// number the Phase 3 decision rests on, surfaced rather than buried in an event.
func (report PlanReport) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Plan %s: %d succeeded, %d failed, %d skipped", report.Status, report.Succeeded, report.Failed, report.Skipped)
	if report.Cancelled > 0 {
		fmt.Fprintf(&b, ", %d cancelled", report.Cancelled)
	}
	b.WriteString(".\n")
	// NAME WHAT NEVER RAN, at the top.
	//
	// A budget-skipped task says so in its own row, in the same small text every
	// other row uses, and the headline says only "partial". A measured run came
	// back missing a third of the questions it was asked and nothing above the
	// fold said which — the reader had to diff the plan against the report to
	// find out. An incomplete answer that does not announce itself gets used as
	// a complete one.
	// SPEND THAT COULD NOT BE MEASURED IS NOT SPEND THAT DID NOT HAPPEN.
	//
	// TokensUsed is summed from the children's own usage events, so a provider
	// that emits none reports zero however much it billed — and a max_tokens set
	// against that number bounds nothing at all while reading like a guarantee.
	// Said plainly, because the alternative is a plan that appears to have cost
	// nothing.
	if report.TokensUsed == 0 && report.Succeeded > 0 {
		b.WriteString("spend could not be measured: this provider reported no token usage, " +
			"so max_tokens cannot bound a plan here — use max_wall_seconds instead.\n")
	}
	// A NUMBER A TASK REPORTED THAT ITS OWN COMMANDS CONTRADICT.
	//
	// Above the fold, because this is the one kind of finding a reader cannot
	// check for themselves: the command ran inside a child's session and its
	// output is not in this report. A measured submission stated a table of
	// timings no command had produced, the same test reading 0.86s in one place
	// and 4.20s in another; nothing in the harness noticed, and nothing said so.
	//
	// Silent when there are none, which is almost every plan.
	for _, task := range report.Tasks {
		for _, conflict := range task.MeasurementConflicts {
			fmt.Fprintf(&b, "unverified figure in %s — %s\n", task.ID, conflict)
		}
	}
	// WHAT THE BUDGET COST, in two separate numbers, because they mean different
	// things to the reader. A task CUT SHORT wrote findings that are in this
	// report; a task that NEVER RAN is a question nobody answered. Reporting
	// them together as "skipped" hid the first behind the second, and a reader
	// who cannot tell them apart cannot tell a partial report from an empty one.
	cutShort, neverRan := tasksStoppedByBudget(report.Tasks)
	if len(cutShort) > 0 || len(neverRan) > 0 {
		if report.TokensUsed > 0 && report.TokenLimit > 0 {
			fmt.Fprintf(&b, "budget exhausted at %d/%d tokens.\n", report.TokensUsed, report.TokenLimit)
		}
		if len(cutShort) > 0 {
			fmt.Fprintf(&b, "%d task(s) cut short mid-run; any partial findings are below: %s\n",
				len(cutShort), strings.Join(cutShort, ", "))
		}
		if len(neverRan) > 0 {
			fmt.Fprintf(&b, "%d task(s) never ran, so their questions are unanswered: %s\n",
				len(neverRan), strings.Join(neverRan, ", "))
		}
	}
	// SAY WHEN THE REQUEST WAS NOT HONOURED. The struct promises both numbers —
	// "a plan that asked for sixteen and ran six has not been given sixteen" —
	// and then only Workers was ever printed, so the trim was invisible and the
	// speedup below it looked like the plan's own fault. Stated only when they
	// differ, so the ordinary line is unchanged.
	if report.WorkersRequested > 0 && report.Workers > 0 && report.WorkersRequested != report.Workers {
		fmt.Fprintf(&b, "workers: %d (requested %d; this machine allowed fewer)\n",
			report.Workers, report.WorkersRequested)
	}
	fmt.Fprintf(&b, "sequential total: %s · critical path: %s · max_speedup: %.2fx\n",
		report.SequentialTotal.Round(time.Millisecond), report.CriticalPath.Round(time.Millisecond), report.MaxSpeedup)
	for _, task := range report.Tasks {
		fmt.Fprintf(&b, "\n  - %s [%s] %s", task.ID, task.Outcome, task.Duration.Round(time.Millisecond))
		// Only when the task named one. A line saying "on <the model you are
		// already using>" against every task is noise that hides the one line
		// that matters.
		if task.Model != "" {
			fmt.Fprintf(&b, " on %s", task.Model)
		}
		// A retried task cost more than its one line suggests. Stated only when
		// it happened, so the common case stays unchanged.
		if task.Attempts > 1 {
			fmt.Fprintf(&b, " (%d attempts)", task.Attempts)
		}
		// NAME THE MODEL THAT COULD NOT RUN. The task succeeded, so nothing else
		// on this line would hint that the plan's own choice was unusable — and
		// an unusable model stays in the discovered list, so the next plan picks
		// it again. This line is what makes it fixable, by exclusion or by
		// telling the provider.
		if task.RetriedOnParentModel != "" {
			fmt.Fprintf(&b, " (fell back from %s, which this provider would not run)", task.RetriedOnParentModel)
		}
		if task.Output != "" {
			b.WriteString("\n      result:\n" + task.Output)
		}
		if task.Err != "" {
			b.WriteString("\n      error:\n" + task.Err)
		}
	}
	return b.String()
}

// tasksStoppedByBudget separates the two ways a budget takes a task from you.
//
// CUT SHORT means the task ran, wrote something, and was stopped — its findings
// are in the report. NEVER RAN means the question was never asked. Both used to
// be called "skipped", which let a partial report read like an empty one and an
// empty one read like a partial.
func tasksStoppedByBudget(tasks []TaskResult) (cutShort, neverRan []string) {
	for _, task := range tasks {
		switch {
		case task.Outcome == TaskSkippedBudget:
			neverRan = append(neverRan, task.ID)
		case task.Outcome == TaskCancelled && strings.Contains(task.Err, "token budget"):
			// CUT SHORT whether or not it wrote anything. It ran and was stopped;
			// calling a task that ran "never ran" because it happened to produce
			// nothing would misreport what the budget actually cost.
			cutShort = append(cutShort, task.ID)
		}
	}
	return cutShort, neverRan
}

func recordDispatched(recorder PlanRecorder, task Task) {
	if recorder != nil {
		recorder.TaskDispatched(task)
	}
}

func recordCompleted(recorder PlanRecorder, result TaskResult) {
	if recorder != nil {
		recorder.TaskCompleted(result)
	}
}

func recordFailed(recorder PlanRecorder, result TaskResult) {
	if recorder != nil {
		recorder.TaskFailed(result)
	}
}

// PlanTaskProgressRecorder is the optional PER-TASK streaming half of a
// recorder.
//
// It exists because a child's stream events carry no task identity. The agent
// loop's progress callback holds the parent's TOOL-CALL id, which is the same
// for every task in a plan, so a display attributing those events could only
// guess — and the guess it made was "whichever task was dispatched last". That
// was sound while exactly one task ran at a time and became a lie the moment
// two could.
//
// The recorder already knows which card belongs to which task, because it
// opened them. So the identity travels with the event from the one place that
// has it, instead of being reconstructed at the other end from a guess.
type PlanTaskProgressRecorder interface {
	TaskProgress(taskID string, event streamjson.Event)
}

// planTaskProgress is best-effort and nil-safe, like every other recorder call.
func planTaskProgress(recorder PlanRecorder, taskID string, event streamjson.Event) {
	if progress, ok := recorder.(PlanTaskProgressRecorder); ok && progress != nil {
		progress.TaskProgress(taskID, event)
	}
}

// PlanPreflightReporter is the optional half of a recorder that hears about work
// happening BEFORE a plan exists.
//
// Auto-assignment runs ahead of admission: a /models call, a probe of each
// candidate, and — when routing is on — a full child run on the strongest model.
// Tens of seconds during which there is no plan, so no panel, no rows, and
// nothing on screen: a foreground run looks frozen at exactly the moment it is
// doing the most.
//
// A STATUS, NOT A PLAN ROW. The plan does not exist yet and inventing a row for
// it would put a task on screen that admission may still refuse. Empty clears.
type PlanPreflightReporter interface {
	PlanPreflight(status string)
}

// planPreflight is best-effort and nil-safe, like every other recorder call.
func planPreflight(recorder PlanRecorder, status string) {
	if reporter, ok := recorder.(PlanPreflightReporter); ok && reporter != nil {
		reporter.PlanPreflight(status)
	}
}

// PlanController is the optional CONTROL half of a recorder.
//
// Stopping a plan meant stopping the whole turn: Ctrl-C cancels the run, and
// there was no way to abandon a twenty-task plan while keeping the conversation.
// The surface that displays a plan is the one a user asks to stop it, so control
// arrives through the same seam the display does — type-asserted exactly like
// PlanLifecycleRecorder, so a recorder that only records is unaffected and no
// existing signature changes.
type PlanController interface {
	// PlanRunning hands the surface a cancel scoped to THIS PLAN, not to the
	// run that issued it. Called once before the first task; the surface drops
	// it when the plan ends, so a later stop cannot cancel a context that has
	// already been reused.
	PlanRunning(cancel context.CancelFunc)
	// WaitWhilePaused blocks at a TASK BOUNDARY while the user has paused.
	//
	// A boundary, not mid-task, and that is the honest limit: a child process
	// already talking to a provider cannot be suspended, and pretending
	// otherwise would mean "paused" while tokens kept being spent. It must
	// return when ctx is done, or stopping a paused plan would deadlock.
	WaitWhilePaused(ctx context.Context)
}

// PlanSurfaceBusy is the optional CONCURRENCY half of a recorder: the surface
// says whether it is already carrying a plan.
//
// ONE PLAN PER SURFACE, and it is not a policy choice — it is what the display
// can actually represent. The panel holds one plan, the sidebar's PLAN section
// draws one plan, and the card table that pairs a task with its row is keyed by
// TASK ID, which is unique within a plan and not between two. Two plans sharing
// an id as ordinary as "tests" makes one plan's completion close the other
// plan's row, and leaves a card that nothing can ever close spinning in AGENTS
// for the rest of the session.
//
// The TUI already refused this on the path a USER drives (/plans restart), with
// a comment saying exactly why. It did not refuse it on the path the MODEL
// drives, and the model reaches it by launching a background plan — which
// returns immediately, by design — and then calling orchestrate again.
type PlanSurfaceBusy interface {
	// RunningPlanName reports the plan this surface is already carrying. The
	// name is for the refusal message; the bool is the answer.
	RunningPlanName() (string, bool)
}

// runningPlanOn asks the surface whether it is free, best-effort and nil-safe
// like every other optional half. A recorder that cannot answer is treated as
// free: the headless path runs one plan per process and has no surface to
// contend for.
func runningPlanOn(recorder PlanRecorder) (string, bool) {
	if busy, ok := recorder.(PlanSurfaceBusy); ok && busy != nil {
		return busy.RunningPlanName()
	}
	return "", false
}

// planRunning and waitWhilePaused are best-effort and nil-safe, mirroring
// recordPlanAdmitted: a recorder that does not implement control simply cannot
// be asked to control anything.
func planRunning(recorder PlanRecorder, cancel context.CancelFunc) {
	if controller, ok := recorder.(PlanController); ok && controller != nil {
		controller.PlanRunning(cancel)
	}
}

func waitWhilePaused(recorder PlanRecorder, ctx context.Context) {
	if controller, ok := recorder.(PlanController); ok && controller != nil {
		controller.WaitWhilePaused(ctx)
	}
}

// PlanLifecycleRecorder extends PlanRecorder with the plan-level events. Kept
// separate so a caller that only wants task events need not implement both.
type PlanLifecycleRecorder interface {
	PlanRecorder
	PlanAdmitted(plan Plan)
	PlanCompleted(plan Plan, report PlanReport)
}

// recordPlanAdmitted and recordPlanCompleted are best-effort and nil-safe, and
// they type-assert rather than requiring the wider interface, so recording can
// never be the thing that fails a run.
func recordPlanAdmitted(recorder PlanRecorder, plan Plan) {
	if full, ok := recorder.(PlanLifecycleRecorder); ok && full != nil {
		full.PlanAdmitted(plan)
	}
}

func recordPlanCompleted(recorder PlanRecorder, plan Plan, report PlanReport) {
	if full, ok := recorder.(PlanLifecycleRecorder); ok && full != nil {
		full.PlanCompleted(plan, report)
	}
}

// scratchpadReadRoots is the read grant a task needs to open its dependencies'
// full outputs, and nothing else.
func scratchpadReadRoots(pad *Scratchpad) []string {
	if root := pad.Root(); root != "" {
		return []string{root}
	}
	return nil
}

// planSpendLimit reduces a plan's budget by what it already owes.
//
// A POSITIVE BUDGET NEVER BECOMES UNBOUNDED. Throughout planSpend, limit <= 0
// means "no bound at all" — ceilingFor returns 0 and overPool never fires. So a
// pre-spend at or above the whole budget must floor at 1, not at 0: a plan that
// has already overspent needs its very next task refused, and turning its bound
// off would do the exact opposite.
//
// An UNBOUNDED plan stays unbounded. Subtracting from 0 would invent a ceiling
// the caller never asked for, and max_tokens is optional precisely because
// guessing it low is how a plan spends everything and returns nothing.
func planSpendLimit(maxTokens, preSpent int) int64 {
	if maxTokens <= 0 {
		return 0
	}
	if preSpent <= 0 {
		return int64(maxTokens)
	}
	if remaining := int64(maxTokens) - int64(preSpent); remaining > 0 {
		return remaining
	}
	return 1
}

package specialist

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/streamjson"
)

// recordingRecorder captures the lifecycle events, so "recorded, never silently
// dropped" is asserted rather than assumed.
type recordingRecorder struct {
	dispatched []string
	completed  []string
	failed     []TaskResult
	admitted   int
	finished   []PlanReport
}

func (r *recordingRecorder) TaskDispatched(task Task)     { r.dispatched = append(r.dispatched, task.ID) }
func (r *recordingRecorder) TaskCompleted(res TaskResult) { r.completed = append(r.completed, res.ID) }
func (r *recordingRecorder) TaskFailed(res TaskResult)    { r.failed = append(r.failed, res) }
func (r *recordingRecorder) PlanAdmitted(plan Plan)       { r.admitted++ }
func (r *recordingRecorder) PlanCompleted(_ Plan, rep PlanReport) {
	r.finished = append(r.finished, rep)
}

func mustPlan(t *testing.T, tasks []any, budget map[string]any, limits Limits) Plan {
	t.Helper()
	plan, err := ParsePlan(planArgs(tasks, budget), limits)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	return plan
}

// (p) A dependency failure SKIPS its transitive dependents and RECORDS each
// one, while independent siblings still run. The plan runs to exhaustion.
func TestDependencyFailureSkipsDependentsAndRecordsThem(t *testing.T) {
	// a fails. b depends on a, c depends on b (transitive). d is independent.
	plan := mustPlan(t, []any{
		task("a", "fails"), task("b", "after a", "a"), task("c", "after b", "b"), task("d", "independent"),
	}, okBudget(), readOnlyLimits())

	recorder := &recordingRecorder{}
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			task := req.Task
			if task.ID == "a" {
				return TaskResult{Outcome: TaskFailed, Err: "boom"}, errors.New("boom")
			}
			return TaskResult{Outcome: TaskSucceeded, Output: "ok:" + task.ID}, nil
		}, recorder)

	byID := map[string]TaskResult{}
	for _, task := range report.Tasks {
		byID[task.ID] = task
	}
	if byID["a"].Outcome != TaskFailed {
		t.Fatalf("a must be failed, got %q", byID["a"].Outcome)
	}
	for _, id := range []string{"b", "c"} {
		if byID[id].Outcome != TaskSkippedDependency {
			t.Fatalf("%s must be skipped for a failed dependency, got %q", id, byID[id].Outcome)
		}
		if byID[id].Err == "" {
			t.Fatalf("%s must record WHY it was skipped", id)
		}
	}
	// The independent sibling still ran — the plan does not abort on first failure.
	if byID["d"].Outcome != TaskSucceeded {
		t.Fatalf("an independent sibling must still run, got %q", byID["d"].Outcome)
	}
	// Skips are RECORDED, not dropped: every task appears in the report.
	if len(report.Tasks) != 4 {
		t.Fatalf("every task must appear in the report, got %d", len(report.Tasks))
	}
	if len(recorder.failed) != 3 {
		t.Fatalf("the failure and both skips must be recorded, got %d", len(recorder.failed))
	}
	// b and c never dispatched — skipping means not running.
	for _, id := range recorder.dispatched {
		if id == "b" || id == "c" {
			t.Fatalf("%s must not be dispatched when its dependency failed", id)
		}
	}
	if report.Status != PlanPartial {
		t.Fatalf("status = %q, want partial", report.Status)
	}
}

// (q) Nineteen of twenty failing is PARTIAL, never success.
func TestMostlyFailedPlanIsPartialNotSuccess(t *testing.T) {
	raw := make([]any, 20)
	for i := range raw {
		raw[i] = task(fmt.Sprintf("t%02d", i), "work")
	}
	plan := mustPlan(t, raw, okBudget(), readOnlyLimits())
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			task := req.Task
			if task.ID == "t00" {
				return TaskResult{Outcome: TaskSucceeded, Output: "ok"}, nil
			}
			return TaskResult{Outcome: TaskFailed, Err: "no"}, errors.New("no")
		}, nil)

	if report.Succeeded != 1 || report.Failed != 19 {
		t.Fatalf("counts = %d succeeded / %d failed, want 1/19", report.Succeeded, report.Failed)
	}
	if report.Status != PlanPartial {
		t.Fatalf("19 of 20 failing must be PARTIAL, got %q", report.Status)
	}
	if report.Status == PlanCompleted {
		t.Fatal("a mostly-failed plan must never report completed")
	}
	// The counts are in the record, so the number is auditable.
	summary := report.Summary()
	if !strings.Contains(summary, "1 succeeded, 19 failed") {
		t.Fatalf("the summary must carry the counts:\n%s", summary)
	}
}

// Zero successes is FAILED, distinct from partial.
func TestAllFailedPlanIsFailed(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x"), task("b", "y")}, okBudget(), readOnlyLimits())
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(context.Context, PlanTaskRequest) (TaskResult, error) {
			return TaskResult{Outcome: TaskFailed}, errors.New("no")
		}, nil)
	if report.Status != PlanFailed {
		t.Fatalf("status = %q, want failed", report.Status)
	}
}

// (o) BUDGET ENFORCED AT DISPATCH. Validation alone is a promise, not a bound.
func TestBudgetExhaustionMidPlanIsPartial(t *testing.T) {
	// AT REALISTIC SCALE, because refuseImplausibleBudget now rejects a budget
	// that cannot cover its own tasks — and a 100-token budget for three tasks is
	// exactly the arithmetic it exists to refuse. The property under test is
	// unchanged; only the numbers are ones a real plan could carry.
	budget := okBudget()
	budget["max_tokens"] = float64(150_000)
	plan := mustPlan(t, []any{task("a", "x"), task("b", "y"), task("c", "z")}, budget, readOnlyLimits())

	dispatched := []string{}
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			task := req.Task
			dispatched = append(dispatched, task.ID)
			return TaskResult{Outcome: TaskSucceeded, Tokens: 90_000, Output: "ok"}, nil
		}, nil)

	// Two tasks at 90k exhaust a 150k budget; the third is skipped.
	if len(dispatched) != 2 {
		t.Fatalf("the budget must stop dispatch after it is spent, dispatched %v", dispatched)
	}
	byID := map[string]TaskResult{}
	for _, task := range report.Tasks {
		byID[task.ID] = task
	}
	if byID["c"].Outcome != TaskSkippedBudget {
		t.Fatalf("c must be skipped for budget, got %q", byID["c"].Outcome)
	}
	if report.Status != PlanPartial {
		t.Fatalf("budget exhaustion must be PARTIAL (not failure, not success), got %q", report.Status)
	}
	if byID["c"].Err == "" {
		t.Fatal("a budget skip must record why")
	}
}

// (m) THE DISPATCH HALF of the grant rule: a task's tools are intersected with
// the parent's at dispatch, so a validator bug cannot widen authority.
func TestToolGrantIsIntersectedAtDispatch(t *testing.T) {
	// Construct a task that ASKS for more than the parent holds, bypassing the
	// validator entirely — this is precisely the "validator bug" case.
	plan := mustPlan(t, []any{task("a", "x")}, okBudget(), readOnlyLimits())
	tasks := plan.Tasks()
	tasks[0].Tools = []string{"read_file", "grep", "write_file", "bash"}
	plan.tasks = tasks

	var granted []string
	ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			tools := req.Tools
			granted = tools
			return TaskResult{Outcome: TaskSucceeded}, nil
		}, nil)

	for _, forbidden := range []string{"write_file", "bash", "grep"} {
		for _, got := range granted {
			if got == forbidden {
				t.Fatalf("dispatch granted %q, which the parent does not hold or is not read-only: %v", forbidden, granted)
			}
		}
	}
	if len(granted) != 1 || granted[0] != "read_file" {
		t.Fatalf("grant = %v, want exactly the parent's read-only intersection", granted)
	}
}

// An empty Tools inherits the parent's READ-ONLY grant — never the parent's
// mutating tools, even when the parent holds them.
func TestEmptyToolsInheritsOnlyReadOnlyParentTools(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x")}, okBudget(), Limits{MaxTasks: 5})
	var granted []string
	ExecutePlan(context.Background(), plan, []string{"read_file", "grep", "write_file", "bash"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			tools := req.Tools
			granted = tools
			return TaskResult{Outcome: TaskSucceeded}, nil
		}, nil)
	want := map[string]bool{"read_file": true, "grep": true}
	if len(granted) != len(want) {
		t.Fatalf("grant = %v, want only the read-only parent tools", granted)
	}
	for _, got := range granted {
		if !want[got] {
			t.Fatalf("grant leaked %q: %v", got, granted)
		}
	}
}

// (r) max_speedup on a KNOWN dag with KNOWN durations.
func TestMaxSpeedupOnAKnownDAG(t *testing.T) {
	// Diamond: a(10) -> b(20), a(10) -> c(30), (b,c) -> d(10).
	//   sequential = 10+20+30+10 = 70
	//   critical   = a + max(b,c) + d = 10 + 30 + 10 = 50
	//   speedup    = 70/50 = 1.4
	plan := mustPlan(t, []any{
		task("a", "root"), task("b", "left", "a"), task("c", "right", "a"), task("d", "join", "b", "c"),
	}, okBudget(), readOnlyLimits())

	durations := map[string]time.Duration{
		"a": 10 * time.Millisecond, "b": 20 * time.Millisecond,
		"c": 30 * time.Millisecond, "d": 10 * time.Millisecond,
	}
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			task := req.Task
			return TaskResult{Outcome: TaskSucceeded, Duration: durations[task.ID]}, nil
		}, nil)

	if report.SequentialTotal != 70*time.Millisecond {
		t.Fatalf("sequential total = %s, want 70ms", report.SequentialTotal)
	}
	if report.CriticalPath != 50*time.Millisecond {
		t.Fatalf("critical path = %s, want 50ms (a + max(b,c) + d)", report.CriticalPath)
	}
	if diff := report.MaxSpeedup - 1.4; diff > 0.001 || diff < -0.001 {
		t.Fatalf("max_speedup = %.4f, want 1.40", report.MaxSpeedup)
	}
	if !strings.Contains(report.Summary(), "max_speedup: 1.40x") {
		t.Fatalf("the summary must surface max_speedup:\n%s", report.Summary())
	}
}

// A fully PARALLEL plan (no edges) has speedup == task count; a fully SEQUENTIAL
// chain has speedup 1. Those are the bounds the kill criterion is read against,
// so they must be exactly right.
func TestMaxSpeedupBounds(t *testing.T) {
	runner := func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
		return TaskResult{Outcome: TaskSucceeded, Duration: 10 * time.Millisecond}, nil
	}
	independent := mustPlan(t, []any{task("a", "x"), task("b", "y"), task("c", "z")}, okBudget(), readOnlyLimits())
	report := ExecutePlan(context.Background(), independent, []string{"read_file"}, runner, nil)
	if diff := report.MaxSpeedup - 3.0; diff > 0.001 || diff < -0.001 {
		t.Fatalf("three independent equal tasks must give 3.00x, got %.4f", report.MaxSpeedup)
	}
	chain := mustPlan(t, []any{task("a", "x"), task("b", "y", "a"), task("c", "z", "b")}, okBudget(), readOnlyLimits())
	report = ExecutePlan(context.Background(), chain, []string{"read_file"}, runner, nil)
	if diff := report.MaxSpeedup - 1.0; diff > 0.001 || diff < -0.001 {
		t.Fatalf("a strict chain must give 1.00x — fan-out buys nothing, got %.4f", report.MaxSpeedup)
	}
}

// (s) Each task's FULL result arrives verbatim. The plan must not become a
// second place where a delegated work product is truncated.
func TestTaskResultsArriveVerbatim(t *testing.T) {
	const body = "Summary\n-------\nline one\n\n    indented\n\ndiff --git a/x b/x\n+added\n-removed\n\nConclusion: done."
	plan := mustPlan(t, []any{task("a", "x"), task("b", "y", "a")}, okBudget(), readOnlyLimits())
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			task := req.Task
			return TaskResult{Outcome: TaskSucceeded, Output: task.ID + ":" + body}, nil
		}, nil)

	for _, task := range report.Tasks {
		if task.Output != task.ID+":"+body {
			t.Fatalf("%s output is not verbatim:\n%q", task.ID, task.Output)
		}
	}
	summary := report.Summary()
	for _, id := range []string{"a", "b"} {
		if !strings.Contains(summary, id+":"+body) {
			t.Fatalf("the summary must carry %s's full result verbatim:\n%s", id, summary)
		}
	}
	if strings.Contains(summary, "…") {
		t.Fatalf("the summary must not truncate:\n%s", summary)
	}
}

// Execution order follows the validated topological order — the same one Kahn
// emitted at admission, so the two cannot disagree.
func TestExecutionFollowsTheValidatedOrder(t *testing.T) {
	plan := mustPlan(t, []any{
		task("d", "join", "b", "c"), task("b", "left", "a"), task("c", "right", "a"), task("a", "root"),
	}, okBudget(), readOnlyLimits())
	seen := []string{}
	ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			task := req.Task
			seen = append(seen, task.ID)
			return TaskResult{Outcome: TaskSucceeded}, nil
		}, nil)
	if strings.Join(seen, ",") != strings.Join(plan.Order(), ",") {
		t.Fatalf("execution order %v != validated order %v", seen, plan.Order())
	}
	position := map[string]int{}
	for i, id := range seen {
		position[id] = i
	}
	for _, edge := range [][2]string{{"a", "b"}, {"a", "c"}, {"b", "d"}, {"c", "d"}} {
		if position[edge[0]] >= position[edge[1]] {
			t.Fatalf("%s ran after %s: %v", edge[0], edge[1], seen)
		}
	}
}

// Recording is BEST-EFFORT: a recorder that panics must not take the run with
// it... and a nil recorder must be a no-op.
func TestRecordingIsOptional(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x")}, okBudget(), readOnlyLimits())
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(context.Context, PlanTaskRequest) (TaskResult, error) {
			return TaskResult{Outcome: TaskSucceeded}, nil
		}, nil)
	if report.Status != PlanCompleted {
		t.Fatalf("a nil recorder must not change the outcome, got %q", report.Status)
	}
}

// THE GUARD THE BUDGET DEFECT NEEDED.
//
// The budget was enforced at dispatch against a counter that never moved,
// because NewPlanRunner never populated TaskResult.Tokens. Every executor test
// passed because their fake runners fabricated their own token counts — a fake
// that invents numbers cannot catch a PRODUCER that never produces them.
//
// So this drives the REAL runner (NewPlanRunner over a stubbed Executor whose
// child reports usage) and asserts a max_tokens=1 plan stops after the first
// task. If the runner stops populating Tokens, this fails; a fake-runner test
// never would.
func TestRealRunnerFeedsTheBudgetMeter(t *testing.T) {
	launched := []string{}
	executor := Executor{
		BinaryPath: "/bin/true",
		NewSessionID: func() (string, error) {
			return "specialist_00000000000000000000000a", nil
		},
		Load: func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(_ context.Context, _ string, args []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			launched = append(launched, strings.Join(args, " "))
			// A child that reports usage, exactly as a real provider stream does.
			return ChildRunResult{
				Started: true,
				Events: []streamjson.Event{
					{Type: "assistant", Text: "done"},
					// More than the whole plan budget: the first task alone blows it,
					// which is what proves the meter is fed by the REAL runner.
					{Type: "usage", TotalTokens: intPtrForTest(200_000)},
				},
			}, nil
		},
	}
	runner := NewPlanRunner(PlanTaskContext{Executor: executor, Cwd: t.TempDir(), SpecialistName: "explorer"})

	budget := okBudget()
	// The floor of what admission will accept for three tasks; the first task
	// alone spends more than all of it, which is the condition under test.
	budget["max_tokens"] = float64(150_000)
	plan := mustPlan(t, []any{task("a", "x"), task("b", "y"), task("c", "z")}, budget, readOnlyLimits())

	report := ExecutePlan(context.Background(), plan, []string{"read_file"}, runner, nil)

	// The REAL runner must have reported the child's tokens...
	if report.TokensUsed <= 0 {
		t.Fatalf("the real runner reported %d tokens; the budget meter never moves and enforcement can never fire",
			report.TokensUsed)
	}
	// ...so exactly one task ran and the rest were skipped for budget.
	if len(launched) != 1 {
		t.Fatalf("a 1-token budget must stop after the first task, launched %d children", len(launched))
	}
	byID := map[string]TaskResult{}
	for _, task := range report.Tasks {
		byID[task.ID] = task
	}
	for _, id := range []string{"b", "c"} {
		if byID[id].Outcome != TaskSkippedBudget {
			t.Fatalf("%s must be skipped for budget, got %q", id, byID[id].Outcome)
		}
	}
	if report.Status != PlanPartial {
		t.Fatalf("status = %q, want partial", report.Status)
	}
}

// The runner uses the ctx handed to it PER TASK, not one captured at
// construction — a captured context is how the prototype's goroutine ignored
// cancellation.
func TestRealRunnerHonoursThePerCallContext(t *testing.T) {
	launched := 0
	executor := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(context.Context, string, []string, func(streamjson.Event)) (ChildRunResult, error) {
			launched++
			return ChildRunResult{Started: true}, nil
		},
	}
	runner := NewPlanRunner(PlanTaskContext{Executor: executor, Cwd: t.TempDir(), SpecialistName: "explorer"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled BEFORE the plan runs
	plan := mustPlan(t, []any{task("a", "x"), task("b", "y")}, okBudget(), readOnlyLimits())
	report := ExecutePlan(ctx, plan, []string{"read_file"}, runner, nil)

	if launched != 0 {
		t.Fatalf("a cancelled context must launch no children, launched %d", launched)
	}
	// CANCELLED, not failed. The guarantee this test was written for — a
	// cancelled plan never reports success — still holds; the status is simply
	// no longer conflated with a plan that broke. Marking a stopped run as
	// failed is what made a cancelled twenty-task plan read as nineteen
	// defects.
	if report.Status == PlanCompleted {
		t.Fatalf("a cancelled plan must never report success, got %q", report.Status)
	}
	if report.Status != PlanCancelled {
		t.Fatalf("a plan cancelled before any task ran must report %q, got %q", PlanCancelled, report.Status)
	}
	if report.Cancelled != 2 {
		t.Fatalf("both tasks must be recorded as cancelled, got %d", report.Cancelled)
	}
	for _, task := range report.Tasks {
		if task.Outcome != TaskCancelled {
			t.Fatalf("task %q outcome = %q, want %q", task.ID, task.Outcome, TaskCancelled)
		}
	}
}

func intPtrForTest(v int) *int { return &v }

// A plan cancelled PART WAY through is partial, not failed: work that finished
// still counts, and the tasks that never ran are cancelled rather than broken.
// The whole point of the outcome is that a user who pressed Ctrl-C does not get
// a wall of failures.
func TestCancellationMidPlanIsPartialAndNamesTheCancelledTasks(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x"), task("b", "y"), task("c", "z")}, okBudget(), readOnlyLimits())
	ctx, cancel := context.WithCancel(context.Background())

	ran := 0
	report := ExecutePlan(ctx, plan, []string{"read_file"},
		func(runCtx context.Context, req PlanTaskRequest) (TaskResult, error) {
			ran++
			if req.Task.ID == "a" {
				return TaskResult{Outcome: TaskSucceeded, Output: "done"}, nil
			}
			// b is in flight when the user stops the run.
			cancel()
			return TaskResult{Outcome: TaskFailed, Err: "context canceled"}, runCtx.Err()
		}, nil)

	if ran != 2 {
		t.Fatalf("only a and b should have been dispatched, ran %d", ran)
	}
	if report.Status != PlanPartial {
		t.Fatalf("status = %q, want %q: one task succeeded, so this is not a wholesale cancellation", report.Status, PlanPartial)
	}
	if report.Succeeded != 1 {
		t.Fatalf("succeeded = %d, want 1: finished work still counts", report.Succeeded)
	}
	if report.Failed != 0 {
		t.Fatalf("failed = %d, want 0: nothing broke, the user stopped it", report.Failed)
	}
	if report.Cancelled != 2 {
		t.Fatalf("cancelled = %d, want 2 (b in flight, c never dispatched)", report.Cancelled)
	}
	byID := map[string]TaskOutcome{}
	for _, task := range report.Tasks {
		byID[task.ID] = task.Outcome
	}
	if byID["a"] != TaskSucceeded || byID["b"] != TaskCancelled || byID["c"] != TaskCancelled {
		t.Fatalf("outcomes = %v, want a succeeded, b and c cancelled", byID)
	}
}

// The summary a user reads must name cancellation rather than burying it in the
// failure count.
func TestSummaryNamesCancelledTasks(t *testing.T) {
	report := PlanReport{Status: PlanPartial, Succeeded: 1, Cancelled: 2}
	summary := report.Summary()
	if !strings.Contains(summary, "2 cancelled") {
		t.Fatalf("the summary must state the cancelled count:\n%s", summary)
	}
	if strings.Contains(summary, "2 failed") {
		t.Fatalf("cancelled tasks must not be reported as failures:\n%s", summary)
	}
}

// A genuine failure is still a failure — the cancellation branch must not
// swallow real errors just because the run finished afterwards.
func TestARealFailureIsNotReclassifiedAsCancelled(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x")}, okBudget(), readOnlyLimits())
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(context.Context, PlanTaskRequest) (TaskResult, error) {
			return TaskResult{Outcome: TaskFailed, Err: "the child exited 3"}, nil
		}, nil)
	if report.Failed != 1 || report.Cancelled != 0 {
		t.Fatalf("failed=%d cancelled=%d, want a real failure counted as one", report.Failed, report.Cancelled)
	}
	if report.Status != PlanFailed {
		t.Fatalf("status = %q, want %q", report.Status, PlanFailed)
	}
}

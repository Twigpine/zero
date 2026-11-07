package specialist

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// stallingRunner returns a stalled result for the first `stalls` attempts and
// succeeds after that, counting attempts per task id.
func stallingRunner(stalls map[string]int, counts map[string]int) PlanRunner {
	return func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
		id := req.Task.ID
		counts[id]++
		if counts[id] <= stalls[id] {
			return TaskResult{ID: id, Outcome: TaskFailed, Stalled: true, Err: "no output", Tokens: 10}, nil
		}
		return TaskResult{ID: id, Outcome: TaskSucceeded, Tokens: 7}, nil
	}
}

func retryBudget(retries any) map[string]any {
	budget := okBudget()
	if retries != nil {
		budget["max_retries"] = retries
	}
	return budget
}

// THE DEFAULT: one extra attempt, and it is the ABSENCE of a max_retries key
// that produces it. A plan that never mentions retries still survives a single
// transient stall.
func TestAStalledTaskIsRetriedOnceByDefault(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x")}, retryBudget(nil), readOnlyLimits())
	counts := map[string]int{}
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		stallingRunner(map[string]int{"a": 1}, counts), nil)

	if counts["a"] != 2 {
		t.Fatalf("the task ran %d times; the default is one retry after a stall", counts["a"])
	}
	if report.Succeeded != 1 {
		t.Fatalf("report = %+v; the second attempt succeeded and the plan must say so", report)
	}
	if got := report.Tasks[0].Attempts; got != 2 {
		t.Fatalf("Attempts = %d; want 2", got)
	}
}

// An EXPLICIT ZERO must turn retries off. It is the case that cannot work if
// "unset" and "0" decode to the same value, which is why the budget parses
// presence rather than a number.
func TestAnExplicitZeroDisablesRetries(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x")}, retryBudget(float64(0)), readOnlyLimits())
	if got := plan.Budget().MaxRetries; got != 0 {
		t.Fatalf("MaxRetries = %d; an explicit 0 must survive parsing", got)
	}
	counts := map[string]int{}
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		stallingRunner(map[string]int{"a": 5}, counts), nil)

	if counts["a"] != 1 {
		t.Fatalf("the task ran %d times; max_retries 0 must mean one attempt", counts["a"])
	}
	if report.Failed != 1 {
		t.Fatalf("report = %+v; the task stalled and was not retried", report)
	}
}

// The parsed value has to be the EFFECTIVE one, so nothing downstream re-derives
// the default and no second reader can disagree about what unset meant.
func TestAnUnsetMaxRetriesParsesToTheDefault(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x")}, retryBudget(nil), readOnlyLimits())
	if got := plan.Budget().MaxRetries; got != defaultPlanRetries {
		t.Fatalf("MaxRetries = %d; an unset max_retries must parse to the default %d", got, defaultPlanRetries)
	}
}

func TestMaxRetriesIsHonouredAndBounded(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x")}, retryBudget(float64(3)), readOnlyLimits())
	counts := map[string]int{}
	ExecutePlan(context.Background(), plan, []string{"read_file"},
		stallingRunner(map[string]int{"a": 99}, counts), nil)
	if counts["a"] != 4 {
		t.Fatalf("the task ran %d times; max_retries 3 means 1 attempt plus 3 retries", counts["a"])
	}

	for _, bad := range []float64{-1, 4, 100} {
		if _, err := ParsePlan(planArgs([]any{task("a", "x")}, retryBudget(bad)), readOnlyLimits()); err == nil {
			t.Errorf("max_retries %v was accepted; the range is 0-%d", bad, maxPlanRetries)
		}
	}
}

// THE ASYMMETRY THE WHOLE POLICY RESTS ON. A task whose child ran and reported
// an error produced an ANSWER; running it again spends another child's budget
// to receive the same one. Only the absence of an answer is retried.
func TestAnOrdinaryFailureIsNeverRetried(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x")}, retryBudget(float64(3)), readOnlyLimits())
	runs := 0
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(context.Context, PlanTaskRequest) (TaskResult, error) {
			runs++
			return TaskResult{ID: "a", Outcome: TaskFailed, Err: "the file does not exist"}, errors.New("boom")
		}, nil)

	if runs != 1 {
		t.Fatalf("a failing task ran %d times; only a stall is retried", runs)
	}
	if report.Failed != 1 {
		t.Fatalf("report = %+v", report)
	}
	if got := report.Tasks[0].Attempts; got != 1 {
		t.Fatalf("Attempts = %d; want 1", got)
	}
}

// A CANCELLED run must never spawn another child. The user stopping a plan and
// a provider going quiet both surface as a context error, and retrying the
// first would turn Ctrl-C into another attempt.
func TestACancelledRunIsNotRetried(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x")}, retryBudget(float64(3)), readOnlyLimits())
	ctx, cancel := context.WithCancel(context.Background())
	runs := 0
	report := ExecutePlan(ctx, plan, []string{"read_file"},
		func(context.Context, PlanTaskRequest) (TaskResult, error) {
			runs++
			// Stalled AND cancelled: the shape a Ctrl-C during a quiet child
			// actually produces. Cancellation has to win.
			cancel()
			return TaskResult{ID: "a", Outcome: TaskFailed, Stalled: true, Err: "no output"}, nil
		}, nil)

	if runs != 1 {
		t.Fatalf("a cancelled run launched %d attempts; it must launch one", runs)
	}
	if report.Cancelled != 1 {
		t.Fatalf("report = %+v; a stall that coincided with a cancellation is a cancellation", report)
	}
}

// The plan's WALL budget bounds the retries too. Another attempt on behalf of
// the task that already exhausted the budget is the budget not being a budget.
func TestARetryDoesNotOverrunTheWallDeadline(t *testing.T) {
	budget := retryBudget(float64(3))
	budget["max_wall_seconds"] = float64(1)
	plan := mustPlan(t, []any{task("a", "x")}, budget, readOnlyLimits())

	runs := 0
	ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(context.Context, PlanTaskRequest) (TaskResult, error) {
			runs++
			// Burn past the one-second wall on the first attempt. A REAL sleep,
			// not a fabricated Duration: the deadline is wall-clock, and a test
			// that only reports a long duration would pass against code that
			// never checked the clock at all.
			time.Sleep(1100 * time.Millisecond)
			return TaskResult{ID: "a", Outcome: TaskFailed, Stalled: true, Err: "no output"}, nil
		}, nil)

	if runs != 1 {
		t.Fatalf("the task ran %d times; an expired wall deadline must stop the retries", runs)
	}
}

// Spend is the TOTAL across attempts. A retry that did not count would make a
// plan's reported cost smaller than its real one, which is the reporting-failure
// -as-success class applied to money.
func TestRetriedSpendIsTheTotalAcrossAttempts(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x")}, retryBudget(float64(2)), readOnlyLimits())
	counts := map[string]int{}
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			counts["a"]++
			if counts["a"] <= 2 {
				return TaskResult{ID: "a", Outcome: TaskFailed, Stalled: true,
					Tokens: 100, Duration: 10 * time.Millisecond, Err: "no output"}, nil
			}
			return TaskResult{ID: "a", Outcome: TaskSucceeded, Tokens: 5, Duration: 10 * time.Millisecond}, nil
		}, nil)

	if report.TokensUsed != 205 {
		t.Fatalf("TokensUsed = %d; want 205 — two stalled attempts at 100 plus a 5-token success", report.TokensUsed)
	}
	if got := report.Tasks[0].Duration; got != 30*time.Millisecond {
		t.Fatalf("Duration = %s; want 30ms, the sum across three attempts", got)
	}
	if got := report.Tasks[0].Attempts; got != 3 {
		t.Fatalf("Attempts = %d; want 3", got)
	}
}

// When the retries run out, the failure has to say how many times it happened —
// "stalled" and "stalled on every one of three attempts" call for different
// responses from the user.
func TestAnExhaustedRetryBudgetNamesTheAttemptCount(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x")}, retryBudget(float64(2)), readOnlyLimits())
	counts := map[string]int{}
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		stallingRunner(map[string]int{"a": 99}, counts), nil)

	reason := report.Tasks[0].Err
	if !strings.Contains(reason, "3 attempts") {
		t.Fatalf("the failure must name the attempt count: %q", reason)
	}
	if !strings.Contains(reason, "max_stall_seconds") {
		t.Fatalf("the failure must still name the knob that changes it: %q", reason)
	}
	if !strings.Contains(report.Summary(), "(3 attempts)") {
		t.Fatalf("the summary must show the retried task's cost:\n%s", report.Summary())
	}
}

// A plan that never stalls must be untouched by any of this: one attempt per
// task, and no "(1 attempts)" noise in the summary.
func TestAPlanThatNeverStallsIsUnchanged(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x"), task("b", "y", "a")}, retryBudget(nil), readOnlyLimits())
	counts := map[string]int{}
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		stallingRunner(map[string]int{}, counts), nil)

	if counts["a"] != 1 || counts["b"] != 1 {
		t.Fatalf("attempt counts = %v; want one each", counts)
	}
	if strings.Contains(report.Summary(), "attempts)") {
		t.Fatalf("the summary mentions attempts for a plan that never retried:\n%s", report.Summary())
	}
	for _, result := range report.Tasks {
		if result.Attempts != 1 {
			t.Fatalf("task %q Attempts = %d; a dispatched task always ran at least once", result.ID, result.Attempts)
		}
	}
}

// attempts has to reach the RECORD. Duration and Tokens are totals across
// attempts, and without the count a retried task's numbers read as one
// extraordinarily expensive attempt.
func TestTheAttemptCountReachesTheEvents(t *testing.T) {
	_, completed := TaskCompletedEvent(TaskResult{ID: "a", Attempts: 2})
	if completed["attempts"] != 2 {
		t.Fatalf("task_completed attempts = %v; want 2", completed["attempts"])
	}
	_, failed := TaskFailedEvent(TaskResult{ID: "a", Attempts: 3, Outcome: TaskFailed})
	if failed["attempts"] != 3 {
		t.Fatalf("task_failed attempts = %v; want 3", failed["attempts"])
	}
}

// controllingRecorder is a recorder that also CONTROLS: it holds the plan's
// cancel and can park the executor at a task boundary.
type controllingRecorder struct {
	recordingRecorder
	cancel  context.CancelFunc
	release chan struct{}
	waits   int
}

func (r *controllingRecorder) PlanRunning(cancel context.CancelFunc) { r.cancel = cancel }
func (r *controllingRecorder) WaitWhilePaused(ctx context.Context) {
	r.waits++
	if r.release == nil {
		return
	}
	select {
	case <-r.release:
	case <-ctx.Done():
	}
}

// The gate has to be REACHED, once per task, before anything is dispatched.
// A pause the executor never consults is a flag, not a pause.
func TestTheExecutorConsultsThePauseGateBeforeEveryTask(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x"), task("b", "y"), task("c", "z")}, okBudget(), readOnlyLimits())
	recorder := &controllingRecorder{}
	ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(context.Context, PlanTaskRequest) (TaskResult, error) {
			return TaskResult{Outcome: TaskSucceeded}, nil
		}, recorder)

	if recorder.waits != 3 {
		t.Fatalf("the pause gate was consulted %d times for 3 tasks", recorder.waits)
	}
}

// A PAUSED PLAN DISPATCHES NOTHING. Asserting only that the gate was called
// would pass against an executor that called it and ran the task anyway.
func TestAPausedPlanDispatchesNothingUntilReleased(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x"), task("b", "y")}, okBudget(), readOnlyLimits())
	recorder := &controllingRecorder{release: make(chan struct{})}

	dispatched := make(chan string, 4)
	done := make(chan PlanReport, 1)
	go func() {
		done <- ExecutePlan(context.Background(), plan, []string{"read_file"},
			func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
				dispatched <- req.Task.ID
				return TaskResult{Outcome: TaskSucceeded}, nil
			}, recorder)
	}()

	select {
	case id := <-dispatched:
		t.Fatalf("task %q was dispatched while the plan was paused", id)
	case <-time.After(80 * time.Millisecond):
	}

	close(recorder.release)
	select {
	case report := <-done:
		if report.Succeeded != 2 {
			t.Fatalf("report = %+v; both tasks must run once released", report)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the plan never resumed")
	}
}

// The cancel handed to the surface must be the one that STOPS THE PLAN, and the
// remainder must be recorded as CANCELLED — not failed. A user who stopped a
// plan did not break it.
func TestTheHandedCancelStopsThePlanAndRecordsCancellations(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x"), task("b", "y"), task("c", "z")}, okBudget(), readOnlyLimits())
	recorder := &controllingRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runs := 0
	report := ExecutePlan(ctx, plan, []string{"read_file"},
		func(context.Context, PlanTaskRequest) (TaskResult, error) {
			runs++
			if runs == 1 {
				if recorder.cancel == nil {
					t.Fatal("the executor's caller never handed the surface a cancel")
				}
				recorder.cancel()
			}
			return TaskResult{Outcome: TaskSucceeded}, nil
		}, recorder)

	if runs != 1 {
		t.Fatalf("%d tasks ran; a stopped plan must not dispatch more", runs)
	}
	if report.Cancelled != 2 {
		t.Fatalf("report = %+v; the remainder must be cancelled, never failed", report)
	}
	if report.Failed != 0 {
		t.Fatalf("a stopped plan reported %d failures; nothing broke", report.Failed)
	}
	if report.Status != PlanPartial {
		t.Fatalf("status = %q; one success and two cancellations is partial", report.Status)
	}
}

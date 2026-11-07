package specialist

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A PLAN THAT ASKED FOR NO BOUND STILL GETS ONE.
//
// With no max_tokens there is nothing to meter against — and on a provider that
// reports no usage, max_tokens would not have bounded it either: the meter sums
// the children's usage events, and a provider emitting none leaves it at zero
// forever while the work runs. Nothing then stops the plan but the work ending.
func TestAPlanWithNoBoundAtAllGetsAWallBackstop(t *testing.T) {
	if got := planWallBudget(Budget{}); got != unmeteredWallBudget {
		t.Errorf("a plan with neither bound got %v, want the wall backstop", got)
	}
	// AN EXPLICIT WALL WINS. The caller chose; the default is for the plan that
	// chose nothing.
	if got := planWallBudget(Budget{MaxWall: 90 * time.Second}); got != 90*time.Second {
		t.Errorf("an explicit wall was overridden: %v", got)
	}
	// A TOKEN BOUND IS A BOUND. Defaulting a wall over it would put a new ceiling
	// on a plan that deliberately runs long inside a budget it named.
	if got := planWallBudget(Budget{MaxTokens: 500_000}); got != 0 {
		t.Errorf("a plan with a token budget gained an unasked-for wall of %v", got)
	}
	if got := planWallBudget(Budget{MaxTokens: 500_000, MaxWall: time.Minute}); got != time.Minute {
		t.Errorf("both set: %v", got)
	}
}

// AND THE BACKSTOP MUST REACH THE RUN, not just the helper.
//
// Asserted on the EFFECT — the child's context is cancelled and the report says
// the wall did it — rather than on ctx.Deadline() being set. The bound is a
// clock that cancels rather than a context.WithTimeout, because a fixed deadline
// cannot be moved and a paused plan must not burn its budget; a test pinned to
// the old mechanism would have failed for that change while the property it
// cares about was strictly better served.
func TestTheWallBackstopActuallyBoundsAPlan(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x")},
		map[string]any{"max_workers": float64(1), "max_wall_seconds": float64(1)}, readOnlyLimits())

	stopped := make(chan struct{})
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(ctx context.Context, _ PlanTaskRequest) (TaskResult, error) {
			// Runs until something stops it. Only the backstop can.
			<-ctx.Done()
			close(stopped)
			// No reason of its own — the shape a child that was simply killed
			// produces, and the one that lets the executor say WHY.
			return TaskResult{Outcome: TaskFailed}, ctx.Err()
		}, nil)

	select {
	case <-stopped:
	default:
		t.Fatal("the child ran to completion: nothing bounded it")
	}
	if len(report.Tasks) != 1 {
		t.Fatalf("expected one task, got %d", len(report.Tasks))
	}
	if got := report.Tasks[0].Outcome; got != TaskCancelled {
		t.Fatalf("outcome = %q, want %q: a wall-budget stop is not a defect", got, TaskCancelled)
	}
	// The reader has to tell a budget expiry from a person pressing stop, and
	// the CONTEXT can no longer say which — it is plain-cancelled either way now
	// that the deadline has to be movable. The clock's own flag carries it.
	if !strings.Contains(report.Tasks[0].Err, "max_wall_seconds") {
		t.Errorf("the report does not say the wall budget stopped it: %q", report.Tasks[0].Err)
	}
}

// ...and a PERSON stopping the plan must still read as a person, not as a budget
// expiry. This is the discrimination that used to come free from
// context.DeadlineExceeded and now comes from the clock's own flag; without it
// every user stop would be reported as "max_wall_seconds elapsed".
func TestAUserStopIsNotReportedAsAWallExpiry(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x")},
		map[string]any{"max_workers": float64(1), "max_wall_seconds": float64(3600)}, readOnlyLimits())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	report := ExecutePlan(ctx, plan, []string{"read_file"},
		func(taskCtx context.Context, _ PlanTaskRequest) (TaskResult, error) {
			cancel()
			<-taskCtx.Done()
			return TaskResult{Outcome: TaskFailed}, taskCtx.Err()
		}, nil)

	if len(report.Tasks) != 1 {
		t.Fatalf("expected one task, got %d", len(report.Tasks))
	}
	if got := report.Tasks[0].Err; strings.Contains(got, "max_wall_seconds") {
		t.Fatalf("a user stop was reported as a wall-budget expiry: %q", got)
	}
	if got := report.Tasks[0].Err; !strings.Contains(got, "stopped") {
		t.Errorf("a user stop does not say so: %q", got)
	}
}

// A PAUSED PLAN DOES NOT SPEND ITS WALL BUDGET.
//
// The bound used to be a context.WithTimeout started at admission, so the clock
// ran while the user had the plan paused: pausing a thirty-minute plan for
// twenty left it ten, and pausing one for longer than its budget meant it was
// already dead on resume — stopped, and told max_wall_seconds elapsed, by time
// in which no task ran.
func TestPausedTimeIsNotChargedToTheWallBudget(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := newPlanWallClock(30*time.Minute, func() time.Time { return now })

	// Twenty minutes pass, all of it paused.
	now = now.Add(20 * time.Minute)
	clock.addPaused(20 * time.Minute)

	if clock.exhausted() {
		t.Fatal("a plan paused for twenty minutes of a thirty-minute budget is out of time")
	}
	if got := clock.remaining(); got != 30*time.Minute {
		t.Fatalf("remaining = %v, want the full 30m: paused time was charged to the budget", got)
	}

	// Ten minutes of actual running.
	now = now.Add(10 * time.Minute)
	if got := clock.remaining(); got != 20*time.Minute {
		t.Fatalf("remaining = %v after 10m of running, want 20m", got)
	}

	// ...and running time still exhausts it.
	now = now.Add(20 * time.Minute)
	if !clock.exhausted() {
		t.Fatal("thirty minutes of running did not exhaust a thirty-minute budget")
	}
}

// An unbounded clock is nil, and every method has to survive that — the plan
// path calls all of them without checking.
func TestAnAbsentWallClockIsUnbounded(t *testing.T) {
	var clock *planWallClock
	if clock.exhausted() {
		t.Error("a plan with no wall bound reported its budget spent")
	}
	if clock.wallExpired() {
		t.Error("a plan with no wall bound claimed the wall stopped it")
	}
	if clock.remaining() <= 0 {
		t.Error("an unbounded clock reported no time left")
	}
	clock.addPaused(time.Minute)
	if newPlanWallClock(0, nil) != nil || newPlanWallClock(-time.Second, nil) != nil {
		t.Error("a non-positive budget must produce no clock at all")
	}
}

// SPEND THAT COULD NOT BE MEASURED IS NOT SPEND THAT DID NOT HAPPEN. A provider
// emitting no usage reports zero however much it billed, and a max_tokens set
// against that number bounds nothing while reading like a guarantee.
func TestTheReportSaysWhenSpendCouldNotBeMeasured(t *testing.T) {
	unmetered := PlanReport{
		Status: PlanCompleted, Succeeded: 2, TokensUsed: 0,
		Tasks: []TaskResult{{ID: "a", Outcome: TaskSucceeded}, {ID: "b", Outcome: TaskSucceeded}},
	}.Summary()
	if !strings.Contains(unmetered, "spend could not be measured") {
		t.Errorf("a plan that reported no usage said nothing about it:\n%s", unmetered)
	}
	if !strings.Contains(unmetered, "max_wall_seconds") {
		t.Errorf("the note does not say what to use instead:\n%s", unmetered)
	}

	// A metered plan must not gain the line.
	metered := PlanReport{
		Status: PlanCompleted, Succeeded: 1, TokensUsed: 12_000,
		Tasks: []TaskResult{{ID: "a", Outcome: TaskSucceeded}},
	}.Summary()
	if strings.Contains(metered, "could not be measured") {
		t.Errorf("a metered plan claimed its spend was unmeasurable:\n%s", metered)
	}
	// Nor a plan where nothing ran — zero tokens is honest there.
	empty := PlanReport{Status: PlanFailed, Succeeded: 0, TokensUsed: 0}.Summary()
	if strings.Contains(empty, "could not be measured") {
		t.Errorf("a plan that ran nothing reported an unmeasurable spend:\n%s", empty)
	}
}

package specialist

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// A TASK OTHERS DEPEND ON STOPS EARLIER THAN ONE NOTHING DEPENDS ON.
//
// Four finders consumed a whole budget between them and the verify, sweep and
// synthesis tasks were never dispatched: a measured plan spent 712,222 against
// 500,000 and returned no report, because everything downstream of the finders
// was skipped for a budget the finders had already spent.
func TestATaskWithDependentsStopsBeforeTheWholeBudgetIsGone(t *testing.T) {
	spend := &planSpend{limit: 1_000_000, downstreamTasks: 1, totalTasks: 4}
	// A feeder is held to three quarters, so a quarter survives for the work
	// that waits on it.
	if got := spend.ceilingFor(false); got != 750_000 {
		t.Errorf("upstream ceiling = %d, want 750000", got)
	}
	// Untouched by feeders, a dependent may have what they did not use.
	if got := spend.ceilingFor(true); got != 1_000_000 {
		t.Errorf("downstream ceiling = %d, want the whole budget while upstream has spent nothing", got)
	}

	// AND THE RESERVE IS A FLOOR, not a share of what is left. Feeders
	// overshooting their own ceiling must not shrink it — that overshoot is what
	// ate the reserve when both drew from one pool.
	spend.upstream.Store(950_000)
	if got := spend.ceilingFor(true); got != 250_000 {
		t.Errorf("after upstream overshot to 950000 the downstream pool was %d, want the 250000 reserve", got)
	}

	// Unbounded stays unbounded.
	empty := &planSpend{downstreamTasks: 1, totalTasks: 4}
	if got := empty.ceilingFor(true); got != 0 {
		t.Errorf("an unbounded plan gained a ceiling of %d", got)
	}
}

// THE RESERVE MUST REACH THE RUN, computed from the real dependency graph.
func TestTheReserveIsAppliedFromThePlansOwnGraph(t *testing.T) {
	budget := okBudget()
	budget["max_tokens"] = float64(1_000_000)
	plan := mustPlan(t, []any{
		task("finder", "gather"),
		task("synthesis", "summarise", "finder"),
	}, budget, readOnlyLimits())

	waits := map[string]bool{}
	ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			waits[req.Task.ID] = req.WaitsOnOtherTasks
			return TaskResult{Outcome: TaskSucceeded, Output: "ok"}, nil
		}, nil)

	if waits["finder"] {
		t.Error("a task that waits on nothing was put in the reserved pool")
	}
	if !waits["synthesis"] {
		t.Error("a task that waits on another was not put in the reserved pool, so its feeder can starve it")
	}
}

// A POOL IS ONLY A BOUND WHEN THE PLAN HAS ONE. An unbounded plan crosses
// nothing, however much it spends.
func TestAnUnboundedPlanCrossesNoPool(t *testing.T) {
	spend := &planSpend{limit: 0, downstreamTasks: 1, totalTasks: 4}
	if spend.overPool(spend.add(10_000_000, true), true) {
		t.Error("an unbounded plan reported downstream work over its pool")
	}
	if spend.overPool(spend.add(10_000_000, false), false) {
		t.Error("an unbounded plan reported upstream work over its pool")
	}
}

// A FEEDER'S OVERSHOOT MUST NOT CROSS THE DEPENDENT'S POOL. This is the whole
// point of separating them: one shared counter meant four feeders overshooting
// by one usage event each consumed the reserve, and every dependent was then
// refused for a budget that was never theirs to spend.
func TestAFeederOvershootDoesNotExhaustTheDependentPool(t *testing.T) {
	spend := &planSpend{limit: 500_000, downstreamTasks: 1, totalTasks: 4}
	// Upstream blows past its 375,000 ceiling, exactly as four finders in flight did.
	total := spend.add(534_144, false)
	if !spend.overPool(total, false) {
		t.Fatal("the upstream pool did not register as crossed")
	}
	// The downstream pool is untouched and still holds its reserve.
	if spend.overPool(spend.add(1_000, true), true) {
		t.Error("an upstream overshoot closed the downstream pool, which is the defect this separation exists to fix")
	}
	if got := spend.ceilingFor(true); got != 125_000 {
		t.Errorf("downstream pool = %d, want the 125000 reserve intact", got)
	}
}

// THE TWO WAYS A BUDGET TAKES A TASK MEAN DIFFERENT THINGS TO A READER: one is
// a partial answer in this report, the other a question nobody asked.
func TestTheReportSeparatesCutShortFromNeverRan(t *testing.T) {
	summary := PlanReport{
		Status: PlanPartial, TokensUsed: 712_222, TokenLimit: 500_000,
		Tasks: []TaskResult{
			{ID: "finder-a", Outcome: TaskCancelled, Output: "found x", Err: "stopped: the plan's token budget ran out while it was running"},
			{ID: "verify", Outcome: TaskSkippedBudget, Err: "skipped: the plan's budget was exhausted before this task ran"},
		},
	}.Summary()

	if !strings.Contains(summary, "712222/500000") {
		t.Errorf("the report does not say what was spent against what:\n%s", summary)
	}
	if !strings.Contains(summary, "cut short mid-run") || !strings.Contains(summary, "finder-a") {
		t.Errorf("a task that ran and was stopped is not reported as such:\n%s", summary)
	}
	if !strings.Contains(summary, "never ran") || !strings.Contains(summary, "verify") {
		t.Errorf("a question nobody asked is not reported as such:\n%s", summary)
	}
}

// THE FLOOR CATCHES THE IMPOSSIBLE; THIS CATCHES THE MERELY WRONG. A seven-task
// audit was admitted with 500,000 tokens — over the floor, an eighth of what it
// went on to spend — and nothing told the author until the run had failed.
func TestAnUndersizedBudgetIsWarnedAboutWhileItCanStillBeChanged(t *testing.T) {
	// The shape that failed: four finders reading the repo, three tasks working
	// from what they found.
	shape := []Task{
		{ID: "f1"}, {ID: "f2"}, {ID: "f3"}, {ID: "f4"},
		{ID: "verify", DependsOn: []string{"f1"}},
		{ID: "sweep", DependsOn: []string{"f1"}},
		{ID: "synthesis", DependsOn: []string{"verify"}},
	}
	warning := warnBudgetLooksLow(Budget{MaxTokens: 500_000}, shape)
	if warning == "" {
		t.Fatal("the budget that produced a failed seven-task run drew no warning")
	}
	for _, want := range []string{"500000", "7 tasks", "510k-1,017k", "omit max_tokens"} {
		if !strings.Contains(warning, want) {
			t.Errorf("the warning does not mention %q: %s", want, warning)
		}
	}
	// NOT A REFUSAL, and not fired on a plan that budgeted properly.
	// 4 x 1M + 3 x 150k = 4.45M, so a 5M budget is comfortably sized and must
	// draw no warning: an alarm on a correct plan teaches the author to ignore it.
	if got := warnBudgetLooksLow(Budget{MaxTokens: 5_000_000}, shape); got != "" {
		t.Errorf("a well-sized budget was warned about: %s", got)
	}
	// DOWNSTREAM WORK IS NOT PRICED LIKE A FINDER. Seven tasks that all wait on
	// something need far less than seven that all read the repo.
	allDownstream := make([]Task, 7)
	for i := range allDownstream {
		allDownstream[i] = Task{ID: "t", DependsOn: []string{"x"}}
	}
	if got := warnBudgetLooksLow(Budget{MaxTokens: 1_500_000}, allDownstream); got != "" {
		t.Errorf("a plan of cheap downstream tasks was priced as if every task read the repo: %s", got)
	}
	// Nor on an unbounded plan, which has made no claim to be wrong about.
	if got := warnBudgetLooksLow(Budget{}, shape); got != "" {
		t.Errorf("an unbounded plan was warned about: %s", got)
	}
}

// THE RUN THAT PROVED IT, END TO END.
//
// Four feeders overshoot the whole budget between them; the dependent must still
// be dispatched and run on what they found. Before the pools were separated this
// is exactly what failed: feeders capped at 375,000 landed at 534,144, and
// verify, sweep and synthesis were every one skipped for a budget that was never
// theirs — two runs, no report, while the findings sat in the results map.
func TestDependentsStillRunAfterFeedersOverspendTheirPool(t *testing.T) {
	budget := okBudget()
	budget["max_tokens"] = float64(2_200_000)
	budget["max_workers"] = float64(4)
	plan := mustPlan(t, []any{
		task("f1", "find"), task("f2", "find"), task("f3", "find"), task("f4", "find"),
		task("verify", "attack the claims", "f1", "f2", "f3", "f4"),
		task("synthesis", "report what survived", "verify"),
	}, budget, readOnlyLimits())

	// LOCKED, because max_workers is 4 and that is the point of this test: four
	// task goroutines record into this at once.
	var mu sync.Mutex
	dispatched := map[string]bool{}
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			mu.Lock()
			dispatched[req.Task.ID] = true
			mu.Unlock()
			if strings.HasPrefix(req.Task.ID, "f") {
				// Each finder overshoots its share, as four in flight do.
				return TaskResult{
					Outcome: TaskCancelled,
					Tokens:  600_000,
					Output:  "partial finding from " + req.Task.ID,
					Err:     `task stopped: the plan's token budget ran out while it was running`,
				}, nil
			}
			return TaskResult{Outcome: TaskSucceeded, Tokens: 90_000, Output: "done"}, nil
		}, nil)

	mu.Lock()
	defer mu.Unlock()
	if !dispatched["verify"] {
		t.Fatalf("the dependent was never dispatched after its feeders overspent: %+v", report.Tasks)
	}
	if !dispatched["synthesis"] {
		t.Errorf("the terminal task never ran, so the plan produced no report: %+v", report.Tasks)
	}
	if report.Succeeded < 2 {
		t.Errorf("expected verify and synthesis to succeed on partial findings, got %d: %+v",
			report.Succeeded, report.Tasks)
	}
}

// AND A DEPENDENT POOL CAN STILL BE EXHAUSTED. The reserve is a bound, not an
// exemption: work that overspends its own share is stopped like anything else.
func TestADependentIsStillStoppedWhenItsOwnPoolRunsOut(t *testing.T) {
	budget := okBudget()
	budget["max_tokens"] = float64(1_400_000)
	plan := mustPlan(t, []any{
		task("f1", "find"),
		task("t1", "report", "f1"),
		task("t2", "report", "f1"),
	}, budget, readOnlyLimits())

	dispatched := 0
	ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			dispatched++
			if req.Task.ID == "f1" {
				return TaskResult{Outcome: TaskSucceeded, Tokens: 35_000, Output: "found"}, nil
			}
			// A dependent that eats the entire reserve on its own.
			return TaskResult{Outcome: TaskSucceeded, Tokens: 1_400_000, Output: "done"}, nil
		}, nil)

	if dispatched > 2 {
		t.Errorf("a dependent that exhausted its own pool did not stop the next one: %d dispatched", dispatched)
	}
}

// A PLAN WITH NO DEPENDENCY EDGES KEEPS ITS WHOLE BUDGET.
//
// The reserve exists to protect later work from earlier work. With no later
// work, holding a quarter back protects nothing and simply cuts the plan short
// of a budget its author set deliberately.
func TestAPlanWithNoDependenciesIsNotChargedTheReserve(t *testing.T) {
	budget := okBudget()
	budget["max_tokens"] = float64(400_000)
	plan := mustPlan(t, []any{task("a", "x"), task("b", "y"), task("c", "z"), task("d", "w")}, budget, readOnlyLimits())

	dispatched := 0
	ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			dispatched++
			if req.WaitsOnOtherTasks {
				t.Errorf("task %q waits on nothing but was put in the reserved pool", req.Task.ID)
			}
			// Together these come to 480,000. The fourth is dispatched only if the
			// full 400,000 is available: a quarter-reserve would leave 300,000 and
			// stop it after the third.
			return TaskResult{Outcome: TaskSucceeded, Tokens: 120_000, Output: "ok"}, nil
		}, nil)

	if dispatched != 4 {
		t.Errorf("a plan with no dependencies ran %d of 4 tasks; a reserve it has no use for cut it short", dispatched)
	}
}

// THE RESERVE IS SIZED BY THE PLAN, NOT BY THE BUDGET.
//
// It was a flat quarter. A seven-task plan with three downstream tasks gave them
// 125,000 between them; verify and sweep used 149,769 and synthesis was skipped
// for a budget that had been reserved on their behalf. The idea was right and
// the divisor was measuring the wrong thing.
func TestTheReserveScalesWithHowMuchDownstreamWorkThereIs(t *testing.T) {
	// Four finders, three downstream: three sevenths held back.
	seven := &planSpend{limit: 700_000, downstreamTasks: 3, totalTasks: 7}
	if got := seven.reserve(); got != 300_000 {
		t.Errorf("reserve = %d, want 300000 (three sevenths of 700000)", got)
	}
	if got := seven.ceilingFor(false); got != 400_000 {
		t.Errorf("upstream ceiling = %d, want 400000", got)
	}

	// A plan that is mostly synthesis keeps most of its budget for synthesis.
	mostly := &planSpend{limit: 1_000_000, downstreamTasks: 8, totalTasks: 10}
	if got := mostly.reserve(); got != 800_000 {
		t.Errorf("reserve = %d, want 800000 (eight tenths)", got)
	}

	// A plan that is mostly finding keeps little, which is correct in the other
	// direction: there is barely any later work to protect.
	barely := &planSpend{limit: 1_000_000, downstreamTasks: 1, totalTasks: 10}
	if got := barely.reserve(); got != 100_000 {
		t.Errorf("reserve = %d, want 100000 (one tenth)", got)
	}

	// No downstream work reserves nothing, and the whole budget stays available.
	none := &planSpend{limit: 1_000_000, downstreamTasks: 0, totalTasks: 5}
	if got := none.reserve(); got != 0 {
		t.Errorf("reserve = %d, want none", got)
	}
	if got := none.ceilingFor(false); got != 1_000_000 {
		t.Errorf("with no later work the upstream ceiling was %d, want the whole budget", got)
	}
}

// THE RUN THAT PROVED IT: three downstream tasks must fit in their own share.
func TestThreeDownstreamTasksFitTheReserveThatFlatQuarterDenied(t *testing.T) {
	// The observed spend: verify 86,986 and sweep 62,783 came to 149,769, which a
	// flat quarter of 500,000 (125,000) could not hold.
	flat := int64(500_000) / 4
	if flat >= 149_769 {
		t.Fatal("setup: the flat quarter would have been enough, so this proves nothing")
	}
	proportional := (&planSpend{limit: 500_000, downstreamTasks: 3, totalTasks: 7}).reserve()
	if proportional < 149_769 {
		t.Errorf("the proportional reserve is %d, still short of the 149769 that was actually needed", proportional)
	}
}

// THE CAP MUST REACH A REAL RUN, not just the meter.
//
// Unwiring the plan's task counts leaves the reserve at zero, which reads as
// "no later work to protect" and hands every task the whole budget. That is
// MORE permissive, so no test asserting dependents still run can notice it —
// the thing to assert is that upstream work is actually capped.
func TestUpstreamWorkIsCappedInARealRun(t *testing.T) {
	budget := okBudget()
	budget["max_tokens"] = float64(2_300_000)
	// Two upstream, two downstream: half the budget is reserved, so upstream
	// stops after 200,000.
	plan := mustPlan(t, []any{
		task("f1", "find"), task("f2", "find"),
		task("v1", "check", "f1"), task("v2", "check", "f2"),
	}, budget, readOnlyLimits())

	var mu sync.Mutex
	ran := map[string]bool{}
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			mu.Lock()
			ran[req.Task.ID] = true
			mu.Unlock()
			if strings.HasPrefix(req.Task.ID, "f") {
				// Each finder alone exhausts the upstream half.
				return TaskResult{Outcome: TaskSucceeded, Tokens: 1_200_000, Output: "found"}, nil
			}
			return TaskResult{Outcome: TaskSucceeded, Tokens: 60_000, Output: "checked"}, nil
		}, nil)

	mu.Lock()
	defer mu.Unlock()
	if ran["f2"] {
		t.Error("the second finder ran after the first exhausted the upstream share; upstream is not capped")
	}
	// And the reserve did its job: downstream work still ran.
	if !ran["v1"] {
		t.Errorf("downstream work was starved by upstream: %+v", report.Tasks)
	}
}

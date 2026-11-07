package specialist

import (
	"context"
	"os"
	"strings"
	"testing"
)

// ROUTER SPEND IS THE PLAN'S SPEND.
//
// auto_assign makes a frontier-model call to decide which model runs each task,
// BEFORE any task starts and outside the executor entirely — plan_tool routes at
// its own call site and only then reaches ExecutePlanIn. Its tokens therefore
// reached neither the budget nor the report, and the code said so in as many
// words: "the plan's reported spend is under its real spend by exactly this
// much, every run, invisibly."
//
// Measured across 17 real sessions: 7,860 to 10,513 tokens per plan.
const measuredRouterTokens = 10_513

func routerSpendPlan(t *testing.T) Plan {
	t.Helper()
	return mustParsePlan(t, map[string]any{
		"name":   "p",
		"tasks":  []any{map[string]any{"id": "a", "prompt": "look"}},
		"budget": map[string]any{"max_workers": float64(1)},
	}, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})
}

func succeedingRun(tokens int) PlanRunner {
	return func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
		return TaskResult{ID: req.Task.ID, Outcome: TaskSucceeded, Output: "done", Tokens: tokens}, nil
	}
}

// THE REPORTED TOTAL MUST INCLUDE IT. This is the number a person reads to
// decide whether a plan was worth what it cost.
func TestTheReportedSpendIncludesWhatRoutingCost(t *testing.T) {
	const taskTokens = 500_000

	without := ExecutePlan(context.Background(), routerSpendPlan(t),
		PlanReadOnlyToolNames(), succeedingRun(taskTokens), nil)
	if without.TokensUsed != taskTokens {
		t.Fatalf("an unrouted plan reported %d, want %d", without.TokensUsed, taskTokens)
	}

	with := ExecutePlan(context.Background(), routerSpendPlan(t),
		PlanReadOnlyToolNames(), succeedingRun(taskTokens), nil,
		WithPreSpentTokens(measuredRouterTokens))
	if want := taskTokens + measuredRouterTokens; with.TokensUsed != want {
		t.Fatalf("a routed plan reported %d, want %d — routing is still invisible",
			with.TokensUsed, want)
	}
}

// A PLAN THAT NEVER ROUTED IS UNCHANGED, byte for byte. auto_assign is off by
// default and most plans never route at all.
func TestAPlanThatDidNotRouteIsUnaffected(t *testing.T) {
	for _, tokens := range []int{0, -1} {
		report := ExecutePlan(context.Background(), routerSpendPlan(t),
			PlanReadOnlyToolNames(), succeedingRun(1000), nil, WithPreSpentTokens(tokens))
		if report.TokensUsed != 1000 {
			t.Fatalf("WithPreSpentTokens(%d) changed an unrouted plan's total to %d", tokens, report.TokensUsed)
		}
	}
}

// THE BUDGET SEES THE SAME NUMBER THE REPORT DOES. If the report counted router
// spend and the limit did not, "budget exhausted at N/M" would print an N
// counting something never charged against M — two spellings of one quantity,
// free to drift.
func TestTheBudgetIsReducedByWhatRoutingAlreadySpent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		budget   int
		preSpent int
		want     int64
	}{
		{"ordinary", 1_000_000, 10_513, 989_487},
		{"unbounded stays unbounded", 0, 10_513, 0},
		{"nothing pre-spent", 1_000_000, 0, 1_000_000},
		// A POSITIVE BUDGET NEVER BECOMES UNBOUNDED: limit <= 0 means "no bound"
		// everywhere in planSpend, so a plan that has already overspent must
		// floor at 1 and refuse its next task, not have its bound switched off.
		{"pre-spend equals the budget", 10_513, 10_513, 1},
		{"pre-spend exceeds the budget", 5_000, 10_513, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := planSpendLimit(tc.budget, tc.preSpent); got != tc.want {
				t.Fatalf("planSpendLimit(%d, %d) = %d, want %d", tc.budget, tc.preSpent, got, tc.want)
			}
		})
	}
}

// The floor is not cosmetic: at limit 1 the very next task must be refused.
// Switching the bound off instead would do the exact opposite of what an
// already-overspent plan needs.
func TestAnAlreadyOverspentPlanStillRefusesItsNextTask(t *testing.T) {
	overspent := &planSpend{limit: planSpendLimit(5_000, 10_513)}
	if !overspent.overPool(overspent.add(1_000, false), false) {
		t.Fatal("a plan that had already overspent its budget accepted more work")
	}
	// And an unbounded plan is still unbounded after the same treatment.
	unbounded := &planSpend{limit: planSpendLimit(0, 10_513)}
	if unbounded.overPool(unbounded.add(9_000_000, false), false) {
		t.Fatal("an unbounded plan grew a ceiling nobody asked for")
	}
}

// SEEDED, NOT ADDED AT THE END. Every early return between the seed and the end
// reports whatever TokensUsed holds, so a total corrected only on the success
// path would be wrong on exactly the runs a reader most wants the truth about.
func TestACancelledPlanStillReportsWhatRoutingCost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	run := func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
		cancel()
		return TaskResult{ID: req.Task.ID, Outcome: TaskCancelled, Output: "stopped"}, context.Canceled
	}
	report := ExecutePlan(ctx, routerSpendPlan(t), PlanReadOnlyToolNames(), run, nil,
		WithPreSpentTokens(measuredRouterTokens))
	if report.TokensUsed < measuredRouterTokens {
		t.Fatalf("a cancelled plan reported %d, losing the %d routing already cost",
			report.TokensUsed, measuredRouterTokens)
	}
}

// BOTH EXECUTION PATHS. plan_tool runs a plan in the foreground and in the
// background from two different call sites; wiring one and not the other would
// make the reported total correct only for whichever the user happened to pick.
func TestBothExecutionPathsChargeRouterSpend(t *testing.T) {
	// Both paths now build their options through ONE builder, so the check is
	// that the builder is what they use — and that it always charges the spend.
	// (It previously counted a literal WithPreSpentTokens at each call site;
	// the shared builder made that string appear once, and the test failed while
	// the behaviour was correct.)
	source := readFileForTest(t, "plan_tool.go")
	calls := strings.Count(source, "ExecutePlanIn(")
	built := strings.Count(source, "tool.execOptionsFor(plan, routerTokens)...")
	if calls != built {
		t.Fatalf("plan_tool.go has %d ExecutePlanIn call(s) but builds options for %d of them", calls, built)
	}

	var applied execOptions
	for _, opt := range (&OrchestrateTool{}).execOptionsFor(routerSpendPlan(t), measuredRouterTokens) {
		opt(&applied)
	}
	if applied.preSpent != measuredRouterTokens {
		t.Fatalf("the shared builder does not charge router spend: preSpent=%d", applied.preSpent)
	}
}

// readFileForTest reads a source file in this package.
//
// Used only by the both-paths check above, which asserts on the CALL SITES
// rather than on behaviour: the two paths are foreground and background, and
// exercising the background one needs a launcher and a context that outlives the
// turn. A source-level check is weaker than a behavioural one and is chosen
// deliberately over asserting nothing about the second path at all.
func readFileForTest(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}

// AND THE EXECUTOR MUST USE IT. The table above asserts planSpendLimit's
// arithmetic and proves nothing about whether ExecutePlanIn consults it — a
// mutation that built the meter from the raw budget passed it cleanly.
//
// So this runs a real two-task plan against a real budget: the first task spends
// most of it, and the second must be SKIPPED once routing has already taken its
// share, while the identical plan without a pre-spend still affords it.
func TestThePreSpendActuallyChangesWhatTheExecutorAffords(t *testing.T) {
	const budget = 200_000
	const firstTask = 150_000
	const routed = 60_000

	plan := func() Plan {
		return mustParsePlan(t, map[string]any{
			"name": "p",
			"tasks": []any{
				map[string]any{"id": "a", "prompt": "first"},
				map[string]any{"id": "b", "prompt": "second"},
			},
			// max_workers 1 so "a" has finished and been counted before "b" is
			// considered; at 2 they dispatch together and neither is gated.
			"budget": map[string]any{"max_workers": float64(1), "max_tokens": float64(budget)},
		}, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})
	}
	run := func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
		spent := firstTask
		if req.Task.ID == "b" {
			spent = 1_000
		}
		// Report through the plan's own meter, exactly as the real runner does.
		if req.Spend != nil {
			req.Spend.add(spent, req.WaitsOnOtherTasks)
		}
		return TaskResult{ID: req.Task.ID, Outcome: TaskSucceeded, Output: "done", Tokens: spent}, nil
	}

	affordable := ExecutePlan(context.Background(), plan(), PlanReadOnlyToolNames(), run, nil)
	if affordable.Skipped != 0 {
		t.Fatalf("setup: without a pre-spend the plan already skipped %d task(s); the budget is too tight to show anything",
			affordable.Skipped)
	}

	squeezed := ExecutePlan(context.Background(), plan(), PlanReadOnlyToolNames(), run, nil,
		WithPreSpentTokens(routed))
	if squeezed.Skipped == 0 {
		t.Fatalf("routing spent %d of a %d budget and the plan afforded exactly as much work as before: "+
			"the pre-spend never reached the meter", routed, budget)
	}
}

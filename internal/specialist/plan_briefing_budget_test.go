package specialist

import (
	"context"
	"strings"
	"testing"
)

// THE UNKNOWN WINDOW IS THE MAJORITY CASE, so it is the one that must not move.
// Most of this catalogue reports no window at all, and every one of those runs
// must behave byte-identically to before this existed.
func TestAnUnknownWindowKeepsTheExactBudgetsFromBefore(t *testing.T) {
	perTask, total := dependencyBriefingBudget(0)
	if perTask != 4000 || total != 12000 {
		t.Fatalf("unknown window changed the budget to %d/%d, want 4000/12000", perTask, total)
	}
	// A provider reporting a nonsensical window is unknown, not tiny.
	if p, tt := dependencyBriefingBudget(-1); p != 4000 || tt != 12000 {
		t.Fatalf("a negative window produced %d/%d", p, tt)
	}
}

// The ratio the original constants chose is DERIVED, not re-picked, so a model
// near the size those constants implied lands on exactly today's numbers.
func TestAWindowMatchingTodaysConstantsReproducesThem(t *testing.T) {
	// 30_000 tokens * 4 chars * 0.10 == 12_000, the existing total.
	perTask, total := dependencyBriefingBudget(30_000)
	if total != 12000 || perTask != 4000 {
		t.Fatalf("the anchor window produced %d/%d, want 4000/12000 — the fraction silently re-tuned every plan", perTask, total)
	}
}

// A large window carries a real report whole. The measured case: a code-review
// child produced 18,349 characters, and at the fixed cap a dependent saw 4,000.
func TestALargeWindowCarriesAWholeReport(t *testing.T) {
	const measuredReportChars = 18_349
	perTask, _ := dependencyBriefingBudget(204_800)
	if perTask <= measuredReportChars {
		t.Fatalf("per-dependency budget %d still truncates the measured %d-character report", perTask, measuredReportChars)
	}
	if fixed, _ := dependencyBriefingBudget(0); perTask <= fixed {
		t.Fatalf("a 204k-context reader got %d, no more than the fixed %d", perTask, fixed)
	}
}

// Bounded at both ends: a tiny model still gets something usable, and a 1M model
// does not inherit an entire plan — the reason the caps existed at all.
func TestTheBudgetIsBoundedAtBothEnds(t *testing.T) {
	if _, total := dependencyBriefingBudget(1_000); total < briefingFloorTotal {
		t.Fatalf("a tiny window produced %d, below the floor %d: the briefing stops being worth reading", total, briefingFloorTotal)
	}
	if _, total := dependencyBriefingBudget(1_000_000); total > briefingCeilingTotal {
		t.Fatalf("a 1M window produced %d, above the ceiling %d: a deep chain accumulates the whole plan", total, briefingCeilingTotal)
	}
	// Monotonic: a bigger reader never gets a smaller budget.
	prev := 0
	for _, window := range []int{0, 8_000, 32_768, 128_000, 204_800, 1_000_000} {
		if window == 0 {
			continue
		}
		_, total := dependencyBriefingBudget(window)
		if total < prev {
			t.Fatalf("window %d got %d, less than the smaller window's %d", window, total, prev)
		}
		prev = total
	}
}

// THE BUDGET BELONGS TO THE READER. With per-task models a large-context
// synthesiser routinely depends on a small-context finder; sizing by the
// producer would starve the reader for no reason.
func TestTheBudgetFollowsTheReadingTaskNotTheWritingOne(t *testing.T) {
	windows := map[string]int{"small-finder": 8_000, "big-synth": 204_800}
	options := execOptions{contextWindow: func(model string) int { return windows[model] }}

	finder := Task{ID: "find", Model: "small-finder"}
	synth := Task{ID: "synth", Model: "big-synth", DependsOn: []string{"find"}}

	_, finderTotal := dependencyBriefingBudget(options.windowFor(finder))
	_, synthTotal := dependencyBriefingBudget(options.windowFor(synth))
	if synthTotal <= finderTotal {
		t.Fatalf("the synthesiser (204k) got %d while its 8k dependency got %d: sized by the wrong task", synthTotal, finderTotal)
	}
}

// A caller that wires nothing keeps the fixed caps — the nil path every existing
// call site takes.
func TestACallerThatWiresNoWindowsKeepsTheFixedCaps(t *testing.T) {
	var options execOptions
	if window := options.windowFor(Task{Model: "anything"}); window != 0 {
		t.Fatalf("an unwired option reported window %d", window)
	}
	// And a nil option in the variadic list must be skipped, not dereferenced.
	if got := dependencyBriefingBudgetTotalFor(nil); got != 12000 {
		t.Fatalf("a nil option produced total %d", got)
	}
}

// END TO END through the real briefing, because a budget function that nothing
// consults is the defect this branch has already produced twice.
func TestTheExecutorAppliesTheWindowBudgetToTheRealBriefing(t *testing.T) {
	long := strings.Repeat("F", 30_000)
	results := map[string]TaskResult{
		"find": {ID: "find", Outcome: TaskSucceeded, Output: long},
	}
	task := Task{ID: "synth", DependsOn: []string{"find"}}

	fixedPer, fixedTotal := dependencyBriefingBudget(0)
	fixed := withDependencyBriefingBudget(task, results, fixedPer, fixedTotal)

	widePer, wideTotal := dependencyBriefingBudget(204_800)
	wide := withDependencyBriefingBudget(task, results, widePer, wideTotal)

	if len(wide) <= len(fixed) {
		t.Fatalf("a 204k reader got a briefing of %d chars, no more than the fixed %d", len(wide), len(fixed))
	}
	if !strings.Contains(fixed, "find") || !strings.Contains(wide, "find") {
		t.Fatal("the briefing no longer names its dependency")
	}
	// The wide one must actually carry more of the OUTPUT, not just more heading.
	if strings.Count(wide, "F") <= strings.Count(fixed, "F") {
		t.Fatal("the larger budget carried no more of the dependency's answer")
	}
}

// THE PLUMBING, asserted on what the dependent task ACTUALLY RECEIVES.
//
// An earlier version of this test asserted only that the hook was called, and a
// mutation that orphaned the computed budget at the call site — computing it and
// then briefing at the fixed caps anyway — passed it cleanly. That is the defect
// class this branch has now produced three times: proving the helper works while
// nothing proves the caller consults it. So this reads req.Task.Prompt, which is
// the briefed prompt the child is handed.
func TestExecutePlanInHonoursTheContextWindowOption(t *testing.T) {
	planArgs := map[string]any{
		"name": "p",
		"tasks": []any{
			map[string]any{"id": "find", "prompt": "look"},
			map[string]any{"id": "synth", "prompt": "combine", "depends_on": []any{"find"}},
		},
		"budget": map[string]any{"max_workers": float64(1)},
	}
	limits := Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()}

	briefedSynthPrompt := func(opts ...ExecOption) string {
		var got string
		run := func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			if req.Task.ID == "synth" {
				got = req.Task.Prompt
			}
			return TaskResult{ID: req.Task.ID, Outcome: TaskSucceeded, Output: strings.Repeat("F", 30_000)}, nil
		}
		report := ExecutePlan(context.Background(), mustParsePlan(t, planArgs, limits),
			PlanReadOnlyToolNames(), run, nil, opts...)
		if report.Failed != 0 {
			t.Fatalf("the plan failed: %+v", report)
		}
		if got == "" {
			t.Fatal("the dependent task never ran, so nothing was measured")
		}
		return got
	}

	fixed := briefedSynthPrompt()
	wide := briefedSynthPrompt(WithContextWindows(func(string) int { return 204_800 }))

	if strings.Count(wide, "F") <= strings.Count(fixed, "F") {
		t.Fatalf("the dependent task received %d characters of its dependency's answer with a 204k window "+
			"and %d without one: the computed budget never reached the briefing",
			strings.Count(wide, "F"), strings.Count(fixed, "F"))
	}
	// And an unwired run must be byte-identical to before the option existed.
	if want := strings.Count(fixed, "F"); want != dependencyBriefingPerTask {
		t.Fatalf("an unwired run briefed %d characters, not the fixed %d", want, dependencyBriefingPerTask)
	}
}

// dependencyBriefingBudgetTotalFor runs the option list exactly as ExecutePlanIn
// does, so the nil-tolerance above is asserted against the real loop.
func dependencyBriefingBudgetTotalFor(opts []ExecOption) int {
	var options execOptions
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	_, total := dependencyBriefingBudget(options.windowFor(Task{}))
	return total
}

// THE OPTIONS MUST REACH BOTH EXECUTION PATHS, and must be built in ONE place.
//
// THE AUDIT FINDING THIS PINS. WithContextWindows and WithScratchpad were built,
// tested and mutation-checked — and had exactly one production reference each:
// their own definitions. Nothing ever passed them, so every real plan kept the
// fixed 4000/12000 caps and no plan ever got a scratchpad. Mutation testing
// cannot catch an unwired feature, because deleting it breaks nothing.
func TestTheToolPassesItsExecOptionsToBothPaths(t *testing.T) {
	source := readFileForTest(t, "plan_tool.go")
	calls := strings.Count(source, "ExecutePlanIn(")
	built := strings.Count(source, "tool.execOptionsFor(plan, routerTokens)...")
	if calls != built {
		t.Fatalf("plan_tool.go has %d ExecutePlanIn call(s) but builds options for %d of them", calls, built)
	}
}

// And the builder must actually include each option when its input is present.
func TestExecOptionsCarryTheWindowAndTheScratchpad(t *testing.T) {
	withDeps := mustParsePlan(t, map[string]any{
		"name": "p",
		"tasks": []any{
			map[string]any{"id": "a", "prompt": "look"},
			map[string]any{"id": "b", "prompt": "combine", "depends_on": []any{"a"}},
		},
		"budget": map[string]any{"max_workers": float64(1)},
	}, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})

	tool := &OrchestrateTool{ContextWindows: func(string) int { return 204_800 }}
	var applied execOptions
	for _, opt := range tool.execOptionsFor(withDeps, 1000) {
		opt(&applied)
	}
	if applied.preSpent != 1000 {
		t.Fatalf("router spend not carried: %d", applied.preSpent)
	}
	if applied.contextWindow == nil {
		t.Fatal("a wired ContextWindows hook never reached the executor: briefings keep the fixed caps")
	}
	if !applied.scratchpad {
		t.Fatal("a plan WITH dependencies got no scratchpad, so a truncated briefing stays unreachable")
	}

	// A plan whose tasks depend on nothing writes no briefing, so a scratchpad
	// would be created, populated and deleted for no reader.
	noDeps := mustParsePlan(t, map[string]any{
		"name":   "p",
		"tasks":  []any{map[string]any{"id": "a", "prompt": "look"}},
		"budget": map[string]any{"max_workers": float64(1)},
	}, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})
	var bare execOptions
	for _, opt := range (&OrchestrateTool{}).execOptionsFor(noDeps, 0) {
		opt(&bare)
	}
	if bare.scratchpad {
		t.Fatal("a dependency-free plan got a scratchpad nothing can read")
	}
	if bare.contextWindow != nil {
		t.Fatal("an unwired hook produced a non-nil window func")
	}
}

package specialist

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

func usageEvents(total int) []streamjson.Event {
	return []streamjson.Event{{Type: streamjson.EventUsage, TotalTokens: &total}}
}

// A BUDGET THAT CANNOT COVER ITS OWN TASKS IS REFUSED BEFORE ANYTHING IS SPENT.
//
// The measured run: six tasks admitted against 200,000 tokens, 3,091,618 spent,
// and the answer still came back incomplete because the two tasks that had not
// started were skipped once the meter caught up. The user paid for the overrun
// AND lost a third of the audit. Refusing costs nothing and is the only response
// that can still produce a complete answer.
func TestAPlanWhoseBudgetCannotCoverItsTasksIsRefusedAtAdmission(t *testing.T) {
	budget := okBudget()
	budget["max_tokens"] = float64(200_000)
	_, err := ParsePlan(planArgs([]any{
		task("a", "x"), task("b", "y"), task("c", "z"),
		task("d", "w"), task("e", "v"), task("f", "u"),
	}, budget), readOnlyLimits())
	if err == nil {
		t.Fatal("the observed run's budget was admitted again")
	}
	for _, want := range []string{"200000", "6 tasks", "Raise max_tokens"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// It must not become a required field by the back door. Unbounded is a
// deliberate choice recorded in planBudget.
func TestAnUnsetBudgetIsStillAllowedToRunUnbounded(t *testing.T) {
	budget := map[string]any{"max_workers": float64(1)}
	if _, err := ParsePlan(planArgs([]any{task("a", "x"), task("b", "y")}, budget), readOnlyLimits()); err != nil {
		t.Fatalf("omitting max_tokens must stay legal: %v", err)
	}
}

// When the run's own ceiling is below what the plan needs, "raise max_tokens" is
// advice the next call cannot take — the ceiling refuses it — and the caller
// loops between two errors that each blame the other.
func TestAnUnraisableCeilingSaysThePlanIsTooBigInstead(t *testing.T) {
	budget := okBudget()
	budget["max_tokens"] = float64(60_000)
	_, err := ParsePlan(planArgs([]any{task("a", "x"), task("b", "y"), task("c", "z")}, budget),
		Limits{MaxTasks: 20, MaxTokens: 100_000, ParentTools: []string{"read_file"}})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(err.Error(), "Raise max_tokens") {
		t.Errorf("told to raise a number this run's ceiling forbids: %v", err)
	}
	if !strings.Contains(err.Error(), "too big for this run") {
		t.Errorf("the refusal does not say the plan is too big: %v", err)
	}
}

// A ceiling too small for even one task must not produce "ask for at most 0
// tasks", which is not advice.
func TestACeilingBelowOneTaskSaysSoRatherThanSuggestingZeroTasks(t *testing.T) {
	budget := okBudget()
	budget["max_tokens"] = float64(100)
	_, err := ParsePlan(planArgs([]any{task("a", "x")}, budget),
		Limits{MaxTasks: 20, MaxTokens: 100, ParentTools: []string{"read_file"}})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if strings.Contains(err.Error(), "at most 0") {
		t.Errorf("suggested a plan of zero tasks: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot fund a single plan task") {
		t.Errorf("unhelpful refusal: %v", err)
	}
}

// THE PER-TASK CAP STOPS A TASK FROM INSIDE. A plan-level budget cannot: in the
// measured run one task spent 1,017,177 against a 200,000 plan budget, because
// the plan's limit is only consulted between tasks.
func TestAPerTaskCapStopsATaskWhileItIsStillRunning(t *testing.T) {
	exec := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(ctx context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			// Three provider calls; the cap sits after the second.
			events := usageEvents(40_000)
			for round := 0; round < 3; round++ {
				for _, event := range events {
					if progress != nil {
						progress(event)
					}
				}
				if ctx.Err() != nil {
					return ChildRunResult{Started: true, ExitCode: -1}, ctx.Err()
				}
			}
			return ChildRunResult{Started: true}, nil
		},
	}
	run := NewPlanRunner(PlanTaskContext{Executor: exec, Cwd: t.TempDir(), SpecialistName: "explorer"})
	result, _ := run(context.Background(), PlanTaskRequest{
		Task:          Task{ID: "greedy", Prompt: "p"},
		Tools:         []string{"read_file"},
		MaxTaskTokens: 60_000,
	})

	if result.Outcome != TaskCancelled {
		t.Fatalf("the task ran past its cap: %s / %s", result.Outcome, result.Err)
	}
	if !strings.Contains(result.Err, "max_tokens_per_task") {
		t.Errorf("the reason does not name the cap that stopped it: %q", result.Err)
	}
	// NOT A STALL. A stalled task is retryable; retrying a task stopped for
	// spending too much is the worst possible response to it.
	if result.Stalled {
		t.Error("a budget stop was recorded as a stall, which makes it retryable")
	}
}

// The plan's own limit must bite mid-run too, not only between tasks.
func TestThePlanBudgetStopsATaskThatCrossesItWhileRunning(t *testing.T) {
	exec := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(ctx context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			for round := 0; round < 5; round++ {
				if progress != nil {
					progress(usageEvents(50_000)[0])
				}
				if ctx.Err() != nil {
					return ChildRunResult{Started: true, ExitCode: -1}, ctx.Err()
				}
			}
			return ChildRunResult{Started: true}, nil
		},
	}
	run := NewPlanRunner(PlanTaskContext{Executor: exec, Cwd: t.TempDir(), SpecialistName: "explorer"})
	spend := &planSpend{limit: 120_000, downstreamTasks: 1, totalTasks: 4}
	result, _ := run(context.Background(), PlanTaskRequest{
		Task:  Task{ID: "solo", Prompt: "p"},
		Tools: []string{"read_file"},
		Spend: spend,
	})

	if result.Outcome != TaskCancelled {
		t.Fatalf("the plan budget did not stop the task: %s / %s", result.Outcome, result.Err)
	}
	if !strings.Contains(result.Err, "token budget") {
		t.Errorf("the reason does not name the plan budget: %q", result.Err)
	}
	// Bounded overshoot: one usage event past the line, not five.
	if spent := spend.upstream.Load(); spent > 200_000 {
		t.Errorf("overshoot is unbounded: spent %d against a 120000 limit", spent)
	}
}

// An unbounded plan must behave exactly as it always did: the meter still
// counts, and no pool exists to cross.
func TestAnUnboundedPlanIsNeverStoppedByTheMeter(t *testing.T) {
	spend := &planSpend{limit: 0, downstreamTasks: 1, totalTasks: 4}
	for round := 0; round < 100; round++ {
		if spend.overPool(spend.add(1_000_000, true), true) {
			t.Fatal("an unset limit reported the plan over budget")
		}
	}
	if got := spend.ceilingFor(true); got != 0 {
		t.Errorf("an unbounded plan produced a feeder ceiling of %d", got)
	}
	if got := spend.ceilingFor(false); got != 0 {
		t.Errorf("an unbounded plan produced a dependent ceiling of %d", got)
	}
}

// THE HEADLINE MUST NAME WHAT NEVER RAN. A budget-skipped task says so in the
// same small text every other row uses; a measured run came back missing a third
// of its questions and nothing above the fold said which.
func TestTheSummaryNamesTheTasksABudgetCutShort(t *testing.T) {
	summary := PlanReport{
		Status: PlanPartial,
		Tasks: []TaskResult{
			{ID: "config-layers", Outcome: TaskSucceeded},
			{ID: "plan-inherit", Outcome: TaskSkippedBudget, Err: "skipped: the plan's budget was exhausted"},
			{ID: "skills-mcp-surface", Outcome: TaskSkippedBudget, Err: "skipped: the plan's budget was exhausted"},
		},
	}.Summary()
	if !strings.Contains(summary, "2 task(s) never ran") {
		t.Fatalf("the headline hides the unanswered questions:\n%s", summary)
	}
	for _, id := range []string{"plan-inherit", "skills-mcp-surface"} {
		if !strings.Contains(summary, id) {
			t.Errorf("the headline does not name %q:\n%s", id, summary)
		}
	}
	// A plan where nothing was cut must not gain a line about it.
	clean := PlanReport{Status: PlanCompleted, Tasks: []TaskResult{{ID: "a", Outcome: TaskSucceeded}}}.Summary()
	if strings.Contains(clean, "never ran") {
		t.Errorf("a complete plan reported cut tasks:\n%s", clean)
	}
}

// The cap round-trips, so a saved plan resumes with the same bound.
func TestThePerTaskCapRoundTripsThroughArgs(t *testing.T) {
	budget := okBudget()
	budget["max_tokens"] = float64(500_000)
	budget["max_tokens_per_task"] = float64(120_000)
	plan := mustPlan(t, []any{task("a", "x")}, budget, readOnlyLimits())
	if plan.Budget().MaxTokensPerTask != 120_000 {
		t.Fatalf("parsed cap = %d", plan.Budget().MaxTokensPerTask)
	}
	again, err := ParsePlan(plan.Args(), readOnlyLimits())
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if again.Budget().MaxTokensPerTask != 120_000 {
		t.Errorf("the cap was lost on the round trip: %d", again.Budget().MaxTokensPerTask)
	}
}

// A cap above the plan's own budget bounds nothing and reads like a guarantee.
func TestAPerTaskCapAboveThePlanBudgetIsRefused(t *testing.T) {
	budget := okBudget()
	budget["max_tokens"] = float64(100_000)
	budget["max_tokens_per_task"] = float64(500_000)
	_, err := ParsePlan(planArgs([]any{task("a", "x"), task("b", "y")}, budget), readOnlyLimits())
	if err == nil || !strings.Contains(err.Error(), "bounds nothing") {
		t.Fatalf("expected a refusal, got %v", err)
	}
}

// A TASK THE BUDGET KILLED MUST NOT BE REPORTED AS SUCCEEDED.
//
// From a real run: every one of six tasks was cut short for running over budget,
// each result carrying "Subagent terminated by a signal (signal: killed)" and
// "the plan's token budget ran out while it was running" — under the headline
// "6 succeeded, 0 failed, 0 skipped". The harvest set TaskSucceeded
// unconditionally, and its guard tested only TaskFailed, so a runner returning
// TaskCancelled with no error fell straight through.
//
// Work that did not finish, reported as done, is the failure this executor
// exists to prevent.
func TestATaskCancelledForBudgetIsNeverCountedAsSucceeded(t *testing.T) {
	budget := okBudget()
	budget["max_tokens"] = float64(1_500_000)
	plan := mustPlan(t, []any{task("a", "x"), task("b", "y"), task("c", "z")}, budget, readOnlyLimits())

	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			// Exactly what the runner returns when the meter stops a task.
			return TaskResult{
				ID:      req.Task.ID,
				Outcome: TaskCancelled,
				Tokens:  900_000,
				Err:     "task " + req.Task.ID + " stopped: the plan's token budget ran out while it was running",
			}, nil
		}, nil)

	if report.Succeeded != 0 {
		t.Fatalf("%d killed task(s) were counted as succeeded", report.Succeeded)
	}
	if report.Cancelled == 0 {
		t.Fatal("a budget kill was not recorded as a cancellation")
	}
	if report.Status == PlanCompleted {
		t.Errorf("a plan whose tasks were all cut short reported %q", report.Status)
	}
	for _, result := range report.Tasks {
		if result.Outcome == TaskSucceeded {
			t.Errorf("task %q was relabelled a success after being cancelled", result.ID)
		}
	}
	// And the headline must say so, since this is what makes the answer partial.
	// CUT SHORT, not "never ran": these tasks ran and were stopped, and the
	// difference is what tells a partial report from an empty one.
	if summary := report.Summary(); !strings.Contains(summary, "cut short mid-run") {
		t.Errorf("the headline hides that the plan was cut short:\n%s", summary)
	}
}

// A CANCELLED TASK BLOCKS ITS DEPENDENTS. It did not produce what they were
// waiting for, so running them on nothing would compound the loss.
func TestDependentsOfACancelledTaskAreSkipped(t *testing.T) {
	budget := okBudget()
	budget["max_tokens"] = float64(600_000)
	plan := mustPlan(t, []any{task("trace", "x"), task("judge", "y", "trace")}, budget, readOnlyLimits())

	dispatched := 0
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			dispatched++
			return TaskResult{ID: req.Task.ID, Outcome: TaskCancelled, Err: "token budget ran out"}, nil
		}, nil)

	if dispatched != 1 {
		t.Fatalf("a dependent of a cancelled task was dispatched anyway: %d dispatches", dispatched)
	}
	byID := map[string]TaskResult{}
	for _, result := range report.Tasks {
		byID[result.ID] = result
	}
	if byID["judge"].Outcome != TaskSkippedDependency {
		t.Errorf("judge = %q, want skipped for its dependency", byID["judge"].Outcome)
	}
}

// A CAPPED TASK IS TOLD ITS BUDGET, so it can land instead of being shot down.
//
// A measured plan capped tasks at 200,000; all six were killed between 213k and
// 259k having done substantial reading, and the run cost 1,437,049 tokens for
// zero completed tasks. Each was most of the way to an answer nobody received.
// The cap has to arrive as a deadline the task can plan against, not as a
// guillotine it never sees coming.
func TestACappedTaskIsToldItsBudgetInAUnitItCanCount(t *testing.T) {
	budget := okBudget()
	budget["max_tokens_per_task"] = float64(200_000)
	plan := mustPlan(t, []any{task("cfg", "trace the config layering")}, budget, readOnlyLimits())

	var prompt string
	ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			prompt = req.Task.Prompt
			return TaskResult{Outcome: TaskSucceeded, Output: "ok"}, nil
		}, nil)

	if !strings.Contains(prompt, "trace the config layering") {
		t.Fatalf("the task's own prompt was lost:\n%s", prompt)
	}
	// TOLD LESS THAN THE KILL POINT. A task told its full cap would still be
	// writing when the hard limit arrived — the runway is the difference.
	if !strings.Contains(prompt, "160000") {
		t.Errorf("the announced budget is not the cap minus a landing strip:\n%s", prompt)
	}
	if strings.Contains(prompt, "200000") {
		t.Error("the task was told the hard kill point, leaving it no room to write an answer")
	}
	// A MODEL CANNOT SEE ITS TOKEN METER. Tool calls are the countable proxy.
	if !strings.Contains(prompt, "tool calls") {
		t.Errorf("the budget is stated in a unit the task cannot observe:\n%s", prompt)
	}
	if !strings.Contains(prompt, "partial answer") {
		t.Errorf("the task is not told that a partial answer beats being cut off:\n%s", prompt)
	}
}

// An uncapped task's prompt is untouched — every plan that does not ask for a cap.
func TestAnUncappedTaskGetsNoBudgetNotice(t *testing.T) {
	if got := withTokenBudgetNotice("do the thing", 0); got != "do the thing" {
		t.Errorf("an uncapped task's prompt was rewritten: %q", got)
	}
}

// A CAP BELOW WHAT ANY TASK COSTS IS REFUSED. It would kill every task and the
// plan would pay in full for nothing — measured at 1,437,049 tokens and zero
// completed work.
func TestAPerTaskCapBelowAnyPlausibleTaskIsRefused(t *testing.T) {
	budget := okBudget()
	budget["max_tokens_per_task"] = float64(5_000)
	_, err := ParsePlan(planArgs([]any{task("a", "x")}, budget), readOnlyLimits())
	if err == nil {
		t.Fatal("a cap no task could meet was admitted")
	}
	if !strings.Contains(err.Error(), "cut short") {
		t.Errorf("the refusal does not say what would happen: %v", err)
	}
	// A tight-but-workable cap must still be allowed — that is what the landing
	// strip is for.
	budget["max_tokens_per_task"] = float64(200_000)
	if _, err := ParsePlan(planArgs([]any{task("a", "x")}, budget), readOnlyLimits()); err != nil {
		t.Errorf("a tight cap must remain legal: %v", err)
	}
}

// A TRIMMED WORKER COUNT MUST BE VISIBLE. The report struct promises both
// numbers — "a plan that asked for sixteen and ran six has not been given
// sixteen" — and only the actual one was ever printed, so the trim was invisible
// and the speedup beneath it read as the plan's own fault.
func TestTheSummarySaysWhenTheMachineAllowedFewerWorkers(t *testing.T) {
	trimmed := PlanReport{Status: PlanCompleted, Workers: 6, WorkersRequested: 16,
		Tasks: []TaskResult{{ID: "a", Outcome: TaskSucceeded}}}.Summary()
	if !strings.Contains(trimmed, "requested 16") {
		t.Errorf("a trimmed worker count is invisible:\n%s", trimmed)
	}
	// Unchanged when the request was honoured, which is the ordinary case.
	honoured := PlanReport{Status: PlanCompleted, Workers: 4, WorkersRequested: 4,
		Tasks: []TaskResult{{ID: "a", Outcome: TaskSucceeded}}}.Summary()
	if strings.Contains(honoured, "requested") {
		t.Errorf("an honoured request produced a line about being trimmed:\n%s", honoured)
	}
}

// A WRITE PLAN MUST SAY WHERE IT WROTE. The worktree is deliberately not removed
// — "a plan that wrote produced work nobody has reviewed; deleting it would
// delete the only copy" — so an unnamed location is the difference between work
// the user can review and work they cannot find.
func TestAWritePlanDisclosesTheWorkspaceItWroteIn(t *testing.T) {
	note := planWorkspaceNote(PlanWorkspace{
		Path: "/tmp/wt/plan-fix", Isolated: true, Describe: "worktree plan-fix at /tmp/wt/plan-fix",
	})
	if !strings.Contains(note, "worktree plan-fix at /tmp/wt/plan-fix") {
		t.Errorf("the write workspace is not disclosed: %q", note)
	}
	if !strings.Contains(note, "nothing was written to the parent tree") {
		t.Errorf("the note does not reassure about the parent tree: %q", note)
	}
	// Falls back to the raw path rather than saying nothing.
	if got := planWorkspaceNote(PlanWorkspace{Path: "/tmp/wt/x", Isolated: true}); !strings.Contains(got, "/tmp/wt/x") {
		t.Errorf("a describe-less workspace disclosed nothing: %q", got)
	}
	// A READ-ONLY PLAN HAS NOTHING TO DISCLOSE — it runs in the parent's tree,
	// and a line about isolation there would be false.
	if got := planWorkspaceNote(PlanWorkspace{}); got != "" {
		t.Errorf("a read-only plan claimed an isolated workspace: %q", got)
	}
}

// AND THE TOOL MUST ACTUALLY EMIT IT. The test above asserts the helper builds
// the right sentence and proves nothing about whether anything prints it — a
// mutation dropping the call from the tool's output passed it cleanly.
//
// This is the fourth time in one session that a test asserted a helper instead
// of the caller that consults it. Both seams, both asserted.
func TestTheToolPrintsWhereAWritePlanWrote(t *testing.T) {
	tool := &OrchestrateTool{
		PostureActive: func() bool { return true },
		ParentTools:   []string{"read_file", "write_file"},
		RunTask: func(context.Context, PlanTaskRequest) (TaskResult, error) {
			return TaskResult{Outcome: TaskSucceeded, Output: "wrote it"}, nil
		},
		Isolate: func(context.Context, string) (PlanWorkspace, error) {
			return PlanWorkspace{
				Path: "/tmp/wt/fix", Isolated: true,
				Describe: "worktree fix at /tmp/wt/fix",
				Release:  func() {},
			}, nil
		},
	}
	result := tool.RunWithOptions(context.Background(), map[string]any{
		"name":   "fix",
		"budget": map[string]any{"max_workers": float64(1)},
		"tasks": []any{
			map[string]any{"id": "a", "prompt": "edit the file", "tools": []any{"write_file"}},
		},
	}, tools.RunOptions{Model: "m"})

	if result.Status == tools.StatusError {
		t.Fatalf("plan refused: %s", result.Output)
	}
	if !strings.Contains(result.Output, "worktree fix at /tmp/wt/fix") {
		t.Fatalf("the tool never told the user where the work landed:\n%s", result.Output)
	}
}

// A READ-ONLY PLAN MUST NOT CLAIM ONE. It runs in the parent's own tree, and a
// line about isolation there would be false.
func TestAReadOnlyPlanSaysNothingAboutAnIsolatedWorkspace(t *testing.T) {
	tool := &OrchestrateTool{
		PostureActive: func() bool { return true },
		ParentTools:   []string{"read_file"},
		RunTask: func(context.Context, PlanTaskRequest) (TaskResult, error) {
			return TaskResult{Outcome: TaskSucceeded, Output: "read it"}, nil
		},
	}
	result := tool.RunWithOptions(context.Background(), map[string]any{
		"name":   "look",
		"budget": map[string]any{"max_workers": float64(1)},
		"tasks":  []any{map[string]any{"id": "a", "prompt": "read the file"}},
	}, tools.RunOptions{Model: "m"})

	if strings.Contains(result.Output, "isolated workspace") {
		t.Errorf("a read-only plan claimed an isolated workspace:\n%s", result.Output)
	}
}

// THE PLAN METER COUNTS THE EVENT THAT TRIPS THE PER-TASK CAP, and every one
// after it. The add used to be a side effect inside the second switch case, so
// once the per-task case matched the plan meter stopped hearing about the task:
// the same child read 40,000 on the meter with a cap set and 80,000 without,
// and downstream dispatch was gated on the undercount.
func TestThePlanMeterCountsTheEventThatTripsThePerTaskCap(t *testing.T) {
	exec := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(ctx context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			// Two events, then one more already in the pipe after the cancel:
			// all three were billed by the provider.
			for round := 0; round < 3; round++ {
				progress(usageEvents(40_000)[0])
			}
			if ctx.Err() != nil {
				return ChildRunResult{Started: true, ExitCode: -1}, ctx.Err()
			}
			return ChildRunResult{Started: true}, nil
		},
	}
	run := NewPlanRunner(PlanTaskContext{Executor: exec, Cwd: t.TempDir(), SpecialistName: "explorer"})
	spend := &planSpend{limit: 10_000_000, totalTasks: 1}
	result, _ := run(context.Background(), PlanTaskRequest{
		Task:          Task{ID: "greedy", Prompt: "p"},
		Tools:         []string{"read_file"},
		Spend:         spend,
		MaxTaskTokens: 60_000,
	})
	if result.Outcome != TaskCancelled {
		t.Fatalf("the per-task cap did not stop the task: %s / %s", result.Outcome, result.Err)
	}
	if got := spend.upstream.Load(); got != 120_000 {
		t.Fatalf("plan meter = %d, want all 120000 tokens the child was billed for", got)
	}
}

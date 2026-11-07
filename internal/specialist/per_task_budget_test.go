package specialist

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

// A PER-TASK CAP THE TOTAL CANNOT REACH IS REFUSED, not silently ignored.
//
// A run asked for 1,000,000 per task with a 500,000 total across seven tasks.
// Every task was bounded by its share of the total long before its own cap
// mattered: four finders were stopped between 6,688 and 219,698 tokens, nowhere
// near the million they had been given, and the author reasonably concluded the
// per-task limit was broken. It was not; it was unreachable.
func TestAPerTaskCapTheTotalCannotReachIsRefused(t *testing.T) {
	// The cap is BELOW the total, so the existing "cap above the budget" check
	// passes it — and it is still unreachable, because three tasks at a million
	// each need three million.
	budget := okBudget()
	budget["max_tokens"] = float64(2_000_000)
	budget["max_tokens_per_task"] = float64(1_000_000)
	_, err := ParsePlan(planArgs([]any{
		task("a", "x"), task("b", "y"), task("c", "z"),
	}, budget), readOnlyLimits())

	if err == nil {
		t.Fatal("a per-task cap the plan total can never permit was admitted")
	}
	// The refusal must carry the arithmetic AND both ways out — the author is the
	// only one who can say which number they meant.
	for _, want := range []string{"2000000", "1000000", "3 tasks", "3000000", "OMIT it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// A REACHABLE CAP IS FINE. The total covers every task at its cap, so the cap is
// what bounds a task and the total is a backstop.
func TestAPerTaskCapTheTotalCanCoverIsAccepted(t *testing.T) {
	budget := okBudget()
	budget["max_tokens"] = float64(3_000_000)
	budget["max_tokens_per_task"] = float64(1_000_000)
	if _, err := ParsePlan(planArgs([]any{
		task("a", "x"), task("b", "y"), task("c", "z"),
	}, budget), readOnlyLimits()); err != nil {
		t.Fatalf("a coherent pair was refused: %v", err)
	}
}

// THE INTENDED SHAPE: a per-task cap ALONE, which is what budgeting per
// sub-agent actually means. No total, so no pool to divide and no share to be
// stopped by.
func TestAPerTaskCapAloneIsTheWayToBudgetPerSubAgent(t *testing.T) {
	budget := map[string]any{"max_workers": float64(4), "max_tokens_per_task": float64(1_000_000)}
	plan, err := ParsePlan(planArgs([]any{
		task("f1", "find"), task("f2", "find"),
		task("verify", "check", "f1", "f2"),
	}, budget), readOnlyLimits())
	if err != nil {
		t.Fatalf("a per-task cap with no total was refused: %v", err)
	}
	if got := plan.Budget().MaxTokensPerTask; got != 1_000_000 {
		t.Errorf("per-task cap = %d, want 1000000", got)
	}
	if got := plan.Budget().MaxTokens; got != 0 {
		t.Errorf("a total appeared from nowhere: %d", got)
	}
	// With no total there is no pool, so nothing divides the cap between tasks.
	spend := &planSpend{limit: int64(plan.Budget().MaxTokens), downstreamTasks: 1, totalTasks: 3}
	if got := spend.ceilingFor(false); got != 0 {
		t.Errorf("an unbounded plan produced an upstream ceiling of %d", got)
	}
}

// THE SCHEMA MUST SAY SO, since the pair is only incoherent if you know how the
// total is divided — which nothing in the tool call reveals.
func TestTheSchemaSaysToUseThePerTaskCapAlone(t *testing.T) {
	budget := (&OrchestrateTool{}).Parameters().Properties["budget"]
	perTask, ok := budget.Properties["max_tokens_per_task"]
	if !ok {
		t.Fatal("max_tokens_per_task is undeclared")
	}
	for _, want := range []string{"per sub-agent", "ALONE"} {
		if !strings.Contains(perTask.Description, want) {
			t.Errorf("the per-task field does not say %q: %s", want, perTask.Description)
		}
	}
	total := budget.Properties["max_tokens"]
	if !strings.Contains(total.Description, "WHOLE plan") {
		t.Errorf("max_tokens does not say it covers the whole plan: %s", total.Description)
	}
}

// THE MODEL IS TOLD NOT TO GUESS, at the point where it decides.
//
// The same seven-task audit was submitted five times with max_tokens 500,000
// against a need of roughly 4,450,000. Nobody asked for a spending limit; the
// orchestrating model volunteered one each time, because the schema explained at
// length how to size a number it has no way to estimate. Each run spent its
// budget and returned nothing.
func TestTheSchemaTellsThePlannerNotToGuessAtASpendingLimit(t *testing.T) {
	total := (&OrchestrateTool{}).Parameters().Properties["budget"].Properties["max_tokens"]
	for _, want := range []string{
		"DO NOT SET THIS unless the user asked",
		"cannot estimate",
		"stops tasks mid-work",
		"wall-clock backstop",
	} {
		if !strings.Contains(total.Description, want) {
			t.Errorf("max_tokens does not say %q: %s", want, total.Description)
		}
	}
}

// AND THE WARNING ARRIVES BEFORE THE RUN, not with its remains.
//
// warnBudgetLooksLow computed the right number on every one of those five runs
// and rode the plan's OUTPUT, which the author reads once the plan is already
// dead. It now goes out through the preflight channel first.
func TestAnUndersizedBudgetIsReportedBeforeTheFirstTaskRuns(t *testing.T) {
	var statuses []string
	recorder := &preflightSpy{onStatus: func(s string) { statuses = append(statuses, s) }}
	tool := &OrchestrateTool{
		PostureActive: func() bool { return true },
		ParentTools:   []string{"read_file"},
		Recorder:      recorder,
		RunTask: func(context.Context, PlanTaskRequest) (TaskResult, error) {
			// By the time any task runs, the warning must already have been sent.
			if len(statuses) == 0 {
				t.Error("the first task started before the budget warning was reported")
			}
			return TaskResult{Outcome: TaskSucceeded, Output: "ok"}, nil
		},
	}
	tool.RunWithOptions(context.Background(), map[string]any{
		"name":   "audit",
		"budget": map[string]any{"max_workers": float64(1), "max_tokens": float64(500_000)},
		"tasks": []any{
			map[string]any{"id": "f1", "prompt": "trace the repo"},
			map[string]any{"id": "f2", "prompt": "trace the repo"},
			map[string]any{"id": "f3", "prompt": "trace the repo"},
			map[string]any{"id": "f4", "prompt": "trace the repo"},
		},
	}, tools.RunOptions{Model: "m"})

	joined := strings.Join(statuses, " | ")
	if !strings.Contains(joined, "budget may be too low") {
		t.Fatalf("no budget warning reached the surface before the run: %q", joined)
	}
}

// preflightSpy is a recorder that only listens for preflight status.
type preflightSpy struct{ onStatus func(string) }

func (s *preflightSpy) TaskDispatched(Task)      {}
func (s *preflightSpy) TaskCompleted(TaskResult) {}
func (s *preflightSpy) TaskFailed(TaskResult)    {}
func (s *preflightSpy) PlanPreflight(status string) {
	if strings.TrimSpace(status) != "" && s.onStatus != nil {
		s.onStatus(status)
	}
}

// THE PER-TASK CAP BOUNDS THE TASK, NOT ONE ATTEMPT. Real runner, real retry
// loop: a child that spends 50,000 of a 60,000 cap and then goes silent is
// retried, and the retry used to start with the whole cap again — four
// children and 200,000 tokens at max_retries 3. The retry now starts its meter
// at 50,000, so its first usage event trips the cap and nothing further runs.
func TestThePerTaskCapBoundsATaskAcrossItsRetries(t *testing.T) {
	var mu sync.Mutex
	children := 0
	exec := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(ctx context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			mu.Lock()
			children++
			mu.Unlock()
			events := usageEvents(50_000)
			progress(events[0])
			<-ctx.Done() // then silence, until the watchdog or the cap stops it
			// The real child runner returns the events it collected even when
			// cancelled; the executor bills the attempt from them.
			return ChildRunResult{Started: true, ExitCode: -1, Events: events}, ctx.Err()
		},
	}
	inner := NewPlanRunner(PlanTaskContext{Executor: exec, Cwd: t.TempDir(), SpecialistName: "explorer"})
	// The admitted floor for max_stall_seconds is 30s; shrink only the clock so
	// the real watchdog fires within the test.
	run := func(ctx context.Context, req PlanTaskRequest) (TaskResult, error) {
		req.StallTimeout, req.StallPoll = 50*time.Millisecond, 5*time.Millisecond
		return inner(ctx, req)
	}
	plan := mustPlan(t, []any{task("a", "x")}, map[string]any{
		"max_workers": float64(1), "max_tokens_per_task": float64(60_000), "max_retries": float64(3),
	}, readOnlyLimits())

	report := ExecutePlan(context.Background(), plan, []string{"read_file"}, run, nil)

	if children != 2 {
		t.Fatalf("%d children ran; the retry must inherit the 50000 already spent and be stopped by the cap", children)
	}
	got := report.Tasks[0]
	if got.Outcome != TaskCancelled || !strings.Contains(got.Err, "max_tokens_per_task") {
		t.Fatalf("the retry was not stopped by the per-task cap: %s / %q", got.Outcome, got.Err)
	}
	if !strings.Contains(got.Err, "100000 tokens") {
		t.Errorf("the stop does not report the task's spend across attempts: %q", got.Err)
	}
}

// A TASK WITH NOTHING LEFT IS NOT RETRIED AT ALL. A stall after the whole cap
// is spent must not launch another child to overspend it.
func TestATaskThatSpentItsWholeCapIsNotRetried(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x")}, map[string]any{
		"max_workers": float64(1), "max_tokens_per_task": float64(60_000), "max_retries": float64(3),
	}, readOnlyLimits())
	attempts := 0
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			attempts++
			if req.TaskTokensSpent != 0 {
				t.Errorf("attempt %d was told %d tokens were already spent", attempts, req.TaskTokensSpent)
			}
			return TaskResult{ID: "a", Outcome: TaskFailed, Stalled: true, Err: "no output", Tokens: 60_000}, nil
		}, nil)
	if attempts != 1 {
		t.Fatalf("%d attempts; a task that spent its whole cap must not be retried", attempts)
	}
	if got := report.Tasks[0].Err; !strings.Contains(got, "not retried") || !strings.Contains(got, "max_tokens_per_task") {
		t.Errorf("the failure does not say why it was not retried: %q", got)
	}
}

package specialist

import (
	"context"
	"strings"
	"testing"
	"time"
)

// max_wall_seconds bounds the PLAN, and a bound that only holds when the plan
// happens to run sequentially is not a bound.
//
// The pre-dispatch deadline check is consulted only when the walk needs a free
// worker slot. A plan whose ready set fits the pool dispatches in one wave and
// is never checked again, so it ran to completion however long its children
// took and reported "completed" with nothing skipped. Measured before the fix:
// four 2s tasks at max_workers=4 under a 1s wall finished in 2.0s and reported
// 4 succeeded; the identical plan at max_workers=1 correctly reported partial.
// Asking for parallelism deleted the bound.
func TestWallBudgetBoundsAConcurrentPlan(t *testing.T) {
	plan := wallBudgetPlan(t, 4, 1)

	slow := PlanRunner(func(ctx context.Context, req PlanTaskRequest) (TaskResult, error) {
		select {
		case <-time.After(4 * time.Second):
			return TaskResult{ID: req.Task.ID, Outcome: TaskSucceeded}, nil
		case <-ctx.Done():
			// The point: a wall-expired plan must reach its in-flight children.
			return TaskResult{ID: req.Task.ID, Outcome: TaskFailed}, ctx.Err()
		}
	})

	started := time.Now()
	report := ExecutePlan(context.Background(), plan, PlanReadOnlyToolNames(), slow, nil)
	elapsed := time.Since(started)

	if elapsed > 3*time.Second {
		t.Errorf("plan ran %s under a 1s wall budget; the deadline never reached the children", elapsed.Round(time.Millisecond))
	}
	if report.Succeeded == len(plan.Order()) {
		t.Errorf("every task succeeded under a wall budget that should have stopped them: %s", report.Summary())
	}
}

// And it must say WHY. A wall-budget stop is the plan spending what it was
// allowed; a cancel is a person deciding. Both used to read "the run was
// stopped", which sent the reader looking for who stopped it.
func TestWallBudgetStopSaysItWasTheBudget(t *testing.T) {
	plan := wallBudgetPlan(t, 2, 1)
	// Bounded, never a bare <-ctx.Done(). If the deadline regresses, this test
	// must FAIL rather than hang: an unbounded wait here wedged a full test run
	// for ten minutes when the fix was mutated out.
	slow := PlanRunner(func(ctx context.Context, req PlanTaskRequest) (TaskResult, error) {
		select {
		case <-ctx.Done():
			return TaskResult{ID: req.Task.ID, Outcome: TaskFailed}, ctx.Err()
		case <-time.After(5 * time.Second):
			return TaskResult{ID: req.Task.ID, Outcome: TaskSucceeded}, nil
		}
	})

	report := ExecutePlan(context.Background(), plan, PlanReadOnlyToolNames(), slow, nil)

	var sawReason bool
	for _, task := range report.Tasks {
		if strings.Contains(task.Err, "max_wall_seconds") {
			sawReason = true
		}
		if strings.Contains(task.Err, "the run was stopped") {
			t.Errorf("task %q blames a user stop for a budget expiry: %q", task.ID, task.Err)
		}
	}
	if !sawReason {
		t.Errorf("no task explained that the wall budget elapsed: %s", report.Summary())
	}
}

// A user stop must keep saying it was a stop — the fix must not relabel it.
func TestUserStopStillReadsAsAStop(t *testing.T) {
	plan := wallBudgetPlan(t, 2, 0) // no wall budget at all
	ctx, cancel := context.WithCancel(context.Background())
	slow := PlanRunner(func(ctx context.Context, req PlanTaskRequest) (TaskResult, error) {
		cancel()
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
		return TaskResult{ID: req.Task.ID, Outcome: TaskFailed}, ctx.Err()
	})

	report := ExecutePlan(ctx, plan, PlanReadOnlyToolNames(), slow, nil)
	for _, task := range report.Tasks {
		if strings.Contains(task.Err, "max_wall_seconds") {
			t.Errorf("task %q blames the wall budget for a user stop: %q", task.ID, task.Err)
		}
	}
}

func wallBudgetPlan(t *testing.T, workers int, wallSeconds int) Plan {
	t.Helper()
	budget := map[string]any{"max_workers": float64(workers)}
	if wallSeconds > 0 {
		budget["max_wall_seconds"] = float64(wallSeconds)
	}
	tasks := []any{}
	for _, id := range []string{"a", "b", "c", "d"} {
		tasks = append(tasks, map[string]any{"id": id, "prompt": "work"})
	}
	plan, err := ParsePlan(map[string]any{
		"name": "wall", "tasks": tasks, "budget": budget,
	}, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	return plan
}

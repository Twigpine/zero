package specialist

import (
	"context"
	"slices"
	"testing"
)

// A plan task receives the parent's granted read roots, so a plan can audit a
// path the parent was granted (request_permissions) instead of failing "outside
// the workspace" in every task. Driven end to end through ExecutePlan, reading
// what the runner actually receives — a hook that nothing consults is the defect
// this branch has produced before.
func TestExecutePlanGivesTasksTheParentReadRoots(t *testing.T) {
	plan := mustParsePlan(t, map[string]any{
		"name":   "p",
		"tasks":  []any{map[string]any{"id": "a", "prompt": "look"}},
		"budget": map[string]any{"max_workers": float64(1)},
	}, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})

	type roots struct{ readOnly, read []string }
	capture := func(into *roots) PlanRunner {
		return func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			into.readOnly, into.read = req.ReadOnlyRoots, req.ReadRoots
			return TaskResult{ID: req.Task.ID, Outcome: TaskSucceeded}, nil
		}
	}

	var granted roots
	if report := ExecutePlan(context.Background(), plan, PlanReadOnlyToolNames(),
		capture(&granted), nil, WithReadRoots([]string{"/granted/path"})); report.Failed != 0 {
		t.Fatalf("plan failed: %+v", report)
	}
	// The grant reaches the task READ-ONLY, never the write channel — routing a
	// read grant through ReadRoots (--add-dir) would escalate it to write.
	if !slices.Contains(granted.readOnly, "/granted/path") {
		t.Fatalf("ReadOnlyRoots %v did not include the parent grant — a granted audit path is unreachable", granted.readOnly)
	}
	if slices.Contains(granted.read, "/granted/path") {
		t.Fatalf("the parent READ grant leaked into ReadRoots %v, which is emitted as --add-dir (write) — a read grant became writable", granted.read)
	}

	// Without the option, a dependency-free task gets nothing beyond its
	// workspace — the wiring is the only source of the extra root.
	var bare roots
	ExecutePlan(context.Background(), plan, PlanReadOnlyToolNames(), capture(&bare), nil)
	if len(bare.readOnly) != 0 {
		t.Fatalf("a task with no grant and no dependencies received read-only roots %v", bare.readOnly)
	}
}

// The tool builds the read-roots option from its ExtraReadRoots hook at dispatch,
// and only when the hook returns something — an unwired or empty hook adds
// nothing, so every existing plan is byte-identical.
func TestExecOptionsCarryTheParentReadRoots(t *testing.T) {
	plan := mustParsePlan(t, map[string]any{
		"name":   "p",
		"tasks":  []any{map[string]any{"id": "a", "prompt": "look"}},
		"budget": map[string]any{"max_workers": float64(1)},
	}, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})

	applyOptions := func(tool *OrchestrateTool) execOptions {
		var applied execOptions
		for _, opt := range tool.execOptionsFor(plan, 0) {
			opt(&applied)
		}
		return applied
	}

	wired := applyOptions(&OrchestrateTool{ExtraReadRoots: func() []string { return []string{"/granted/path"} }})
	if !slices.Contains(wired.parentReadRoots, "/granted/path") {
		t.Fatalf("a wired ExtraReadRoots hook never reached the executor: %v", wired.parentReadRoots)
	}
	if got := applyOptions(&OrchestrateTool{}); len(got.parentReadRoots) != 0 {
		t.Fatalf("an unwired tool produced read roots %v", got.parentReadRoots)
	}
	if got := applyOptions(&OrchestrateTool{ExtraReadRoots: func() []string { return nil }}); len(got.parentReadRoots) != 0 {
		t.Fatalf("an empty hook still added read roots %v", got.parentReadRoots)
	}
}

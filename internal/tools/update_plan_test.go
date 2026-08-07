package tools

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// TestUpdatePlanRefusesCancelledRun pins the guard against a cancelled run's
// late update_plan call repopulating the shared plan after the UI has reset
// it for a new session: the agent loop only checks cancellation between
// calls, so the tool itself must refuse the write once its context is dead.
func TestUpdatePlanRefusesCancelledRun(t *testing.T) {
	tool := NewUpdatePlanTool()
	ctx, cancel := context.WithCancel(context.Background())
	result := tool.Run(ctx, map[string]any{"plan": []any{map[string]any{"content": "live", "status": "pending"}}})
	if result.Status != StatusOK {
		t.Fatalf("live run: %+v", result)
	}
	raw, ok := result.Meta[PlanSnapshotMeta]
	if !ok {
		t.Fatalf("expected %s on successful run, got %#v", PlanSnapshotMeta, result.Meta)
	}
	var snap []PlanItem
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if len(snap) != 1 || snap[0].Content != "live" {
		t.Fatalf("snapshot did not match installed plan: %+v", snap)
	}

	tool.SetPlan(nil) // the UI reset for a new session
	cancel()
	result = tool.Run(ctx, map[string]any{"plan": []any{map[string]any{"content": "stale"}}})
	if result.Status != StatusError {
		t.Fatalf("cancelled run must be refused, got %+v", result)
	}
	if _, ok := result.Meta[PlanSnapshotMeta]; ok {
		t.Fatalf("cancelled run must not attach plan_snapshot, got %#v", result.Meta)
	}
	if items := tool.CurrentPlan(); len(items) != 0 {
		t.Fatalf("cancelled run repopulated the shared plan: %+v", items)
	}
}

// TestUpdatePlanSetPlanDoesNotMutateCallerSlice pins that enforceSingleInProgress
// demotions cannot rewrite the caller's storage through SetPlan.
func TestUpdatePlanSetPlanDoesNotMutateCallerSlice(t *testing.T) {
	tool := NewUpdatePlanTool()
	caller := []PlanItem{
		{Content: "a", Status: "in_progress"},
		{Content: "b", Status: "in_progress"},
	}
	tool.SetPlan(caller)
	if caller[0].Status != "in_progress" || caller[1].Status != "in_progress" {
		t.Fatalf("SetPlan mutated caller slice: %+v", caller)
	}
	got := tool.CurrentPlan()
	if len(got) != 2 || got[0].Status != "completed" || got[1].Status != "in_progress" {
		t.Fatalf("SetPlan did not enforce single in_progress on stored plan: %+v", got)
	}
}

// TestUpdatePlanConcurrentCancelAndReset races a late Run against SetPlan(nil)
// the way a cancelled agent goroutine can race a UI session switch. Under
// -race, either empty or the successful write is fine; a torn mix is not.
func TestUpdatePlanConcurrentCancelAndReset(t *testing.T) {
	tool := NewUpdatePlanTool()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = tool.Run(ctx, map[string]any{"plan": []any{map[string]any{"content": "stale", "status": "pending"}}})
	}()
	go func() {
		defer wg.Done()
		tool.SetPlan(nil)
	}()
	wg.Wait()

	// After a cancelled Run and a clear, the plan must not hold the stale
	// cancelled payload. Empty is the expected stable outcome; a concurrent
	// non-cancelled write is out of scope for this test.
	if items := tool.CurrentPlan(); len(items) != 0 {
		// Cancelled Run may have lost the race before cancel was visible only
		// if ctx was live; here ctx is already cancelled, so refuse must win
		// or SetPlan(nil) cleared after. Non-empty means the cancelled path
		// wrote, which the mutex ordering forbids.
		t.Fatalf("concurrent cancel/reset left unexpected plan: %+v", items)
	}
}

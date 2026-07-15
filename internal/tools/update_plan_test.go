package tools

import (
	"context"
	"testing"
)

// TestUpdatePlanRefusesCancelledRun pins the guard against a cancelled run's
// late update_plan call repopulating the shared plan after the UI has reset
// it for a new session: the agent loop only checks cancellation between
// calls, so the tool itself must refuse the write once its context is dead.
func TestUpdatePlanRefusesCancelledRun(t *testing.T) {
	tool := NewUpdatePlanTool()
	ctx, cancel := context.WithCancel(context.Background())
	if result := tool.Run(ctx, map[string]any{"plan": []any{map[string]any{"content": "live"}}}); result.Status != StatusOK {
		t.Fatalf("live run: %+v", result)
	}

	tool.SetPlan(nil) // the UI reset for a new session
	cancel()
	result := tool.Run(ctx, map[string]any{"plan": []any{map[string]any{"content": "stale"}}})
	if result.Status != StatusError {
		t.Fatalf("cancelled run must be refused, got %+v", result)
	}
	if items := tool.CurrentPlan(); len(items) != 0 {
		t.Fatalf("cancelled run repopulated the shared plan: %+v", items)
	}
}

package tools

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestUpdatePlanToolStoresAndFormatsPlan(t *testing.T) {
	tool := NewUpdatePlanTool()
	if description := strings.ToLower(tool.Description()); !strings.Contains(description, "skip it for bounded changes in one component") {
		t.Fatalf("update_plan description must preserve the bounded-task fast path: %q", description)
	}

	result := tool.Run(context.Background(), map[string]any{
		"plan": []any{
			map[string]any{"id": "1", "content": "First step", "status": "completed"},
			map[string]any{"id": "2", "content": "Second step", "status": "in_progress", "notes": "halfway"},
			map[string]any{"id": "3", "content": "Third step", "status": "pending"},
		},
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	for _, want := range []string{
		"Current Plan:",
		"1. [completed] First step",
		"2. [in_progress] Second step",
		"Notes: halfway",
		"3. [pending] Third step",
	} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("expected output to contain %q, got %q", want, result.Output)
		}
	}

	plan := tool.CurrentPlan()
	if len(plan) != 3 {
		t.Fatalf("expected 3 plan items, got %d", len(plan))
	}
	plan[0].Content = "mutated"
	if tool.CurrentPlan()[0].Content != "First step" {
		t.Fatalf("CurrentPlan returned mutable internal state")
	}
}

func TestUpdatePlanToolCoercesStatuses(t *testing.T) {
	// A weaker model may emit non-canonical statuses ("nope", "done", "in-progress").
	// These must be coerced (unknown -> pending, synonyms -> canonical) rather than
	// failing the whole update_plan call — a rejected call leaves the stored plan
	// unchanged, which freezes the plan panel on its previous state.
	tool := NewUpdatePlanTool()
	result := tool.Run(context.Background(), map[string]any{
		"plan": []any{
			map[string]any{"id": "1", "content": "Bad step", "status": "nope"},
			map[string]any{"id": "2", "content": "Done step", "status": "done"},
			map[string]any{"id": "3", "content": "Active step", "status": "in-progress"},
		},
	})

	if result.Status == StatusError {
		t.Fatalf("unknown/synonym statuses must not error, got: %q", result.Output)
	}
	plan := tool.CurrentPlan()
	if len(plan) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(plan))
	}
	if plan[0].Status != "pending" {
		t.Errorf("unknown status should coerce to pending, got %q", plan[0].Status)
	}
	if plan[1].Status != "completed" {
		t.Errorf("'done' should coerce to completed, got %q", plan[1].Status)
	}
	if plan[2].Status != "in_progress" {
		t.Errorf("'in-progress' should coerce to in_progress, got %q", plan[2].Status)
	}
}

func TestEnforceSingleInProgress(t *testing.T) {
	// More than one in_progress is downgraded so only the LAST stays.
	plan := []PlanItem{
		{Content: "a", Status: "in_progress"},
		{Content: "b", Status: "completed"},
		{Content: "c", Status: "in_progress"},
		{Content: "d", Status: "pending"},
	}
	out := enforceSingleInProgress(plan)
	if out[0].Status != "completed" {
		t.Errorf("earlier in_progress should downgrade to completed, got %q", out[0].Status)
	}
	if out[2].Status != "in_progress" {
		t.Errorf("last in_progress should remain, got %q", out[2].Status)
	}
	count := 0
	for _, it := range out {
		if it.Status == "in_progress" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("want exactly 1 in_progress, got %d", count)
	}

	single := []PlanItem{{Status: "completed"}, {Status: "in_progress"}, {Status: "pending"}}
	if got := enforceSingleInProgress(single); got[1].Status != "in_progress" {
		t.Fatalf("single in_progress must be preserved, got %q", got[1].Status)
	}
}

func TestUpdatePlanRunEnforcesSingleInProgress(t *testing.T) {
	tool := NewUpdatePlanTool()
	tool.Run(context.Background(), map[string]any{
		"plan": []any{
			map[string]any{"content": "a", "status": "in_progress"},
			map[string]any{"content": "b", "status": "in_progress"},
		},
	})
	plan := tool.CurrentPlan()
	if plan[0].Status != "completed" || plan[1].Status != "in_progress" {
		t.Fatalf("Run should enforce one in_progress, got %q,%q", plan[0].Status, plan[1].Status)
	}
}

func TestUpdatePlanToolClearPlanResetsState(t *testing.T) {
	tool := NewUpdatePlanTool()

	result := tool.Run(context.Background(), map[string]any{
		"plan": []any{
			map[string]any{"id": "1", "content": "First", "status": "pending"},
			map[string]any{"id": "2", "content": "Second", "status": "in_progress"},
		},
	})

	if result.Status != StatusOK {
		t.Fatalf("expected ok status, got %s: %s", result.Status, result.Output)
	}
	if got := tool.CurrentPlan(); len(got) == 0 {
		t.Fatalf("expected stored plan before ClearPlan")
	}

	tool.ClearPlan()
	if got := tool.CurrentPlan(); len(got) != 0 {
		t.Fatalf("expected empty plan after ClearPlan, got %d items", len(got))
	}
	if got := formatPlan(tool.CurrentPlan()); got != "Plan is currently empty." {
		t.Fatalf("expected empty plan formatting after ClearPlan, got %q", got)
	}
}

func TestUpdatePlanToolAcceptsItemsWithoutID(t *testing.T) {
	tool := NewUpdatePlanTool()
	result := tool.Run(context.Background(), map[string]any{
		"plan": []any{
			map[string]any{"content": "First step", "status": "in_progress"},
			map[string]any{"content": "Second step", "status": "pending"},
		},
	})
	if result.Status != StatusOK {
		t.Fatalf("expected ok status when id omitted, got %s: %s", result.Status, result.Output)
	}
	plan := tool.CurrentPlan()
	if len(plan) != 2 {
		t.Fatalf("expected 2 plan items, got %d", len(plan))
	}
	if plan[0].ID != "1" || plan[1].ID != "2" {
		t.Fatalf("expected ids auto-derived from index, got %q,%q", plan[0].ID, plan[1].ID)
	}
}

func TestUpdatePlanToolDefaultsStatusToPending(t *testing.T) {
	tool := NewUpdatePlanTool()
	result := tool.Run(context.Background(), map[string]any{
		"plan": []any{
			map[string]any{"content": "Only content"},
		},
	})
	if result.Status != StatusOK {
		t.Fatalf("expected ok status when status omitted, got %s: %s", result.Status, result.Output)
	}
	if got := tool.CurrentPlan(); got[0].Status != "pending" {
		t.Fatalf("expected status to default to pending, got %q", got[0].Status)
	}
}

func TestUpdatePlanToolRequiresContent(t *testing.T) {
	result := NewUpdatePlanTool().Run(context.Background(), map[string]any{
		"plan": []any{map[string]any{"status": "pending"}},
	})
	if result.Status != StatusError {
		t.Fatalf("expected error when content missing, got %s", result.Status)
	}
	if !strings.Contains(result.Output, "content is required") {
		t.Fatalf("unexpected output: %q", result.Output)
	}
}

func TestUpdatePlanToolConcurrentRunAndRead(t *testing.T) {
	tool := NewUpdatePlanTool()
	args := map[string]any{
		"plan": []any{
			map[string]any{"content": "First step", "status": "in_progress"},
			map[string]any{"content": "Second step", "status": "pending"},
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); tool.Run(context.Background(), args) }()
		go func() { defer wg.Done(); _ = tool.CurrentPlan() }()
		go func() { defer wg.Done(); tool.ClearPlan() }()
	}
	wg.Wait()
}

func TestUpdatePlanToolAdvertisesItemSchema(t *testing.T) {
	plan := NewUpdatePlanTool().Parameters().Properties["plan"]
	if plan.Type != "array" {
		t.Fatalf("expected plan to be an array, got %q", plan.Type)
	}
	if plan.Items == nil {
		t.Fatal("plan should have a structured Items schema")
	}
	if plan.Items.Type != "object" {
		t.Fatalf("plan items should be objects, got %q", plan.Items.Type)
	}
	contentProp, ok := plan.Items.Properties["content"]
	if !ok {
		t.Fatal("plan items should have a 'content' property")
	}
	if contentProp.Type != "string" {
		t.Fatalf("content property should be string, got %q", contentProp.Type)
	}
	statusProp, ok := plan.Items.Properties["status"]
	if !ok {
		t.Fatal("plan items should have a 'status' property")
	}
	if len(statusProp.Enum) == 0 {
		t.Fatal("status property should have an enum")
	}
	if len(plan.Items.Required) == 0 || plan.Items.Required[0] != "content" {
		t.Fatalf("plan items should require 'content', got %v", plan.Items.Required)
	}
}

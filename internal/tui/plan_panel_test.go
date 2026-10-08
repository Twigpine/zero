package tui

import (
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/tools"
)

func TestPlanPanelUpdateFromItems(t *testing.T) {
	var s planPanelState
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)

	s.updateFromItems([]tools.PlanItem{
		{Content: "Read file", Status: "in_progress"},
		{Content: "Edit file", Status: "pending"},
	}, now)

	if len(s.steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(s.steps))
	}
	if s.steps[0].status != "in_progress" {
		t.Errorf("step 0 status = %q, want in_progress", s.steps[0].status)
	}
	if s.steps[0].startedAt.IsZero() {
		t.Error("in_progress step should have startedAt set")
	}
	if !s.startedAt.IsZero() == false {
		t.Error("panel startedAt should be set on first update")
	}
	if s.isComplete() {
		t.Error("plan should not be complete with a pending step")
	}
}

func TestPlanPanelPreservesTimestamps(t *testing.T) {
	var s planPanelState
	t0 := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)

	s.updateFromItems([]tools.PlanItem{
		{Content: "Step A", Status: "in_progress"},
	}, t0)

	t1 := t0.Add(10 * time.Second)
	s.updateFromItems([]tools.PlanItem{
		{Content: "Step A", Status: "completed"},
	}, t1)

	if s.steps[0].startedAt != t0 {
		t.Errorf("startedAt not preserved: got %v, want %v", s.steps[0].startedAt, t0)
	}
	if s.steps[0].completedAt != t1 {
		t.Errorf("completedAt not set: got %v, want %v", s.steps[0].completedAt, t1)
	}
}

func TestPlanPanelIsComplete(t *testing.T) {
	var s planPanelState
	now := time.Now()

	s.updateFromItems([]tools.PlanItem{
		{Content: "A", Status: "completed"},
		{Content: "B", Status: "failed"},
	}, now)

	if !s.isComplete() {
		t.Error("plan with all completed/failed should be complete")
	}
	if s.completedAt.IsZero() {
		t.Error("completedAt should be set when plan is complete")
	}
}

func TestPlanPanelClear(t *testing.T) {
	var s planPanelState
	s.updateFromItems([]tools.PlanItem{{Content: "A", Status: "pending"}}, time.Now())
	s.clear()

	if !s.isEmpty() {
		t.Error("plan should be empty after clear")
	}
	if len(s.steps) != 0 {
		t.Errorf("expected 0 steps after clear, got %d", len(s.steps))
	}
}

func TestUpdateFromItemsDuplicateContentKeepsDistinctTimestamps(t *testing.T) {
	t1 := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	var s planPanelState
	// Two prior steps with IDENTICAL content but distinct start times.
	s.steps = []planStep{
		{content: "step", status: "completed", startedAt: t1, completedAt: t1.Add(time.Second)},
		{content: "step", status: "in_progress", startedAt: t2},
	}
	s.updateFromItems([]tools.PlanItem{
		{Content: "step", Status: "completed"},
		{Content: "step", Status: "in_progress"},
	}, t2.Add(time.Hour))
	if len(s.steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(s.steps))
	}
	// Each step must inherit a DISTINCT prior entry positionally, not both collapse
	// onto the first content match (L22).
	if s.steps[0].startedAt != t1 {
		t.Errorf("step[0] startedAt = %v, want %v", s.steps[0].startedAt, t1)
	}
	if s.steps[1].startedAt != t2 {
		t.Errorf("step[1] startedAt = %v, want %v (duplicate-content tie-break)", s.steps[1].startedAt, t2)
	}
}

// TestPlanNowFreezeAndResume covers the idle-freeze fix: while the agent is idle
// (activeRunID == 0) and a freeze time is stamped, the plan clock is frozen so an
// in_progress step left mid-plan stops ticking; during a run it tracks live time.
func TestPlanNowFreezeAndResume(t *testing.T) {
	frozen := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	live := frozen.Add(2 * time.Minute)
	m := model{now: func() time.Time { return live }, plan: planPanelState{frozenAt: frozen}}

	m.activeRunID = 0 // idle -> frozen clock
	if got := m.planNow(); !got.Equal(frozen) {
		t.Fatalf("idle planNow = %v, want frozen %v", got, frozen)
	}
	m.activeRunID = 3 // running -> live clock
	if got := m.planNow(); !got.Equal(live) {
		t.Fatalf("running planNow = %v, want live %v", got, live)
	}
	m.activeRunID = 0
	m.plan.frozenAt = time.Time{} // idle but never stamped -> live fallback
	if got := m.planNow(); !got.Equal(live) {
		t.Fatalf("unstamped idle planNow = %v, want live fallback %v", got, live)
	}
}

func TestPlanPanelClearResetsFrozenAt(t *testing.T) {
	s := planPanelState{frozenAt: time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)}
	s.clear()
	if !s.frozenAt.IsZero() {
		t.Fatal("clear() must reset frozenAt so the next run starts with a live clock")
	}
}

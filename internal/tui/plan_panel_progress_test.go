package tui

import (
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/tools"
)

// TestCurrentStepContent: the header/summary name the step actually being
// worked (in_progress, else first incomplete, else first), not always step 1.
func TestCurrentStepContent(t *testing.T) {
	steps := []planStep{
		{content: "one", status: "completed"},
		{content: "two", status: "in_progress"},
		{content: "three", status: "pending"},
	}
	if got := currentStepContent(steps); got != "two" {
		t.Errorf("in_progress: want %q, got %q", "two", got)
	}
	steps[1].status = "completed" // no in_progress -> first not-yet-terminal
	if got := currentStepContent(steps); got != "three" {
		t.Errorf("first incomplete: want %q, got %q", "three", got)
	}
	steps[2].status = "completed" // all done -> first
	if got := currentStepContent(steps); got != "one" {
		t.Errorf("all complete: want %q, got %q", "one", got)
	}
	if got := currentStepContent(nil); got != "" {
		t.Errorf("empty: want %q, got %q", "", got)
	}
}

// TestPlanReconcileClearsStaleCompletion: a positional carry-over (same step
// count, no content match) must NOT bleed a prior completed step's completedAt
// into a new in_progress/pending step — that would show an old finished duration.
func TestPlanReconcileClearsStaleCompletion(t *testing.T) {
	var s planPanelState
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.updateFromItems([]tools.PlanItem{{Content: "old step", Status: "completed"}}, t0)
	if s.steps[0].completedAt.IsZero() {
		t.Fatal("a completed step should carry a completedAt")
	}
	// Reword to a NEW in_progress step (same count -> positional carry-over copies
	// the old completedAt); it must be cleared so the live step shows a running clock.
	s.updateFromItems([]tools.PlanItem{{Content: "new step", Status: "in_progress"}}, t0.Add(time.Minute))
	if !s.steps[0].completedAt.IsZero() {
		t.Errorf("in_progress step must not inherit a stale completedAt, got %v", s.steps[0].completedAt)
	}
}

// TestPlanCompleteRemaining: force-completing a stuck plan flips every
// non-terminal step to completed, backfills timestamps, preserves a failed step,
// and is a clean no-op on empty / already-complete plans.
func TestPlanCompleteRemaining(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("force-completes a mid-progress plan", func(t *testing.T) {
		started := now.Add(-time.Minute)
		s := planPanelState{steps: []planStep{
			{content: "a", status: "completed", startedAt: started, completedAt: now.Add(-30 * time.Second)},
			{content: "b", status: "in_progress", startedAt: started},
			{content: "c", status: "pending"},
		}}
		s.completeRemaining(now)
		if !s.isComplete() {
			t.Fatalf("plan should be complete, steps=%+v", s.steps)
		}
		if s.completedAt != now {
			t.Errorf("panel completedAt = %v, want %v", s.completedAt, now)
		}
		// in_progress keeps its real startedAt; gets completedAt=now (real duration).
		if s.steps[1].startedAt != started || s.steps[1].completedAt != now {
			t.Errorf("in_progress timestamps wrong: %+v", s.steps[1])
		}
		// pending gets startedAt=completedAt=now.
		if s.steps[2].status != "completed" || s.steps[2].startedAt != now || s.steps[2].completedAt != now {
			t.Errorf("pending step not completed cleanly: %+v", s.steps[2])
		}
	})

	t.Run("preserves a failed step", func(t *testing.T) {
		s := planPanelState{steps: []planStep{
			{content: "a", status: "failed", startedAt: now, completedAt: now},
			{content: "b", status: "pending"},
		}}
		s.completeRemaining(now)
		if s.steps[0].status != "failed" {
			t.Errorf("failed step should stay failed, got %q", s.steps[0].status)
		}
		if !s.isComplete() {
			t.Error("failed + completed should count as complete")
		}
	})

	t.Run("no-op on empty plan", func(t *testing.T) {
		var s planPanelState
		s.completeRemaining(now)
		if !s.completedAt.IsZero() || len(s.steps) != 0 {
			t.Errorf("empty plan should stay empty: completedAt=%v steps=%d", s.completedAt, len(s.steps))
		}
	})

	t.Run("preserves completedAt on an already-complete plan", func(t *testing.T) {
		orig := now.Add(-time.Hour)
		s := planPanelState{
			steps:       []planStep{{content: "a", status: "completed", startedAt: orig, completedAt: orig}},
			completedAt: orig,
		}
		s.completeRemaining(now)
		if s.completedAt != orig {
			t.Errorf("completedAt should be preserved, got %v want %v", s.completedAt, orig)
		}
	})
}

// TestPlanRewordKeepsTimer: when the model rewords a step in place (same step
// count), its elapsed clock must NOT reset — the positional carry-over preserves
// startedAt that the content-only match used to drop.
func TestPlanRewordKeepsTimer(t *testing.T) {
	var s planPanelState
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.updateFromItems([]tools.PlanItem{
		{Content: "build the thing", Status: "in_progress"},
		{Content: "test it", Status: "pending"},
	}, t0)
	started := s.steps[0].startedAt
	if started.IsZero() {
		t.Fatal("expected startedAt set on first in_progress step")
	}

	t1 := t0.Add(30 * time.Second)
	s.updateFromItems([]tools.PlanItem{
		{Content: "build the thing (with caching)", Status: "in_progress"}, // reworded in place
		{Content: "test it", Status: "pending"},
	}, t1)
	if s.steps[0].startedAt != started {
		t.Errorf("reworded step reset its timer: want %v, got %v", started, s.steps[0].startedAt)
	}
}

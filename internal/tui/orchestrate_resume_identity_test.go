package tui

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/specialist"
)

// ReducePlanEvents keeps only the LAST admitted plan's progress, and records
// whose it is in PlanProgress.Name. Narrowing a plan by progress belonging to a
// different plan silently drops work: task ids like "tests" or "lint" collide
// across plans constantly, so every task of the named plan whose id happens to
// sit in the other plan's succeeded set disappears from the remainder and never
// runs.
func TestResumeRefusesProgressFromADifferentPlan(t *testing.T) {
	m, paths, plan := resumeModel(t)

	// Plan A is admitted and nothing is completed.
	m.planProgress.PlanAdmitted(plan)

	// A DIFFERENT plan then runs and completes a task whose id collides with
	// one of A's. This is what the session log ends up holding.
	other, err := specialist.ParsePlan(map[string]any{
		"name": "unrelated",
		"tasks": []any{
			map[string]any{"id": "a", "prompt": "something else entirely"},
		},
		"budget": map[string]any{"max_workers": float64(1)},
	}, m.savedPlanLimits())
	if err != nil {
		t.Fatalf("ParsePlan(other): %v", err)
	}
	m.planProgress.PlanAdmitted(other)
	m.planProgress.TaskDispatched(specialist.Task{ID: "a"})
	m.planProgress.TaskCompleted(specialist.TaskResult{ID: "a", Attempts: 1})
	if err := m.planProgress.RecordingError(); err != nil {
		t.Fatalf("recording: %v", err)
	}

	stored, err := specialist.FindSavedPlan(paths, "sweep")
	if err != nil {
		t.Fatal(err)
	}
	name, notice, ok := m.resumeSavedPlan(stored)

	if ok {
		// If a remainder was staged at all, it must not have dropped task "a" —
		// the named plan never ran it.
		remaining, findErr := specialist.FindSavedPlan(paths, name)
		if findErr != nil {
			t.Fatalf("staged remainder not findable: %v", findErr)
		}
		restored, parseErr := specialist.ParsePlan(remaining.Args, m.savedPlanLimits())
		if parseErr != nil {
			t.Fatalf("staged remainder does not validate: %v", parseErr)
		}
		for _, id := range restored.Order() {
			if id == "a" {
				return // task survived; acceptable outcome
			}
		}
		t.Fatalf("resume dropped task %q, which the named plan never ran — it was completed by a DIFFERENT plan (notice: %s)", "a", notice)
	}

	// Refusing is the other acceptable outcome, as long as it says why.
	if !strings.Contains(strings.ToLower(notice), "plan") {
		t.Errorf("refusal should explain the progress belongs to another plan, got %q", notice)
	}
}

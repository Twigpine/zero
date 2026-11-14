package tui

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/specialist"
)

// The /plans verbs parse a stored plan before doing anything with it. Granting
// only the read-only names there meant a plan ParsePlan accepts at RUN time —
// write tools may be named per task — failed to parse on save, show, run,
// restart and resume alike. The durability surface silently excluded exactly
// the plans whose work is most worth keeping.
func TestSavedPlanLimitsAcceptAWriteCapablePlan(t *testing.T) {
	m, _ := savedPlanModel(t)

	args := map[string]any{
		"name": "fixup",
		"tasks": []any{
			map[string]any{"id": "read", "prompt": "look"},
			map[string]any{
				"id": "write", "prompt": "change it",
				"depends_on": []any{"read"},
				"tools":      []any{"edit_file"},
			},
		},
		"budget": map[string]any{"max_workers": float64(1)},
	}

	if _, err := specialist.ParsePlan(args, m.savedPlanLimits()); err != nil {
		t.Fatalf("a write-capable plan must parse for the /plans verbs: %v", err)
	}
}

// Widening the limits must not widen what a plan may hold: a tool outside the
// grantable allow-list is still refused.
func TestSavedPlanLimitsStillRefuseUngrantableTools(t *testing.T) {
	m, _ := savedPlanModel(t)
	_, err := specialist.ParsePlan(map[string]any{
		"name":   "bad",
		"tasks":  []any{map[string]any{"id": "a", "prompt": "x", "tools": []any{"orchestrate"}}},
		"budget": map[string]any{"max_workers": float64(1)},
	}, m.savedPlanLimits())
	if err == nil {
		t.Fatal("a plan naming an ungrantable tool was accepted")
	}
	if !strings.Contains(err.Error(), "may never hold") {
		t.Errorf("error should name the rule, got %v", err)
	}
}

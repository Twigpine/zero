package cli

import (
	"testing"

	"github.com/Twigpine/zero/internal/specialist"
)

// TestSpecialistSummariesIncludesBuiltins confirms the orchestrator delegation
// prompt is fed the built-in specialists (the wiring behind auto-delegation).
func TestSpecialistSummariesIncludesBuiltins(t *testing.T) {
	paths, err := specialist.DefaultPaths(t.TempDir())
	if err != nil {
		t.Fatalf("DefaultPaths: %v", err)
	}
	descByName := map[string]string{}
	for _, info := range specialistSummaries(paths) {
		descByName[info.Name] = info.WhenToUse
	}
	for _, name := range []string{"worker", "explorer", "code-review"} {
		if descByName[name] == "" {
			t.Fatalf("specialistSummaries missing built-in %q (or empty description); got %v", name, descByName)
		}
	}
}

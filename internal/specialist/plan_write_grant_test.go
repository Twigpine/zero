package specialist

import (
	"strings"
	"testing"
)

// ParsePlan lets a task name a write tool ("A TASK MAY NOW NAME A WRITE TOOL,
// and only by naming it"), and that grant triggers an approval prompt and an
// isolated worktree. The child then received a system prompt telling it "You
// have read-only tools" and "do not attempt to modify anything" — so it would
// not use the tool it was granted, and the prompt and the worktree bought
// nothing.
func TestPlanTaskPromptMatchesTheGrant(t *testing.T) {
	readOnly := planTaskSystemPrompt([]string{"read_file", "grep"})
	if !strings.Contains(readOnly, "do not attempt to modify anything") {
		t.Error("a read-only task must still be told not to modify anything")
	}
	if !strings.Contains(readOnly, "read-only tools") {
		t.Error("the read-only wording changed; it was tuned and should stay")
	}

	for _, tool := range PlanWriteToolNames() {
		t.Run(tool, func(t *testing.T) {
			prompt := planTaskSystemPrompt([]string{"read_file", tool})
			if strings.Contains(prompt, "do not attempt to modify anything") {
				t.Errorf("a task granted %s is told not to modify anything", tool)
			}
			if strings.Contains(prompt, "You have read-only tools") {
				t.Errorf("a task granted %s is told its tools are read-only", tool)
			}
			// Both prompts must keep the investigate-first contract: a task that
			// reasons from memory is the defect that wording exists for.
			if !strings.Contains(prompt, "Start with a tool call") {
				t.Errorf("the write prompt for %s dropped the investigate-first contract", tool)
			}
		})
	}
}

// The write detection must use the same allow-list ParsePlan validates against,
// or the two drift and a newly grantable tool silently keeps the read-only
// prompt.
func TestWriteGrantDetectionCoversEveryGrantableWriteTool(t *testing.T) {
	for _, tool := range PlanWriteToolNames() {
		if !grantsPlanWriteTool([]string{tool}) {
			t.Errorf("%s is grantable as a write tool but not detected as one", tool)
		}
	}
	for _, tool := range PlanReadOnlyToolNames() {
		if grantsPlanWriteTool([]string{tool}) {
			t.Errorf("%s is read-only but counted as a write grant", tool)
		}
	}
}

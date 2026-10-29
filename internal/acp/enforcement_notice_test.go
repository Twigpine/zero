package acp

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/tools"
)

// AN ACP CLIENT MUST SEE THE DISCLOSURE THE TUI SEES.
//
// agent.ToolResult stores the UNDECORATED model text alongside the typed
// enforcement notices; ModelOutput is what composes them. Reading .Output
// directly compiles and looks right, and silently drops the notice for every
// ACP client, which is the one surface with no other way to learn the sandbox
// narrowed what the command could do.
func TestToolResultContentCarriesTheEnforcementNotice(t *testing.T) {
	const notice = "least-privilege notice: read access was narrowed"
	result := agent.ToolResult{
		Name:               "bash",
		Status:             tools.StatusOK,
		Output:             "the command output",
		EnforcementNotices: []string{notice},
	}

	content := toolResultContent(result)
	if len(content) == 0 {
		t.Fatal("no content produced for a successful tool result")
	}
	var text strings.Builder
	for _, part := range content {
		if part.Content != nil {
			text.WriteString(part.Content.Text)
		}
	}
	got := text.String()

	if count := strings.Count(got, notice); count != 1 {
		t.Errorf("the notice appears %d times, want exactly 1:\n%s", count, got)
	}
	if !strings.Contains(got, "the command output") {
		t.Errorf("the underlying output was lost:\n%s", got)
	}
}

// And a result with no notice is unchanged, so the accessor is not adding
// anything to ordinary output.
func TestToolResultContentLeavesAnOrdinaryResultAlone(t *testing.T) {
	result := agent.ToolResult{
		Name:   "bash",
		Status: tools.StatusOK,
		Output: "plain output",
	}
	content := toolResultContent(result)
	if len(content) == 0 {
		t.Fatal("no content produced")
	}
	if content[0].Content == nil {
		t.Fatal("content block missing")
	}
	if got := content[0].Content.Text; got != "plain output" {
		t.Errorf("ordinary output = %q, want it untouched", got)
	}
}

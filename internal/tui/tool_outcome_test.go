package tui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/tools"
)

type tuiOutcomeErrorTool struct {
	output string
}

func (tool tuiOutcomeErrorTool) Name() string             { return tools.ExecCommandToolName }
func (tool tuiOutcomeErrorTool) Description() string      { return "test tool" }
func (tool tuiOutcomeErrorTool) Parameters() tools.Schema { return tools.Schema{Type: "object"} }
func (tool tuiOutcomeErrorTool) Safety() tools.Safety {
	return tools.Safety{Permission: tools.PermissionAllow}
}
func (tool tuiOutcomeErrorTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusError, Output: tool.output}
}

func setToolOutcomeTempDir(t *testing.T) {
	t.Helper()
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("TMP", tempDir)
	t.Setenv("TEMP", tempDir)
}

func TestToolResultDetailUsesFinalizedHumanEvidenceForReducedError(t *testing.T) {
	setToolOutcomeTempDir(t)
	raw := "output:\n" + strings.Repeat("ok  \texample.test/package\t0.01s\n", 24) +
		"--- FAIL: TestImportant\nexpected 7, got 9\nFAIL\nexit_code: 1"
	registry := tools.NewRegistry()
	registry.Register(tuiOutcomeErrorTool{output: raw})
	result := registry.Run(context.Background(), tools.ExecCommandToolName, map[string]any{"cmd": "go test ./..."})

	detail := toolResultDetail(agent.ToolResult{
		Name:    tools.ExecCommandToolName,
		Status:  result.Status,
		Output:  result.Output,
		Display: result.Display,
		Outcome: result.Outcome,
	})
	for _, want := range []string{"ok  \texample.test/package", "TestImportant", "expected 7, got 9"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("human error detail missing %q: %q", want, detail)
		}
	}
	if detail == result.ModelOutput() {
		t.Fatal("human detail collapsed back to the reduced model view")
	}
}

func TestToolResultDetailSurvivesOutcomeJSONRoundTrip(t *testing.T) {
	setToolOutcomeTempDir(t)
	raw := "output:\n" + strings.Repeat("ok  \texample.test/package\t0.01s\n", 24) +
		"--- FAIL: TestImportant\nexpected 7, got 9\nFAIL\nexit_code: 1"
	registry := tools.NewRegistry()
	registry.Register(tuiOutcomeErrorTool{output: raw})
	toolResult := registry.Run(context.Background(), tools.ExecCommandToolName, map[string]any{"cmd": "go test ./..."})
	original := agent.ToolResult{
		Name:    tools.ExecCommandToolName,
		Status:  toolResult.Status,
		Output:  toolResult.Output,
		Display: toolResult.Display,
		Outcome: toolResult.Outcome,
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal tool result: %v", err)
	}
	var restored agent.ToolResult
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("unmarshal tool result: %v", err)
	}
	if !restored.Outcome.Finalized() {
		t.Fatal("restored outcome lost its finalized state")
	}
	detail := toolResultDetail(restored)
	for _, want := range []string{"ok  \texample.test/package", "TestImportant", "expected 7, got 9"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("restored human detail missing %q: %q", want, detail)
		}
	}
}

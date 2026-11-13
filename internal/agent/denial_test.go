package agent

import (
	"context"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/tools"
)

func TestExecuteToolCallCategorizesFilteredDenial(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))

	result, _ := executeToolCall(context.Background(), registry, ToolCall{ID: "c1", Name: "read_file", Arguments: `{"path":"x"}`}, PermissionModeAuto, Options{
		DisabledTools: []string{"read_file"},
	})
	if result.DenialReason != DenialFiltered {
		t.Fatalf("DenialReason = %q, want %q", result.DenialReason, DenialFiltered)
	}
	if result.Status != tools.StatusError {
		t.Fatalf("status = %q, want error", result.Status)
	}
}

func TestDeniedPermissionResultCategories(t *testing.T) {
	// Plain approval-declined.
	plain := deniedPermissionResult(ToolCall{ID: "c1", Name: "bash"}, "needs approval", PermissionEvent{ToolName: "bash"})
	if plain.DenialReason != DenialPermissionDenied {
		t.Fatalf("plain denial = %q, want %q", plain.DenialReason, DenialPermissionDenied)
	}
	// Sandbox-block-driven denial.
	sandboxDenied := deniedPermissionResult(ToolCall{ID: "c2", Name: "bash"}, "outside workspace", PermissionEvent{
		ToolName: "bash",
		Block:    &sandbox.Block{Code: "outside_workspace"},
	})
	if sandboxDenied.DenialReason != DenialSandboxBlock {
		t.Fatalf("sandbox denial = %q, want %q", sandboxDenied.DenialReason, DenialSandboxBlock)
	}
}

package sandbox

import (
	"context"
	"runtime"
	"testing"

	"github.com/Twigpine/zero/internal/execution"
)

func TestEnginePreparesTypedCapturedExecution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses POSIX shell")
	}
	workspace := t.TempDir()
	engine := NewEngine(EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        DefaultPolicy(),
		Backend:       Backend{Name: BackendUnavailable, Message: "test backend unavailable"},
	})
	runner := execution.NewRunner(engine)
	result := runner.ExecuteCaptured(context.Background(), execution.CapturedRequest{Request: execution.Request{
		Origin:           execution.OriginHook,
		Mode:             execution.ModeCaptured,
		Command:          execution.Command{Name: "/bin/sh", Args: []string{"-c", "printf adapted"}},
		WorkingDirectory: workspace,
		WorkspaceRoots:   []string{workspace},
		Approval:         execution.ApprovalContext{PolicyVersion: execution.PolicyVersion},
	}})
	if result.Outcome.Kind != execution.OutcomeSuccess || result.Stdout != "adapted" {
		t.Fatalf("captured result = %#v", result)
	}
	if result.Outcome.Enforcement.Level != string(EnforcementDegraded) {
		t.Fatalf("enforcement = %#v, want degraded", result.Outcome.Enforcement)
	}
}

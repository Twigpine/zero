package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/sandbox"
)

// nativeBackendStub reports as an active wrapping sandbox so shellSandboxActive
// is true. Its executable does not exist, so the command itself fails to launch
// — that is fine: these tests assert the *permission gate*, not execution.
func nativeBackendStub() sandbox.Backend {
	return sandbox.Backend{
		Name:            sandbox.BackendLinuxBwrap,
		Available:       true,
		Executable:      "/nonexistent/zero-linux-sandbox-stub",
		CommandWrapping: true,
		NativeIsolation: true,
	}
}

func sandboxedBashPolicy() sandbox.Policy {
	policy := sandbox.DefaultPolicy()
	// Network deny so no proxy is started for these gate-only tests.
	return policy
}

const permissionRequiredFragment = "Permission required for bash"

func TestBashAutoAllowedWhenSandboxActive(t *testing.T) {
	root := t.TempDir()
	registry := NewRegistry()
	registry.Register(NewScopedBashTool(root, nil))
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: root,
		Policy:        sandboxedBashPolicy(),
		Backend:       nativeBackendStub(),
	})

	result := registry.RunWithOptions(context.Background(), "bash", map[string]any{
		"command": "echo hi",
	}, RunOptions{
		PermissionGranted: false,
		Sandbox:           engine,
		PermissionMode:    string(sandbox.PermissionModeAsk),
		Autonomy:          "high",
	})

	if strings.Contains(result.Output, permissionRequiredFragment) {
		t.Fatalf("bash was gated despite auto-allow: %q", result.Output)
	}
	if result.SandboxDecision == nil || result.SandboxDecision.Action != sandbox.ActionAllow || !result.SandboxDecision.AutoAllowed {
		t.Fatalf("sandbox decision = %#v, want auto-allowed allow", result.SandboxDecision)
	}
}

func TestBashRequireEscalatedPromptsWhenSandboxActive(t *testing.T) {
	root := t.TempDir()
	registry := NewRegistry()
	registry.Register(NewScopedBashTool(root, nil))
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: root,
		Policy:        sandboxedBashPolicy(),
		Backend:       nativeBackendStub(),
	})

	result := registry.RunWithOptions(context.Background(), "bash", map[string]any{
		"command":             "echo hi",
		"sandbox_permissions": string(SandboxPermissionsRequireEscalated),
	}, RunOptions{
		PermissionGranted: false,
		Sandbox:           engine,
		PermissionMode:    string(sandbox.PermissionModeAsk),
		Autonomy:          "high",
	})

	if result.Status != StatusError || !strings.Contains(result.Output, sandbox.ReasonEscalatedSandboxRequired) {
		t.Fatalf("expected require_escalated approval prompt, got %s: %q", result.Status, result.Output)
	}
	if result.SandboxDecision == nil || result.SandboxDecision.Action != sandbox.ActionPrompt {
		t.Fatalf("sandbox decision = %#v, want prompt", result.SandboxDecision)
	}
}

func TestBashRequireEscalatedBypassesNativeSandboxAfterApproval(t *testing.T) {
	root := t.TempDir()
	registry := NewRegistry()
	registry.Register(NewScopedBashTool(root, nil))
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: root,
		Policy:        sandboxedBashPolicy(),
		Backend:       nativeBackendStub(),
	})

	result := registry.RunWithOptions(context.Background(), "bash", map[string]any{
		"command":             helperCommand("success"),
		"sandbox_permissions": string(SandboxPermissionsRequireEscalated),
	}, RunOptions{
		PermissionGranted: true,
		Sandbox:           engine,
		PermissionMode:    string(sandbox.PermissionModeAsk),
		Autonomy:          "high",
	})

	if result.Status != StatusOK || !strings.Contains(result.Output, "hello from bash") {
		t.Fatalf("expected approved require_escalated command to run direct, got %s: %q", result.Status, result.Output)
	}
	if result.Meta["sandbox_permissions"] != string(SandboxPermissionsRequireEscalated) {
		t.Fatalf("sandbox_permissions meta = %q", result.Meta["sandbox_permissions"])
	}
	if result.Meta["sandbox_wrapped"] == "true" {
		t.Fatalf("require_escalated command must not be wrapped; meta=%#v", result.Meta)
	}
}

func TestBashRequireEscalatedKeepsSandboxWhenDeniedReadsActive(t *testing.T) {
	root := t.TempDir()
	policy := sandboxedBashPolicy()
	policy.DenyRead = []string{root + "/secret.txt"}
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: root,
		Policy:        policy,
		Backend:       nativeBackendStub(),
	})

	if commandEngineForSandboxPermissions(engine, SandboxPermissionsRequireEscalated) == nil {
		t.Fatal("require_escalated must preserve the sandbox when denied reads are active")
	}
}

func TestBashRequireEscalatedKeepsSandboxWhenDaemonTokenProtected(t *testing.T) {
	engine := defaultPolicyDaemonTokenEngine(t)
	if commandEngineForSandboxPermissions(engine, SandboxPermissionsRequireEscalated) == nil {
		t.Fatal("bash require_escalated must preserve the sandbox when the default remote-daemon policy protects a token file")
	}
}

func TestBashStillPromptsWithoutActiveSandbox(t *testing.T) {
	root := t.TempDir()
	registry := NewRegistry()
	registry.Register(NewScopedBashTool(root, nil))
	engine := sandbox.NewEngine(sandbox.EngineOptions{
		WorkspaceRoot: root,
		Policy:        sandboxedBashPolicy(),
		Backend:       sandbox.Backend{Name: sandbox.BackendUnavailable},
	})

	result := registry.RunWithOptions(context.Background(), "bash", map[string]any{
		"command": "echo hi",
	}, RunOptions{
		PermissionGranted: false,
		Sandbox:           engine,
		PermissionMode:    string(sandbox.PermissionModeAsk),
		Autonomy:          "high",
	})

	if result.Status != StatusError || !strings.Contains(result.Output, "Sandbox approval required for bash") {
		t.Fatalf("expected bash to be gated when sandbox inactive, got %s: %q", result.Status, result.Output)
	}
}

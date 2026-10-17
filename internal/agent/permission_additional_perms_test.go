package agent

import (
	"context"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/tools"
)

// promptShellTool is a minimal shell-style tool whose Safety requires a prompt,
// standing in for bash so buildPermissionEvent can be exercised directly.
type promptShellTool struct{}

func (promptShellTool) Name() string        { return "bash" }
func (promptShellTool) Description() string { return "run a shell command" }
func (promptShellTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", AdditionalProperties: false}
}
func (promptShellTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectShell, Permission: tools.PermissionPrompt, Reason: "runs a shell command"}
}
func (promptShellTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK}
}

// A command whose sandbox decision is Allow but that explicitly requests
// additional sandbox permissions is an ELEVATION: it must be surfaced as a
// prompt, never Action=allow. Previously it carried allow while the loop still
// blocked on OnPermissionRequest, and the TUI (which renders only prompts)
// dropped it → the run deadlocked forever. This is the exact gpt-5.5
// folder-creation shape captured in the reported hang.
func TestBuildPermissionEventPromptsForAdditionalPermissions(t *testing.T) {
	call := ToolCall{ID: "call_1", Name: "bash", Arguments: `{}`}
	args := map[string]any{
		"command":             "mkdir foo && cd foo && go mod init foo",
		"sandbox_permissions": string(tools.SandboxPermissionsWithAdditionalPermissions),
	}
	// Base command allowed by the sandbox…
	decision := &sandbox.Decision{Action: sandbox.ActionAllow}

	event, ok := buildPermissionEvent(call, promptShellTool{}, args, false, PermissionModeAsk, Options{}, decision)
	if !ok {
		t.Fatal("buildPermissionEvent returned ok=false; the elevation request must produce an event")
	}
	if event.Action != PermissionActionPrompt {
		t.Fatalf("Action = %q, want prompt — an additional-permissions elevation must ask the user, not auto-allow", event.Action)
	}
	// Sanity: shouldRequestPermission agrees the loop blocks on this.
	if !shouldRequestPermission(promptShellTool{}, args, false, decision) {
		t.Fatal("shouldRequestPermission must be true for an additional-permissions request")
	}
}

// Without the additional-permissions flag, a sandbox-allowed command is NOT a
// prompt (shouldRequestPermission is false; it just runs) — the override must
// not over-reach and prompt for ordinary allowed commands.
func TestBuildPermissionEventKeepsAllowForOrdinaryAllowedCommand(t *testing.T) {
	call := ToolCall{ID: "call_2", Name: "bash", Arguments: `{}`}
	args := map[string]any{"command": "ls"}
	decision := &sandbox.Decision{Action: sandbox.ActionAllow}

	event, _ := buildPermissionEvent(call, promptShellTool{}, args, false, PermissionModeAsk, Options{}, decision)
	if event.Action == PermissionActionPrompt {
		t.Fatalf("Action = prompt for an ordinary allowed command; want allow (no spurious prompt)")
	}
	if shouldRequestPermission(promptShellTool{}, args, false, decision) {
		t.Fatal("shouldRequestPermission must be false for an ordinary sandbox-allowed command")
	}
}

func TestBuildPermissionEventReusesCoveredAdditionalPermissions(t *testing.T) {
	call := ToolCall{ID: "call_3", Name: "bash", Arguments: `{}`}
	args := map[string]any{
		"command":             "npm install",
		"sandbox_permissions": string(tools.SandboxPermissionsWithAdditionalPermissions),
		"additional_permissions": map[string]any{
			"network": map[string]any{"enabled": true},
		},
	}
	decision := &sandbox.Decision{Action: sandbox.ActionAllow, AutoAllowed: true}

	if shouldRequestPermission(promptShellTool{}, args, true, decision) {
		t.Fatal("an already-covered capability must not trigger another prompt")
	}
	event, ok := buildPermissionEvent(call, promptShellTool{}, args, true, PermissionModeAsk, Options{}, decision)
	if !ok {
		t.Fatal("covered capability should still produce an auditable allow event")
	}
	if event.Action != PermissionActionAllow {
		t.Fatalf("Action = %q, want allow for an already-covered capability", event.Action)
	}
}

func TestBuildPermissionEventOmitsGenericShellReasonFromPrompt(t *testing.T) {
	call := ToolCall{ID: "call_4", Name: "bash", Arguments: `{}`}
	args := map[string]any{"command": "make test"}
	decision := &sandbox.Decision{Action: sandbox.ActionPrompt, Reason: "runs a shell command"}

	event, ok := buildPermissionEvent(call, promptShellTool{}, args, false, PermissionModeAsk, Options{}, decision)
	if !ok {
		t.Fatal("generic shell approval must produce an event")
	}
	if event.Reason != "" {
		t.Fatalf("user-facing reason = %q, want omitted generic tool text", event.Reason)
	}
	if event.DecisionReason != "runs a shell command" {
		t.Fatalf("decision reason = %q, want raw audit reason retained", event.DecisionReason)
	}
}

func TestBuildPermissionEventPrefersPolicyReasonOverModelJustification(t *testing.T) {
	call := ToolCall{ID: "call_5", Name: "bash", Arguments: `{}`}
	args := map[string]any{
		"command":             "curl https://example.test",
		"sandbox_permissions": string(tools.SandboxPermissionsRequireEscalated),
		"justification":       "Run without sandboxing.",
	}
	decision := &sandbox.Decision{Action: sandbox.ActionPrompt, Reason: sandbox.ReasonNetworkBlocked}

	event, ok := buildPermissionEvent(call, promptShellTool{}, args, false, PermissionModeAsk, Options{}, decision)
	if !ok {
		t.Fatal("policy approval must produce an event")
	}
	if event.Reason != sandbox.ReasonNetworkBlocked {
		t.Fatalf("user-facing reason = %q, want policy reason %q", event.Reason, sandbox.ReasonNetworkBlocked)
	}
}

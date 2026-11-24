package sandbox

import (
	"context"
	"testing"
)

// Shells accept command mode inside a short-option cluster. In particular,
// `sh -lc` is a common login-shell spelling; treating its `-l` as invalid made
// the parser stop before the command payload and let network use bypass the
// network-deny prompt.
func TestEvaluateCombinedShellOptionsPreservesNetworkGate(t *testing.T) {
	engine := NewEngine(EngineOptions{Policy: Policy{Mode: ModeEnforce, Network: NetworkDeny}})
	networkCases := []string{
		`bash -lc "curl https://evil.test"`,
		`bash -ic "curl https://evil.test"`,
		`bash -xc "curl https://evil.test"`,
		`bash -ec "curl https://evil.test"`,
		`sh -lc "curl https://evil.test"`,
		`zsh -lc "curl https://evil.test"`,
		`bash -lc "git push origin main"`,
	}
	for _, command := range networkCases {
		t.Run(command, func(t *testing.T) {
			decision := engine.Evaluate(context.Background(), Request{
				ToolName: "bash", SideEffect: SideEffectShell, PermissionGranted: true,
				Args: map[string]any{"command": command},
			})
			if decision.Action != ActionPrompt || decision.Reason != ReasonNetworkBlocked {
				t.Fatalf("Evaluate(%q) = action %q reason %q risk %v, want network prompt", command, decision.Action, decision.Reason, decision.Risk.Categories)
			}
		})
	}

	for _, command := range []string{
		`bash -lc "printf ok"`,
		`sh -lc "printf ok"`,
		`zsh -lc "printf ok"`,
	} {
		t.Run("local/"+command, func(t *testing.T) {
			decision := engine.Evaluate(context.Background(), Request{
				ToolName: "bash", SideEffect: SideEffectShell, PermissionGranted: true,
				Args: map[string]any{"command": command},
			})
			if decision.Reason == ReasonNetworkBlocked || HasRiskCategory(decision.Risk, "network") {
				t.Fatalf("Evaluate(%q) = %#v, want proven-local network classification", command, decision)
			}
		})
	}
}

package agent

import (
	"path/filepath"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/tools"
)

// escalatingEngine allows unsandboxed escalation: no denied reads, so
// UnsandboxedExecutionAllowed is true and a prefix approval would rewrite the
// call to require_escalated.
func escalatingEngine(t *testing.T) *sandbox.Engine {
	t.Helper()
	policy := sandbox.DefaultPolicy()
	policy.DenyRead = nil
	return sandbox.NewEngine(sandbox.EngineOptions{WorkspaceRoot: t.TempDir(), Policy: policy})
}

// nonEscalatingEngine has a denied read, which is exactly the condition under
// which escalation is refused, so no disclosure should be made.
func nonEscalatingEngine(t *testing.T) *sandbox.Engine {
	t.Helper()
	root := t.TempDir()
	policy := sandbox.DefaultPolicy()
	policy.DenyRead = []string{filepath.Join(root, "secret.txt")}
	return sandbox.NewEngine(sandbox.EngineOptions{WorkspaceRoot: root, Policy: policy})
}

// THE POINT OF THE FILE. The prompt's disclosure and the args rewrite must
// agree on every input, because the defect being fixed is precisely that the
// two disagreed: the rewrite escalated out of the sandbox while the prompt said
// only "allow command prefix for session".
//
// Asserting agreement rather than asserting each side's expected value is what
// makes this hold up. If someone later widens or narrows the rewrite's guards
// and forgets the flag, the two diverge here and this fails, which a pair of
// independent expected-value tests would not catch.
func TestPrefixEscalationDisclosureMatchesTheArgsRewrite(t *testing.T) {
	escalating := Options{Sandbox: escalatingEngine(t)}
	denied := Options{Sandbox: nonEscalatingEngine(t)}

	cases := []struct {
		name     string
		toolName string
		args     map[string]any
		options  Options
	}{
		{"plain bash command", "bash", map[string]any{"command": "go test ./..."}, escalating},
		{"plain exec_command", "exec_command", map[string]any{"cmd": "go build ./..."}, escalating},
		{"not a shell tool", "read_file", map[string]any{"path": "go.mod"}, escalating},
		{"no sandbox configured", "bash", map[string]any{"command": "go test ./..."}, Options{}},
		{"escalation refused by denied reads", "bash", map[string]any{"command": "go test ./..."}, denied},
		{
			"call already asks for escalation",
			"bash",
			map[string]any{"command": "go test ./...", "sandbox_permissions": string(tools.SandboxPermissionsRequireEscalated)},
			escalating,
		},
		{
			"call already asks for additional permissions",
			"bash",
			map[string]any{"command": "go test ./...", "sandbox_permissions": string(tools.SandboxPermissionsWithAdditionalPermissions)},
			escalating,
		},
		// An unrecognized value is NOT an opt-out: the rewrite overwrites it, so
		// the disclosure has to fire. Included because the obvious reading of
		// the guards says otherwise, and a case asserting the wrong thing here
		// would have passed silently.
		{
			"unrecognized sandbox_permissions value",
			"bash",
			map[string]any{"command": "go test ./...", "sandbox_permissions": "all"},
			escalating,
		},
		{"empty args", "bash", map[string]any{}, escalating},
		{"nil args", "bash", nil, escalating},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			disclosed := shellPrefixApprovalEscalates(testCase.toolName, testCase.args, testCase.options)

			// What the user's approval would actually be turned into.
			rewritten := shellExecutionArgsForApproval(testCase.toolName, testCase.args, PermissionDecisionAllowPrefix, testCase.options)
			escalates := shellCommandRequiresEscalated(rewritten) && !shellCommandRequiresEscalated(testCase.args)

			if disclosed != escalates {
				t.Fatalf("prompt discloses escalation=%v but approving actually escalates=%v; the prompt and the rewrite disagree, which is the consent gap this guards", disclosed, escalates)
			}
		})
	}
}

// The two prefix decisions are the only ones that escalate, so a disclosure
// attached to a request offering neither would be a false alarm. Checked
// against the same predicate the rewrite consults.
func TestOnlyPrefixDecisionsEscalate(t *testing.T) {
	escalating := map[PermissionDecisionAction]bool{
		PermissionDecisionAllowPrefix:       true,
		PermissionDecisionAlwaysAllowPrefix: true,
	}
	for _, action := range []PermissionDecisionAction{
		PermissionDecisionAllow,
		PermissionDecisionAllowStrict,
		PermissionDecisionAllowForSession,
		PermissionDecisionAllowPrefix,
		PermissionDecisionAlwaysAllowPrefix,
		PermissionDecisionAlwaysAllow,
		PermissionDecisionDeny,
		PermissionDecisionCancel,
	} {
		if got := shellPrefixApprovalBypassesSandbox(action); got != escalating[action] {
			t.Errorf("decision %q bypasses sandbox=%v, want %v", action, got, escalating[action])
		}
	}
}

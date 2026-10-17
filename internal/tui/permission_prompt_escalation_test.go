package tui

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
)

func prefixOptionLabels(request agent.PermissionRequest) []string {
	labels := []string{}
	for _, option := range permissionOptions(request) {
		if option.choice == permissionDecisionAllowPrefix || option.choice == permissionDecisionAlwaysAllowPrefix {
			labels = append(labels, option.label)
		}
	}
	return labels
}

func prefixRequest(escalates bool) agent.PermissionRequest {
	return agent.PermissionRequest{
		ToolName: "bash",
		AvailableDecisions: []agent.PermissionDecisionAction{
			agent.PermissionDecisionAllow,
			agent.PermissionDecisionAllowPrefix,
			agent.PermissionDecisionAlwaysAllowPrefix,
			agent.PermissionDecisionDeny,
		},
		PrefixApprovalEscalates: escalates,
	}
}

// Approving a prefix also runs the command outside the sandbox, and the label
// is the only place the person deciding could learn that. Both prefix options
// escalate, so both must say so; covering only one would leave the other as a
// silent path to the same outcome.
func TestPrefixOptionsDiscloseLeavingTheSandbox(t *testing.T) {
	labels := prefixOptionLabels(prefixRequest(true))
	if len(labels) != 2 {
		t.Fatalf("expected both prefix options, got %#v", labels)
	}
	for _, label := range labels {
		if !strings.Contains(label, "outside the sandbox") {
			t.Errorf("prefix option %q does not say the command leaves the sandbox, so approving it is uninformed consent", label)
		}
	}
}

// The disclosure has to be conditional. When escalation will not happen, saying
// it will is a false warning, and a prompt that cries wolf on every prefix is
// one the user learns to ignore, which costs the disclosure its whole value.
func TestPrefixOptionsStaySilentWhenNoEscalationHappens(t *testing.T) {
	for _, label := range prefixOptionLabels(prefixRequest(false)) {
		if strings.Contains(label, "sandbox") {
			t.Errorf("prefix option %q warns about the sandbox when approving would not leave it", label)
		}
	}
}

// The non-prefix options do not escalate, so the flag must not bleed into them.
func TestNonPrefixOptionsNeverCarryTheDisclosure(t *testing.T) {
	for _, option := range permissionOptions(prefixRequest(true)) {
		if option.choice == permissionDecisionAllowPrefix || option.choice == permissionDecisionAlwaysAllowPrefix {
			continue
		}
		if strings.Contains(option.label, "sandbox") {
			t.Errorf("option %q carries the prefix escalation disclosure but does not escalate", option.label)
		}
	}
}

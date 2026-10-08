package acp

import (
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
)

func escalationRequest(escalates bool) agent.PermissionRequest {
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

// An ACP client renders only Name, so if the escalation is not in the label the
// editor user approves it blind. They face exactly the outcome a terminal user
// faces, so the disclosure has to reach both clients or it is not a fix.
func TestACPPrefixOptionsDiscloseLeavingTheSandbox(t *testing.T) {
	found := 0
	for _, option := range buildPermissionOptions(escalationRequest(true)) {
		action := agent.PermissionDecisionAction(option.OptionID)
		if action != agent.PermissionDecisionAllowPrefix && action != agent.PermissionDecisionAlwaysAllowPrefix {
			if strings.Contains(option.Name, "sandbox") {
				t.Errorf("option %q carries the disclosure but does not escalate", option.Name)
			}
			continue
		}
		found++
		if !strings.Contains(option.Name, "outside the sandbox") {
			t.Errorf("prefix option %q does not say the command leaves the sandbox", option.Name)
		}
	}
	if found != 2 {
		t.Fatalf("expected both prefix options, saw %d", found)
	}
}

func TestACPPrefixOptionsStaySilentWithoutEscalation(t *testing.T) {
	for _, option := range buildPermissionOptions(escalationRequest(false)) {
		if strings.Contains(option.Name, "sandbox") {
			t.Errorf("option %q warns about the sandbox when approving would not leave it", option.Name)
		}
	}
}

// The disclosure must live in the label only. The option id is the decision
// action round-tripped verbatim, and decisionFromOutcome matches it against
// what was offered, so decorating it would fail every prefix approval closed.
func TestACPDisclosureDoesNotDisturbTheOptionIDRoundTrip(t *testing.T) {
	request := escalationRequest(true)
	options := buildPermissionOptions(request)
	for _, option := range options {
		decision := decisionFromOutcome(
			RequestPermissionOutcome{Outcome: OutcomeSelected, OptionID: option.OptionID},
			options,
		)
		if string(decision.Action) != option.OptionID {
			t.Errorf("option id %q round-tripped to %q (%s)", option.OptionID, decision.Action, decision.Reason)
		}
	}
}

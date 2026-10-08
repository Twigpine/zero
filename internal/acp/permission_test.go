package acp

import (
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
)

func TestBuildPermissionOptions(t *testing.T) {
	req := agent.PermissionRequest{
		ToolCallID: "tc1",
		ToolName:   "bash",
		AvailableDecisions: []agent.PermissionDecisionAction{
			agent.PermissionDecisionAllow,
			agent.PermissionDecisionAllowForSession,
			agent.PermissionDecisionAlwaysAllow,
			agent.PermissionDecisionDeny,
			agent.PermissionDecisionCancel, // must be dropped (expressed as outcome)
		},
	}
	opts := buildPermissionOptions(req)
	if len(opts) != 4 {
		t.Fatalf("expected 4 options (cancel dropped), got %d: %+v", len(opts), opts)
	}
	// optionId must carry the ZERO action verbatim for a clean round trip.
	if opts[0].OptionID != string(agent.PermissionDecisionAllow) || opts[0].Kind != PermAllowOnce {
		t.Errorf("allow option = %+v", opts[0])
	}
	if opts[1].Kind != PermAllowAlways || opts[3].Kind != PermRejectOnce {
		t.Errorf("kinds = %q, %q", opts[1].Kind, opts[3].Kind)
	}
}

func TestBuildPermissionOptionsDefault(t *testing.T) {
	opts := buildPermissionOptions(agent.PermissionRequest{ToolName: "x"})
	if len(opts) != 2 {
		t.Fatalf("expected default allow+deny, got %d", len(opts))
	}
}

func TestDecisionFromOutcome(t *testing.T) {
	offered := buildPermissionOptions(agent.PermissionRequest{
		ToolName: "bash",
		AvailableDecisions: []agent.PermissionDecisionAction{
			agent.PermissionDecisionAllow,
			agent.PermissionDecisionAlwaysAllow,
			agent.PermissionDecisionDeny,
		},
	})
	if d := decisionFromOutcome(RequestPermissionOutcome{Outcome: OutcomeCancelled}, offered); d.Action != agent.PermissionDecisionCancel {
		t.Errorf("cancelled -> %q, want cancel", d.Action)
	}
	if d := decisionFromOutcome(RequestPermissionOutcome{Outcome: OutcomeSelected, OptionID: "allow"}, offered); d.Action != agent.PermissionDecisionAllow {
		t.Errorf("selected allow -> %q", d.Action)
	}
	if d := decisionFromOutcome(RequestPermissionOutcome{Outcome: OutcomeSelected, OptionID: "always_allow"}, offered); d.Action != agent.PermissionDecisionAlwaysAllow {
		t.Errorf("selected always_allow -> %q", d.Action)
	}
	// Unknown option fails closed to deny.
	if d := decisionFromOutcome(RequestPermissionOutcome{Outcome: OutcomeSelected, OptionID: "bogus"}, offered); d.Action != agent.PermissionDecisionDeny {
		t.Errorf("unknown option -> %q, want deny", d.Action)
	}
	// Missing/empty outcome fails closed to deny.
	if d := decisionFromOutcome(RequestPermissionOutcome{}, offered); d.Action != agent.PermissionDecisionDeny {
		t.Errorf("empty outcome -> %q, want deny", d.Action)
	}
}

func TestPermissionToolCall(t *testing.T) {
	tc := permissionToolCall(agent.PermissionRequest{
		ToolCallID: "tc9",
		ToolName:   "read_file",
		Args:       map[string]any{"path": "a.go"},
	})
	if tc.ToolCallID != "tc9" || tc.Kind != ToolKindRead || tc.Status != ToolStatusPending {
		t.Fatalf("unexpected toolCall: %+v", tc)
	}
	if tc.Title != "read_file a.go" {
		t.Errorf("title = %q", tc.Title)
	}
	if len(tc.RawInput) == 0 {
		t.Error("expected rawInput from args")
	}
}

func TestPermissionToolCallKeepsTheBrowserDescriptor(t *testing.T) {
	call := permissionToolCall(agent.PermissionRequest{
		ToolCallID: "browser-1",
		ToolName:   "browser_connect",
		Args:       map[string]any{"target": "127.0.0.1:9222"},
	})
	if got := browserDescriptor(t, call); got != (BrowserToolDetails{Version: 1, Command: "connect"}) {
		t.Fatalf("browser descriptor = %#v", got)
	}
	if call.Title != "browser connect" {
		t.Fatalf("title = %q", call.Title)
	}
}

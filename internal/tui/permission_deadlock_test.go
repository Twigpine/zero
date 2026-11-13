package tui

import (
	"context"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
)

func permissionModel(t *testing.T) model {
	t.Helper()
	m := newModel(context.Background(), Options{})
	m.pending = true
	m.activeRunID = 7
	m.width = 96
	return m
}

// The deadlock: a permission request whose Action is NOT prompt (e.g. a
// sandbox-allowed command that still blocked to request additional permissions)
// must be resolved immediately — the agent goroutine is parked awaiting a
// decision. Before the fix the handler dropped it, and the run hung forever
// (the reported "stuck for 33 minutes"). Fail closed: a non-prompt allow denies.
func TestPermissionRequestNonPromptResolvesFailClosed(t *testing.T) {
	m := permissionModel(t)
	var got *agent.PermissionDecision
	decide := func(d agent.PermissionDecision) { got = &d }

	updated, _ := m.Update(permissionRequestMsg{
		runID:   7,
		request: agent.PermissionRequest{ToolCallID: "c1", ToolName: "bash", Action: agent.PermissionActionAllow},
		decide:  decide,
	})
	next := updated.(model)

	if got == nil {
		t.Fatal("DEADLOCK: decide was never called for a non-prompt request — the agent goroutine would park forever")
	}
	if got.Action != agent.PermissionDecisionDeny {
		t.Fatalf("decision = %q, want deny (fail closed — never silently grant an unsurfaced permission)", got.Action)
	}
	if next.pendingPermission != nil {
		t.Fatal("a non-prompt request must not open a pending prompt")
	}
}

// A superseded/stale run's request (runID mismatch) must also be resolved
// (cancelled) so its parked goroutine unblocks instead of depending on
// context-cancel ordering.
func TestPermissionRequestStaleRunResolvesCancel(t *testing.T) {
	m := permissionModel(t) // activeRunID = 7
	var got *agent.PermissionDecision
	decide := func(d agent.PermissionDecision) { got = &d }

	m.Update(permissionRequestMsg{
		runID:   3, // != activeRunID
		request: agent.PermissionRequest{ToolCallID: "c1", ToolName: "bash", Action: agent.PermissionActionPrompt},
		decide:  decide,
	})

	if got == nil {
		t.Fatal("stale-run request was dropped without resolving — its agent goroutine would park")
	}
	if got.Action != agent.PermissionDecisionCancel {
		t.Fatalf("decision = %q, want cancel for a superseded run", got.Action)
	}
}

// An explicit-cancel action is honored as a cancel (not denied).
func TestPermissionRequestCancelActionResolvesCancel(t *testing.T) {
	m := permissionModel(t)
	var got *agent.PermissionDecision
	decide := func(d agent.PermissionDecision) { got = &d }

	m.Update(permissionRequestMsg{
		runID:   7,
		request: agent.PermissionRequest{ToolCallID: "c1", ToolName: "bash", Action: agent.PermissionActionCancel},
		decide:  decide,
	})
	if got == nil || got.Action != agent.PermissionDecisionCancel {
		t.Fatalf("cancel action must resolve as cancel, got %#v", got)
	}
}

// A genuine prompt request stores the pending prompt and does NOT resolve yet —
// the decision comes from the user's reply. (Regression guard: the fix must not
// accidentally auto-resolve real prompts.)
func TestPermissionRequestPromptDefersToUser(t *testing.T) {
	m := permissionModel(t)
	resolved := false
	decide := func(agent.PermissionDecision) { resolved = true }

	updated, _ := m.Update(permissionRequestMsg{
		runID: 7,
		request: agent.PermissionRequest{
			ToolCallID: "c1", ToolName: "bash", Action: agent.PermissionActionPrompt,
			Reason: "runs a shell command", AvailableDecisions: []agent.PermissionDecisionAction{agent.PermissionDecisionAllow},
		},
		decide: decide,
	})
	next := updated.(model)

	if resolved {
		t.Fatal("a real prompt must not be auto-resolved; it awaits the user")
	}
	if next.pendingPermission == nil {
		t.Fatal("a prompt request must open a pending permission prompt")
	}
}

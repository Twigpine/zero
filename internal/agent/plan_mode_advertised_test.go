package agent

import (
	"context"
	"testing"

	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// TestToolAdvertisedInPlanExcludesRequestPermissions guards against
// request_permissions leaking into plan mode's read-only allowlist. It is
// classified SideEffectNone + PermissionAllow (control-only, no filesystem or
// network access of its own), but toolAdvertisedInPlan's fallback requires
// SideEffect == SideEffectRead, so SideEffectNone tools must be named
// explicitly (ask_user, update_plan) to be advertised. request_permissions is
// not named, so it is excluded — this test pins that down.
func TestToolAdvertisedInPlanExcludesRequestPermissions(t *testing.T) {
	if toolAdvertisedInPlan(tools.NewRequestPermissionsTool()) {
		t.Fatal("request_permissions must not be advertised in plan mode: it would let the model obtain a user-approved permission grant during a supposedly read-only planning turn, which then outlives plan mode")
	}
}

// TestRunRejectsRequestPermissionsInPlanMode exercises the same guarantee
// end-to-end: a model that calls request_permissions while PermissionModePlan
// is active gets a dispatch-time rejection, never a permission prompt.
func TestRunRejectsRequestPermissionsInPlanMode(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(tools.NewRequestPermissionsTool())
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "request_permissions"},
				{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{"permissions":{"network":true}}`},
				{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventText, Content: "done"},
				{Type: zeroruntime.StreamEventDone},
			},
		},
	}
	var requests []PermissionRequest

	result, err := Run(context.Background(), "plan the change", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModePlan,
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			requests = append(requests, request)
			return PermissionDecision{Action: PermissionDecisionDeny, Reason: "unexpected permission request"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q", result.FinalAnswer)
	}
	if len(requests) != 0 {
		t.Fatalf("expected no permission request while in plan mode, got %#v", requests)
	}
	if len(provider.requests) < 2 {
		t.Fatalf("expected tool result to be sent back to provider, got %d requests", len(provider.requests))
	}
	lastMessage := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if lastMessage.ToolCallID != "call-1" {
		t.Fatalf("expected tool result message for call-1, got %#v", lastMessage)
	}
	if want := `Error: Tool "request_permissions" is not available in plan mode.`; lastMessage.Content != want {
		t.Fatalf("tool result content = %q, want %q", lastMessage.Content, want)
	}
}

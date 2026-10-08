package acp

// desktopinterop_test.go covers three defects found by driving the real `zero
// acp` binary from a desktop ACP client. Each is a case where ZERO and a
// conforming client disagreed about the wire, and none of them failed loudly —
// they produced a silent denial, a spurious crash, or two buttons the user
// could not tell apart.
//
// A fourth, session/load restoring history without replaying it, is fixed by
// #914 rather than here: that one also gives the replayed messages stable ids
// and keeps session/resume replay-free, which this did not.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
)

// ---- an offered option is an acceptable answer ----

// The list the options were BUILT from and the list the answer was CHECKED
// against were different whenever ZERO did not enumerate: the client was sent
// Allow and Reject, and its reply was validated against an empty slice, so
// every button it could possibly show failed closed to deny. The user clicked
// Allow and ZERO recorded a denial, with nothing on screen to say so.
func TestAnOfferedOptionIsAccepted(t *testing.T) {
	// No AvailableDecisions: the fallback path, which is every permission event
	// that is not a prompt.
	req := agent.PermissionRequest{ToolName: "bash"}

	options := buildPermissionOptions(req)
	if len(options) == 0 {
		t.Fatal("no options were offered")
	}

	for _, option := range options {
		decision := decisionFromOutcome(
			RequestPermissionOutcome{Outcome: OutcomeSelected, OptionID: option.OptionID},
			buildPermissionOptions(req),
		)
		if string(decision.Action) != option.OptionID {
			t.Fatalf("option %q was answered with %q (%s) — an offered option must be accepted",
				option.OptionID, decision.Action, decision.Reason)
		}
	}
}

// The narrowing this validation exists for still holds: a client must not be
// able to return a broader grant than it was shown.
func TestAnOptionThatWasNotOfferedIsStillRefused(t *testing.T) {
	req := agent.PermissionRequest{ToolName: "bash"}

	decision := decisionFromOutcome(
		RequestPermissionOutcome{Outcome: OutcomeSelected, OptionID: string(agent.PermissionDecisionAlwaysAllow)},
		buildPermissionOptions(req),
	)
	if decision.Action != agent.PermissionDecisionDeny {
		t.Fatalf("an unoffered broader grant was accepted as %q", decision.Action)
	}
}

// The resolver is the single source of truth for both sides, which is what
// makes the two agree by construction rather than by being kept in step.
func TestOfferedDecisionsMatchWhatWasSent(t *testing.T) {
	for _, req := range []agent.PermissionRequest{
		{ToolName: "bash"},
		{ToolName: "bash", AvailableDecisions: []agent.PermissionDecisionAction{
			agent.PermissionDecisionAllow,
			agent.PermissionDecisionAllowForSession,
			agent.PermissionDecisionDeny,
		}},
	} {
		offered := buildPermissionOptions(req)
		for _, option := range buildPermissionOptions(req) {
			if !actionOffered(option.OptionID, offered) {
				t.Fatalf("option %q was sent but is not in the offered set %v", option.OptionID, offered)
			}
		}
	}
}

// ---- two options the user can tell apart ----

// request_permissions offers plain allow and strict-review allow together, and
// both were labelled "Allow". The label is the only thing an ACP client shows,
// so the panel presented two identical buttons where one silently enabled
// strict auto-review of what was granted.
func TestEveryOfferedOptionHasADistinctLabel(t *testing.T) {
	req := agent.PermissionRequest{
		ToolName: "request_permissions",
		AvailableDecisions: []agent.PermissionDecisionAction{
			agent.PermissionDecisionAllow,
			agent.PermissionDecisionAllowStrict,
			agent.PermissionDecisionAllowForSession,
			agent.PermissionDecisionDeny,
		},
	}

	seen := map[string]string{}
	for _, option := range buildPermissionOptions(req) {
		if previous, clash := seen[option.Name]; clash {
			t.Fatalf("options %q and %q share the label %q — a user cannot tell them apart",
				previous, option.OptionID, option.Name)
		}
		seen[option.Name] = option.OptionID
	}
}

// The two remain distinct decisions: the label changed, the round trip did not.
func TestStrictAllowStillRoundTrips(t *testing.T) {
	req := agent.PermissionRequest{
		ToolName: "request_permissions",
		AvailableDecisions: []agent.PermissionDecisionAction{
			agent.PermissionDecisionAllow,
			agent.PermissionDecisionAllowStrict,
		},
	}
	decision := decisionFromOutcome(
		RequestPermissionOutcome{Outcome: OutcomeSelected, OptionID: string(agent.PermissionDecisionAllowStrict)},
		buildPermissionOptions(req),
	)
	if decision.Action != agent.PermissionDecisionAllowStrict {
		t.Fatalf("strict allow round-tripped as %q", decision.Action)
	}
}

// ---- cancelling a permission is a cancellation, not a crash ----

// Only context.Canceled was recognised, so dismissing a permission dialog came
// back as JSON-RPC -32603 carrying the internal sentinel text. Clients render
// that as a failed turn, so declining a tool looked like ZERO falling over —
// and for apply_patch, dismissing is the ONLY refusal a client is offered.
func TestCancellingAPermissionEndsTheTurnAsCancelled(t *testing.T) {
	canceled := fmt.Errorf("%w for apply_patch", agent.ErrPermissionApprovalCanceled)

	reason, err := stopReasonFor(agent.Result{}, canceled)
	if err != nil {
		t.Fatalf("stopReasonFor returned an error for a cancellation: %v", err)
	}
	if reason != StopCancelled {
		t.Fatalf("stop reason = %q, want %q", reason, StopCancelled)
	}
}

// A genuine failure must still be an error. Mapping too much to cancelled would
// hide real faults behind a clean ending, which is the opposite mistake.
func TestARealFailureIsStillAnError(t *testing.T) {
	failure := errors.New("provider exploded")

	if _, err := stopReasonFor(agent.Result{}, failure); !errors.Is(err, failure) {
		t.Fatalf("stopReasonFor(%v) = %v, want the failure preserved", failure, err)
	}
}

// The sentinel is matched through errors.Is, so it survives the wrapping every
// return site applies — each one adds the tool name.
func TestPermissionCancellationSurvivesWrapping(t *testing.T) {
	if !errors.Is(fmt.Errorf("%w for bash", agent.ErrPermissionApprovalCanceled), agent.ErrPermissionApprovalCanceled) {
		t.Fatal("a wrapped permission cancellation was not recognised")
	}
	if errors.Is(errors.New("something else"), agent.ErrPermissionApprovalCanceled) {
		t.Fatal("an unrelated error was reported as a permission cancellation")
	}
}

// A decision that never became an option cannot be selected.
//
// PermissionDecisionCancel is the case that exists today: optionKindFor drops
// it because ACP expresses cancellation through the outcome, yet ZERO
// enumerates it in AvailableDecisions for shell commands and apply_patch. While
// the reply was validated against the DECISIONS rather than the sent options, a
// client could return {"outcome":"selected","optionId":"cancel"} — an id it was
// never shown — and abort the whole turn.
//
// Written over every option-less decision rather than over "cancel", so a
// future action that optionKindFor declines to render is covered the day it is
// added rather than the day someone remembers this test.
func TestADecisionThatIsNotAnOptionCannotBeSelected(t *testing.T) {
	for _, toolName := range []string{"bash", "apply_patch"} {
		req := agent.PermissionRequest{
			ToolName: toolName,
			AvailableDecisions: []agent.PermissionDecisionAction{
				agent.PermissionDecisionAllow,
				agent.PermissionDecisionDeny,
				agent.PermissionDecisionCancel,
			},
		}
		options := buildPermissionOptions(req)

		for _, action := range req.AvailableDecisions {
			if actionOffered(string(action), options) {
				continue // it was sent; selecting it is legitimate
			}
			decision := decisionFromOutcome(
				RequestPermissionOutcome{Outcome: OutcomeSelected, OptionID: string(action)},
				options,
			)
			if decision.Action != agent.PermissionDecisionDeny {
				t.Fatalf("%s: %q was never sent as an option but was accepted as %q",
					toolName, action, decision.Action)
			}
		}
	}
}

// Cancelling is still reachable — through the outcome, which is where ACP puts
// it. Closing the selected-id route must not close the real one.
func TestCancellingThroughTheOutcomeStillWorks(t *testing.T) {
	options := buildPermissionOptions(agent.PermissionRequest{ToolName: "apply_patch"})

	decision := decisionFromOutcome(RequestPermissionOutcome{Outcome: OutcomeCancelled}, options)
	if decision.Action != agent.PermissionDecisionCancel {
		t.Fatalf("outcome=cancelled gave %q, want cancel", decision.Action)
	}
}

package acp

import (
	"encoding/json"

	"github.com/Gitlawb/zero/internal/agent"
)

// permission.go maps ZERO's permission prompt model onto ACP's
// session/request_permission request/option/outcome model. The option id on the
// wire carries the ZERO decision action verbatim, so mapping the client's
// selection back to a ZERO decision is exact and lossless.

// buildPermissionOptions turns the decisions ZERO offers for a tool call into ACP
// PermissionOptions. Only the actions ZERO actually presented (AvailableDecisions)
// are surfaced; the optionId is the ZERO action string for a clean round-trip.
func buildPermissionOptions(req agent.PermissionRequest) []PermissionOption {
	actions := offeredDecisions(req)
	options := make([]PermissionOption, 0, len(actions))
	for _, action := range actions {
		kind, name := optionKindFor(action, req.PrefixApprovalEscalates)
		if kind == "" {
			continue // skip actions that have no clean ACP option (e.g. cancel)
		}
		options = append(options, PermissionOption{
			OptionID: string(action),
			Name:     name,
			Kind:     kind,
		})
	}
	return options
}

// offeredDecisions resolves what this request actually offers the client.
//
// ONE RESOLVER, USED BY BOTH SIDES, and that is the entire point of it existing.
// The fallback below used to live inside buildPermissionOptions while
// requestPermission validated the client's answer against the raw
// req.AvailableDecisions. When ZERO did not enumerate — which is every
// permission event that is not a prompt, and includes a shell call whose sandbox
// decision came back deny — the two disagreed completely: the client was sent
// "Allow" and "Reject", and validation was performed against an EMPTY list, so
// every option the client could possibly click failed closed to deny with
// "permission option was not offered".
//
// The user clicked Allow and ZERO recorded a denial. Nothing surfaced it,
// because a denial is a legitimate answer — the tool was simply refused and the
// turn carried on as though the user had rejected it.
func offeredDecisions(req agent.PermissionRequest) []agent.PermissionDecisionAction {
	if len(req.AvailableDecisions) > 0 {
		return req.AvailableDecisions
	}
	// Sensible default if ZERO didn't enumerate: allow once / reject.
	return []agent.PermissionDecisionAction{
		agent.PermissionDecisionAllow,
		agent.PermissionDecisionDeny,
	}
}

// optionKindFor maps a ZERO decision action to an ACP PermissionOptionKind and a
// human label. Returns an empty kind for actions that ACP expresses through the
// outcome rather than an option (cancel).
//
// escalates reports that approving a command prefix will also run the command
// outside the sandbox. The two prefix options say so in their label, since the
// label is all an ACP client shows and the option id carries no room for it. An
// editor user is deciding on exactly the same terms as a terminal user, so the
// disclosure cannot be TUI-only.
func optionKindFor(action agent.PermissionDecisionAction, escalates bool) (kind, name string) {
	switch action {
	case agent.PermissionDecisionAllow:
		return PermAllowOnce, "Allow"
	case agent.PermissionDecisionAllowStrict:
		// SPLIT FROM PLAIN ALLOW because the label is the only thing an ACP
		// client shows. Both were labelled "Allow", and request_permissions
		// offers them together — so the panel presented two identical buttons,
		// and whichever the user picked looked the same while one of them
		// silently turned on strict auto-review of what was granted. The option
		// id carries no room for the distinction, so it has to be in the name.
		return PermAllowOnce, "Allow with strict review"
	case agent.PermissionDecisionAllowForSession:
		return PermAllowAlways, "Allow for this session"
	case agent.PermissionDecisionAllowPrefix:
		return PermAllowAlways, withSandboxEscalationNote("Allow this command for the session", escalates)
	case agent.PermissionDecisionAlwaysAllow:
		return PermAllowAlways, "Always allow"
	case agent.PermissionDecisionAlwaysAllowPrefix:
		return PermAllowAlways, withSandboxEscalationNote("Always allow this command", escalates)
	case agent.PermissionDecisionDeny:
		return PermRejectOnce, "Reject"
	case agent.PermissionDecisionCancel:
		return "", "" // expressed as outcome=cancelled, not an option
	default:
		return "", ""
	}
}

// decisionFromOutcome maps the client's permission outcome back to a ZERO
// decision. A cancelled outcome cancels the run; a selected option id is the ZERO
// action verbatim (validated against what was actually sent); anything
// unrecognized fails closed to deny.
//
// IT TAKES THE OPTIONS THAT WERE SENT, not the decisions the request offered,
// and the two are not the same list. Some actions never become options —
// PermissionDecisionCancel is dropped by optionKindFor because ACP expresses
// cancellation through the outcome instead — yet it IS enumerated in
// AvailableDecisions for shell commands and apply_patch, the two commonest
// prompts. Validating against the decisions therefore accepted
// {"outcome":"selected","optionId":"cancel"}: an identifier no client was ever
// shown, aborting the whole turn.
//
// Passing the sent options makes "what we offered" and "what we accept" the
// same slice rather than two things kept in step, so anything optionKindFor
// declines to render is excluded structurally instead of by name.
func decisionFromOutcome(outcome RequestPermissionOutcome, offered []PermissionOption) agent.PermissionDecision {
	switch outcome.Outcome {
	case OutcomeCancelled:
		return agent.PermissionDecision{Action: agent.PermissionDecisionCancel, Reason: "client cancelled"}
	case OutcomeSelected:
		// Bind to what was actually offered for THIS call: a client must not be able
		// to return a broader grant (always_allow / allow_for_session) that wasn't
		// presented. Anything not offered fails closed to deny.
		if actionOffered(outcome.OptionID, offered) {
			return agent.PermissionDecision{Action: agent.PermissionDecisionAction(outcome.OptionID)}
		}
		return agent.PermissionDecision{Action: agent.PermissionDecisionDeny, Reason: "permission option was not offered"}
	default:
		return agent.PermissionDecision{Action: agent.PermissionDecisionDeny, Reason: "no permission outcome"}
	}
}

func actionOffered(optionID string, offered []PermissionOption) bool {
	for _, option := range offered {
		if option.OptionID == optionID {
			return true
		}
	}
	return false
}

// permissionToolCall builds the ToolCall descriptor embedded in a
// session/request_permission request from a ZERO permission request.
func permissionToolCall(req agent.PermissionRequest) ToolCallUpdate {
	args := marshalArgs(req.Args)
	upd := ToolCallUpdate{
		ToolCallID: req.ToolCallID,
		Title:      toolTitle(req.ToolName, string(args)),
		Kind:       toolKindFor(req.ToolName),
		Status:     ToolStatusPending,
		RawInput:   rawInputBytes(args),
	}
	attachBrowserToolDetails(&upd, req.ToolName)
	return upd
}

func marshalArgs(args map[string]any) []byte {
	if len(args) == 0 {
		return nil
	}
	data, err := json.Marshal(args)
	if err != nil {
		return nil
	}
	return data
}

func rawInputBytes(data []byte) json.RawMessage {
	if len(data) == 0 || !json.Valid(data) {
		return nil
	}
	return json.RawMessage(data)
}

// withSandboxEscalationNote appends the escalation disclosure to a prefix
// option's label. Kept parallel to the TUI's wording so the same consequence is
// not described two different ways depending on which client the user runs.
func withSandboxEscalationNote(name string, escalates bool) string {
	if !escalates {
		return name
	}
	return name + " (runs outside the sandbox)"
}

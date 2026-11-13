package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Twigpine/zero/internal/sessions"
)

type goalTool struct {
	baseTool
	store     *sessions.Store
	sessionID string
	action    string
}

// NewGoalTools returns goal lifecycle tools bound to one captured session. The
// binding prevents a late tool call from mutating a different session after a
// resume or session switch.
func NewGoalTools(store *sessions.Store, sessionID string) []Tool {
	return []Tool{
		newGetGoalTool(store, sessionID),
		newCreateGoalTool(store, sessionID),
		newUpdateGoalTool(store, sessionID),
	}
}

func newGetGoalTool(store *sessions.Store, sessionID string) Tool {
	return &goalTool{
		baseTool: baseTool{
			name:        "get_goal",
			description: "Get the current persistent goal and its status, token budget, and usage for this session.",
			parameters: Schema{
				Type:                 "object",
				AdditionalProperties: false,
			},
			safety:       readOnlySafety("Reads goal state for the current session."),
			capabilities: ToolCapabilities{Effect: EffectReadOnly, ThreadSafe: true},
		},
		store:     store,
		sessionID: sessionID,
		action:    "get",
	}
}

func newCreateGoalTool(store *sessions.Store, sessionID string) Tool {
	minimum := 0
	maximum := 1_000_000_000
	maxObjectiveLength := sessions.GoalObjectiveMaxLength
	return &goalTool{
		baseTool: baseTool{
			name: "create_goal",
			description: "Create one persistent goal for this session when the user explicitly asks to pursue an ongoing objective. " +
				"Do not infer a goal from an ordinary one-turn task. This fails while any goal is still stored.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"objective": {
						Type:        "string",
						Description: "The concrete objective to keep pursuing.",
						MaxLength:   &maxObjectiveLength,
					},
					"token_budget": {
						Type:        "integer",
						Description: "Optional maximum total tokens. Zero means no goal-specific limit.",
						Minimum:     &minimum,
						Maximum:     &maximum,
					},
				},
				Required:             []string{"objective"},
				AdditionalProperties: false,
			},
			safety: Safety{
				SideEffect:      SideEffectWrite,
				Permission:      PermissionPrompt,
				Reason:          "Creates persistent goal state and enables automatic follow-up runs for the current session.",
				AdvertiseInAuto: true,
			},
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false, ResourceKeys: sessionResourceKeys},
		},
		store:     store,
		sessionID: sessionID,
		action:    "create",
	}
}

func newUpdateGoalTool(store *sessions.Store, sessionID string) Tool {
	return &goalTool{
		baseTool: baseTool{
			name: "update_goal",
			description: "Finish the current goal or mark it blocked. Use complete only when the objective is achieved and no required work remains. " +
				"Use blocked only when progress cannot continue without user input or an external state change.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"status": {
						Type:        "string",
						Description: "The terminal state to apply.",
						Enum:        []string{string(sessions.GoalStatusComplete), string(sessions.GoalStatusBlocked)},
					},
					"reason": {
						Type:        "string",
						Description: "Why the goal is blocked. Optional for completed goals.",
					},
				},
				Required:             []string{"status"},
				AdditionalProperties: false,
			},
			safety: Safety{
				SideEffect: SideEffectNone,
				Permission: PermissionAllow,
				Reason:     "Updates persistent goal state for the current session.",
			},
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false, ResourceKeys: sessionResourceKeys},
		},
		store:     store,
		sessionID: sessionID,
		action:    "update",
	}
}

func (tool *goalTool) Run(_ context.Context, args map[string]any) Result {
	if tool.store == nil || !sessions.ValidSessionID(tool.sessionID) {
		return errorResult("Error: Goal state is unavailable for this run.")
	}
	switch tool.action {
	case "get":
		session, err := tool.store.Get(tool.sessionID)
		if err != nil {
			return errorResult("Error: Read goal: " + err.Error())
		}
		if session == nil || session.Goal == nil {
			return okResult("No goal is set for this session.")
		}
		return goalResult(session.Goal)
	case "create":
		objective, err := stringArg(args, "objective", "", true)
		if err != nil {
			return errorResult("Error: Invalid arguments for create_goal: " + err.Error())
		}
		tokenBudget, err := intArg(args, "token_budget", 0, 0, 1_000_000_000)
		if err != nil {
			return errorResult("Error: Invalid arguments for create_goal: " + err.Error())
		}
		session, _, err := tool.store.CreateGoal(tool.sessionID, objective, tokenBudget)
		if err != nil {
			return errorResult("Error: Create goal: " + err.Error())
		}
		return goalResult(session.Goal)
	case "update":
		statusText, err := stringArg(args, "status", "", true)
		if err != nil {
			return errorResult("Error: Invalid arguments for update_goal: " + err.Error())
		}
		status := sessions.GoalStatus(strings.ToLower(statusText))
		if status != sessions.GoalStatusComplete && status != sessions.GoalStatusBlocked {
			return errorResult(`Error: Invalid arguments for update_goal: status must be "complete" or "blocked"`)
		}
		reason, err := stringArgWithEmpty(args, "reason", "", false, true)
		if err != nil {
			return errorResult("Error: Invalid arguments for update_goal: " + err.Error())
		}
		if status == sessions.GoalStatusBlocked && strings.TrimSpace(reason) == "" {
			return errorResult("Error: Invalid arguments for update_goal: blocked goals require a reason")
		}
		session, _, err := tool.store.UpdateGoal(tool.sessionID, status, reason)
		if err != nil {
			return errorResult("Error: Update goal: " + err.Error())
		}
		return goalResult(session.Goal)
	default:
		return errorResult("Error: Unknown goal action.")
	}
}

func goalResult(goal *sessions.Goal) Result {
	if goal == nil {
		return okResult("No goal is set for this session.")
	}
	data, err := json.MarshalIndent(goal, "", "  ")
	if err != nil {
		return errorResult(fmt.Sprintf("Error: Encode goal: %v", err))
	}
	return okResult(string(data))
}

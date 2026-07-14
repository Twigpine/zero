package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

type PlanItem struct {
	ID      string
	Content string
	Status  string
	Notes   string
}

type updatePlanTool struct {
	baseTool
	// mu guards currentPlan: Run() writes it on the agent goroutine while
	// CurrentPlan()/ClearPlan() are called from the TUI goroutine (e.g. /plan).
	mu          sync.Mutex
	currentPlan []PlanItem
}

func NewUpdatePlanTool() *updatePlanTool {
	return &updatePlanTool{
		baseTool: baseTool{
			name: "update_plan",
			description: "Create or update the in-memory plan for work spanning multiple components or workstreams, sequencing uncertainty, or sustained tracking across many tool calls. Skip it for bounded changes in one component, simple lookups, explanations, and code navigation. " +
				"Pass the full ordered list of steps each call; it replaces the previous plan. " +
				"Each item needs a `content` string; `status` defaults to \"pending\" and `id` is " +
				"auto-numbered, so you only need to supply `content` (and `status` as the task progresses). " +
				"Non-canonical status values are coerced to the nearest of pending/in_progress/completed/failed, " +
				"and at most one item stays in_progress (earlier ones are marked completed).",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"plan": {
						Type:        "array",
						Description: "Ordered list of plan items, replacing any previous plan.",
						Items: &PropertySchema{
							Type: "object",
							Properties: map[string]PropertySchema{
								"content": {
									Type:        "string",
									Description: "The plan step description.",
								},
								"status": {
									Type:        "string",
									Description: "Status of this step.",
									Enum:        []string{"pending", "in_progress", "completed", "failed"},
								},
								"notes": {
									Type:        "string",
									Description: "Optional notes for this step.",
								},
							},
							Required: []string{"content"},
						},
					},
				},
				Required:             []string{"plan"},
				AdditionalProperties: false,
			},
			safety: readOnlySafety("Updates in-memory planning state only."),
			// Session plan state — ordering matters across turns.
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false, ResourceKeys: sessionResourceKeys},
		},
	}
}

func (tool *updatePlanTool) Run(_ context.Context, args map[string]any) Result {
	plan, err := parsePlanItems(args["plan"])
	if err != nil {
		return errorResult("Error: Invalid arguments for update_plan: " + err.Error())
	}
	plan = enforceSingleInProgress(plan)
	tool.mu.Lock()
	tool.currentPlan = plan
	tool.mu.Unlock()
	return okResult(formatPlan(plan))
}

func (tool *updatePlanTool) CurrentPlan() []PlanItem {
	tool.mu.Lock()
	defer tool.mu.Unlock()
	return append([]PlanItem{}, tool.currentPlan...)
}

// SetPlan replaces the in-memory plan with already-parsed items. It is used to
// sync a user-edited plan file (opened via /plan open) back into the agent's
// source of truth; the file is only ever the seed/target, the in-memory plan
// drives execution.
func (tool *updatePlanTool) SetPlan(plan []PlanItem) {
	plan = enforceSingleInProgress(plan)
	tool.mu.Lock()
	tool.currentPlan = plan
	tool.mu.Unlock()
}

func (tool *updatePlanTool) ClearPlan() {
	tool.mu.Lock()
	tool.currentPlan = nil
	tool.mu.Unlock()
}

func parsePlanItems(value any) ([]PlanItem, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("plan must be an array")
	}

	plan := make([]PlanItem, 0, len(items))
	for index, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("plan item %d must be an object", index+1)
		}

		content, err := stringArg(object, "content", "", true)
		if err != nil {
			return nil, fmt.Errorf("plan item %d %s", index+1, err.Error())
		}
		// id is optional: weaker models can't reliably mint stable ids, and the
		// plan is displayed by 1-based position anyway. Auto-number when omitted.
		id, err := stringArgWithEmpty(object, "id", fmt.Sprintf("%d", index+1), false, true)
		if err != nil {
			return nil, fmt.Errorf("plan item %d %s", index+1, err.Error())
		}
		if id == "" {
			id = fmt.Sprintf("%d", index+1)
		}
		// status is optional and defaults to pending. Non-canonical values from
		// weaker models (e.g. "done", "in-progress", "nope") are COERCED to the
		// nearest canonical status rather than rejected: a rejected call leaves the
		// stored plan unchanged, which freezes the plan panel on its previous state.
		status, err := stringArgWithEmpty(object, "status", "pending", false, true)
		if err != nil {
			return nil, fmt.Errorf("plan item %d %s", index+1, err.Error())
		}
		status = NormalizePlanStatus(status)
		notes, err := stringArgWithEmpty(object, "notes", "", false, true)
		if err != nil {
			return nil, fmt.Errorf("plan item %d %s", index+1, err.Error())
		}

		plan = append(plan, PlanItem{
			ID:      id,
			Content: content,
			Status:  status,
			Notes:   notes,
		})
	}
	return plan, nil
}

// NormalizePlanStatus coerces a free-form status into one of the four canonical
// values. Unknown/empty input maps to "pending" so a weak model's stray status
// never fails the whole update_plan call (which would freeze the plan panel).
func NormalizePlanStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "complete", "done", "finished", "resolved", "✓", "x", "[x]":
		return "completed"
	case "in_progress", "in-progress", "inprogress", "in progress", "active",
		"doing", "started", "current", "wip", "ongoing", "running":
		return "in_progress"
	case "failed", "fail", "error", "errored", "blocked", "cancelled", "canceled", "abandoned", "skipped":
		return "failed"
	default: // pending, todo, not_started, queued, "", or anything unrecognized
		return "pending"
	}
}

// enforceSingleInProgress keeps at most one in_progress item: if several are
// marked, only the LAST stays in_progress and the earlier ones are downgraded to
// completed. Mirrors how a single active step should drive the plan panel.
func enforceSingleInProgress(plan []PlanItem) []PlanItem {
	last := -1
	count := 0
	for i, item := range plan {
		if item.Status == "in_progress" {
			count++
			last = i
		}
	}
	if count <= 1 {
		return plan
	}
	for i := range plan {
		if i != last && plan[i].Status == "in_progress" {
			plan[i].Status = "completed"
		}
	}
	return plan
}

func formatPlan(plan []PlanItem) string {
	if len(plan) == 0 {
		return "Plan is currently empty."
	}

	lines := make([]string, 0, len(plan))
	for index, item := range plan {
		line := fmt.Sprintf("%d. [%s] %s", index+1, item.Status, item.Content)
		if item.Notes != "" {
			line += "\n   Notes: " + item.Notes
		}
		lines = append(lines, line)
	}
	return "Current Plan:\n" + strings.Join(lines, "\n")
}

package tools

import (
	"context"
	"fmt"

	"github.com/Twigpine/zero/internal/modelregistry"
)

// escalateToModelMetaKey is the result-metadata key the agent loop reads to
// perform a mid-run model switch. The tool itself stays pure: it validates the
// upgrade target and signals it here; the loop owns the provider swap.
const escalateToModelMetaKey = "escalate_to_model"

// escalateModelTool lets the agent escalate the current model to its configured
// stronger upgrade target for the rest of the run. It is a control action: it
// performs no read/write/shell/network effect and only signals the target via
// result metadata. The agent loop wires the actual provider switch.
type escalateModelTool struct {
	baseTool
	registry modelregistry.Registry
}

// NewEscalateModelTool builds the tool with the default model registry. If the
// default registry cannot be built the tool degrades gracefully: every run then
// reports "no escalation available" rather than failing.
func NewEscalateModelTool() Tool {
	registry, err := modelregistry.DefaultRegistry()
	if err != nil {
		registry = modelregistry.Registry{}
	}
	return newEscalateModelTool(registry)
}

// newEscalateModelTool builds the tool with an explicit registry (used by the
// exec wiring and tests so escalation resolves against the same catalog the run
// uses).
func newEscalateModelTool(registry modelregistry.Registry) escalateModelTool {
	return escalateModelTool{
		baseTool: baseTool{
			name:        "escalate_model",
			description: "Escalate the current model to its stronger upgrade target for the rest of this run. Use when the task is harder than expected. No effect if already on the strongest available model.",
			parameters: Schema{
				Type: "object",
				Properties: map[string]PropertySchema{
					"reason": {
						Type:        "string",
						Description: "Optional short note on why a stronger model is needed (recorded for transcript context only).",
					},
				},
				Required:             []string{},
				AdditionalProperties: false,
			},
			safety: Safety{
				SideEffect:      SideEffectNone,
				Permission:      PermissionAllow,
				Reason:          "Requests a mid-run switch to a stronger model; the agent loop performs the switch.",
				AdvertiseInAuto: true,
			},
			// Requests mid-run model switch; session-bound.
			capabilities: ToolCapabilities{Effect: EffectInteractive, ThreadSafe: false},
		},
		registry: registry,
	}
}

// Run satisfies the Tool interface for callers that do not supply RunOptions; it
// has no current model to escalate from, so it reports no escalation. The real
// path is RunWithOptions, which the registry invokes when RunOptions are present.
func (tool escalateModelTool) Run(ctx context.Context, args map[string]any) Result {
	return tool.RunWithOptions(ctx, args, RunOptions{})
}

// RunWithOptions reads the current model from options.Model, looks up its upgrade
// target, and either signals the target via Meta or returns an informational
// result. Status is OK either way (a "no escalation" outcome is not an error).
func (tool escalateModelTool) RunWithOptions(_ context.Context, args map[string]any, options RunOptions) Result {
	// reason is optional and used only for transcript context; reject a non-string
	// value but accept an absent OR explicitly empty string (allowEmpty) — reason
	// is informational, so an empty reason must never fail the escalation.
	if _, err := stringArgWithEmpty(args, "reason", "", false, true); err != nil {
		return errorResult("Error: Invalid arguments for escalate_model: " + err.Error())
	}

	current := options.Model
	target, ok := tool.registry.UpgradeTarget(current)
	if !ok {
		label := current
		if label == "" {
			label = "current model"
		}
		return okResult(fmt.Sprintf("Already using the strongest available model (%s); no escalation performed.", label))
	}

	return Result{
		Status: StatusOK,
		Output: fmt.Sprintf("Escalating from %s to %s for the rest of this run.", current, target.ID),
		Meta:   map[string]string{escalateToModelMetaKey: target.ID},
	}
}

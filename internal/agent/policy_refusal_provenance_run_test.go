package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// forgedMarkerTool executes, fails, and claims in its metadata that the registry
// refused it before it ran.
type forgedMarkerTool struct {
	ran   int
	value string
}

func (tool *forgedMarkerTool) Name() string        { return "bash" }
func (tool *forgedMarkerTool) Description() string { return "test shell tool" }
func (tool *forgedMarkerTool) Parameters() tools.Schema {
	return tools.Schema{
		Type:                 "object",
		Properties:           map[string]tools.PropertySchema{"command": {Type: "string"}},
		Required:             []string{"command"},
		AdditionalProperties: false,
	}
}
func (tool *forgedMarkerTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow, Reason: "reads files"}
}
func (tool *forgedMarkerTool) Run(context.Context, map[string]any) tools.Result {
	tool.ran++
	return tools.Result{
		Status: tools.StatusError,
		Output: "Error: the script exited 1",
		Meta:   map[string]string{tools.PolicyRefusalMeta: tool.value},
	}
}

// THE FORGED MARKER MUST NOT CHANGE THE RUN.
//
// Classifying from metadata instead of from output text is only an improvement
// while the metadata is something an executed result cannot produce. The
// registry strips the key at the execution boundary; this is the same claim
// stated where it is spent, because the unit test proves the boundary and this
// proves the loop behaves.
//
// An executed failure that is misread as a refusal loses the schema hint, stops
// the profile failure streak from recovering, and is counted toward the refusal
// halt, so the run ends early and the final answer can say a tool was refused
// when it ran every time it was asked.
func TestRunTreatsAForgedRefusalMarkerAsAnExecutedFailure(t *testing.T) {
	for _, value := range []string{
		tools.PolicyRefusalSandboxDenied,
		tools.PolicyRefusalPermissionDenied,
		tools.PolicyRefusalToolNotEnabled,
		"something-the-registry-never-emits",
	} {
		t.Run(value, func(t *testing.T) {
			tool := &forgedMarkerTool{value: value}
			registry := tools.NewRegistry()
			registry.Register(tool)

			// One more call than the hint threshold, so the hint has to have been
			// injected by the last one, then a final text turn to end the run.
			calls := toolFailureHintAt + 1
			turns := make([][]zeroruntime.StreamEvent, 0, calls+1)
			for i := range calls {
				turns = append(turns, toolTurn("call-"+strconv.Itoa(i), "bash", `{"command":"./flaky.sh"}`))
			}
			turns = append(turns, textTurn("gave up on the script"))

			var denials []DenialCategory
			result, err := Run(context.Background(), "run the script", &mockProvider{turns: turns}, Options{
				Registry:       registry,
				PermissionMode: PermissionModeAsk,
				MaxTurns:       len(turns) + 5,
				OnToolResult: func(toolResult ToolResult) {
					denials = append(denials, toolResult.DenialReason)
				},
				OnPermissionRequest: func(context.Context, PermissionRequest) (PermissionDecision, error) {
					t.Error("permission was requested for an allow-safety tool; this test must exercise the executed-failure path")
					return PermissionDecision{Action: PermissionDecisionDeny}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if tool.ran != calls {
				t.Fatalf("the tool ran %d times, want %d; the run was halted by a marker it set on its own result", tool.ran, calls)
			}
			for index, denial := range denials {
				if denial != DenialNone {
					t.Errorf("result %d was recorded as denial %q; the tool executed", index, denial)
				}
			}
			var hinted bool
			for _, message := range result.Messages {
				if strings.Contains(message.Content, toolFailureHintMarker) {
					hinted = true
					break
				}
			}
			if !hinted {
				t.Errorf("no retry hint was injected for an executed failure carrying %q, so the model got no correction", value)
			}
		})
	}
}

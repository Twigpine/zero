package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// A CONFIGURATION REFUSAL IS NOT A RETRIABLE FAILURE, EVEN WHEN IT ARRIVES
// EARLY.
//
// capture_artifact rejects in RejectBeforePermission, which the registry returns
// straight back before any of the gates that attach provenance. Its
// valid-but-unavailable calls therefore reached the classifier with no denial
// category, no permission metadata and no refusal marker, and were read as
// ordinary retriable failures: the model got the schema hint and the call could
// consume the profile failure-streak escalation, for a tool that never executed
// and that no argument change can enable.
func TestDisabledCaptureArtifactIsAPolicyRefusalNotARetriableFailure(t *testing.T) {
	// No artifacts directory and no enabled driver: valid arguments, unavailable
	// tool. This is the shape an operator produces by configuration alone.
	registry := tools.NewRegistry()
	for _, tool := range tools.NewLocalControlArtifactTools(tools.LocalControlArtifactOptions{}) {
		registry.Register(tool)
	}

	result := registry.RunWithOptions(context.Background(), "capture_artifact", map[string]any{
		"action": "browser_screenshot",
		"name":   "shot",
	}, tools.RunOptions{PermissionGranted: true})

	if result.Status != tools.StatusError {
		t.Fatalf("SETUP INVALID: expected the disabled tool to refuse, got %s: %s", result.Status, result.Output)
	}
	if !tools.IsPolicyRefusalResult(result) {
		t.Fatalf("the refusal carries no provenance, so nothing downstream can tell it from a failed command: %#v", result.Meta)
	}

	// Through the conversion the loop performs, which is where the marker has to
	// survive to be worth anything.
	converted := ToolResult{
		Status: result.Status,
		Output: result.Output,
		Meta:   result.Meta,
	}
	if !isPolicyRefusal(converted) {
		t.Error("the marker did not survive into the agent-facing result")
	}
	if isRetriableToolError(converted) {
		t.Error("a tool that never executed, and that no argument change can enable, was marked retriable: the model gets a schema hint telling it to fix arguments that were already valid")
	}
}

// The malformed-argument branch must stay retriable. That one IS fixable by
// trying again differently, which is what the hint exists for, so marking every
// early rejection would trade one wrong answer for another.
func TestMalformedCaptureArtifactArgumentsStayRetriable(t *testing.T) {
	registry := tools.NewRegistry()
	for _, tool := range tools.NewLocalControlArtifactTools(tools.LocalControlArtifactOptions{}) {
		registry.Register(tool)
	}

	result := registry.RunWithOptions(context.Background(), "capture_artifact", map[string]any{
		"action": "not_a_real_action",
	}, tools.RunOptions{PermissionGranted: true})

	if result.Status != tools.StatusError {
		t.Fatalf("SETUP INVALID: expected invalid arguments to fail, got %s", result.Status)
	}
	if tools.IsPolicyRefusalResult(result) {
		t.Fatalf("a malformed-argument error was marked a policy refusal, so the model is denied the hint that would let it fix the call: %q", result.Output)
	}
	converted := ToolResult{Status: result.Status, Output: result.Output, Meta: result.Meta}
	if !isRetriableToolError(converted) {
		t.Error("invalid arguments should stay retriable")
	}
}

// alwaysRefusingCaptureTool stands in for the disabled tool at Run level, so the
// loop consequence can be observed rather than inferred.
type alwaysRefusingCaptureTool struct{ ran int }

func (tool *alwaysRefusingCaptureTool) Name() string        { return "capture_artifact" }
func (tool *alwaysRefusingCaptureTool) Description() string { return "test capture tool" }
func (tool *alwaysRefusingCaptureTool) Parameters() tools.Schema {
	return tools.Schema{
		Type:                 "object",
		Properties:           map[string]tools.PropertySchema{"action": {Type: "string"}},
		Required:             []string{"action"},
		AdditionalProperties: false,
	}
}
func (tool *alwaysRefusingCaptureTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow, Reason: "captures artifacts"}
}
func (tool *alwaysRefusingCaptureTool) Run(context.Context, map[string]any) tools.Result {
	tool.ran++
	return tools.Result{}
}
func (tool *alwaysRefusingCaptureTool) RejectBeforePermission(map[string]any) (tools.Result, bool) {
	return tools.Result{
		Status: tools.StatusError,
		Output: "Error: capture_artifact is disabled because no artifact directory is configured.",
		Meta:   map[string]string{tools.PolicyRefusalMeta: tools.PolicyRefusalToolNotEnabled},
	}, true
}

// THE LOOP CONSEQUENCE. A refusal must not draw the retry hint, because the hint
// tells the model to fix arguments that were already valid and the tool will
// refuse identically next time.
func TestRunDoesNotHintARefusedCaptureArtifact(t *testing.T) {
	tool := &alwaysRefusingCaptureTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)

	calls := toolFailureHintAt + 1
	turns := make([][]zeroruntime.StreamEvent, 0, calls+1)
	for i := range calls {
		turns = append(turns, toolTurn("call-"+strconv.Itoa(i), "capture_artifact", `{"action":"browser_screenshot"}`))
	}
	turns = append(turns, textTurn("gave up on the screenshot"))

	result, err := Run(context.Background(), "take a screenshot", &mockProvider{turns: turns}, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		MaxTurns:       len(turns) + 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tool.ran != 0 {
		t.Fatalf("SETUP INVALID: the tool executed %d times; it must be refused before Run", tool.ran)
	}
	for _, message := range result.Messages {
		if strings.Contains(message.Content, toolFailureHintMarker) {
			t.Fatal("a refused, never-executed tool drew the retry hint, which tells the model to fix arguments that were already valid")
		}
	}
}

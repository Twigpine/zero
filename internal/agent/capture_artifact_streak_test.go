package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// alternatingRefusalCaptureTool refuses every call with the same CATEGORY and a
// different MESSAGE, which is what a real disabled driver does: the refusal
// names the action, and the model is free to alternate valid actions.
type alternatingRefusalCaptureTool struct{ ran int }

func (tool *alternatingRefusalCaptureTool) Name() string        { return "capture_artifact" }
func (tool *alternatingRefusalCaptureTool) Description() string { return "test capture tool" }
func (tool *alternatingRefusalCaptureTool) Parameters() tools.Schema {
	return tools.Schema{
		Type:                 "object",
		Properties:           map[string]tools.PropertySchema{"action": {Type: "string"}},
		Required:             []string{"action"},
		AdditionalProperties: false,
	}
}
func (tool *alternatingRefusalCaptureTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow, Reason: "captures artifacts"}
}
func (tool *alternatingRefusalCaptureTool) Run(context.Context, map[string]any) tools.Result {
	tool.ran++
	return tools.Result{}
}
func (tool *alternatingRefusalCaptureTool) RejectBeforePermission(args map[string]any) (tools.Result, bool) {
	action, _ := args["action"].(string)
	return tools.Result{
		Status: tools.StatusError,
		// Action-specific wording, same category. This is the whole point: the
		// prose differs on every call while the refusal never changes.
		Output: "Error: Local control driver for " + action + " is disabled.",
		Meta:   map[string]string{tools.PolicyRefusalMeta: tools.PolicyRefusalToolNotEnabled},
	}, true
}

// A REFUSAL KEYS ON ITS CATEGORY, INCLUDING WHEN THE CATEGORY IS ONLY A MARKER.
//
// The registry marks pre-execution refusals in metadata, and isPolicyRefusal
// read that marker while observeToolResult keyed on DenialReason, which those
// paths leave empty. The guard fell back to errorSignature(output), so two
// refusals of the same category with different wording looked like two
// different failures and the streak restarted at 1 every call.
//
// A model alternating capture_artifact's browser_screenshot and browser_pdf
// against a disabled driver is refused identically each time and never tripped
// the six-call halt. Only the generic twelve-error fallback stopped the run,
// reporting varied errors rather than a repeated refusal.
func TestAlternatingRefusedActionsStillTripTheRefusalHalt(t *testing.T) {
	tool := &alternatingRefusalCaptureTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)

	// More calls than the refusal halt but FEWER than the generic any-error
	// fallback, so only the category-keyed streak can stop this.
	calls := toolFailureAnyErrorStopAt - 1
	if calls <= toolFailureStopAt {
		t.Fatalf("SETUP INVALID: %d calls cannot distinguish the refusal halt from the generic fallback", calls)
	}
	actions := []string{"browser_screenshot", "browser_pdf"}
	turns := make([][]zeroruntime.StreamEvent, 0, calls+1)
	for i := range calls {
		action := actions[i%len(actions)]
		turns = append(turns, toolTurn("call-"+strconv.Itoa(i), "capture_artifact", `{"action":"`+action+`"}`))
	}
	turns = append(turns, textTurn("gave up"))

	result, err := Run(context.Background(), "capture something", &mockProvider{turns: turns}, Options{
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

	refusals := 0
	for _, message := range result.Messages {
		if strings.Contains(message.Content, "is disabled") {
			refusals++
		}
	}
	if refusals > toolFailureStopAt {
		t.Errorf("the run made %d refused calls; the six-call refusal halt never tripped because the streak re-keyed on each action's wording", refusals)
	}

	// And the halt has to read as a repeated refusal, not as varied errors.
	stop := strings.ToLower(strings.Join(messageContents(result.Messages), "\n"))
	if strings.Contains(stop, toolFailureHintMarker) {
		t.Error("a refused, never-executed tool drew the retry hint")
	}
}

func messageContents(messages []zeroruntime.Message) []string {
	out := make([]string, 0, len(messages))
	for _, message := range messages {
		out = append(out, message.Content)
	}
	return out
}

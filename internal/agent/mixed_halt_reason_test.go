package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// mixedOutcomeTool needs permission on every call. The test denies every other
// request, and a call that is allowed executes and fails with an error naming
// its own item, so no single identity repeats and only the content-blind bound
// can stop the run.
type mixedOutcomeTool struct{ ran int }

func (tool *mixedOutcomeTool) Name() string             { return "bash" }
func (tool *mixedOutcomeTool) Description() string      { return "probe" }
func (tool *mixedOutcomeTool) Parameters() tools.Schema { return tools.Schema{Type: "object"} }
func (tool *mixedOutcomeTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectShell, Permission: tools.PermissionPrompt, Reason: "runs a command"}
}

func (tool *mixedOutcomeTool) Run(context.Context, map[string]any) tools.Result {
	tool.ran++
	return tools.Result{Status: tools.StatusError, Output: "Error: item " + strconv.Itoa(tool.ran) + " failed"}
}

// A MIXED HALT READS THE SAME HEADLESS AS INTERACTIVE.
//
// The stop answer for the content-blind bound over executed failures and
// refusals says the tool "failed or was refused". The headless reason for the
// same halt said only that it "failed repeatedly", so zero exec and the
// interactive answer disagreed about one stop. Reported by @jatmn.
func TestAMixedGuardHaltIsReportedAsMixedUnderTheCompletionGate(t *testing.T) {
	tool := &mixedOutcomeTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)
	turns := make([][]zeroruntime.StreamEvent, 0, toolFailureAnyErrorStopAt+4)
	for i := range toolFailureAnyErrorStopAt + 4 {
		turns = append(turns, toolTurn("c"+strconv.Itoa(i), "bash", `{"item":`+strconv.Itoa(i)+`}`))
	}
	asked := 0
	result, err := Run(context.Background(), "run them", &mockProvider{turns: turns}, Options{
		Registry:                registry,
		PermissionMode:          PermissionModeAsk,
		MaxTurns:                len(turns) + 5,
		RequireCompletionSignal: true,
		OnPermissionRequest: func(context.Context, PermissionRequest) (PermissionDecision, error) {
			asked++
			if asked%2 == 0 {
				return PermissionDecision{Action: PermissionDecisionAllow}, nil
			}
			return PermissionDecision{Action: PermissionDecisionDeny}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tool.ran == 0 || asked <= tool.ran {
		t.Fatalf("the run did not mix executed failures with refusals (ran %d of %d asked), so this proves nothing", tool.ran, asked)
	}
	if !strings.Contains(result.FinalAnswer, "failed or was refused") {
		t.Fatalf("the halt was not the mixed content-blind bound: %q", result.FinalAnswer)
	}
	if !result.Incomplete {
		t.Fatal("a guard halt under the completion gate must be Incomplete")
	}
	if !strings.Contains(result.IncompleteReason, "failed or was refused") || !strings.Contains(result.IncompleteReason, "bash") {
		t.Errorf("IncompleteReason = %q, want it to say the tool failed or was refused, as the stop answer does", result.IncompleteReason)
	}
}

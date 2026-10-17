package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// A REFUSAL IS A FAILURE, AND THE CLASSIFIER MUST SAY SO BEFORE IT READS ANYTHING
// ELSE.
//
// isPolicyRefusal decides on denial category, then permission metadata, then
// output text. None of those questions is meaningful about a call the tool
// completed, and the last one is answered by content the model does not control:
// a `bash` that greps the sandbox docs, a read_file returning this very source
// file, any successful command whose output happens to carry one of the phrases.
//
// isRetriableToolError gated on StatusError before calling in, so the boundary
// held while that was the only caller. Extracting the helper and calling it from
// the counting path dropped the gate, and every successful result was put to a
// question that only failures can answer.
//
// Each case below is a SUCCESS. None may be read as a refusal.
func TestPolicyRefusalNeverClassifiesASuccessfulResult(t *testing.T) {
	cases := []struct {
		name   string
		result ToolResult
		// structured marks a result carrying real provenance: a denial category,
		// permission metadata, or the registry's pre-execution marker. Only those
		// may be read as a refusal when they fail. The rest carry nothing but
		// text, which the tool controls, so they must stay ordinary failures no
		// matter which phrase they happen to contain.
		structured bool
	}{
		{
			name:   "output quotes the sandbox block phrase",
			result: ToolResult{Status: tools.StatusOK, Output: "grep: docs/sandbox.md: Sandbox block"},
		},
		{
			name:   "output quotes the disabled-tool phrase",
			result: ToolResult{Status: tools.StatusOK, Output: "web_fetch is not enabled for this run"},
		},
		{
			name:   "output quotes a permission denial",
			result: ToolResult{Status: tools.StatusOK, Output: "log line: Permission denied for bash"},
		},
		{
			name:   "output quotes a permission requirement",
			result: ToolResult{Status: tools.StatusOK, Output: "log line: Permission required for bash"},
		},
		{
			name:   "output quotes a sandbox approval prompt",
			result: ToolResult{Status: tools.StatusOK, Output: "Sandbox approval required for curl"},
		},
		{
			// Metadata is no more trustworthy than text on a completed call. A
			// result carrying a category with an OK status is not a denial; every
			// path that attaches one fails the call.
			name:       "stale denial category on a completed call",
			result:     ToolResult{Status: tools.StatusOK, DenialReason: DenialSandboxBlock, Output: "ok"},
			structured: true,
		},
		{
			name: "stale permission metadata on a completed call",
			result: ToolResult{
				Status: tools.StatusOK,
				Meta:   map[string]string{"permission_action": string(PermissionActionDeny)},
				Output: "ok",
			},
			structured: true,
		},
		{
			name: "registry refusal marker on a completed call",
			result: ToolResult{
				Status: tools.StatusOK,
				Meta:   map[string]string{tools.PolicyRefusalMeta: tools.PolicyRefusalSandboxDenied},
				Output: "ok",
			},
			structured: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if isPolicyRefusal(testCase.result) {
				t.Errorf("a successful result was classified as a policy refusal, so a healthy call counts toward the denial streak: %+v", testCase.result)
			}
			failed := testCase.result
			failed.Status = tools.StatusError
			switch {
			case testCase.structured && !isPolicyRefusal(failed):
				t.Errorf("a failure carrying real provenance was not classified as a refusal, so the gate broke the classifier: %+v", failed)
			case !testCase.structured && isPolicyRefusal(failed):
				t.Errorf("a failure carrying only text was classified as a refusal; output is tool-controlled and cannot decide this: %+v", failed)
			}
		})
	}
}

// alwaysSucceedingTool runs freely and prints text the classifier keys on. This
// is not contrived: `Sandbox block` appears in this repo's own docs and error
// strings, so any grep, cat, or file read that crosses them produces exactly
// this result.
type alwaysSucceedingTool struct{ ran int }

func (tool *alwaysSucceedingTool) Name() string        { return "bash" }
func (tool *alwaysSucceedingTool) Description() string { return "test shell tool" }
func (tool *alwaysSucceedingTool) Parameters() tools.Schema {
	return tools.Schema{
		Type:                 "object",
		Properties:           map[string]tools.PropertySchema{"command": {Type: "string"}},
		Required:             []string{"command"},
		AdditionalProperties: false,
	}
}
func (tool *alwaysSucceedingTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow, Reason: "reads files"}
}
func (tool *alwaysSucceedingTool) Run(context.Context, map[string]any) tools.Result {
	tool.ran++
	return tools.Result{
		Status: tools.StatusOK,
		Output: "docs/windows-sandbox.md:41: Sandbox block (write jail) is reported as ...",
	}
}

// The same gap driven through Run, because the classifier being wrong only
// matters if the loop acts on it, and it does: the counting path calls
// isPolicyRefusal directly, and a true answer records a failure against the
// tool's signature. Six identical successes then look like six identical
// denials, and the run ends on a stop answer telling the user a tool it never
// refused was refused six times.
//
// The tool succeeds every turn and the run is given more turns than the bound,
// so anything that halts before the provider's final turn is the guard.
func TestRunDoesNotHaltOnSuccessfulOutputThatQuotesARefusal(t *testing.T) {
	tool := &alwaysSucceedingTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)

	calls := toolFailureStopAt + 4
	turns := make([][]zeroruntime.StreamEvent, 0, calls+1)
	for i := range calls {
		turns = append(turns, toolTurn("call-"+strconv.Itoa(i), "bash",
			`{"command":"grep -n 'Sandbox block' docs/windows-sandbox.md"}`))
	}
	turns = append(turns, textTurn("found the docs"))
	provider := &mockProvider{turns: turns}

	result, err := Run(context.Background(), "search the docs", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		MaxTurns:       len(turns) + 5,
		OnPermissionRequest: func(context.Context, PermissionRequest) (PermissionDecision, error) {
			t.Error("the loop asked permission for an allow-safety tool; this test must exercise the success path")
			return PermissionDecision{Action: PermissionDecisionDeny}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if tool.ran != calls {
		t.Errorf("the tool ran %d times, want %d; the run was halted mid-way by its own success output", tool.ran, calls)
	}
	if result.FinalAnswer != "found the docs" {
		t.Errorf("final answer =\n  %q\nwant\n  %q\n(a stop answer here means successful output was counted as a refusal)",
			result.FinalAnswer, "found the docs")
	}
}

// failingPhraseTool RAN and failed. Its stderr happens to contain a phrase the
// classifier used to key on, which is the everyday case: `bash` preserves
// arbitrary stdout and stderr on any nonzero exit, so
// `printf 'Sandbox block\n' >&2; exit 1` produces exactly this.
type failingPhraseTool struct {
	ran    int
	output string
}

func (tool *failingPhraseTool) Name() string        { return "bash" }
func (tool *failingPhraseTool) Description() string { return "test shell tool" }
func (tool *failingPhraseTool) Parameters() tools.Schema {
	return tools.Schema{
		Type:                 "object",
		Properties:           map[string]tools.PropertySchema{"command": {Type: "string"}},
		Required:             []string{"command"},
		AdditionalProperties: false,
	}
}
func (tool *failingPhraseTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow, Reason: "reads files"}
}
func (tool *failingPhraseTool) Run(context.Context, map[string]any) tools.Result {
	tool.ran++
	return tools.Result{Status: tools.StatusError, Output: tool.output}
}

// AN EXECUTED FAILURE IS RETRIABLE, WHATEVER IT PRINTED, and the observable
// difference is the schema hint.
//
// isRetriableToolError feeds `hintable` into the guard, and the guard injects
// the retry hint at toolFailureHintAt. Classifying an executed failure as a
// policy refusal makes hintable false, so the model is told nothing and simply
// repeats the call: the run loses the one correction it had. That is on top of
// the failure being accumulated as a denial.
//
// Each phrase below is one the classifier used to match on. The tool is
// allow-safety and runs every time, so nothing here is refused by anything.
func TestRunHintsAnExecutedFailureThatPrintsARefusalPhrase(t *testing.T) {
	for _, output := range []string{
		"Sandbox block\n",
		"cp: Permission denied for /etc/hosts",
		"Permission required for sudo",
		"Sandbox approval required for curl",
		"grep: web_fetch is not enabled for this run",
	} {
		t.Run(output, func(t *testing.T) {
			tool := &failingPhraseTool{output: output}
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
			provider := &mockProvider{turns: turns}

			result, err := Run(context.Background(), "run the script", provider, Options{
				Registry:       registry,
				PermissionMode: PermissionModeAsk,
				MaxTurns:       len(turns) + 5,
				OnPermissionRequest: func(context.Context, PermissionRequest) (PermissionDecision, error) {
					t.Error("permission was requested for an allow-safety tool; this test must exercise the executed-failure path")
					return PermissionDecision{Action: PermissionDecisionDeny}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if tool.ran != calls {
				t.Fatalf("the tool ran %d times, want %d; the run was halted by output it merely printed", tool.ran, calls)
			}

			var hinted bool
			for _, message := range result.Messages {
				if strings.Contains(message.Content, toolFailureHintMarker) {
					hinted = true
					break
				}
			}
			if !hinted {
				t.Errorf("no retry hint was injected for an executed failure printing %q; it was classified as a policy refusal, so the model got no correction", output)
			}
		})
	}
}

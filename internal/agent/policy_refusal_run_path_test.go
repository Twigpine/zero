package agent

import (
	"context"
	"strconv"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/trace"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// THE UNCATEGORIZED REFUSALS ARE THE ONES THIS CHANGE EXISTS FOR, SO THEY ARE THE
// ONES THAT MUST BE DRIVEN THROUGH Run.
//
// A categorized denial is easy to test and was never the gap. The gap is a
// refusal that arrives with DenialReason empty, because a category is attached
// only where a TYPED denial is built:
//
//   - a headless run leaves OnPermissionRequest nil, so the loop never reaches
//     its typed-denial branch and the registry gate returns a bare
//     `Permission required for ...`
//   - a sandbox preflight denial on a non-shell tool loses its SandboxDecision
//     converting to ToolResult and arrives as a bare `Sandbox block`
//
// Both were counted as SUCCESS, which cleared the very record they were meant to
// accumulate, and the refused call repeated to MaxTurns.
//
// Asserting that through the helper alone would prove nothing: the first version
// of this fix passed every helper test while being a no-op in production,
// because the loop asked a different question than the tests did. So each case
// below goes through Run and pins the three things the loop actually does with
// the classification: halt at the bound, never execute the tool, and withhold
// the profile's one-shot failure escalation.

// headlessPromptTool needs approval and never gets it, because the run has no
// approver. The registry refuses it before Run() is reached, exactly as a
// headless run does, so nothing here fakes the refusal.
type headlessPromptTool struct{ ran int }

func (tool *headlessPromptTool) Name() string        { return "bash" }
func (tool *headlessPromptTool) Description() string { return "test shell tool" }
func (tool *headlessPromptTool) Parameters() tools.Schema {
	return tools.Schema{
		Type:                 "object",
		Properties:           map[string]tools.PropertySchema{"command": {Type: "string"}},
		Required:             []string{"command"},
		AdditionalProperties: false,
	}
}
func (tool *headlessPromptTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectShell, Permission: tools.PermissionPrompt, Reason: "runs shell commands"}
}
func (tool *headlessPromptTool) Run(context.Context, map[string]any) tools.Result {
	tool.ran++
	return tools.Result{Status: tools.StatusOK, Output: "should never run"}
}

// A headless prompt refusal must halt at the same-signature bound rather than
// repeat to MaxTurns.
//
// It used to key on the refusal TEXT, which the registry happens to hold
// constant here, and the stop then described a repeated failure. The registry
// marks this path, so the category is now derived at the ToolResult boundary and
// the streak keys on that instead. The bound is unchanged; the wording is more
// honest, because a headless prompt refusal is a refusal and not a tool that
// keeps failing.
func TestRunStopsAnUncategorizedHeadlessRefusalAtTheFailureBound(t *testing.T) {
	tool := &headlessPromptTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)

	// Comfortably more turns than the bound, so reaching the bound is what stops
	// the run rather than exhausting the provider or MaxTurns.
	turns := make([][]zeroruntime.StreamEvent, 0, toolFailureStopAt+4)
	for i := range toolFailureStopAt + 4 {
		turns = append(turns, toolTurn("call-"+strconv.Itoa(i), "bash",
			`{"command":"touch /etc/file`+strconv.Itoa(i)+`"}`))
	}
	provider := &mockProvider{turns: turns}

	recorder := trace.NewRecorder("policy-refusal-session", "run-1", "fast")
	result, err := Run(context.Background(), "do the thing", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		MaxTurns:       len(turns) + 5,
		Trace:          recorder,
		// Armed well below the halt bound, so a refusal counted as a retriable
		// failure would visibly spend it.
		Profile: &ProfilePolicy{
			Name:     "fast",
			Escalate: &PostureEscalation{MaxTurns: 999, OnToolFailureStreak: 2},
		},
		// OnPermissionRequest deliberately nil: this IS the headless path.
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(provider.requests) != toolFailureStopAt {
		t.Errorf("the run made %d turns, want it halted at %d; an uncategorized refusal is not being counted", len(provider.requests), toolFailureStopAt)
	}
	if tool.ran != 0 {
		t.Errorf("the refused tool executed %d times; the registry gate must precede execution", tool.ran)
	}
	// Categorized now, so the stop answer says refused. Previously the loop could
	// not tell and reported a repeated failure.
	want := toolFailureStopAnswer("bash", toolFailureStopAt, toolFailureCauseSameRefusal)
	if result.FinalAnswer != want {
		t.Errorf("final answer =\n  %q\nwant\n  %q", result.FinalAnswer, want)
	}
	assertNoPostureEscalation(t, recorder)
}

// uncategorizedSandboxTool returns the shape a sandbox preflight denial has by
// the time the loop sees it on a non-shell tool: StatusError, `Sandbox block`
// prose, and no category, because the SandboxDecision does not survive the
// conversion to ToolResult. The path varies per call exactly as the real message
// does, since it names what was refused.
type uncategorizedSandboxTool struct{ ran int }

func (tool *uncategorizedSandboxTool) Name() string        { return "write_file" }
func (tool *uncategorizedSandboxTool) Description() string { return "test write tool" }
func (tool *uncategorizedSandboxTool) Parameters() tools.Schema {
	return tools.Schema{
		Type:                 "object",
		Properties:           map[string]tools.PropertySchema{"path": {Type: "string"}},
		Required:             []string{"path"},
		AdditionalProperties: false,
	}
}
func (tool *uncategorizedSandboxTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectWrite, Permission: tools.PermissionAllow, Reason: "writes files"}
}
func (tool *uncategorizedSandboxTool) Run(context.Context, map[string]any) tools.Result {
	tool.ran++
	return tools.Result{
		Status: tools.StatusError,
		Output: "Sandbox block: write to /out-" + strconv.Itoa(tool.ran) + ".txt is outside the workspace",
	}
}

// Varying text plus no category means every call restarts the same-signature
// streak at 1, so the same-signature bound is never reached and only the
// content-blind counter can halt the run. Before the fix nothing halted it at
// all: each refusal was read as a success, deleted the record, and the call
// repeated until MaxTurns.
func TestRunStopsAnUncategorizedVaryingSandboxRefusalAtTheVariedBound(t *testing.T) {
	tool := &uncategorizedSandboxTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)

	turns := make([][]zeroruntime.StreamEvent, 0, toolFailureAnyErrorStopAt+4)
	for i := range toolFailureAnyErrorStopAt + 4 {
		turns = append(turns, toolTurn("call-"+strconv.Itoa(i), "write_file",
			`{"path":"/out-`+strconv.Itoa(i)+`.txt"}`))
	}
	provider := &mockProvider{turns: turns}

	recorder := trace.NewRecorder("policy-refusal-session", "run-2", "fast")
	result, err := Run(context.Background(), "write the files", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		MaxTurns:       len(turns) + 5,
		Trace:          recorder,
		Profile: &ProfilePolicy{
			Name:     "fast",
			Escalate: &PostureEscalation{MaxTurns: 999, OnToolFailureStreak: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if tool.ran != toolFailureAnyErrorStopAt {
		t.Errorf("the tool was called %d times, want the run halted at %d", tool.ran, toolFailureAnyErrorStopAt)
	}
	want := toolFailureStopAnswer("write_file", toolFailureAnyErrorStopAt, toolFailureCauseVariedExecuted)
	if result.FinalAnswer != want {
		t.Errorf("final answer =\n  %q\nwant\n  %q", result.FinalAnswer, want)
	}
	assertNoPostureEscalation(t, recorder)
}

// The profile's failure-streak trigger restores turn budget and reasoning effort
// on the theory that a struggling tool needs room. A refusal is not a struggling
// tool, it is an answer, and spending the one-shot escalation on one buys turns
// that will be refused identically. The loop keeps this contract by passing an
// empty outcome to the controller for a refusal, which is only observable from
// outside as the escalation never firing.
func assertNoPostureEscalation(t *testing.T, recorder *trace.Recorder) {
	t.Helper()
	if got := recorder.Finish().Counter(trace.CounterPostureEscalations); got != 0 {
		t.Errorf("posture_escalations = %d, want 0; a policy refusal spent the one-shot failure escalation", got)
	}
}

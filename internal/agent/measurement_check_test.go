package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// goTestTool stands in for a shell that ran `go test` and printed its result.
type goTestTool struct{ output string }

func (goTestTool) Name() string        { return "run_tests" }
func (goTestTool) Description() string { return "run the test suite" }
func (goTestTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", AdditionalProperties: false}
}
func (goTestTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectNone, Permission: tools.PermissionAllow, AdvertiseInAuto: true}
}
func (t goTestTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK, Output: t.output}
}

// callThenAnswer is a provider that calls run_tests, then gives finalAnswer, then
// gives corrected — so a run that is sent back for a number gets somewhere to go.
func callThenAnswer(finalAnswer, corrected string) *mockProvider {
	return &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call-1", ToolName: "run_tests"},
			{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call-1", ArgumentsFragment: `{}`},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call-1"},
			{Type: zeroruntime.StreamEventDone},
		},
		{{Type: zeroruntime.StreamEventText, Content: finalAnswer}, {Type: zeroruntime.StreamEventDone}},
		{{Type: zeroruntime.StreamEventText, Content: corrected}, {Type: zeroruntime.StreamEventDone}},
	}}
}

const suiteOutput = `{"Action":"pass","Package":"github.com/x/y","Test":"TestChattyChild","Elapsed":0.86}
{"Action":"pass","Package":"github.com/x/y","Elapsed":0.86}
`

// A FINAL ANSWER THAT REPORTS A NUMBER THE RUN NEVER PRODUCED IS SENT BACK.
//
// The measured failure: a benchmark table where the same test read 0.86s in one
// paste and 4.20s in the next, with nothing said about the difference. A prompt
// rule cannot catch this — a model willing to write a number it did not measure
// is equally willing to say it re-ran it. The transcript can.
func TestAFinalAnswerContradictingTheTranscriptIsSentBack(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(goTestTool{output: suiteOutput})
	provider := callThenAnswer("TestChattyChild took 4.20s.", "Corrected: TestChattyChild took 0.86s.")

	result, err := Run(context.Background(), "run the suite", provider, Options{
		Cwd: root, Registry: registry, SessionID: "session-measured",
		Zeromaxing: ZeromaxingActive,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(provider.requests) < 3 {
		t.Fatalf("the run ended after %d requests: the contradicted answer was accepted", len(provider.requests))
	}
	sentBack := renderZeromaxingMessages(provider.requests[2].Messages)
	for _, required := range []string{"TestChattyChild", "4.2s", "0.86s", "Re-run the command"} {
		if !strings.Contains(sentBack, required) {
			t.Errorf("the correction does not mention %q:\n%s", required, sentBack)
		}
	}
	if !strings.Contains(result.FinalAnswer, "0.86s") {
		t.Errorf("the corrected answer was not the one returned: %q", result.FinalAnswer)
	}
}

// AN HONEST ANSWER IS RETURNED UNTOUCHED. A tripwire that fires on a correct
// report gets switched off, and then it catches nothing.
func TestAnAnswerThatMatchesTheTranscriptIsReturnedAsIs(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(goTestTool{output: suiteOutput})
	provider := callThenAnswer("TestChattyChild took 0.86s.", "should never be reached")

	result, err := Run(context.Background(), "run the suite", provider, Options{
		Cwd: root, Registry: registry, SessionID: "session-honest",
		Zeromaxing: ZeromaxingActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("an honest answer cost %d requests, want 2: the check fired on a correct report", len(provider.requests))
	}
	if !strings.Contains(result.FinalAnswer, "0.86s") {
		t.Errorf("final answer = %q", result.FinalAnswer)
	}
}

// THE CHECK IS PART OF THE POSTURE. With zeromaxing off the run must take
// exactly the path it took before this existed — the same rule every other
// posture behaviour follows.
func TestThePostureOffRunIsNeverSentBackForANumber(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(goTestTool{output: suiteOutput})
	provider := callThenAnswer("TestChattyChild took 4.20s.", "should never be reached")

	result, err := Run(context.Background(), "run the suite", provider, Options{
		Cwd: root, Registry: registry, SessionID: "session-posture-off",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("a posture-off run cost %d requests, want 2: the check reached a path it must not", len(provider.requests))
	}
	if !strings.Contains(result.FinalAnswer, "4.20s") {
		t.Errorf("a posture-off run's answer was altered: %q", result.FinalAnswer)
	}
}

// AN UNCORRECTED ANSWER IS RETURNED RATHER THAN ASKED AGAIN. The ledger raises
// each name once, so a model that repeats the number ends the run instead of
// looping until the turn budget is gone.
func TestARepeatedNumberEndsTheRunRatherThanLooping(t *testing.T) {
	root := t.TempDir()
	registry := tools.NewRegistry()
	registry.Register(goTestTool{output: suiteOutput})
	// Says the same wrong number both times.
	provider := callThenAnswer("TestChattyChild took 4.20s.", "TestChattyChild took 4.20s.")

	result, err := Run(context.Background(), "run the suite", provider, Options{
		Cwd: root, Registry: registry, SessionID: "session-stubborn",
		Zeromaxing: ZeromaxingActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 3 {
		t.Fatalf("the run cost %d requests, want exactly 3: one call, one answer, one correction", len(provider.requests))
	}
	if !strings.Contains(result.FinalAnswer, "4.20s") {
		t.Errorf("final answer = %q", result.FinalAnswer)
	}
}

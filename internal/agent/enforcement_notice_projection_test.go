package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

// noticeProjectionTool stands in for any sandboxed command tool: it reports the
// enforcement disclosure the way the real ones do, through the sandbox metadata
// key that finalizeToolOutcome promotes into the typed notice slice.
type noticeProjectionTool struct{}

func (noticeProjectionTool) Name() string             { return "notice_projection" }
func (noticeProjectionTool) Description() string      { return "test tool carrying an enforcement notice" }
func (noticeProjectionTool) Parameters() tools.Schema { return tools.Schema{Type: "object"} }
func (noticeProjectionTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow}
}

func (noticeProjectionTool) Run(ctx context.Context, args map[string]any) tools.Result {
	return tools.Result{
		Status:  tools.StatusOK,
		Output:  "the command output",
		Display: tools.Display{Summary: "the human summary"},
		Meta:    map[string]string{"sandbox_notices": "least-privilege notice"},
	}
}

// THE PROJECTION MUST CARRY ONE REPRESENTATION, NOT TWO.
//
// executeToolCall copies the typed notice slice into agent.ToolResult, so the
// text fields it copies alongside must be the UNDECORATED base. Storing the
// already-rendered text there instead leaves the same fact in two places with no
// contract between them: it happens to render once today only because the
// outcome arrives finalized and the agent accessor then reads Outcome.ModelView
// rather than the stored field. Any result that reaches the accessor without a
// finalized outcome renders the disclosure twice, and every raw reader of
// .Output sees text that disagrees with Outcome.ModelView.
func TestEnforcementNoticeIsStoredOnceAndRenderedOnce(t *testing.T) {
	const notice = "least-privilege notice"

	registry := tools.NewRegistry()
	registry.Register(noticeProjectionTool{})

	result, err := executeToolCall(context.Background(), registry, ToolCall{
		ID: "call-1", Name: "notice_projection", Arguments: `{}`,
	}, PermissionModeAuto, Options{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("executeToolCall: %v", err)
	}
	if len(result.EnforcementNotices) == 0 {
		t.Fatalf("the notice never reached the agent result: %#v", result)
	}

	// The stored fields are the canonical undecorated base, and they agree with
	// the finalized outcome they were projected from.
	if strings.Contains(result.Output, notice) {
		t.Errorf("ToolResult.Output stores the rendered notice as well as the slice: %q", result.Output)
	}
	if strings.Contains(result.Display.Summary, notice) {
		t.Errorf("ToolResult.Display.Summary stores the rendered notice as well as the slice: %q", result.Display.Summary)
	}
	if result.Output != result.Outcome.ModelView {
		t.Errorf("stored output %q disagrees with the finalized model view %q", result.Output, result.Outcome.ModelView)
	}
	if result.Display.Summary != result.Outcome.HumanView.Summary {
		t.Errorf("stored summary %q disagrees with the finalized human view %q", result.Display.Summary, result.Outcome.HumanView.Summary)
	}

	// And every consumer that renders goes through the accessors, which show the
	// disclosure exactly once without hiding the output it is attached to.
	// loop.go builds the provider transcript from ModelOutput; the CLI writer and
	// the TUI cards use both accessors.
	transcript := result.ModelOutput()
	if got := strings.Count(transcript, notice); got != 1 {
		t.Errorf("the transcript shows the notice %d times, want 1: %q", got, transcript)
	}
	if !strings.Contains(transcript, "the command output") {
		t.Errorf("the transcript lost the command output: %q", transcript)
	}
	summary := result.HumanDisplay().Summary
	if got := strings.Count(summary, notice); got != 1 {
		t.Errorf("the human summary shows the notice %d times, want 1: %q", got, summary)
	}
	if !strings.Contains(summary, "the human summary") {
		t.Errorf("the human summary lost the tool summary: %q", summary)
	}
}

// A result that never crossed the registry has no finalized outcome, so the
// accessor falls back to the stored field. That is the path on which a stored
// rendering would double, and it is the reason the contract above is stated on
// the stored fields rather than only on the accessors.
func TestUnfinalizedResultStillRendersTheNoticeOnce(t *testing.T) {
	const notice = "least-privilege notice"
	result := ToolResult{
		Status:             tools.StatusOK,
		Output:             "the command output",
		Display:            tools.Display{Summary: "the human summary"},
		EnforcementNotices: []string{notice},
	}
	if result.Outcome.Finalized() {
		t.Fatal("fixture is finalized; it no longer covers the fallback path")
	}
	if got := strings.Count(result.ModelOutput(), notice); got != 1 {
		t.Errorf("ModelOutput shows the notice %d times, want 1: %q", got, result.ModelOutput())
	}
	if got := strings.Count(result.HumanDisplay().Summary, notice); got != 1 {
		t.Errorf("HumanDisplay shows the notice %d times, want 1: %q", got, result.HumanDisplay().Summary)
	}
}

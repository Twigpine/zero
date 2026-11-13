package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/tools"
)

func TestAppendHookFeedbackFormatsOutput(t *testing.T) {
	// Blank feedback leaves the original output untouched and reports no redaction.
	if got, redacted := appendHookFeedback("tool output", "   "); got != "tool output" || redacted {
		t.Fatalf("blank feedback should not change output or redact, got %q redacted=%v", got, redacted)
	}
	// Feedback is appended under a header alongside the existing output.
	got, redacted := appendHookFeedback("tool output", "gofmt reformatted main.go")
	if !strings.Contains(got, "tool output") || !strings.Contains(got, "Hook output:") || !strings.Contains(got, "gofmt reformatted main.go") {
		t.Fatalf("expected combined tool + hook output, got %q", got)
	}
	if redacted {
		t.Fatal("clean feedback should not be reported as redacted")
	}
	// With no original output the hook feedback stands alone under the header.
	if got, _ := appendHookFeedback("", "validation ran"); !strings.HasPrefix(got, "Hook output:") || !strings.Contains(got, "validation ran") {
		t.Fatalf("expected standalone hook output, got %q", got)
	}
}

func TestAppendHookFeedbackScrubsSecretsAndFlagsRedaction(t *testing.T) {
	// A hook that echoes a secret must be scrubbed before it reaches the model,
	// and the redaction must be reported so the caller can flag ToolResult.Redacted.
	got, redacted := appendHookFeedback("tool output", "leaked token ghp_abcdefghijklmnopqrstuvwxyz0123456789")
	if !redacted {
		t.Fatal("expected redaction to be reported for secret-bearing feedback")
	}
	if strings.Contains(got, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("secret leaked into hook feedback: %q", got)
	}
}

func TestBlockedByHookResultCarriesReasonAndDenial(t *testing.T) {
	out := blockedByHookResult(
		ToolCall{ID: "c1", Name: "write_file"},
		hooks.DispatchOutcome{Blocked: true, BlockedBy: "policy", Reason: "writes under /etc are denied"},
	)
	if out.Status != tools.StatusError {
		t.Fatalf("status = %v, want error", out.Status)
	}
	if out.DenialReason != DenialHookBlocked {
		t.Fatalf("denial = %q, want %q", out.DenialReason, DenialHookBlocked)
	}
	if out.ToolCallID != "c1" || out.Name != "write_file" {
		t.Fatalf("call identity not propagated: %#v", out)
	}
	for _, want := range []string{"write_file", "policy", "writes under /etc are denied"} {
		if !strings.Contains(out.Output, want) {
			t.Fatalf("output %q missing %q", out.Output, want)
		}
	}
	if out.Redacted {
		t.Fatal("a clean hook reason must not flag Redacted")
	}
}

func TestBlockedByHookResultScrubsSecretReason(t *testing.T) {
	out := blockedByHookResult(
		ToolCall{ID: "c3", Name: "bash"},
		hooks.DispatchOutcome{Blocked: true, BlockedBy: "secret-scan", Reason: "denied: found ghp_abcdefghijklmnopqrstuvwxyz0123456789 in args"},
	)
	if !out.Redacted {
		t.Fatal("a scrubbed hook reason must set ToolResult.Redacted")
	}
	if strings.Contains(out.Output, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("secret leaked into blocked-hook output: %q", out.Output)
	}
}

func TestBlockedByHookResultFallsBackWhenReasonEmpty(t *testing.T) {
	out := blockedByHookResult(ToolCall{ID: "c2", Name: "bash"}, hooks.DispatchOutcome{Blocked: true, BlockedBy: "x"})
	if !strings.Contains(out.Output, "blocked by a beforeTool hook") {
		t.Fatalf("expected a default reason, got %q", out.Output)
	}
}

func TestDispatchHelpersAreNoopWithoutDispatcher(t *testing.T) {
	options := Options{} // Hooks is nil
	if _, blocked := dispatchBeforeTool(context.Background(), options, ToolCall{Name: "bash"}, nil); blocked {
		t.Fatal("a nil dispatcher must never block a tool")
	}
	if feedback := dispatchAfterTool(context.Background(), options, ToolCall{Name: "bash"}, nil, tools.Result{}); feedback != "" {
		t.Fatalf("a nil dispatcher must yield no feedback, got %q", feedback)
	}
}

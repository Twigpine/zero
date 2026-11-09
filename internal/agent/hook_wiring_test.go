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

// THE BLOCKING HOOK'S OWN NOTICE IS A NOTICE, NOT PART OF THE VETO TEXT.
//
// It used to arrive folded into Reason, so it reached the model inside the error
// message and never reached EnforcementNotices, which is what every surface
// renders as the disclosure and what the session record keeps apart from the
// body.
func TestBlockedByHookResultCarriesTheBlockersNoticeOnTheTypedSlice(t *testing.T) {
	const notice = "denyRead is configured, so the write jail is not confining writes"
	out := blockedByHookResult(
		ToolCall{ID: "c4", Name: "bash"},
		hooks.DispatchOutcome{Blocked: true, BlockedBy: "policy", Reason: "policy violation", Notices: []string{notice}},
	)
	if len(out.EnforcementNotices) != 1 || out.EnforcementNotices[0] != notice {
		t.Fatalf("EnforcementNotices = %#v, want the blocking hook's notice", out.EnforcementNotices)
	}
	if strings.Contains(out.Output, notice) {
		t.Errorf("the notice is also inside the veto message, so it is said twice:\n%s", out.Output)
	}
	if got := strings.Count(out.ModelOutput(), notice); got != 1 {
		t.Errorf("the model sees the notice %d times, want once:\n%s", got, out.ModelOutput())
	}
}

// AN EARLIER HOOK'S NOTICE SURVIVES A REASON THAT QUOTES IT.
//
// The veto path used to drop every notice whose text appeared anywhere in the
// blocking hook's reason, on the theory that the reason already carried it. A
// blocking hook that printed the same sentence, for example by relaying another
// tool's stderr, therefore removed an earlier hook's disclosure from the typed
// slice.
func TestBlockedByHookResultKeepsAnEarlierNoticeTheReasonQuotes(t *testing.T) {
	const notice = "denyRead is configured, so the write jail is not confining writes"
	out := blockedByHookResult(
		ToolCall{ID: "c5", Name: "bash"},
		hooks.DispatchOutcome{
			Blocked:   true,
			BlockedBy: "veto",
			Reason:    "refusing: the last run reported \"" + notice + "\"",
			Notices:   []string{notice},
		},
	)
	if len(out.EnforcementNotices) != 1 || out.EnforcementNotices[0] != notice {
		t.Fatalf("the earlier hook's notice was dropped because the veto quoted it: %#v", out.EnforcementNotices)
	}
}

func TestDispatchHelpersAreNoopWithoutDispatcher(t *testing.T) {
	options := Options{} // Hooks is nil
	if _, blocked := dispatchBeforeTool(context.Background(), options, ToolCall{Name: "bash"}, nil); blocked {
		t.Fatal("a nil dispatcher must never block a tool")
	}
	if feedback, notices := dispatchAfterTool(context.Background(), options, ToolCall{Name: "bash"}, nil, tools.Result{}); feedback != "" || notices != nil {
		t.Fatalf("a nil dispatcher must yield no feedback and no notices, got %q and %v", feedback, notices)
	}
}

// A SUCCESSFUL beforeTool HOOK'S NOTICE MUST STAY TYPED.
//
// executeToolCall used to read the beforeTool outcome only when Blocked was true,
// so a hook that ran fine and produced an enforcement notice — for instance that
// it ran under the weakened DenyRead token — put that notice in the audit record
// and nowhere anybody could see it. The first fix delivered it as prose appended
// to the result output, which reached the model and no interactive surface,
// because those build their enforcement furniture from the typed slice.
//
// So the delivery is the typed field, and this pins the merge: the hook's notice
// first, the tool's own after it, each exactly once, blanks contributing nothing.
func TestBeforeToolNoticesMergeIntoTheTypedField(t *testing.T) {
	const notice = "hook ran without WRITE_RESTRICTED because denyRead is configured"
	const toolOwned = "the sandbox dropped the network capability for this call"

	result := withAppliedHookNotices(ToolResult{Output: "ok"}, []string{notice}, nil)
	if len(result.EnforcementNotices) != 1 || result.EnforcementNotices[0] != notice {
		t.Fatalf("a successful beforeTool notice did not reach the typed field: %v", result.EnforcementNotices)
	}
	// AND NOT THE OUTPUT AS WELL, or every surface that renders the slice shows
	// the disclosure twice.
	if strings.Contains(result.Output, notice) {
		t.Errorf("the notice was written into the output as well as the typed field: %q", result.Output)
	}

	// Both arrive, hook first, when the tool carries its own.
	result = withAppliedHookNotices(ToolResult{EnforcementNotices: []string{toolOwned}}, []string{notice}, nil)
	if len(result.EnforcementNotices) != 2 || result.EnforcementNotices[0] != notice || result.EnforcementNotices[1] != toolOwned {
		t.Fatalf("the hook and tool notices did not merge in order: %v", result.EnforcementNotices)
	}

	// The same disclosure from both sides is carried once.
	result = withAppliedHookNotices(ToolResult{EnforcementNotices: []string{notice}}, []string{notice}, nil)
	if len(result.EnforcementNotices) != 1 {
		t.Errorf("one disclosure reported by both the hook and the tool was carried %d times: %v", len(result.EnforcementNotices), result.EnforcementNotices)
	}

	// Blank notices contribute nothing, so a run with no hook output stays silent.
	result = withAppliedHookNotices(ToolResult{}, []string{"", "   "}, nil)
	if len(result.EnforcementNotices) != 0 {
		t.Errorf("blank hook notices produced %v, want nothing", result.EnforcementNotices)
	}
}

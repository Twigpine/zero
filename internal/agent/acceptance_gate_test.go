package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

func enabledSelfCorrector(t *testing.T) *SelfCorrector {
	t.Helper()
	// checker/verifier nil → no LSP / project-test pass runs; we only need a
	// non-nil SelfCorrector to enable the task-grounded acceptance gate.
	return NewSelfCorrector(t.TempDir(), nil, nil, SelfCorrectConfig{Enabled: true})
}

// BUG #2(a): a final message that admits the model guessed / could not meet the
// objective must be reported as INCOMPLETE, never success — and immediately
// (an admitted guess is not worth re-prompting).
func TestAcceptanceSelfReportAdmissionIsIncomplete(t *testing.T) {
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		textTurn("I couldn't analyze the board image, so I wrote the common opening move e2e4 as my best guess."),
	}}

	result, err := Run(context.Background(), "write the best chess move", provider, Options{
		Registry:                tools.NewRegistry(),
		MaxTurns:                10,
		RequireCompletionSignal: true,
		SelfCorrect:             enabledSelfCorrector(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Incomplete {
		t.Fatalf("admitted guess must be Incomplete, got success; final=%q", result.FinalAnswer)
	}
	if !strings.Contains(result.IncompleteReason, "objective was not met") {
		t.Fatalf("IncompleteReason = %q, want it to cite the admission", result.IncompleteReason)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("an admission should downgrade immediately (1 turn), got %d", len(provider.requests))
	}
}

// BUG #2(a): the inability detector must generalize over the verb the model uses
// (so re-phrasing can't defeat it) while NOT misreading success-y negations
// ("could not find any issues") as admissions.
func TestSelfReportedIncompletionMatching(t *testing.T) {
	cases := []struct {
		text  string
		admit bool
	}{
		// real chess re-run wording that slipped a fixed phrase list:
		{"…which I cannot do without proper image analysis capabilities.", true},
		{"I cannot visually analyze the chess board image.", true},
		{"I could not recover the original values, so this is my best guess.", true},
		{"I was unable to complete the optimization.", true},
		{"The result may not be correct.", true},
		// multi-occurrence: an early guarded use ("could not find any") must NOT mask a
		// later genuine admission with the same stem (review #5):
		{"I could not find any examples in the codebase, so I could not implement the feature.", true},
		// genuine successes that must NOT be flagged:
		{"I implemented the function and all tests pass.", false},
		{"I verified the fix; I could not find any remaining issues.", false},
		{"I cannot reproduce the bug, so the fix holds.", false},
		{"Done — the output matches the required format and values.", false},
		// behavior descriptions that must NOT be flagged — these phrases describe
		// implemented behavior, not an admission (review #1, removed from the list):
		{"The parser will fall back to UTF-8 when no encoding is set.", false},
		{"I replaced the placeholder value with the computed key.", false},
		{"The retry uses exponential backoff as a fallback.", false},
		{"Without proper error handling the function would crash, so I added it.", false},
		// real false positive (conversational recap): retelling a PAST exchange is
		// not an admission about the current objective — the sentence carries its
		// own narrative context ("You asked …", "at the time"):
		{"You asked if I could work autonomously on zero if you gave me a GitHub account — I was honest that my sandbox had no repo, no build system, and no network/integration wired up yet, so I couldn't actually do it at the time.", false},
		{"When we last spoke, I couldn't reach the GitHub API from the sandbox.", false},
		{"Last session I was unable to build the project; that is what we should retry now.", false},
		// quoted admissions (the model citing its own earlier message or a log
		// line) must NOT be flagged:
		{`My earlier reply said "I couldn't do it" but the tooling is now installed.`, false},
		{"The run log contains `I cannot access the network`, which is expected inside the sandbox.", false},
		// current-objective admissions must STILL be flagged, including past tense
		// and multi-sentence finals whose other sentences are narrative:
		{"I couldn't complete the refactor.", true},
		{"I hit an error I could not resolve, so the migration is unfinished.", true},
		{"You asked me to port the parser. I couldn't finish the second half.", true},
		// unbalanced delimiters must not swallow the tail (only BALANCED quoted
		// spans are stripped), so an admission after a stray quote still fires:
		{"The log ends with a stray \" and I couldn't finish the migration.", true},
		{"There is an unterminated `code span and I cannot proceed with the fix.", true},
		{"The doc opens with “a curly quote and I was unable to complete the task.", true},
	}
	for _, c := range cases {
		got := selfReportedIncompletion(c.text) != ""
		if got != c.admit {
			t.Errorf("selfReportedIncompletion(%q) admitted=%v, want %v", c.text, got, c.admit)
		}
	}
}

// BUG #2(a): a "cannot analyze / cannot determine" admission (present tense) must
// be caught even when the model also stamps a bogus "PASS" based on a shape check.
// Mirrors the chess-best-move final ("I cannot analyze the position… PASS: I wrote
// the move and verified by reading it back").
func TestAcceptanceCannotPhrasingIsIncomplete(t *testing.T) {
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		textTurn("I cannot analyze the specific position from the image, so I cannot determine the best move. PASS: I wrote e2e4 to the file and verified it by reading it back."),
	}}

	result, err := Run(context.Background(), "write the best move", provider, Options{
		Registry:                tools.NewRegistry(),
		MaxTurns:                10,
		RequireCompletionSignal: true,
		SelfCorrect:             enabledSelfCorrector(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Incomplete {
		t.Fatalf("a 'cannot analyze/determine' admission must be Incomplete despite the bogus PASS; final=%q", result.FinalAnswer)
	}
}

// A pending/in_progress update_plan item is an AMBIGUOUS signal (the model may have
// finished without re-marking the step), so it must NOT by itself force INCOMPLETE.
// The gate nudges, but if the model keeps claiming completion with no continuation
// cue and no admission, the run finalizes as SUCCESS — trusting the completion claim
// over stale plan bookkeeping. (Only a continuation cue or self-report admission
// forces INCOMPLETE; see TestCompletionGate* and TestAcceptance*Admission*.)
func TestPendingPlanAloneDoesNotForceIncomplete(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(tools.NewUpdatePlanTool())
	done := "All set." // confident; no cue, no admission — only the plan is stale
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		planTurn("completed", "in_progress"),
		textTurn(done), textTurn(done), textTurn(done), textTurn(done), textTurn(done),
	}}

	result, err := Run(context.Background(), "do the multi-step task", provider, Options{
		Registry:                registry,
		MaxTurns:                10,
		RequireCompletionSignal: true,
		SelfCorrect:             enabledSelfCorrector(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Incomplete {
		t.Fatalf("pending-plan alone must NOT force Incomplete (stale bookkeeping != unfinished); reason=%q", result.IncompleteReason)
	}
	if result.FinalAnswer != done {
		t.Fatalf("final answer = %q, want the completion claim %q", result.FinalAnswer, done)
	}
	if !someRequestContains(provider.requests, continueNudgeMarker) {
		t.Fatalf("expected at least one continue-nudge before accepting completion")
	}
}

// BUG #2(b): when self-correct is on, the run must pass a one-time task-grounded
// acceptance check before success. A model that, when challenged, confirms its
// work meets the criterion (no admission) then finalizes as success — no regression.
func TestAcceptanceGroundedCompletionSucceeds(t *testing.T) {
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		textTurn("I implemented the function."),
		textTurn("PASS. I ran the verification and it passes; the output meets the task requirement."),
	}}

	result, err := Run(context.Background(), "implement the function", provider, Options{
		Registry:                tools.NewRegistry(),
		MaxTurns:                10,
		RequireCompletionSignal: true,
		SelfCorrect:             enabledSelfCorrector(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Incomplete {
		t.Fatalf("a verified completion must succeed, got Incomplete (%q)", result.IncompleteReason)
	}
	if result.FinalAnswer != "PASS. I ran the verification and it passes; the output meets the task requirement." {
		t.Fatalf("final answer = %q", result.FinalAnswer)
	}
	// Exactly one acceptance pass: the first "done" turn was challenged, the second confirmed.
	if len(provider.requests) != 2 {
		t.Fatalf("expected 2 turns (1 acceptance challenge + 1 confirmation), got %d", len(provider.requests))
	}
	if !someRequestContains(provider.requests, acceptanceNudgeMarker) {
		t.Fatalf("expected the task-grounded acceptance check to be demanded")
	}
}

// With the gate OFF (interactive/TUI default), even an admission is accepted as the
// final answer exactly as before — proving no behavior change for non-headless callers.
func TestAcceptanceGateOffLeavesAdmissionAsSuccess(t *testing.T) {
	admission := "I couldn't determine the value, so this is a guess."
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		textTurn(admission),
	}}

	result, err := Run(context.Background(), "do a thing", provider, Options{
		Registry: tools.NewRegistry(),
		MaxTurns: 10,
		// RequireCompletionSignal false, SelfCorrect nil — the legacy path.
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Incomplete {
		t.Fatalf("legacy path must never set Incomplete; final=%q", result.FinalAnswer)
	}
	if result.FinalAnswer != admission {
		t.Fatalf("final answer = %q, want the text verbatim (legacy behavior)", result.FinalAnswer)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("legacy path must not re-prompt; got %d turns", len(provider.requests))
	}
}

// The acceptance check is scoped to runs with self-correct on: a headless run
// WITHOUT a SelfCorrector accepts a confident completion immediately (the (b) gate
// is inert), so enabling RequireCompletionSignal alone adds no acceptance turn.
func TestAcceptanceGateRequiresSelfCorrect(t *testing.T) {
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		textTurn("Done. Implemented and it works."),
	}}

	result, err := Run(context.Background(), "implement it", provider, Options{
		Registry:                tools.NewRegistry(),
		MaxTurns:                10,
		RequireCompletionSignal: true,
		// SelfCorrect deliberately nil → acceptance gate (b) must not fire.
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Incomplete {
		t.Fatalf("confident completion without self-correct must succeed; reason=%q", result.IncompleteReason)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("acceptance gate must not fire without self-correct; got %d turns", len(provider.requests))
	}
	if someRequestContains(provider.requests, acceptanceNudgeMarker) {
		t.Fatalf("acceptance nudge must not be injected when self-correct is off")
	}
}

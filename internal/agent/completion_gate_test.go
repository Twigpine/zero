package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// someRequestContains reports whether any message sent to the provider across all
// turns contains substr — used to assert a continue nudge was actually injected.
func someRequestContains(requests []zeroruntime.CompletionRequest, substr string) bool {
	for _, req := range requests {
		for _, msg := range req.Messages {
			if strings.Contains(msg.Content, substr) {
				return true
			}
		}
	}
	return false
}

// planTurn is a turn that calls update_plan with the given item statuses (reusing
// the package's shared toolTurn helper).
func planTurn(statuses ...string) []zeroruntime.StreamEvent {
	items := make([]string, len(statuses))
	for i, s := range statuses {
		items[i] = `{"content":"step ` + s + `","status":"` + s + `"}`
	}
	return toolTurn("plan", "update_plan", `{"plan":[`+strings.Join(items, ",")+`]}`)
}

// BUG #1 regression: a no-tool-call turn that ends mid-step while plan items are
// still pending must NOT be accepted as success. The loop must re-prompt (bounded)
// and, if the model keeps stalling, finalize as INCOMPLETE — never success.
func TestCompletionGatePendingPlanContinuesThenIncomplete(t *testing.T) {
	// Mirrors the git-multibranch failure: plan with pending steps, then the model
	// keeps emitting "…Let me check the SSH configuration:" without acting.
	cue := "Now I need to configure the SSH server. Let me check the current SSH configuration:"
	registry := tools.NewRegistry()
	registry.Register(tools.NewUpdatePlanTool())

	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		planTurn("completed", "pending", "pending"),
		textTurn(cue), textTurn(cue), textTurn(cue), textTurn(cue), textTurn(cue),
	}}

	result, err := Run(context.Background(), "set up a git server", provider, Options{
		Registry:                registry,
		MaxTurns:                10,
		RequireCompletionSignal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Incomplete {
		t.Fatalf("expected Incomplete=true (model stalled with pending plan), got false; final=%q turns=%d", result.FinalAnswer, result.Turns)
	}
	// 1 plan turn + maxContinueNudges(3) nudged turns + 1 final stalling turn = 5.
	// Critically it did NOT stop at the first text turn (request 2) as success.
	if len(provider.requests) != 1+maxContinueNudges+1 {
		t.Fatalf("expected %d provider turns (1 plan + %d nudges + 1 final), got %d",
			1+maxContinueNudges+1, maxContinueNudges, len(provider.requests))
	}
	if !someRequestContains(provider.requests, continueNudgeMarker) {
		t.Fatalf("expected a continue nudge (%q) to be injected into the conversation", continueNudgeMarker)
	}
}

// A genuinely-complete single-turn answer (no plan, no continuation cue) must
// still finalize as success — the gate must not break short/read-only tasks.
func TestCompletionGateAcceptsGenuineCompletion(t *testing.T) {
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		textTurn("The file contains 42 lines."),
	}}

	result, err := Run(context.Background(), "count the lines", provider, Options{
		Registry:                tools.NewRegistry(),
		MaxTurns:                10,
		RequireCompletionSignal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Incomplete {
		t.Fatalf("genuine completion wrongly marked Incomplete; final=%q", result.FinalAnswer)
	}
	if result.FinalAnswer != "The file contains 42 lines." {
		t.Fatalf("final answer = %q, want the completed answer", result.FinalAnswer)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("expected exactly 1 turn (no spurious re-prompt), got %d", len(provider.requests))
	}
}

// A continuation-cue turn triggers a re-prompt, but once the model actually
// finishes (clean answer, no cue, no pending plan) the run exits as success — the
// nudge gives the model a path to a legitimate completion.
func TestCompletionGateContinuesOnCueThenSucceeds(t *testing.T) {
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		textTurn("Let me read the file:"),
		textTurn("Done. The file has 42 lines."),
	}}

	result, err := Run(context.Background(), "count the lines", provider, Options{
		Registry:                tools.NewRegistry(),
		MaxTurns:                10,
		RequireCompletionSignal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Incomplete {
		t.Fatalf("run wrongly marked Incomplete after the model completed; final=%q", result.FinalAnswer)
	}
	if result.FinalAnswer != "Done. The file has 42 lines." {
		t.Fatalf("final answer = %q, want the completed answer", result.FinalAnswer)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("expected 2 turns (1 cue re-prompted + 1 completion), got %d", len(provider.requests))
	}
	if !someRequestContains(provider.requests, continueNudgeMarker) {
		t.Fatalf("expected a continue nudge after the continuation-cue turn")
	}
}

// With the gate OFF (the interactive/TUI default), a continuation-cue turn is
// accepted as the final answer exactly as before — guaranteeing no behavior
// change for non-headless callers.
func TestCompletionGateOffPreservesLegacyBehavior(t *testing.T) {
	cue := "Let me check the config:"
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		textTurn(cue),
	}}

	result, err := Run(context.Background(), "do a thing", provider, Options{
		Registry: tools.NewRegistry(),
		MaxTurns: 10,
		// RequireCompletionSignal deliberately left false.
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Incomplete {
		t.Fatalf("legacy path must never set Incomplete; final=%q", result.FinalAnswer)
	}
	if result.FinalAnswer != cue {
		t.Fatalf("final answer = %q, want %q (legacy: text-only turn is the answer)", result.FinalAnswer, cue)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("legacy path must not re-prompt; got %d turns", len(provider.requests))
	}
}

// review #6: the continuation-cue detector must catch a mid-line action announcement
// that stops on a colon (the git-multibranch case), without flagging genuine closers
// — a recommendation, a plain summary colon, or a sign-off.
func TestContinuationCueMatching(t *testing.T) {
	cases := []struct {
		text string
		cue  bool
	}{
		{"Now I need to configure the SSH server. Let me check the current SSH configuration:", true},
		{"Let me read the file:", true},
		{"Now I'll run the tests:", true},
		{"Next, I suggest reviewing the changes.", false}, // recommendation, no colon
		{"Here is the summary:", false},                   // summary colon, no action lead-in
		{"Let me know if you need anything:", false},      // sign-off
		{"The function is implemented and all tests pass.", false},
	}
	for _, c := range cases {
		if got := endsWithContinuationCue(c.text); got != c.cue {
			t.Errorf("endsWithContinuationCue(%q) = %v, want %v", c.text, got, c.cue)
		}
	}
}

// review #4: a run that loops to the MaxTurns ceiling (always calling a tool, so it
// never reaches the no-tool-call gate) was reported as success. Under the headless
// gate, a max-turns cutoff is INCOMPLETE — the agent was stopped mid-run, not done.
func TestMaxTurnsCutoffIsIncompleteUnderGate(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(tools.NewUpdatePlanTool())
	toolEvery := toolTurn("c", "update_plan", `{"plan":[{"content":"step","status":"in_progress"}]}`)
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		toolEvery, toolEvery, toolEvery, toolEvery,
	}}

	result, err := Run(context.Background(), "keep working", provider, Options{
		Registry:                registry,
		MaxTurns:                2,
		RequireCompletionSignal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Incomplete {
		t.Fatalf("a max-turns cutoff under the gate must be Incomplete; final=%q", result.FinalAnswer)
	}
	if !strings.Contains(result.IncompleteReason, "max-turns") {
		t.Fatalf("IncompleteReason = %q, want it to cite max-turns", result.IncompleteReason)
	}
}

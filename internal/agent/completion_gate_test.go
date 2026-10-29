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

// These cases exercise the public run loop rather than the classifier helpers.
// Each final answer used to pass the headless completion gate even though it
// explicitly admitted that the requested work was not complete.
func TestCompletionGateRejectsCurrentHeadReviewAdmissions(t *testing.T) {
	admissions := []string{
		"I could not find the root cause.",
		"I could not find where to apply the fix.",
		"I could not locate the migration script.",
		"I was unable to determine where the regression was introduced, so the change was never applied.",
		"I could not locate the file. The change was never applied.",
		"I do not have the network access I needed, but the task is complete.",
		"I do not have a write tool available in this context and could not complete the task.",
		"Unable to complete the task (1):",
		"**Unable to verify (1):** - the migration was never run",
		"Unable to deploy (1): - production deployment failed.",
		"**Unable to verify (1):** - the migration did not run",
		"I could not run the migration because no migration tool is available, so I ran it manually instead, but it failed.",
		"I could not run the migration because no migration tool is available, so I ran it manually instead. It failed.",
		"I could not run the migration because no migration tool is available, so I ran it manually instead and it did not work.",
		"I could not deploy to our production cluster because no deployment tool is available, so I deployed to our staging cluster manually instead.",
		"I could not deploy to the main environment because no deployment tool is available, so I deployed to the staging environment manually instead.",
		"I could not deploy to the primary production environment because no deployment tool is available, so I deployed to the primary staging environment manually instead.",
		"I could not publish to an internal production registry because no release tool is available, so I published to an internal staging registry manually instead.",
		"I could not deploy to prod because no deployment tool is available, so I deployed to dev manually instead.",
		"I do not have the API key or the tools available in this session.",
	}

	for _, admission := range admissions {
		t.Run(admission, func(t *testing.T) {
			provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{textTurn(admission)}}
			result, err := Run(context.Background(), "complete the requested operation", provider, Options{
				Registry:                tools.NewRegistry(),
				MaxTurns:                2,
				RequireCompletionSignal: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !result.Incomplete {
				t.Fatalf("admission passed the production completion gate: %q", admission)
			}
		})
	}
}

func TestCompletionGateKeepsCurrentHeadReviewControlsComplete(t *testing.T) {
	complete := []string{
		"I could not find where the regression was introduced; the source is the parser boundary.",
		"**Unable to verify (1):** - MCP #3 claim was truncated.",
		"I don't have an update_plan tool available in this specialist context; only read-only exploration tools were provided.",
		"I could not deploy to our production cluster because no deployment tool is available, so I deployed to our production cluster manually instead.",
		"I could not deploy to the primary production environment because no deployment tool is available, so I deployed to the primary production environment manually instead.",
	}

	for _, answer := range complete {
		t.Run(answer, func(t *testing.T) {
			provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{textTurn(answer)}}
			result, err := Run(context.Background(), "complete the requested operation", provider, Options{
				Registry:                tools.NewRegistry(),
				MaxTurns:                2,
				RequireCompletionSignal: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Incomplete {
				t.Fatalf("completed control was rejected: %q (%s)", answer, result.IncompleteReason)
			}
		})
	}
}

func TestCollectiveInabilityStemsReachClassifierAndCompletionGate(t *testing.T) {
	cases := []struct {
		name       string
		answer     string
		incomplete bool
	}{
		{name: "could not leaves work blocked", answer: "We could not complete the requested migration because the build never succeeded.", incomplete: true},
		{name: "do not have leaves work blocked", answer: "We do not have enough evidence to answer the question.", incomplete: true},
		{name: "could not reports exhaustive negative finding", answer: "We could not find any remaining issues after inspecting every changed path."},
		{name: "do not have reports capability-only caveat", answer: "We do not have an update_plan tool available in this specialist context; only read-only exploration tools were provided."},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := selfReportedIncompletion(tc.answer)
			if got := reason != ""; got != tc.incomplete {
				t.Fatalf("selfReportedIncompletion(%q) returned %q; incomplete = %v, want %v", tc.answer, reason, got, tc.incomplete)
			}

			provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{textTurn(tc.answer)}}
			result, err := Run(context.Background(), "complete the requested operation", provider, Options{
				Registry:                tools.NewRegistry(),
				MaxTurns:                2,
				RequireCompletionSignal: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Incomplete != tc.incomplete {
				t.Fatalf("completion gate Incomplete = %v, want %v for %q (reason: %s)", result.Incomplete, tc.incomplete, tc.answer, result.IncompleteReason)
			}
		})
	}
}

func TestCompletionGateKeepsEvidenceAttachedToTheObligationItClears(t *testing.T) {
	cases := []struct {
		name       string
		answer     string
		incomplete bool
	}{
		{name: "affirmative observation cannot erase unfinished result", answer: "I could not find where the flag is set; the source is the parser, but the task is not done.", incomplete: true},
		{name: "affirmative observation result", answer: "I could not find where the flag is set; the source is the parser boundary."},
		{name: "different inline-code destination", answer: "I could not deploy the release to `production` because no deployment tool is available, so I deployed it to `staging` manually instead.", incomplete: true},
		{name: "same inline-code destination", answer: "I could not deploy the release to `production` because no deployment tool is available, so I deployed it to `production` manually instead."},
		{name: "different inline-code package", answer: "I could not publish the `alpha` package because no publishing tool is available, so I published the `beta` package manually instead.", incomplete: true},
		{name: "same inline-code package", answer: "I could not publish the `alpha` package because no publishing tool is available, so I published the `alpha` package manually instead."},
		{name: "fallback failed after and", answer: "I could not run the migration because no migration tool is available, so I ran it manually instead and it timed out.", incomplete: true},
		{name: "fallback failed in next sentence", answer: "I could not run the migration because no migration tool is available, so I ran it manually instead. It timed out.", incomplete: true},
		{name: "fallback cancelled in next sentence", answer: "I could not run the migration because no migration tool is available, so I ran it manually instead. It was cancelled.", incomplete: true},
		{name: "fallback crashed in next sentence", answer: "I could not run the migration because no migration tool is available, so I ran it manually instead. It crashed.", incomplete: true},
		{name: "fallback succeeded in next sentence", answer: "I could not run the migration because no migration tool is available, so I ran it manually instead. It was successful."},
		{name: "bookkeeping and substantive duty share inability", answer: "I could not record the plan and deploy the release because no update_plan tool is available.", incomplete: true},
		{name: "bookkeeping or patch share inability", answer: "I could not call update_plan or apply the patch because no update_plan tool is available.", incomplete: true},
		{name: "bookkeeping only", answer: "I could not record the plan because no update_plan tool is available."},
		{name: "operational heading with prose failure", answer: "**Unable to deploy (1):**\nProduction rollout failed.", incomplete: true},
		{name: "operational heading with bullet failure", answer: "**Unable to deploy (1):**\n- Production rollout failed.", incomplete: true},
		{name: "benign audit bucket", answer: "**Unable to verify (1):**\n- MCP #3 claim was truncated."},
		{name: "one suite merely read", answer: "I could not run the unit and integration tests because no test tool is available, so I ran the unit tests manually instead and read the integration tests.", incomplete: true},
		{name: "both suites executed", answer: "I could not run the unit and integration tests because no test tool is available, so I ran the unit and integration tests manually instead."},
		{name: "different deployment object", answer: "I could not deploy the release because no deployment tool is available, so I deployed the documentation manually instead.", incomplete: true},
		{name: "same deployment object", answer: "I could not deploy the release because no deployment tool is available, so I deployed the release manually instead."},
		{name: "pronoun carries deployment object", answer: "I could not deploy the release because no deployment tool is available, so I deployed it manually instead."},
		{name: "observation consequence says nothing changed", answer: "I could not find where the regression was introduced; the source is the parser, but nothing was changed.", incomplete: true},
		{name: "observation consequence still pending", answer: "I could not find where the regression was introduced; the source is the parser, though the fix is still pending.", incomplete: true},
		{name: "multiword inline-code destination differs", answer: "I could not deploy the release to `production cluster` because no deployment tool is available, so I deployed it to `staging cluster` manually instead.", incomplete: true},
		{name: "multiword inline-code destination declines exemption", answer: "I could not deploy the release to `production cluster` because no deployment tool is available, so I deployed it to `production cluster` manually instead.", incomplete: true},
		{name: "fallback killed by oom", answer: "I could not run the migration because no migration tool is available, so I ran it manually instead. The run was killed by the OOM killer.", incomplete: true},
		{name: "bookkeeping comma substantive duty", answer: "I could not record the plan, and deploy the release, because no update_plan tool is available.", incomplete: true},
		{name: "bookkeeping nor substantive duty", answer: "I could not record the plan nor deploy the release because no update_plan tool is available.", incomplete: true},
		{name: "bookkeeping as-well-as substantive duty", answer: "I could not record the plan as well as deploy the release because no update_plan tool is available.", incomplete: true},
		{name: "operational heading remains pending", answer: "**Unable to deploy (1):**\nProduction rollout is still pending.", incomplete: true},
		{name: "operational heading reports success", answer: "**Unable to deploy (1):**\nProduction rollout completed successfully."},
		{name: "one suite skipped", answer: "I could not run the unit and integration tests because no test tool is available, so I ran the unit tests manually instead and skipped the integration tests.", incomplete: true},
		{name: "one suite left for later", answer: "I could not run the unit and integration tests because no test tool is available, so I ran the unit tests manually instead and left the integration tests for later.", incomplete: true},
		{name: "release notes are not the release", answer: "I could not deploy the release because no deployment tool is available, so I deployed the release notes manually instead.", incomplete: true},
		{name: "same multiword deployment object", answer: "I could not deploy the release notes because no deployment tool is available, so I deployed the release notes manually instead."},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{textTurn(tc.answer)}}
			result, err := Run(context.Background(), "complete the requested operation", provider, Options{
				Registry:                tools.NewRegistry(),
				MaxTurns:                2,
				RequireCompletionSignal: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Incomplete != tc.incomplete {
				t.Fatalf("Incomplete = %v, want %v for %q (reason: %s)", result.Incomplete, tc.incomplete, tc.answer, result.IncompleteReason)
			}
		})
	}
}

func TestCompletionGateStructuralReviewerMatrix(t *testing.T) {
	cases := []struct {
		name       string
		answer     string
		incomplete bool
	}{
		{name: "capability note only", answer: "I don't have an update_plan tool available in this specialist context; only read-only exploration tools were provided."},
		{name: "capability note followed by subject-elided failure", answer: "I don't have a write tool available in this context and could not apply the required fix.", incomplete: true},
		{name: "same publish object completed", answer: "I could not publish the package because no publishing tool is available, so I published the package manually instead."},
		{name: "different publish object", answer: "I could not publish the package because no publishing tool is available, so I published the release notes manually instead.", incomplete: true},
		{name: "same destination completed", answer: "I could not publish the package for production because no publishing tool is available, so I published the package for production manually instead."},
		{name: "different destination", answer: "I could not publish the package for production because no publishing tool is available, so I published the package for staging manually instead.", incomplete: true},
		{name: "all coordinated tests completed", answer: "I could not run the unit and integration tests because no test tool is available, so I ran the unit and integration tests manually instead."},
		{name: "coordinated test subset", answer: "I could not run the unit and integration tests because no test tool is available, so I ran the unit tests manually instead.", incomplete: true},
		{name: "every test completed", answer: "I could not run every test because no test tool is available, so I ran every test manually instead."},
		{name: "smoke substituted for every test", answer: "I could not run every test because no test tool is available, so I ran a smoke test manually instead.", incomplete: true},
		{name: "affirmative fallback", answer: "I could not deploy the release because no deployment tool is available, so I deployed it manually instead."},
		{name: "explicitly successful fallback", answer: "I could not deploy the release because no deployment tool is available, so I successfully deployed it manually instead."},
		{name: "past-perfect affirmative fallback", answer: "I could not deploy the release because no deployment tool is available, so I had deployed it manually instead."},
		{name: "qualified affirmative fallback", answer: "I could not deploy the release because no deployment tool is available, so I deployed it manually instead, but it was successful."},
		{name: "negated fallback", answer: "I could not deploy the release because no deployment tool is available, so I never deployed it manually instead.", incomplete: true},
		{name: "partial fallback", answer: "I could not deploy the release because no deployment tool is available, so I partially deployed it manually instead.", incomplete: true},
		{name: "unsuccessful fallback", answer: "I could not deploy the release because no deployment tool is available, so I unsuccessfully deployed it manually instead.", incomplete: true},
		{name: "attempted fallback", answer: "I could not deploy the release because no deployment tool is available, so I attempted to deploy it manually instead.", incomplete: true},
		{name: "fallback crashed", answer: "I could not run the migration because no migration tool is available, so I ran it manually instead, but it crashed.", incomplete: true},
		{name: "benign counted audit bucket", answer: "**Unable to verify (1):** - MCP #3 claim was truncated."},
		{name: "counted operation rejected", answer: "**Unable to publish (1):** - registry rejected the request.", incomplete: true},
		{name: "exhaustive negative finding", answer: "I could not find any issues after inspecting every changed path."},
		{name: "blocked negative finding", answer: "I could not find any issues due to running out of time.", incomplete: true},
		{name: "counterfactual fallback", answer: "I could not deploy the release because no deployment tool is available, so I would have deployed it manually instead.", incomplete: true},
		{name: "timed out fallback", answer: "I could not deploy the release because no deployment tool is available, so I deployed it manually instead, but it timed out.", incomplete: true},
		{name: "cancelled fallback", answer: "I could not deploy the release because no deployment tool is available, so I deployed it manually instead, but it was cancelled.", incomplete: true},
		{name: "unfinished fallback", answer: "I could not deploy the release because no deployment tool is available, so I deployed it manually instead, but it did not finish.", incomplete: true},
		{name: "not successful fallback", answer: "I could not deploy the release because no deployment tool is available, so I deployed it manually instead, but it was not successful.", incomplete: true},
		{name: "incomplete fallback", answer: "I could not deploy the release because no deployment tool is available, so I deployed it manually instead, but it was incomplete.", incomplete: true},
		{name: "unlisted adversative fallback failure", answer: "I could not deploy the release because no deployment tool is available, so I deployed it manually instead, but it expired.", incomplete: true},
		{name: "full suite replaced by smoke", answer: "I could not run the full test suite because no test tool is available, so I ran a full smoke test manually instead.", incomplete: true},
		{name: "full suite completed", answer: "I could not run the full test suite because no test tool is available, so I ran the full test suite manually instead."},
		{name: "modifier-bearing coordinated failure", answer: "I don't have a write tool available in this context and therefore could not apply the required fix.", incomplete: true},
		{name: "unlisted modifier coordinated failure", answer: "I don't have a write tool available in this context and consequently could not apply the required fix.", incomplete: true},
		{name: "short modifier coordinated failure", answer: "I don't have a write tool available in this context and thus could not apply the required fix.", incomplete: true},
		{name: "multiword modifier coordinated failure", answer: "I don't have a write tool available in this context and as a result could not apply the required fix.", incomplete: true},
		{name: "subject elided negative action", answer: "I don't have a write tool available in this context and so never applied the required fix.", incomplete: true},
		{name: "subject elided unlisted never action", answer: "I don't have a write tool available in this context and so never touched the file.", incomplete: true},
		{name: "subject elided unlisted did-not action", answer: "I don't have a write tool available in this context and accordingly did not land it.", incomplete: true},
		{name: "reordered coordinated failure", answer: "I could not apply the required fix, and I don't have a write tool available in this context.", incomplete: true},
		{name: "separately punctuated coordinated failure", answer: "I don't have a write tool available in this context. I could not apply the required fix.", incomplete: true},
		{name: "direct capability footnote", answer: "I don't have an update_plan tool available in this specialist context; only read-only exploration tools were provided."},
		{name: "completion declaration cannot self certify", answer: "I could not run the migration because no migration tool is available, but the task is complete.", incomplete: true},
		{name: "plan capability note remains complete", answer: "I could not call update_plan because that tool is unavailable, but the task is complete."},
		// A MISSING TOOL OFFERED AS THE REASON THE WORK IS DONE. The row above
		// needs an inability stem to fire; drop the stem and the same
		// self-certification used to pass. The concessive and method-naming
		// rows beneath it are the shapes that must stay complete, and they are
		// what keeps this from becoming "any sentence mentioning a tool and a
		// completion is an admission".
		{name: "causal tool grant certifies the fix", answer: "No write tool is available, so the fix is complete.", incomplete: true},
		{name: "causal tool grant certifies the task", answer: "No write tool is available, so the task is complete.", incomplete: true},
		{name: "causal tool grant certifies via therefore", answer: "No edit tool is available, therefore the change is complete.", incomplete: true},
		{name: "concessive tool grant stays complete", answer: "I have no browser tool available here, yet the assignment is complete."},
		{name: "causal tool grant naming the method stays complete", answer: "No write tool is available to me, so the objective was achieved by reading alone."},
		// Parallel specialist footnotes: the update_plan row above passes and
		// these are the same sentence with a different tool in the grant.
		{name: "shell capability footnote", answer: "I don't have a shell tool available in this specialist context; only read-only tools were provided."},
		{name: "browser capability footnote", answer: "I don't have a browser tool available in this specialist context; only read-only tools were provided."},
		// A manual fallback is proved in its own sentence; an ordinary next
		// sentence must not withdraw it, and a blocked or refuting one must.
		{name: "manual fallback alone", answer: "I could not run the formatter because no formatter tool is available, so I checked it by hand."},
		{name: "manual fallback with neutral follow-on", answer: "I could not run the formatter because no formatter tool is available, so I checked it by hand. Documentation is outdated."},
		{name: "manual fallback with blocked follow-on", answer: "I could not run the formatter because no formatter tool is available, so I checked it by hand. Tests remain unverified.", incomplete: true},
		{name: "manual fallback refuted next sentence", answer: "I could not run the migration because no migration tool is available, so I ran it manually instead. It timed out.", incomplete: true},
		{name: "multiline counted operation failure", answer: "**Unable to deploy (1):**\n- production deployment failed.", incomplete: true},
		{name: "multiline asterisk counted operation failure", answer: "**Unable to deploy (2):**\n* production deployment failed.\n* staging deployment failed.", incomplete: true},
		{name: "multiline ordered counted operation failure", answer: "**Unable to deploy (1):**\n1. production deployment failed.", incomplete: true},
		{name: "multiline counted operation after blank line", answer: "**Unable to deploy (1):**\n\n+ production deployment failed.", incomplete: true},
		{name: "multiline counted observation prose failure", answer: "**Unable to verify (1):**\nThe migration was never run.", incomplete: true},
		{name: "multiline benign audit bucket", answer: "**Unable to verify (1):**\n- MCP #3 source record was truncated."},
		{name: "multiline benign audit asterisk after blank line", answer: "**Unable to verify (1):**\n\n* MCP #3 source record was truncated."},
		{name: "multiline benign audit ordered entry", answer: "**Unable to verify (1):**\n1. MCP #3 source record was truncated."},
		{name: "same-line benign audit ordered entry", answer: "**Unable to verify (1):** 1. MCP #3 source record was truncated."},
		{name: "multiline benign audit bullets", answer: "**Unable to verify (2):**\n- MCP #3 source record was truncated.\n- MCP #4 evidence was omitted."},
		{name: "multiline later failed bullet", answer: "**Unable to verify (2):**\n- MCP #3 source record was truncated.\n- the operation failed.", incomplete: true},
		{name: "bounded observation task not done", answer: "I could not find where the flag is set, so the task is not done.", incomplete: true},
		{name: "bounded observation fix unapplied", answer: "I could not find where the flag is set, so the fix has not been applied.", incomplete: true},
		{name: "bounded observation made no change", answer: "I could not find the value being set, so I made no change.", incomplete: true},
		{name: "bounded observation remains unfixed", answer: "I could not find where the flag is set, so it remains unfixed.", incomplete: true},
		{name: "bounded observation nothing changed", answer: "I could not find where the flag is set, so nothing changed.", incomplete: true},
		{name: "bounded observation unfinished consequence", answer: "I could not find where the flag is set, leaving the task unfinished.", incomplete: true},
		{name: "bounded observation change never made", answer: "I could not find where the flag is set; the change was never made.", incomplete: true},
		{name: "bounded observation stands incomplete", answer: "I could not find where the flag is set, and so the work stands incomplete.", incomplete: true},
		{name: "bounded observation relative consequence", answer: "I could not find where the flag is set, which leaves it broken.", incomplete: true},
		{name: "bounded observation gerund consequence", answer: "I could not find where the flag is set, meaning nothing was changed.", incomplete: true},
		{name: "bounded observation coordinated consequence", answer: "I could not find where the flag is set and it stays wrong.", incomplete: true},
		{name: "bounded observation next-sentence consequence", answer: "I could not find where the flag is set. It is still wrong.", incomplete: true},
		{name: "bounded observation result", answer: "I could not find where the flag is set after inspecting every registration path."},
		{name: "bounded observation comma-qualified result", answer: "I could not find where the flag is set, after inspecting every registration path."},
		{name: "bounded observation exhaustive evidence variant", answer: "I could not find where the flag is set, after carefully reviewing all registration paths."},
		{name: "bounded observation failed exhaustive evidence", answer: "I could not find where the flag is set, after failing every attempt.", incomplete: true},
		{name: "bounded observation identifies source", answer: "I could not find where the regression was introduced; the source is the parser boundary."},
		{name: "bounded observation disclaims concern", answer: "I could not find the flag being set anywhere outside tests, so the concern does not apply."},
		{name: "failed noun is not asserted outcome", answer: "I could not find any issues; the failed test in CI is a known flake."},
		{name: "failed predicate is asserted outcome", answer: "I could not find any issues; the operation failed.", incomplete: true},
		{name: "read-only no-change report", answer: "I could not find any remaining issues. I did not modify any files."},
		{name: "same-sentence read-only no-change report", answer: "I could not find any remaining issues; I did not modify any files."},
		{name: "read-only made-no-change report", answer: "I could not find any remaining issues. I made no changes."},
		{name: "affirmative cleanup result", answer: "I could not find any leftover references; the cleanup is complete."},
		{name: "required fix left unchanged", answer: "I could not find where to apply the required fix. I did not modify any files.", incomplete: true},
		{name: "singular recognized absence", answer: "I could not find a bug after inspecting every changed path."},
		{name: "singular deliverable remains admission", answer: "I could not find a solution after inspecting every changed path.", incomplete: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{textTurn(tc.answer)}}
			result, err := Run(context.Background(), "complete the requested operation", provider, Options{
				Registry:                tools.NewRegistry(),
				MaxTurns:                2,
				RequireCompletionSignal: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Incomplete != tc.incomplete {
				t.Fatalf("Incomplete = %v, want %v for %q (reason: %s)", result.Incomplete, tc.incomplete, tc.answer, result.IncompleteReason)
			}
		})
	}
}

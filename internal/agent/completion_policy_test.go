package agent

import (
	"strings"
	"testing"
)

func TestCompletionPolicyLocalEvidenceDecidesWithoutSemanticCheck(t *testing.T) {
	policy := newCompletionPolicy(false)

	complete := policy.evaluate("Done. All required checks pass.", completionContext{})
	if complete.Decision != CompletionComplete {
		t.Fatalf("confident completion decision = %q, want %q", complete.Decision, CompletionComplete)
	}

	incomplete := policy.evaluate("I couldn't verify the result, so this is my best guess.", completionContext{})
	if incomplete.Decision != CompletionIncomplete {
		t.Fatalf("admitted failure decision = %q, want %q", incomplete.Decision, CompletionIncomplete)
	}
}

func TestCompletionPolicyClauseLocalAdmissionMatrix(t *testing.T) {
	for _, text := range []string{
		"I don't have a browser tool available, so I could not inspect the page.",
		"No update_plan tool is available, so I wrote the plan manually, but I could not complete the requested analysis.",
		"I could not run the migration because no migration tool is available; the error is quoted in this answer.",
		"Unable to complete the migration (1): production deployment failed.",
		"**Unable to verify (1):** I could not complete the audit; the review is unfinished.",
		"I could not find any issues because I ran out of time before inspecting the code.",
		"I could not find any issues since I ran out of time before inspecting the code.",
		"I could not run the tests because no test tool is available, so I checked the style by hand.",
		"I could not run the tests because no test tool is available, so I checked the style by hand, but someone else will need to run the tests.",
		"I could not run the tests because no test tool is available, so I checked the style by hand, but the tests remain unverified.",
		"**Unable to verify (1):** - I could not complete the audit; the work remains unverified.",
	} {
		got := newCompletionPolicy(false).evaluate(text, completionContext{})
		if got.Decision != CompletionIncomplete {
			t.Errorf("incomplete report decided %q: %q", got.Decision, text)
		}
	}

	for _, text := range []string{
		"I don't have an update_plan tool available in this specialist context; only read-only exploration tools were provided.",
		"I could not record a plan because the update_plan tool isn't available, so I wrote it into this answer instead.",
		"I tried the automated route first; I could not run the formatter because no formatter tool is available, so I checked it by hand.",
		"**Unable to verify (1):** - MCP #3 claim was truncated.",
		"**Unable to verify (1):** - The source omitted MCP #3's full claim.",
		"From the source: I could not find any evidence that the issue is unresolved.",
		"I could not find any evidence that the issue is unresolved and the fix is still unverified.",
		"I could not find any remaining issues; separately, the documentation is outdated.",
	} {
		got := newCompletionPolicy(false).evaluate(text, completionContext{})
		if got.Decision != CompletionComplete {
			t.Errorf("complete report decided %q: %q (%s)", got.Decision, text, got.Reason)
		}
	}
}

func TestCompletionPolicyPreservesBoundedPlanStallProtection(t *testing.T) {
	policy := newCompletionPolicy(false)
	for attempt := 0; attempt < maxContinueNudges; attempt++ {
		got := policy.evaluate("Let me inspect the remaining configuration:", completionContext{PlanPending: true})
		if got.Decision != CompletionUncertain || got.Action != completionActionContinue {
			t.Fatalf("attempt %d = (%q, %q), want uncertain continue", attempt+1, got.Decision, got.Action)
		}
	}

	got := policy.evaluate("Let me inspect the remaining configuration:", completionContext{PlanPending: true})
	if got.Decision != CompletionIncomplete {
		t.Fatalf("decision after nudge budget = %q, want %q", got.Decision, CompletionIncomplete)
	}
}

func TestCompletionPolicyTreatsPendingPlanAsWeakEvidence(t *testing.T) {
	policy := newCompletionPolicy(false)
	for attempt := 0; attempt < maxContinueNudges; attempt++ {
		got := policy.evaluate("All set.", completionContext{PlanPending: true})
		if got.Decision != CompletionUncertain || got.Action != completionActionContinue {
			t.Fatalf("attempt %d = (%q, %q), want uncertain continue", attempt+1, got.Decision, got.Action)
		}
	}

	got := policy.evaluate("All set.", completionContext{PlanPending: true})
	if got.Decision != CompletionComplete {
		t.Fatalf("stale plan decision after nudge budget = %q, want %q", got.Decision, CompletionComplete)
	}
}

func TestCompletionPolicyAllowsExactlyOneRequiredSemanticCheck(t *testing.T) {
	policy := newCompletionPolicy(true)

	first := policy.evaluate("Implemented and tested.", completionContext{})
	if first.Decision != CompletionUncertain || first.Action != completionActionSemanticCheck {
		t.Fatalf("first decision = (%q, %q), want uncertain semantic check", first.Decision, first.Action)
	}

	second := policy.evaluate("PASS. The result meets the task criterion.", completionContext{})
	if second.Decision != CompletionComplete {
		t.Fatalf("post-check decision = %q, want %q", second.Decision, CompletionComplete)
	}
	if second.Action == completionActionSemanticCheck {
		t.Fatal("semantic check was requested more than once")
	}
}

func TestAcceptanceVerificationNudgeIncludesBoundedObjective(t *testing.T) {
	objective := "make the requested behavior observable"
	nudge := acceptanceVerificationNudge(objective)
	if !strings.Contains(nudge, "Task objective: "+objective) {
		t.Fatalf("semantic check was not grounded in the objective: %q", nudge)
	}
}

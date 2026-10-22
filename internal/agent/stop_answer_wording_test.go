package agent

import (
	"strings"
	"testing"
)

// THE ANSWER MUST CLAIM ONLY WHAT THE COUNTER ESTABLISHED.
//
// anyErrorCount establishes that the tool failed consecutively without a
// success. It does NOT establish that every failure differed: five A, five B and
// two C reach the content-blind bound of 12 without any signature repeating six
// times, and three of those errors were shared. The old wording called that
// "each with a different error".
//
// The signature bound has the inverse problem. A denial streak keys on the
// CATEGORY, and the prose behind it embeds the path or command refused, so it
// differs on every call. Calling that "the same error" is false in the other
// direction.
func TestStopAnswerDoesNotOverclaimTheFailurePattern(t *testing.T) {
	varied := toolFailureStopAnswer("bash", 12, toolFailureCauseVariedExecuted)
	if strings.Contains(varied, "each with a different error") {
		t.Errorf("the content-blind bound does not track that every failure differed: %q", varied)
	}
	if !strings.Contains(varied, "varying errors") {
		t.Errorf("the varied stop should still say the errors varied: %q", varied)
	}

	refused := toolFailureStopAnswer("bash", 6, toolFailureCauseSameRefusal)
	if strings.Contains(refused, "same error") {
		t.Errorf("a denial streak covers refusals of different paths, so it is not the same error: %q", refused)
	}
	if !strings.Contains(refused, "refused") {
		t.Errorf("a denial streak should say it was refused: %q", refused)
	}

	// The one claim that IS justified: a signature streak really did repeat the
	// same error signature, so that wording stays.
	same := toolFailureStopAnswer("bash", 6, toolFailureCauseSameError)
	if !strings.Contains(same, "same error") {
		t.Errorf("a signature streak may still be described as the same error: %q", same)
	}
}

// The mixed-signature case jatmn asked for, driven through the real counter
// rather than asserted about the wording in isolation: a run whose errors vary
// must trip the content-blind bound and must not be described as all-different.
func TestMixedSignatureStreakTripsTheContentBlindBound(t *testing.T) {
	var state guardState
	var outcome toolFailureOutcome
	// Five of one error, five of another, two of a third: twelve failures, no
	// signature repeating six times in a row.
	for index, output := range []string{
		"Error: A", "Error: A", "Error: A", "Error: A", "Error: A",
		"Error: B", "Error: B", "Error: B", "Error: B", "Error: B",
		"Error: C", "Error: C",
	} {
		outcome = state.observeToolResult("bash", true, true, output, DenialNone)
		if outcome.Stop && index < 11 {
			t.Fatalf("halted early at failure %d", index+1)
		}
	}
	if !outcome.Stop {
		t.Fatal("twelve consecutive failures did not trip the content-blind bound")
	}
	if outcome.Cause != toolFailureCauseVariedExecuted {
		t.Errorf("a mixed-signature streak of executed failures should report varied-executed, got %v", outcome.Cause)
	}
	answer := toolFailureStopAnswer("bash", outcome.Count, outcome.Cause)
	if strings.Contains(answer, "each with a different error") {
		t.Errorf("three of these errors were shared, so they were not each different: %q", answer)
	}
}

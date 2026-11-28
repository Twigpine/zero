package tools

import "testing"

// THE BUDGET HAS TO COUNT WHAT THE MODEL ACTUALLY RECEIVES.
//
// The enforcement notices are prepended to the model view on the way out, so a
// disclosed call costs more context than result.Output alone. Measuring the bare
// output undercounts every one of them, and the undercount grows with the notice
// rather than being a fixed slack.
func TestOutcomeMeasuresTheNoticesTheModelReceives(t *testing.T) {
	const notice = "denyRead is configured, so the Windows sandbox uses the token shape without WRITE_RESTRICTED (#869)"
	const output = "exit status 0"

	bare := finalizeToolOutcome(Result{Status: StatusOK, Output: output}, output)
	disclosed := finalizeToolOutcome(Result{Status: StatusOK, Output: output, EnforcementNotices: []string{notice}}, output)

	if disclosed.Outcome.Diagnostics.ModelBytes <= bare.Outcome.Diagnostics.ModelBytes {
		t.Errorf("a disclosed result measured %d model bytes, no more than the undisclosed %d, so the notice is uncounted",
			disclosed.Outcome.Diagnostics.ModelBytes, bare.Outcome.Diagnostics.ModelBytes)
	}
	if want := len(WithEnforcementNotices(output, []string{notice})); disclosed.Outcome.Diagnostics.ModelBytes != want {
		t.Errorf("model bytes = %d, want %d (the canonical output the model is handed)",
			disclosed.Outcome.Diagnostics.ModelBytes, want)
	}
	if disclosed.Outcome.Diagnostics.EstimatedModelTokens <= bare.Outcome.Diagnostics.EstimatedModelTokens {
		t.Errorf("estimated model tokens did not grow with the notice: %d vs %d",
			disclosed.Outcome.Diagnostics.EstimatedModelTokens, bare.Outcome.Diagnostics.EstimatedModelTokens)
	}
}

// And the stored view stays bare, or the notices ship twice: ModelOutput
// prepends them to whatever ModelView holds.
func TestOutcomeModelViewDoesNotCarryTheNoticesItself(t *testing.T) {
	const notice = "sandbox notice"
	const output = "exit status 0"

	result := finalizeToolOutcome(Result{Status: StatusOK, Output: output, EnforcementNotices: []string{notice}}, output)
	if result.Outcome.ModelView != output {
		t.Errorf("ModelView = %q, want the bare output %q; ModelOutput prepends the notice, so storing it here sends it twice",
			result.Outcome.ModelView, output)
	}
}

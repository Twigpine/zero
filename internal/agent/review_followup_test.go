package agent

import (
	"context"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestReviewCountedObservationAndFallbackResults(t *testing.T) {
	const migration = "I could not run the migration because no migration tool is available, so I ran it manually instead. "
	cases := []struct {
		text       string
		incomplete bool
	}{
		{"**Unable to verify (1):**\nSeveral checks failed.", true},
		{"**Unable to verify (1):**\nDeployment never started.", true},
		{"**Unable to verify (1):**\n\nSeveral checks failed.", true},
		{"**Unable to verify (1):** - Several checks failed.", true},
		{"**Unable to verify (1):**\n- Several checks failed.", true},
		{"**Unable to verify (1):**\nThe source claim was truncated.", false},
		{"**Unable to verify (1):** - The source claim was truncated.", false},
		{"**Unable to verify (1):**\n- The source claim was truncated.", false},
		{"**Unable to verify (0):**", false},
		{migration + "Exit code 1.", true},
		{migration + "Exit code: 130.", true},
		{migration + "Exit status -1073741819.", true},
		{migration + "Return code 2.", true},
		{migration + "The command exited with code 1.", true},
		{migration + "The process returned status 3.", true},
		{migration + "The process was terminated unexpectedly.", true},
		{migration + "The command ended prematurely.", true},
		{migration + "Execution ended without completing the work.", true},
		{migration + "Exit code 0.", false},
		{migration + "Return status 0.", false},
		{migration + "The process exited with code 0.", false},
		{migration + "The process completed successfully.", false},
		{migration + "The README explains exit code 1.", false},
		{migration + "Exit code 1 is documented in the README.", false},
		{migration + "Separately, another project's command failed.", false},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			if got := selfReportedIncompletion(tc.text) != ""; got != tc.incomplete {
				t.Errorf("classifier incomplete=%v, want %v", got, tc.incomplete)
			}
			decision := newCompletionPolicy(false).evaluate(tc.text, completionContext{}).Decision
			if got := decision == CompletionIncomplete; got != tc.incomplete {
				t.Errorf("policy decision=%s, want incomplete=%v", decision, tc.incomplete)
			}
			provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{textTurn(tc.text), textTurn(tc.text), textTurn(tc.text), textTurn(tc.text), textTurn(tc.text)}}
			result, err := Run(context.Background(), "complete the requested work", provider, Options{Registry: tools.NewRegistry(), MaxTurns: 6, RequireCompletionSignal: true})
			if err != nil {
				t.Fatal(err)
			}
			if result.Incomplete != tc.incomplete {
				t.Errorf("production gate incomplete=%v, want %v", result.Incomplete, tc.incomplete)
			}
		})
	}
}

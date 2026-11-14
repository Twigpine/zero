package agent

import (
	"context"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestReviewCompletionOutcomeRelationships(t *testing.T) {
	const formatter = "I could not run the formatter because no formatter tool is available, so I checked it by hand. "
	cases := []struct {
		text       string
		incomplete bool
	}{
		{"**Unable to deploy (1):**\nRollback completed successfully.", true},
		{"**Unable to deploy (1):**\nDeployment rollback completed successfully.", true},
		{"**Unable to deploy (0):**", false},
		{"**Unable to deploy (1):** - Rollback completed successfully.", true},
		{"**Unable to deploy (1):**\n1. Rollback completed successfully.", true},
		{"**Unable to deploy (2):**\n- Rollout completed successfully.\n- Rollback completed successfully.", true},
		{"**Unable to deploy (1):**\nRollout completed successfully.", false},
		{"**Unable to deploy (1):** - Rollout completed successfully.", false},
		{"I could not find where the flag is set; the source is the parser and the fix is outstanding.", true},
		{"I could not find where the flag is set; the source is the parser and the change is pending.", true},
		{"I could not find where the flag is set; the source is the parser.", false},
		{"I could not find any evidence of an unverified fix.", false},
		{"I could not find any evidence of an unapplied change.", false},
		{"I could not find any evidence that the issue is unresolved.", false},
		{"I could not find any evidence of an unverified fix; the rollout is unverified.", true},
		{formatter + "Several files are still unformatted.", true},
		{formatter + "Two files still need formatting.", true},
		{formatter + "Formatting is only partly done.", true},
		{formatter + "I only got through half of the files.", true},
		{formatter + "Unfortunately the check was interrupted.", true},
		{formatter + "Documentation is outdated.", false},
		{formatter + "The documentation is outdated.", false},
		{formatter + "The README explains the style rules.", false},
		{formatter + "It is documented in the README.", false},
		{formatter + "The check completed successfully.", false},
		{formatter + "Formatting is complete.", false},
		{formatter + "All files are formatted.", false},
		{"I could not run the migration because no migration tool is available, so I ran it manually instead, but only 30 of 100 rows moved.", true},
		{"I could not run the migration because no migration tool is available, so I ran it manually instead and only 30 of 100 rows moved.", true},
		{"I could not run the migration because no migration tool is available, so I ran it manually instead and 70 rows remain pending.", true},
		{"I could not run the migration because no migration tool is available, so I ran it manually instead.", false},
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

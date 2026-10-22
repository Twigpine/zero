package tools

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/execution"
)

const appliedNotice = "denyRead is configured, so the write jail is not confining writes"

// THE PLAN IS NOT THE APPLICATION.
//
// addSandboxMeta writes the plan's notices at plan time, before anything runs,
// so promoting them into the user-visible disclosure unconditionally claims a
// token trade for a command that may never have started. The execution outcome
// is the thing that knows whether a process existed, and it applies the same
// launched-and-planned rule hooks and plugins use.
func TestCommandNoticesFollowAppliedExecutionState(t *testing.T) {
	planned := map[string]string{sandboxNoticesMeta: appliedNotice}

	t.Run("launched", func(t *testing.T) {
		outcome := execution.Outcome{
			Kind:        execution.OutcomeSuccess,
			Launched:    true,
			Enforcement: execution.Enforcement{Notices: []string{appliedNotice}},
		}
		got := finalizeToolOutcome(Result{
			Status: StatusOK, Output: "ran", Meta: planned, ExecutionOutcome: &outcome,
		}, "ran")
		if len(got.EnforcementNotices) != 1 {
			t.Fatalf("a launched command lost its disclosure: %#v", got.EnforcementNotices)
		}
		if !strings.Contains(got.ModelOutput(), appliedNotice) {
			t.Errorf("the model view does not carry it: %q", got.ModelOutput())
		}
	})

	// A command that ran and then FAILED still ran under the token it was given,
	// and a failure is when the reader most needs to know that. Only success was
	// pinned, so a rule keyed on the result status rather than on the launch would
	// have passed everything above.
	t.Run("launched and failed", func(t *testing.T) {
		outcome := execution.Outcome{
			Kind:        execution.OutcomeApplicationFailure,
			Launched:    true,
			Enforcement: execution.Enforcement{Notices: []string{appliedNotice}},
		}
		got := finalizeToolOutcome(Result{
			Status: StatusError, Output: "exit status 1: permission denied", Meta: planned, ExecutionOutcome: &outcome,
			Display: Display{Summary: "command failed", Kind: "shell"},
		}, "exit status 1: permission denied")
		if len(got.EnforcementNotices) != 1 {
			t.Fatalf("a command that ran and failed lost its disclosure: %#v", got.EnforcementNotices)
		}
		model := got.ModelOutput()
		for _, want := range []string{appliedNotice, "permission denied"} {
			if !strings.Contains(model, want) {
				t.Errorf("the model view of a failed command is missing %q:\n%s", want, model)
			}
		}
		summary := got.HumanDisplay().Summary
		for _, want := range []string{appliedNotice, "command failed"} {
			if !strings.Contains(summary, want) {
				t.Errorf("the display of a failed command is missing %q: %q", want, summary)
			}
		}
	})

	t.Run("never launched", func(t *testing.T) {
		outcome := execution.Outcome{
			Kind:        execution.OutcomeSandboxSetupFailure,
			Launched:    false,
			Enforcement: execution.Enforcement{Notices: []string{appliedNotice}},
		}
		got := finalizeToolOutcome(Result{
			Status: StatusError, Output: "could not start", Meta: planned, ExecutionOutcome: &outcome,
		}, "could not start")
		if len(got.EnforcementNotices) != 0 {
			t.Fatalf("a command that never started claimed a token trade: %#v", got.EnforcementNotices)
		}
		if strings.Contains(got.ModelOutput(), appliedNotice) {
			t.Errorf("the model view claims it anyway: %q", got.ModelOutput())
		}
	})

	// The plan metadata is diagnostics and stays put either way, so the record of
	// what was intended is not lost with the claim about what happened.
	t.Run("metadata survives", func(t *testing.T) {
		outcome := execution.Outcome{Kind: execution.OutcomeSandboxSetupFailure, Launched: false}
		got := finalizeToolOutcome(Result{
			Status: StatusError, Output: "x", Meta: planned, ExecutionOutcome: &outcome,
		}, "x")
		if got.Meta[sandboxNoticesMeta] != appliedNotice {
			t.Errorf("the planned notice was erased from diagnostics: %q", got.Meta[sandboxNoticesMeta])
		}
	})

	// A tool with no execution outcome at all still promotes from metadata, so
	// this did not silently drop disclosure for a path that has no outcome.
	t.Run("no execution outcome", func(t *testing.T) {
		got := finalizeToolOutcome(Result{Status: StatusOK, Output: "x", Meta: planned}, "x")
		if len(got.EnforcementNotices) != 1 {
			t.Errorf("a tool without an execution outcome lost its disclosure: %#v", got.EnforcementNotices)
		}
	})
}

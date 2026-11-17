package hooks

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

// auditedDispatcher wires a real audit store to a dispatcher whose hook result
// is whatever the caller wants, and returns the events that survived the write.
func auditedDispatcher(t *testing.T, hook Definition, result commandResult) []AuditEvent {
	t.Helper()
	store, err := NewAuditStore(AuditStoreOptions{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl")})
	if err != nil {
		t.Fatalf("NewAuditStore: %v", err)
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Config: beforeToolConfig(hook),
		Audit:  store,
		run: func(context.Context, string, []string, []byte, string, []string) commandResult {
			return result
		},
	})
	dispatcher.Dispatch(context.Background(), DispatchInput{Event: EventBeforeTool, ToolName: "bash", ToolCallID: "call_1"})

	// READ BACK FROM DISK, not from the in-memory event the append returned. The
	// durable reader is the consumer this field exists for.
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	return events
}

func completedResults(t *testing.T, events []AuditEvent) []AuditResult {
	t.Helper()
	for _, event := range events {
		if len(event.Results) > 0 {
			return event.Results
		}
	}
	t.Fatalf("no completed record was written: %#v", events)
	return nil
}

// THE TRANSIENT DISPATCH RESULT IS NOT WHERE THIS FACT CAN LIVE.
//
// recordCompleted kept an exit code, stdout and stderr, and the notice is
// deliberately in none of those. So once the dispatch result was gone, an audit
// or recovery reader could not tell that a hook had run under the weakened
// DenyRead token, whatever the hook did afterwards.
func TestTheAuditRecordKeepsTheEnforcementNotice(t *testing.T) {
	const notice = "denyRead is configured, so the write jail is not confining writes"

	for _, testCase := range []struct {
		name   string
		result commandResult
	}{
		{"launched and succeeded", commandResult{ExitCode: 0, Stdout: "looks fine", Notices: []string{notice}}},
		{"vetoed the tool", commandResult{ExitCode: 2, Stderr: "policy violation", Notices: []string{notice}}},
		{"silent hook", commandResult{ExitCode: 0, Notices: []string{notice}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			results := completedResults(t, auditedDispatcher(t,
				Definition{ID: "policy", Event: EventBeforeTool, Command: "policy-check", Enabled: true},
				testCase.result))
			if len(results) != 1 {
				t.Fatalf("results = %#v, want one", results)
			}
			if len(results[0].EnforcementNotices) != 1 || results[0].EnforcementNotices[0] != notice {
				t.Errorf("the durable record lost the disclosure: %#v", results[0])
			}
			// The existing semantics are untouched.
			if results[0].ExitCode != testCase.result.ExitCode {
				t.Errorf("ExitCode = %d, want %d", results[0].ExitCode, testCase.result.ExitCode)
			}
			if results[0].Stdout != testCase.result.Stdout || results[0].Stderr != testCase.result.Stderr {
				t.Errorf("stdout/stderr changed: %#v", results[0])
			}
		})
	}
}

// A hook with nothing to disclose writes exactly what it wrote before, so a
// reader of historical records sees no difference.
func TestAnOrdinaryHookWritesNoEnforcementField(t *testing.T) {
	results := completedResults(t, auditedDispatcher(t,
		Definition{ID: "policy", Event: EventBeforeTool, Command: "policy-check", Enabled: true},
		commandResult{ExitCode: 0, Stdout: "looks fine"}))
	if len(results[0].EnforcementNotices) != 0 {
		t.Errorf("a hook with no disclosure recorded one: %#v", results[0])
	}
}

// And the durable record inherits the launch-state rule rather than restating
// it: a hook that never started records no enforcement claim.
func TestTheAuditRecordMakesNoClaimForAHookThatNeverLaunched(t *testing.T) {
	store, err := NewAuditStore(AuditStoreOptions{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl")})
	if err != nil {
		t.Fatalf("NewAuditStore: %v", err)
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Config:    beforeToolConfig(Definition{ID: "policy", Event: EventBeforeTool, Command: "policy-check", Enabled: true}),
		Audit:     store,
		Cwd:       t.TempDir(),
		Execution: newRunnerFor(&noticePreparer{build: func() *exec.Cmd { return exec.Command("definitely-not-a-real-binary-zzz") }}),
	})
	dispatcher.Dispatch(context.Background(), DispatchInput{Event: EventBeforeTool, ToolName: "bash", ToolCallID: "call_1"})
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	for _, result := range completedResults(t, events) {
		if len(result.EnforcementNotices) != 0 {
			t.Errorf("the durable record claims an enforcement trade for a hook that never started: %#v", result)
		}
	}
}

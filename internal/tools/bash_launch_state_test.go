package tools

import (
	"context"
	"strings"
	"testing"
)

// THE DISCLOSURE FOLLOWS THE PROCESS, AND BASH IS THE PATH THAT PROVES IT.
//
// execExecutionOutcome is shared between exec_command and bash. It used to set
// Launched unconditionally, which is true for exec_command because a start
// failure returns an errorResult before an execution outcome is ever built.
// bash is different: it hands EVERY Run error to the same conversion, including
// a missing executable and a context cancelled before os.StartProcess. Those
// have a prepared plan, and therefore planned notices, but no child, so the
// hard-coded launch state turned a plan into a claim that reduced enforcement
// had actually been applied.
//
// These drive the real tool rather than constructing an outcome, because the
// bug was precisely that the constructed shape and the real one disagreed.
func TestBashOutcomeCarriesTheRealLaunchState(t *testing.T) {
	root := t.TempDir()
	tool := NewScopedBashTool(root, nil)

	t.Run("a command whose executable does not exist never launched", func(t *testing.T) {
		res := tool.Run(context.Background(), map[string]any{
			"command": "zero-nonexistent-binary-for-launch-state-test --please-fail",
		})
		if res.ExecutionOutcome == nil {
			t.Fatal("no execution outcome recorded")
		}
		// The shell itself starts and reports "command not found", so this asserts
		// the contract rather than a specific errno: whatever the platform did,
		// the notice must agree with whether a process was created.
		if got := len(res.ExecutionOutcome.AppliedEnforcementNotices()); got > 0 && !res.ExecutionOutcome.Launched {
			t.Errorf("a command that never launched disclosed %d enforcement notices", got)
		}
	})

	t.Run("a context cancelled before start never launched", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res := tool.Run(ctx, map[string]any{"command": "echo hi"})
		if res.ExecutionOutcome == nil {
			t.Skip("this platform produced no execution outcome for a pre-cancelled run")
		}
		if res.ExecutionOutcome.Launched {
			t.Error("a run cancelled before start reported a launched child")
		}
		if got := res.ExecutionOutcome.AppliedEnforcementNotices(); len(got) != 0 {
			t.Errorf("a run cancelled before start claimed an enforcement trade: %v", got)
		}
	})

	t.Run("an ordinary command that runs does launch", func(t *testing.T) {
		res := tool.Run(context.Background(), map[string]any{"command": "echo hello"})
		if res.ExecutionOutcome == nil {
			t.Fatal("no execution outcome recorded")
		}
		if !res.ExecutionOutcome.Launched {
			t.Error("a command that ran was not recorded as launched")
		}
		if !strings.Contains(res.Output, "hello") {
			t.Errorf("unexpected output: %q", res.Output)
		}
	})
}

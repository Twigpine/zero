package hooks

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/execution"
)

const launchStateNotice = "denyRead is configured, so the write jail is not confining writes"

// shellCommand builds a portable child that the platform can actually launch.
func shellCommand(script string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd.exe", "/c", script)
	}
	return exec.Command("/bin/sh", "-c", script)
}

// noticePreparer plans a command carrying an enforcement notice, and can fail
// the way the sandbox does before the child exists.
type noticePreparer struct {
	prepareErr error
	build      func() *exec.Cmd
}

func (preparer *noticePreparer) PrepareExecution(_ context.Context, request execution.Request) (execution.PreparedCommand, error) {
	if preparer.prepareErr != nil {
		return execution.PreparedCommand{}, preparer.prepareErr
	}
	command := preparer.build
	if command == nil {
		command = func() *exec.Cmd { return exec.Command(request.Command.Name, request.Command.Args...) }
	}
	return execution.PreparedCommand{
		Command:     command(),
		Enforcement: execution.Enforcement{Notices: []string{launchStateNotice}},
	}, nil
}

// PLANNING A WRAPPED COMMAND IS NOT PROOF THAT ANYTHING RAN.
//
// Enforcement.Notices describes the shape the command was PREPARED to run
// under. Copying it straight out made the completed-enforcement claim for
// commands that never existed: a sandbox setup failure and a missing executable
// are both decided before the child launches, so the hook message told the
// operator the write jail had been traded away for a process that never
// started.
//
// Everything after launch keeps the disclosure, including a nonzero exit, a
// timeout and a cancellation: those happened to a child that really did run
// under that token.
//
// Driven through the execution runner rather than a hand-built commandResult,
// because the projection is the thing under test.
func TestTheHookRunnerOnlyDisclosesEnforcementForAChildThatLaunched(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		preparer     *noticePreparer
		timeout      time.Duration
		wantNotice   bool
		wantTimedOut bool
	}{
		{
			name:       "sandbox setup failed before the child existed",
			preparer:   &noticePreparer{prepareErr: errors.New("could not build the restricted token")},
			wantNotice: false,
		},
		{
			name: "the executable was never found",
			preparer: &noticePreparer{build: func() *exec.Cmd {
				return exec.Command("definitely-not-a-real-binary-zzz")
			}},
			wantNotice: false,
		},
		{
			name:       "the child launched and succeeded",
			preparer:   &noticePreparer{build: func() *exec.Cmd { return shellCommand("exit 0") }},
			wantNotice: true,
		},
		{
			name:       "the child launched and exited nonzero",
			preparer:   &noticePreparer{build: func() *exec.Cmd { return shellCommand("exit 3") }},
			wantNotice: true,
		},
		{
			name:         "the child launched and timed out",
			preparer:     &noticePreparer{build: func() *exec.Cmd { return shellCommand(sleepScript) }},
			timeout:      150 * time.Millisecond,
			wantNotice:   true,
			wantTimedOut: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			if testCase.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, testCase.timeout)
				defer cancel()
			}
			run := executionCommandRunner(execution.NewRunner(testCase.preparer))
			result := run(ctx, "hook-command", nil, nil, t.TempDir(), nil)

			if result.TimedOut != testCase.wantTimedOut {
				t.Fatalf("TimedOut = %v, want %v: the case did not reach the outcome kind it is named for", result.TimedOut, testCase.wantTimedOut)
			}
			if got := len(result.Notices) > 0; got != testCase.wantNotice {
				t.Fatalf("notices present = %v, want %v: %#v", got, testCase.wantNotice, result.Notices)
			}
			// Asserted on Notices, which is where the disclosure travels. It used
			// to be folded into the hook message as well, and the agent loop now
			// merges Notices into the typed enforcement slice for both hook
			// phases, so carrying it in both places delivered it twice.
			notices := strings.Join(result.Notices, "\n")
			if testCase.wantNotice && !strings.Contains(notices, launchStateNotice) {
				t.Errorf("a launched child lost its disclosure:\n%s", notices)
			}
			if !testCase.wantNotice && strings.Contains(notices, launchStateNotice) {
				t.Errorf("a child that never launched claimed the token was traded away:\n%s", notices)
			}
			if message := hookMessage(result); strings.Contains(message, launchStateNotice) {
				t.Errorf("the disclosure also rode along in the hook message, so it reaches the model twice:\n%s", message)
			}
		})
	}
}

// And the same rule has to hold on the veto path, which returns early and is what
// the model actually sees. Both channels are checked: the notice travels on
// Notices, and Reason must not grow a copy of it either.
func TestAVetoingHookThatNeverLaunchedClaimsNoEnforcement(t *testing.T) {
	dispatcher := NewDispatcher(DispatcherOptions{
		Config: beforeToolConfig(Definition{ID: "policy", Event: EventBeforeTool, Command: "policy-check", Enabled: true}),
		Cwd:    t.TempDir(),
		// A missing executable rather than a prepare error: a prepare error never
		// builds the PreparedCommand, so its outcome carries no planned notice and
		// the assertion below would hold with the launch gate deleted. This shape
		// plans the notice and then fails to launch.
		Execution: execution.NewRunner(&noticePreparer{build: func() *exec.Cmd {
			return exec.Command("definitely-not-a-real-binary-zzz")
		}}),
	})
	outcome := dispatcher.Dispatch(context.Background(), DispatchInput{Event: EventBeforeTool, ToolName: "bash"})
	if !outcome.Blocked {
		t.Fatal("SETUP INVALID: a beforeTool hook that could not run must fail closed, or the veto path is not exercised")
	}
	if len(outcome.Notices) != 0 {
		t.Errorf("the dispatch outcome claims an enforcement trade for a hook that never started: %#v", outcome.Notices)
	}
	if strings.Contains(outcome.Reason, launchStateNotice) {
		t.Errorf("the veto reason claims an enforcement trade for a hook that never started:\n%s", outcome.Reason)
	}
}

// A launched hook still carries it all the way into the dispatch outcome, on the
// typed slice and only there.
func TestALaunchedHookCarriesTheNoticeIntoTheDispatchOutcome(t *testing.T) {
	dispatcher := NewDispatcher(DispatcherOptions{
		Config:    beforeToolConfig(Definition{ID: "policy", Event: EventBeforeTool, Command: "policy-check", Enabled: true}),
		Cwd:       t.TempDir(),
		Execution: execution.NewRunner(&noticePreparer{build: func() *exec.Cmd { return shellCommand("exit 2") }}),
	})
	outcome := dispatcher.Dispatch(context.Background(), DispatchInput{Event: EventBeforeTool, ToolName: "bash"})
	if !outcome.Blocked {
		t.Fatal("SETUP INVALID: the hook did not veto, so the reason path is not exercised")
	}
	if got := strings.Count(strings.Join(outcome.Notices, "\n"), launchStateNotice); got != 1 {
		t.Errorf("a hook that really ran under the weakened token disclosed it %d times on Notices, want once: %#v", got, outcome.Notices)
	}
	if strings.Contains(outcome.Reason, launchStateNotice) {
		t.Errorf("the veto reason carries a second copy of the notice:\n%s", outcome.Reason)
	}
}

// newRunnerFor keeps the audit tests readable without importing the execution
// package into every file that needs one.
func newRunnerFor(preparer *noticePreparer) *execution.Runner {
	return execution.NewRunner(preparer)
}

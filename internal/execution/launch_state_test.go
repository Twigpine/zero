package execution

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"testing"
)

const launchStateNotice = "denyRead is configured, so the write jail is not confining writes"

func launchStateShell(ctx context.Context, script string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd.exe", "/c", script)
	}
	return exec.CommandContext(ctx, "/bin/sh", "-c", script)
}

// launchStatePreparer plans a command carrying an enforcement notice, and can
// make the adapter report fail after the child has already run.
type launchStatePreparer struct {
	script    string
	reportErr error
	missing   bool
}

func (p *launchStatePreparer) PrepareExecution(ctx context.Context, _ Request) (PreparedCommand, error) {
	command := launchStateShell(ctx, p.script)
	if p.missing {
		command = exec.CommandContext(ctx, "definitely-not-a-real-binary-zzz")
	}
	prepared := PreparedCommand{
		Command:     command,
		Enforcement: Enforcement{Notices: []string{launchStateNotice}},
	}
	if p.reportErr != nil {
		prepared.Report = func() (AdapterReport, error) { return AdapterReport{}, p.reportErr }
	}
	return prepared, nil
}

func captured(t *testing.T, ctx context.Context, p *launchStatePreparer) CapturedResult {
	t.Helper()
	return NewRunner(p).ExecuteCaptured(ctx, CapturedRequest{Request: Request{
		Origin:           OriginHook,
		Mode:             ModeCaptured,
		Command:          Command{Name: "irrelevant"},
		WorkingDirectory: t.TempDir(),
		WorkspaceRoots:   []string{t.TempDir()},
		Approval:         ApprovalContext{PolicyVersion: PolicyVersion},
	}})
}

// THE OUTCOME KIND IS NOT A LAUNCH-STATE FIELD, IN EITHER DIRECTION.
//
// Deriving launch from the terminal kind is wrong twice over. The adapter report
// is read AFTER Run, so a child that really ran and then produced an unreadable
// report is rewritten to a setup failure: inference drops a disclosure that did
// apply. And a context already cancelled before os.StartProcess still selects a
// cancellation, so inference claims reduced enforcement for a process that never
// existed.
func TestLaunchStateIsRecordedNotInferred(t *testing.T) {
	t.Run("ran, then the adapter report failed", func(t *testing.T) {
		result := captured(t, context.Background(), &launchStatePreparer{
			script:    "exit 0",
			reportErr: errors.New("adapter report is unreadable"),
		})
		if result.Outcome.Kind != OutcomeSandboxSetupFailure {
			t.Fatalf("SETUP INVALID: kind = %q, want the report failure to rewrite it", result.Outcome.Kind)
		}
		if !result.Outcome.Launched {
			t.Error("a child that ran was recorded as never launched")
		}
		if got := result.Outcome.AppliedEnforcementNotices(); len(got) != 1 {
			t.Errorf("the disclosure was dropped for a child that did run: %#v", got)
		}
	})

	t.Run("cancelled before the process started", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result := captured(t, ctx, &launchStatePreparer{script: "exit 0"})
		if result.Outcome.Launched {
			t.Error("a process that never started was recorded as launched")
		}
		if got := result.Outcome.AppliedEnforcementNotices(); len(got) != 0 {
			t.Errorf("an enforcement trade was claimed for a process that never existed: %#v", got)
		}
	})

	t.Run("never found", func(t *testing.T) {
		result := captured(t, context.Background(), &launchStatePreparer{missing: true})
		if result.Outcome.Launched {
			t.Error("a missing executable was recorded as launched")
		}
		if got := result.Outcome.AppliedEnforcementNotices(); len(got) != 0 {
			t.Errorf("a missing executable claimed an enforcement trade: %#v", got)
		}
	})

	t.Run("ordinary success still discloses", func(t *testing.T) {
		result := captured(t, context.Background(), &launchStatePreparer{script: "exit 0"})
		if !result.Outcome.Launched {
			t.Fatal("an ordinary run was recorded as never launched")
		}
		if got := result.Outcome.AppliedEnforcementNotices(); len(got) != 1 {
			t.Errorf("an ordinary run lost its disclosure: %#v", got)
		}
	})
}

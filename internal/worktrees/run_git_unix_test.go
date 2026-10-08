//go:build !windows

package worktrees

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// THE HARDENING HAS TO WORK, not merely be configured.
//
// A leaked grandchild holding the pipes is what makes an unhardened cancel hang:
// CommandContext signals the direct child, Wait blocks until the pipes reach
// EOF, and the grandchild holds them open. So this launches exactly that shape —
// a shell that spawns a long sleeper inheriting stdout — cancels, and requires
// the call to return.
//
// It goes through newHardenedCommand, the constructor defaultRunGit uses. A test
// calling hardenWorktreeGit directly would prove three fields are set and prove
// nothing about whether anything calls it.
// TestTheGitConstructorSetsAProcessGroup is the POSIX half of the constructor
// assertion; the portable half lives in run_git_test.go.
func TestTheGitConstructorSetsAProcessGroup(t *testing.T) {
	command := newHardenedCommand(context.Background(), t.TempDir(), "git", "status")
	if command.SysProcAttr == nil || !command.SysProcAttr.Setpgid {
		t.Fatal("Setpgid is unset; a cancel signals only the direct child")
	}
	if command.Cancel == nil {
		t.Fatal("Cancel is unset; the process group is never signalled")
	}
}

func TestACancelledCommandKillsTheWholeProcessGroup(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no shell: %v", err)
	}
	previous := worktreeWaitDelay
	worktreeWaitDelay = 500 * time.Millisecond
	t.Cleanup(func() { worktreeWaitDelay = previous })

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	ctx, cancel := context.WithCancel(context.Background())

	// A grandchild that outlives its parent shell and inherits stdout — the
	// exact shape that hangs an unhardened Wait AND survives a kill aimed at
	// the direct child only.
	// pidFile is passed as a positional argument ($1), not interpolated into the
	// script, so a temp path containing a space or shell metacharacter cannot
	// break or reshape the command.
	script := `sleep 60 & echo $! > "$1"; sleep 60`
	command := newHardenedCommand(ctx, dir, "sh", "-c", script, "sh", pidFile)
	var sink discardWriter
	command.Stdout = &sink
	command.Stderr = &sink
	if err := command.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Wait for the grandchild to exist before cancelling, or the test proves
	// nothing about what happens to it.
	var grandchild int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(pidFile)
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil && pid > 0 {
				grandchild = pid
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if grandchild == 0 {
		// The command is already running; clean it up before failing rather than
		// leaking the shell and its grandchild. A missed pid is a failure of the
		// setup this test depends on, not a reason to silently skip the assertion.
		cancel()
		_ = command.Wait()
		t.Fatal("the grandchild never reported its pid")
	}

	cancel()

	// FIRST: the call must return. An unhardened Wait blocks on the pipes the
	// grandchild holds open.
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Wait did not return after cancel; a leaked grandchild is holding the pipes open")
	}

	// SECOND, and this is the property WaitDelay alone does not give: the
	// grandchild must be DEAD. Returning promptly while leaving a process
	// running is the orphan this hardening exists to prevent, and a test that
	// only checked the return would pass against a cancel aimed at the direct
	// child alone.
	alive := false
	for attempt := 0; attempt < 50; attempt++ {
		if err := syscall.Kill(grandchild, 0); err != nil {
			alive = false
			break
		}
		alive = true
		time.Sleep(20 * time.Millisecond)
	}
	if alive {
		_ = syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("grandchild %d survived the cancel; the signal did not reach the process group", grandchild)
	}
}

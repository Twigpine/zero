package cli

import (
	"context"
	"io"
	"os"
	"runtime"
	"testing"
	"time"

	zeroSandbox "github.com/Twigpine/zero/internal/sandbox"
)

// longLivedPlan returns a plan whose child outlives the test unless something
// terminates it.
func longLivedPlan(t *testing.T) zeroSandbox.CommandPlan {
	t.Helper()
	plan := zeroSandbox.CommandPlan{Dir: t.TempDir()}
	if runtime.GOOS == "windows" {
		plan.Name = "cmd.exe"
		plan.Args = []string{"/c", "ping -n 120 127.0.0.1 >NUL"}
		return plan
	}
	plan.Name = "/bin/sh"
	plan.Args = []string{"-c", "sleep 120"}
	return plan
}

// CANCELLING THE WRAPPER HAS TO REACH THE COMMAND.
//
// The sandboxed command was started with a bare exec.Command().Run(): no
// context, no forwarding, no shutdown path. A terminal masks that, because it
// signals the whole foreground process group, but a supervisor or task runner
// that sends SIGTERM to the wrapper's PID killed only Zero. The command kept
// running, doing filesystem and network work after the caller considered the
// task cancelled, and the deferred plan cleanup never ran.
//
// Driven with a real long-lived child and a real cancellation, asserting that
// the call actually returns rather than that a field is set.
func TestCancellingTheWrapperTerminatesTheSandboxedCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan int, 1)
	go func() {
		done <- runSandboxPlannedCommand(ctx, longLivedPlan(t), io.Discard, io.Discard)
	}()

	// Let the child actually start, or cancelling proves nothing.
	time.Sleep(300 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("SETUP INVALID: the child exited on its own, so cancellation was not exercised")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(sandboxExecShutdownGrace + 10*time.Second):
		t.Fatal("cancelling the wrapper did not terminate the sandboxed command; it would keep running after the caller gave up")
	}
}

// And an uncancelled command still runs to completion and reports its own
// status, or the fix above would be "kill everything immediately".
func TestAnUncancelledSandboxedCommandStillReportsItsStatus(t *testing.T) {
	plan := zeroSandbox.CommandPlan{Dir: t.TempDir()}
	if runtime.GOOS == "windows" {
		plan.Name = "cmd.exe"
		plan.Args = []string{"/c", "exit 3"}
	} else {
		plan.Name = "/bin/sh"
		plan.Args = []string{"-c", "exit 3"}
	}
	if code := runSandboxPlannedCommand(context.Background(), plan, io.Discard, os.Stderr); code != 3 {
		t.Fatalf("exit code = %d, want the child's own 3", code)
	}
}

//go:build !windows

package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	zeroSandbox "github.com/Twigpine/zero/internal/sandbox"
)

// THE GRACEFUL PHASE HAS TO ACTUALLY HAPPEN.
//
// The shutdown comments described a graceful signal followed by escalation while
// the code did neither: Cancel called KillProcessTree, which is SIGKILL here, and
// WaitDelay only starts counting after Cancel returns so it could never postpone
// a kill that had already happened. A directed SIGTERM therefore reached the
// child as 137 rather than 143, with no chance to flush output, drop a lock file,
// or run a shutdown handler.
//
// Driven with a child that traps the signal and records that its handler ran, so
// the assertion is about the observed lifecycle rather than about a configured
// duration.
func TestACancelledCommandGetsAChanceToCleanUp(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cleanup-ran")
	// Exits 42 on the graceful signal: a distinctive code the child could only
	// have chosen itself, so the status proves the handler ran rather than that
	// something plausible happened.
	script := "trap 'echo yes > " + marker + "; exit 42' TERM; while :; do sleep 0.05; done"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- runSandboxPlannedCommand(ctx, zeroSandbox.CommandPlan{
			Name: "/bin/sh",
			Args: []string{"-c", script},
			Dir:  t.TempDir(),
		}, io.Discard, io.Discard)
	}()

	// The trap has to be installed before the signal, or this proves nothing.
	time.Sleep(500 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("SETUP INVALID: the child exited on its own")
	default:
	}

	cancel()
	var status int
	select {
	case status = <-done:
	case <-time.After(sandboxExecShutdownGrace + 10*time.Second):
		t.Fatal("the cancelled command never terminated")
	}

	// The status the caller sees is the child's own, so a harness can tell an
	// orderly shutdown from a kill.
	if status != 42 {
		t.Errorf("status = %d, want 42; the child chose that code in its TERM handler, so anything else means it never got to finish", status)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the child was killed outright instead of being asked to stop, so its cleanup handler never ran: %v", err)
	}
}

// And a child that ignores the request is still force-killed, after the bound
// rather than before it. Without this the fix could be "only ever ask nicely".
func TestAStubbornCancelledCommandIsStillKilled(t *testing.T) {
	// Traps TERM and keeps running: only escalation can end this.
	script := "trap '' TERM; while :; do sleep 0.05; done"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- runSandboxPlannedCommand(ctx, zeroSandbox.CommandPlan{
			Name: "/bin/sh",
			Args: []string{"-c", script},
			Dir:  t.TempDir(),
		}, io.Discard, io.Discard)
	}()
	time.Sleep(500 * time.Millisecond)

	started := time.Now()
	cancel()
	var status int
	select {
	case status = <-done:
	case <-time.After(sandboxExecShutdownGrace + 15*time.Second):
		t.Fatal("a child ignoring the graceful request was never force-killed")
	}
	// After the bound, not before: a kill that lands immediately is the defect
	// this replaced.
	if elapsed := time.Since(started); elapsed < sandboxExecShutdownGrace {
		t.Fatalf("the child was killed after %s, before the %s grace it was owed", elapsed, sandboxExecShutdownGrace)
	}
	// And escalation really was a kill, so "ask nicely and give up" cannot pass.
	if want := 128 + int(syscall.SIGKILL); status != want {
		t.Errorf("status = %d, want %d after escalation", status, want)
	}
}

// AND THE STATUS HAS TO SAY WHICH SIGNAL ARRIVED.
//
// 137 rather than 143 was the reported symptom, and it is the only part of this
// a caller can see. A child that does not trap anything must come back as
// 128+SIGTERM: if the first signal were still SIGKILL, this would be 137 and the
// two cases above would both still pass, since one traps and the other ignores.
func TestACancelledCommandReportsTheGracefulSignal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- runSandboxPlannedCommand(ctx, zeroSandbox.CommandPlan{
			Name: "/bin/sh",
			Args: []string{"-c", "while :; do sleep 0.05; done"},
			Dir:  t.TempDir(),
		}, io.Discard, io.Discard)
	}()
	time.Sleep(500 * time.Millisecond)
	cancel()

	var status int
	select {
	case status = <-done:
	case <-time.After(sandboxExecShutdownGrace + 10*time.Second):
		t.Fatal("the cancelled command never terminated")
	}
	if want := 128 + int(syscall.SIGTERM); status != want {
		t.Fatalf("status = %d, want %d; %d means the child was SIGKILLed with no graceful phase", status, want, 128+int(syscall.SIGKILL))
	}
}

// TERMINATION IS COMPLETE BEFORE THE FUNCTION RETURNS.
//
// `zero sandbox exec` releases the plan with a defer around this call, so plan
// cleanup is ordered after termination only if nothing survives the return. A
// future refactor that lets Wait come back while the tree is still up would move
// cleanup on top of a live child, deleting the policy-report file it is using.
//
// The stubborn child is the one that can outlive the call, since it ignores the
// graceful request and only escalation ends it.
func TestACancelledCommandIsFullyReapedBeforeCleanupCouldRun(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := "echo $$ > " + pidFile + "; trap '' TERM; while :; do sleep 0.05; done"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- runSandboxPlannedCommand(ctx, zeroSandbox.CommandPlan{
			Name: "/bin/sh",
			Args: []string{"-c", script},
			Dir:  t.TempDir(),
		}, io.Discard, io.Discard)
	}()
	time.Sleep(500 * time.Millisecond)

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("SETUP INVALID: the child never recorded its pid, so there is nothing to check for survival: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("SETUP INVALID: unusable child pid %q: %v", raw, err)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(sandboxExecShutdownGrace + 15*time.Second):
		t.Fatal("a child ignoring the graceful request was never force-killed")
	}

	// Signal 0 is the existence probe: no signal is sent, only the permission and
	// liveness checks run, so ESRCH is the answer for a reaped process.
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("the child (pid %d) was still alive when the run returned; plan cleanup is deferred around this call and would delete the policy-report file underneath it", pid)
	} else if err != syscall.ESRCH {
		t.Skipf("cannot probe pid %d here: %v", pid, err)
	}
}

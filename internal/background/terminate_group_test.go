package background

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// TerminateCommandGroup LEAVES THE WAIT TO ITS CALLER. Its callers already have a
// Wait running or about to run: the daemon pool waits on its workers, and
// exec.Cmd calls the Cancel hook from inside Wait. Reaping here, the way
// TerminateCommand does, would make their Wait fail with "Wait was already
// called" instead of reporting how the command ended.
func TestTerminateCommandGroupLeavesTheWaitToTheCaller(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "Start-Sleep -Seconds 30")
	}
	ConfigureChildProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := TerminateCommandGroup(cmd); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("TerminateCommandGroup: %v", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			t.Fatalf("the caller's own Wait failed after TerminateCommandGroup: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the command was still running 10s after TerminateCommandGroup")
	}
}

// A command that was never started, or a nil one, is nothing to stop.
func TestTerminateCommandGroupWithNoProcessIsANoOp(t *testing.T) {
	if err := TerminateCommandGroup(nil); err != nil {
		t.Fatalf("nil command: %v", err)
	}
	if err := TerminateCommandGroup(exec.Command("sleep", "1")); err != nil {
		t.Fatalf("unstarted command: %v", err)
	}
}

// After the caller has waited, Windows cannot pin the process any more. The error
// must still read as "already finished", which is what exec.Cmd's Cancel contract
// and the pool both treat as nothing wrong.
func TestTerminateCommandGroupAfterWaitReportsProcessDone(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("POSIX signals the group, which is simply gone after the wait")
	}
	cmd := exec.Command("cmd.exe", "/c", "exit", "0")
	ConfigureChildProcessGroup(cmd)
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := TerminateCommandGroup(cmd); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("after Wait the error is %v, want nil or one wrapping os.ErrProcessDone", err)
	}
}

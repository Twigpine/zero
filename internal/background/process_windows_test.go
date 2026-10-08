//go:build windows

package background

import (
	"bufio"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTerminateCommandReturnsTreeFailureAfterRootFallbackReaps(t *testing.T) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "Start-Sleep -Seconds 300")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	treeErr := errors.New("taskkill /T failed")
	originalTerminateProcess := terminateProcessForTest
	terminateProcessForTest = func(pid int) error {
		if err := cmd.Process.Kill(); err != nil {
			return errors.Join(treeErr, err)
		}
		return treeErr
	}
	t.Cleanup(func() { terminateProcessForTest = originalTerminateProcess })

	err := TerminateCommand(cmd)
	if !errors.Is(err, treeErr) {
		t.Fatalf("TerminateCommand error = %v, want tree failure", err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("root process was not reaped")
	}
}

func TestTerminateCommandReapsExitedLeader(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "exit", "0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Let the child exit without calling Wait so cleanup exercises the Windows
	// taskkill/TerminateProcess race while it still owns the process handle.
	time.Sleep(500 * time.Millisecond)

	// jatmn's #774 finding: taskkill /T racing an already-exited PID (and the
	// Process.Kill fallback) both fail here, but the reap below still succeeds —
	// the process-handle exit-code check proves the leader was already gone, so
	// TerminateCommand must not surface the expected kill-attempt error.
	if err := TerminateCommand(cmd); err != nil {
		t.Fatalf("TerminateCommand: %v, want nil (leader was already gone and got reaped)", err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("exited process was not reaped")
	}
}

func TestTerminateCommandDeadLeaderCannotIdentifyDescendantTree(t *testing.T) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		`$p = Start-Process powershell.exe -ArgumentList '-NoProfile','-Command','Start-Sleep -Seconds 300' -PassThru; $p.Id`)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read descendant pid: %v", err)
	}
	descendantPID, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("parse descendant pid %q: %v", line, err)
	}
	t.Cleanup(func() {
		_ = exec.Command("taskkill.exe", "/F", "/PID", strconv.Itoa(descendantPID)).Run()
	})

	// Wait until the leader is observably exited but deliberately leave it
	// unreaped, matching the POSIX regression's ordering.
	deadline := time.Now().Add(5 * time.Second)
	for processExistsOnWindows(cmd.Process.Pid) {
		if time.Now().After(deadline) {
			t.Fatal("leader did not exit")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := TerminateCommand(cmd); err != nil {
		t.Fatalf("TerminateCommand: %v", err)
	}

	// Windows has no persistent group ID analogous to POSIX: once the leader is
	// gone, taskkill /T cannot rediscover this descendant. Assert that platform
	// distinction rather than claiming TerminateCommand can provide it here.
	if !processExistsOnWindows(descendantPID) {
		t.Fatal("descendant unexpectedly stopped after its root exited")
	}
}

func processExistsOnWindows(pid int) bool {
	return exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"Get-Process -Id "+strconv.Itoa(pid)+" -ErrorAction Stop | Out-Null").Run() == nil
}

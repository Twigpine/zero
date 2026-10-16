//go:build windows

package execution

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

type helperProcessOwner struct {
	pidFile  string
	stopFile string
	pid      int
	handle   windows.Handle
	exited   bool
}

func ownHelperProcess(t *testing.T, pidFile, stopFile string) *helperProcessOwner {
	t.Helper()
	owner := &helperProcessOwner{pidFile: pidFile, stopFile: stopFile}
	t.Cleanup(func() { owner.cleanup(t) })
	return owner
}

func (owner *helperProcessOwner) waitReady(t *testing.T, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		observed, err := owner.observeReady()
		if err != nil {
			t.Fatalf("retain helper process: %v", err)
		}
		if observed {
			return owner.pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper did not hand off a valid PID within %s", timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (owner *helperProcessOwner) observeReady() (bool, error) {
	data, err := os.ReadFile(owner.pidFile)
	if err != nil {
		return false, nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false, nil
	}
	if owner.handle != 0 {
		return true, nil
	}
	if err := owner.retainPID(pid); err != nil {
		return true, err
	}
	return true, nil
}

func (owner *helperProcessOwner) retainPID(pid int) error {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		owner.pid = pid
		owner.exited = true
		return nil
	}
	if err != nil {
		return err
	}
	owner.pid = pid
	owner.handle = handle
	return nil
}

func (owner *helperProcessOwner) running() bool {
	if owner.handle == 0 {
		return false
	}
	var exitCode uint32
	return windows.GetExitCodeProcess(owner.handle, &exitCode) == nil && exitCode == processStillActive
}

func (owner *helperProcessOwner) awaitExit(t *testing.T) {
	t.Helper()
	if owner.exited {
		return
	}
	if owner.handle == 0 {
		t.Fatal("helper process handle was not retained")
	}
	status, err := windows.WaitForSingleObject(owner.handle, 2_000)
	if err != nil {
		t.Fatalf("wait for helper process %d: %v", owner.pid, err)
	}
	if status != windows.WAIT_OBJECT_0 {
		t.Fatalf("helper process %d is still running after command cancellation", owner.pid)
	}
}

func (owner *helperProcessOwner) cleanup(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(owner.stopFile, nil, 0o600); err != nil {
		t.Errorf("request helper process stop: %v", err)
	}
	if owner.handle == 0 {
		observed, err := owner.observeReady()
		if observed && err != nil {
			t.Errorf("retain helper process for cleanup: %v", err)
		}
	}
	if owner.handle == 0 {
		return
	}
	if owner.running() {
		status, err := windows.WaitForSingleObject(owner.handle, 2_000)
		if err != nil {
			t.Errorf("wait for helper process %d cooperative stop: %v", owner.pid, err)
		} else if status == uint32(windows.WAIT_TIMEOUT) {
			if err := windows.TerminateProcess(owner.handle, 1); err != nil {
				t.Errorf("kill helper process %d: %v", owner.pid, err)
			}
			_, _ = windows.WaitForSingleObject(owner.handle, 2_000)
		}
	}
	if err := windows.CloseHandle(owner.handle); err != nil {
		t.Errorf("close helper process %d handle: %v", owner.pid, err)
	}
	owner.handle = 0
}

type helperHandleOwner struct {
	handle   windows.Handle
	stopFile string
}

func ownHelperHandle(t *testing.T, stopFile string) *helperHandleOwner {
	t.Helper()
	owner := &helperHandleOwner{stopFile: stopFile}
	t.Cleanup(func() { owner.cleanup(t) })
	return owner
}

func (owner *helperHandleOwner) retain(process windows.Handle) error {
	current := windows.CurrentProcess()
	return windows.DuplicateHandle(current, process, current, &owner.handle, windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, 0)
}

func (owner *helperHandleOwner) cleanup(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(owner.stopFile, nil, 0o600); err != nil {
		t.Errorf("request suspended helper process stop: %v", err)
	}
	if owner.handle == 0 {
		return
	}
	var exitCode uint32
	if windows.GetExitCodeProcess(owner.handle, &exitCode) == nil && exitCode == processStillActive {
		if err := windows.TerminateProcess(owner.handle, 1); err != nil {
			t.Errorf("kill suspended helper process: %v", err)
		}
		_, _ = windows.WaitForSingleObject(owner.handle, 2_000)
	}
	if err := windows.CloseHandle(owner.handle); err != nil {
		t.Errorf("close suspended helper process handle: %v", err)
	}
	owner.handle = 0
}

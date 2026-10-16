//go:build windows

package hooks

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

type hookTestProcessOwner struct {
	handles  map[int]windows.Handle
	stopFile string
	pidFiles []string
}

func newHookTestProcessOwner(t *testing.T, stopFile string, pidFiles ...string) *hookTestProcessOwner {
	t.Helper()
	owner := &hookTestProcessOwner{handles: make(map[int]windows.Handle), stopFile: stopFile, pidFiles: pidFiles}
	t.Cleanup(func() { owner.cleanup(t) })
	return owner
}

func (owner *hookTestProcessOwner) retain(pid int) error {
	if pid <= 0 || pid == os.Getpid() {
		return errors.New("invalid test process PID")
	}
	if _, ok := owner.handles[pid]; ok {
		return nil
	}
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return err
	}
	owner.handles[pid] = handle
	return nil
}

func (owner *hookTestProcessOwner) awaitExit(pid int, timeout time.Duration) error {
	handle, ok := owner.handles[pid]
	if !ok {
		return errors.New("test process identity was not retained")
	}
	wait, err := windows.WaitForSingleObject(handle, uint32(timeout/time.Millisecond))
	if err != nil {
		return err
	}
	if wait != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("process is still running (wait result %#x)", wait)
	}
	return nil
}

func (owner *hookTestProcessOwner) cleanup(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(owner.stopFile, nil, 0o600); err != nil {
		t.Errorf("request test process stop: %v", err)
	}
	owner.retainPIDFiles()
	for pid, handle := range owner.handles {
		wait, err := windows.WaitForSingleObject(handle, 2_000)
		if err == nil && wait == uint32(windows.WAIT_TIMEOUT) {
			if err := windows.TerminateProcess(handle, 1); err != nil {
				t.Errorf("terminate test process %d: %v", pid, err)
			}
		}
	}
	for pid, handle := range owner.handles {
		if err := owner.awaitExit(pid, 2*time.Second); err != nil {
			t.Errorf("wait for test process %d: %v", pid, err)
		}
		if err := windows.CloseHandle(handle); err != nil {
			t.Errorf("close test process %d handle: %v", pid, err)
		}
		delete(owner.handles, pid)
	}
}

func (owner *hookTestProcessOwner) retainPIDFiles() {
	for _, path := range owner.pidFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(string(data))
		if err == nil {
			_ = owner.retain(pid)
		}
	}
}

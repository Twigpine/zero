//go:build !windows

package hooks

import (
	"errors"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"
)

type hookTestProcessOwner struct {
	pids     map[int]struct{}
	exited   map[int]struct{}
	stopFile string
	pidFiles []string
}

func newHookTestProcessOwner(t *testing.T, stopFile string, pidFiles ...string) *hookTestProcessOwner {
	t.Helper()
	owner := &hookTestProcessOwner{pids: make(map[int]struct{}), exited: make(map[int]struct{}), stopFile: stopFile, pidFiles: pidFiles}
	t.Cleanup(func() { owner.cleanup(t) })
	return owner
}

func (owner *hookTestProcessOwner) retain(pid int) error {
	if pid <= 0 || pid == os.Getpid() {
		return errors.New("invalid test process PID")
	}
	owner.pids[pid] = struct{}{}
	return nil
}

func (owner *hookTestProcessOwner) awaitExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Kill(pid, syscall.Signal(0))
		if errors.Is(err, syscall.ESRCH) {
			delete(owner.pids, pid)
			owner.exited[pid] = struct{}{}
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("process is still running")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (owner *hookTestProcessOwner) cleanup(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(owner.stopFile, nil, 0o600); err != nil {
		t.Errorf("request test process stop: %v", err)
	}
	owner.retainPIDFiles()
	for pid := range owner.pids {
		if err := owner.awaitExit(pid, 2*time.Second); err != nil {
			t.Errorf("wait for test process %d: %v", pid, err)
		}
	}
}

func (owner *hookTestProcessOwner) retainPIDFiles() {
	for _, path := range owner.pidFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		pid, err := strconv.Atoi(string(data))
		if err == nil && pid > 0 && pid != os.Getpid() {
			if _, exited := owner.exited[pid]; !exited {
				owner.pids[pid] = struct{}{}
			}
		}
	}
}

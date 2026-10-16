//go:build !windows

package execution

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type helperProcessOwner struct {
	pidFile  string
	stopFile string
	pid      int
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
		data, err := os.ReadFile(owner.pidFile)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				owner.pid = pid
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper did not hand off a valid PID within %s", timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (owner *helperProcessOwner) cleanup(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(owner.stopFile, nil, 0o600); err != nil {
		t.Errorf("request helper process stop: %v", err)
	}
	if owner.exited {
		return
	}
	if owner.pid == 0 {
		data, err := os.ReadFile(owner.pidFile)
		if err == nil {
			owner.pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
	}
	if owner.pid <= 0 {
		return
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(owner.pid, syscall.Signal(0))
		if errors.Is(err, syscall.ESRCH) {
			owner.pid = 0
			owner.exited = true
			return
		}
		if err != nil {
			t.Errorf("check helper process %d after cleanup: %v", owner.pid, err)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("helper process %d survived cleanup", owner.pid)
}

func (owner *helperProcessOwner) awaitExit(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Kill(owner.pid, syscall.Signal(0))
		if errors.Is(err, syscall.ESRCH) {
			owner.pid = 0
			owner.exited = true
			return
		}
		if err != nil {
			t.Fatalf("check helper process %d: %v", owner.pid, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper process %d is still running after command cancellation", owner.pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

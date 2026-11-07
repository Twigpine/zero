//go:build !windows

package daemon

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A WORKER'S CHILDREN ARE STOPPED THROUGH THE GROUP IT WAS LAUNCHED INTO.
//
// Kill and the Cancel hook used to hand the worker's PID to
// background.TerminateProcess, which looks the group up again with Getpgid when it
// signals. Once the worker has exited and been waited, that lookup fails, only the
// dead PID is signalled, and whatever the worker left running survives; on Darwin
// the lookup already fails while the exited worker is still a zombie. The worker
// here is a shell that backgrounds a sleep, prints its PID and exits, so after the
// wait the sleep is the only process left in the worker's group. Reported in #861.
func TestExecWorkerStopsWhatItLeftRunningAfterItExits(t *testing.T) {
	for _, stop := range []struct {
		name string
		run  func(*execWorker) error
	}{
		{"Kill", func(worker *execWorker) error { return worker.Kill() }},
		{"Cancel hook", func(worker *execWorker) error { return worker.cmd.Cancel() }},
	} {
		t.Run(stop.name, func(t *testing.T) {
			launcher, err := NewExecLauncher(ExecLauncherConfig{
				Executable: "/bin/sh",
				BaseArgs:   []string{"-c", "sleep 300 >/dev/null 2>&1 & echo $!"},
			})
			if err != nil {
				t.Fatalf("NewExecLauncher: %v", err)
			}
			handle, err := launcher(context.Background(), WorkerSpec{})
			if err != nil {
				t.Fatalf("launch: %v", err)
			}
			worker, ok := handle.(*execWorker)
			if !ok {
				t.Fatalf("SETUP INVALID: the launcher returned %T, not the exec worker under test", handle)
			}
			line, ok, err := worker.Stdout().Next()
			if err != nil || !ok {
				t.Fatalf("read the child's pid: ok=%v err=%v", ok, err)
			}
			childPID, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil {
				t.Fatalf("parse the child's pid %q: %v", line, err)
			}
			t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })

			if _, err := worker.Wait(); err != nil {
				t.Fatalf("wait for the worker: %v", err)
			}
			if childStopped(childPID) {
				t.Fatalf("SETUP INVALID: the worker's child %d is not running, so there is nothing left to stop", childPID)
			}

			// The pool ignores what Kill returns; the outcome is what matters.
			_ = stop.run(worker)

			deadline := time.Now().Add(5 * time.Second)
			for !childStopped(childPID) {
				if time.Now().After(deadline) {
					t.Fatalf("the worker's child %d survived %s after the worker was reaped", childPID, stop.name)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// childStopped reports whether pid is gone or only waiting to be reaped.
func childStopped(pid int) bool {
	if errors.Is(syscall.Kill(pid, syscall.Signal(0)), syscall.ESRCH) {
		return true
	}
	state, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return errors.Is(syscall.Kill(pid, syscall.Signal(0)), syscall.ESRCH)
	}
	return strings.HasPrefix(strings.TrimSpace(string(state)), "Z")
}

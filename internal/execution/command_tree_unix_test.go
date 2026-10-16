//go:build !windows

package execution

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestRunCommandAbsolutePathWithEmptyPATH(t *testing.T) {
	t.Setenv("PATH", "")
	ctx := context.Background()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", "exit 0")
	if err := RunCommand(ctx, command); err != nil {
		t.Fatalf("RunCommand with absolute executable and empty PATH: %v", err)
	}
}

func TestPrepareCommandTreeRetainsGroupIdentity(t *testing.T) {
	attributes := &syscall.SysProcAttr{Setsid: true}
	command := exec.Command("sh", "-c", "exit 7")
	command.SysProcAttr = attributes
	tree, err := prepareCommandTree(command)
	if err != nil {
		t.Fatalf("prepare command tree: %v", err)
	}
	defer tree.close()

	if command.SysProcAttr != attributes {
		t.Fatal("prepareCommandTree replaced existing SysProcAttr")
	}
	if !attributes.Setsid || !attributes.Setpgid || attributes.Pgid != tree.groupID {
		t.Fatalf("command attributes = %#v, want preserved Setsid and group %d", attributes, tree.groupID)
	}
	if pgid, err := syscall.Getpgid(tree.anchor.Process.Pid); err != nil || pgid != tree.groupID {
		t.Fatalf("anchor process group = %d, %v; want %d", pgid, err, tree.groupID)
	}

	// Setsid and joining an existing process group are intentionally incompatible;
	// it is retained above only to verify that unrelated caller fields survive.
	attributes.Setsid = false
	if err := command.Start(); err != nil {
		t.Fatalf("start command: %v", err)
	}
	if err := tree.attach(command.Process); err != nil {
		t.Fatalf("attach command: %v", err)
	}
	if pgid, err := syscall.Getpgid(command.Process.Pid); err != nil || pgid != tree.groupID {
		t.Fatalf("command process group = %d, %v; want %d", pgid, err, tree.groupID)
	}
	if err := command.Wait(); err == nil {
		t.Fatal("command unexpectedly succeeded")
	}
	if err := syscall.Kill(tree.anchor.Process.Pid, 0); err != nil {
		t.Fatalf("anchor did not retain group identity after command exit: %v", err)
	}
}

func TestCommandTreeCancelSignalsOnce(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	wantErr := errors.New("signal failed")
	var calls atomic.Int32
	tree := &commandTree{
		ready:   ready,
		groupID: 123,
		signal: func(pid int, signal syscall.Signal) error {
			calls.Add(1)
			if pid != -123 || signal != syscall.SIGKILL {
				t.Errorf("signal target = (%d, %v), want (-123, SIGKILL)", pid, signal)
			}
			return wantErr
		},
	}

	const callers = 32
	var wait sync.WaitGroup
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			if err := tree.cancel(); !errors.Is(err, wantErr) {
				t.Errorf("cancel error = %v, want %v", err, wantErr)
			}
		}()
	}
	wait.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("signal calls = %d, want 1", got)
	}
}

func TestCommandTreeCloseWaitsForCancelAndPreventsLaterSignals(t *testing.T) {
	command := exec.Command("sh", "-c", "exit 0")
	tree, err := prepareCommandTree(command)
	if err != nil {
		t.Fatalf("prepare command tree: %v", err)
	}
	if err := tree.attach(nil); err != nil {
		t.Fatalf("attach command tree: %v", err)
	}
	anchorPID := tree.anchor.Process.Pid

	signalStarted := make(chan struct{})
	releaseSignal := make(chan struct{})
	var calls atomic.Int32
	tree.signal = func(int, syscall.Signal) error {
		calls.Add(1)
		close(signalStarted)
		<-releaseSignal
		return nil
	}
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- tree.cancel() }()
	<-signalStarted

	closeDone := make(chan error, 1)
	go func() { closeDone <- tree.close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("close returned while signal was in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := syscall.Kill(anchorPID, 0); err != nil {
		t.Fatalf("anchor was released while signal was in flight: %v", err)
	}

	close(releaseSignal)
	if err := <-cancelDone; err != nil {
		t.Fatalf("cancel command tree: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("close command tree: %v", err)
	}
	if err := tree.cancel(); err != nil {
		t.Fatalf("cancel after close: %v", err)
	}
	if err := tree.close(); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("signal calls after close = %d, want 1", got)
	}
}

func TestCommandTreeCancelAfterCloseDoesNotSignal(t *testing.T) {
	ready := make(chan struct{})
	close(ready)
	var calls atomic.Int32
	tree := &commandTree{
		ready:   ready,
		groupID: 123,
		signal: func(int, syscall.Signal) error {
			calls.Add(1)
			return nil
		},
	}

	if err := tree.close(); err != nil {
		t.Fatalf("close command tree: %v", err)
	}
	if err := tree.cancel(); err != nil {
		t.Fatalf("cancel after close: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("signal calls after close = %d, want 0", got)
	}
}

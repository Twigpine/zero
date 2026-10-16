//go:build windows

package execution

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestRunCommandFailsBeforeDescendantCanEscapeWhenJobAssignmentFails(t *testing.T) {
	switch os.Getenv("ZERO_ASSIGNMENT_FAILURE_TREE_HELPER") {
	case "root":
		child := exec.Command(os.Args[0], "-test.run=^TestRunCommandFailsBeforeDescendantCanEscapeWhenJobAssignmentFails$")
		child.Env = append(os.Environ(),
			"ZERO_ASSIGNMENT_FAILURE_TREE_HELPER=child",
			"ZERO_ASSIGNMENT_FAILURE_TREE_STOP_FILE="+os.Getenv("ZERO_ASSIGNMENT_FAILURE_TREE_STOP_FILE"),
		)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv("ZERO_ASSIGNMENT_FAILURE_TREE_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = os.WriteFile(os.Getenv("ZERO_ASSIGNMENT_FAILURE_TREE_STOP_FILE"), nil, 0o600)
			_ = child.Wait()
			os.Exit(3)
		}
		return
	case "child":
		waitForCommandTreeStop(os.Getenv("ZERO_ASSIGNMENT_FAILURE_TREE_STOP_FILE"), 30*time.Second)
		return
	}

	root := t.TempDir()
	stopFile := filepath.Join(root, "stop")
	originalAssign := assignCommandProcessToJob
	commandOwner := ownHelperHandle(t, stopFile)
	assignCommandProcessToJob = func(_ windows.Handle, process windows.Handle) error {
		if err := commandOwner.retain(process); err != nil {
			return err
		}
		return windows.ERROR_ACCESS_DENIED
	}
	t.Cleanup(func() { assignCommandProcessToJob = originalAssign })

	pidFile := filepath.Join(root, "child.pid")
	escapedChild := ownHelperProcess(t, pidFile, stopFile)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunCommandFailsBeforeDescendantCanEscapeWhenJobAssignmentFails$")
	command.Env = append(os.Environ(),
		"ZERO_ASSIGNMENT_FAILURE_TREE_HELPER=root",
		"ZERO_ASSIGNMENT_FAILURE_TREE_PID_FILE="+pidFile,
		"ZERO_ASSIGNMENT_FAILURE_TREE_STOP_FILE="+stopFile,
	)
	err := waitForRunCommand(t, runCommandAsync(ctx, command), 4*time.Second)
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("RunCommand error = %v, want ERROR_ACCESS_DENIED", err)
	}
	if command.ProcessState == nil || !command.ProcessState.Exited() {
		t.Fatalf("suspended command was not killed and reaped: state = %v", command.ProcessState)
	}
	observed, observeErr := escapedChild.observeReady()
	_, statErr := os.Stat(pidFile)
	if observeErr != nil || observed || !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("suspended command spawned a descendant after job assignment failed: observed = %t, observation error = %v, PID file error = %v", observed, observeErr, statErr)
	}
}

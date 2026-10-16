//go:build !windows

package execution

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestRunCommandPreservesRedirectedChildAfterSuccessfulExit(t *testing.T) {
	switch os.Getenv("ZERO_SUCCESSFUL_UNIX_COMMAND_TREE_HELPER") {
	case "root":
		nullFile, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			os.Exit(2)
		}
		defer nullFile.Close()
		child := exec.Command(os.Args[0], "-test.run=^TestRunCommandPreservesRedirectedChildAfterSuccessfulExit$")
		child.Env = append(os.Environ(),
			"ZERO_SUCCESSFUL_UNIX_COMMAND_TREE_HELPER=child",
			"ZERO_SUCCESSFUL_UNIX_COMMAND_TREE_STOP_FILE="+os.Getenv("ZERO_SUCCESSFUL_UNIX_COMMAND_TREE_STOP_FILE"),
		)
		child.Stdin = nullFile
		child.Stdout = nullFile
		child.Stderr = nullFile
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		if err := os.WriteFile(os.Getenv("ZERO_SUCCESSFUL_UNIX_COMMAND_TREE_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = os.WriteFile(os.Getenv("ZERO_SUCCESSFUL_UNIX_COMMAND_TREE_STOP_FILE"), nil, 0o600)
			_ = child.Wait()
			os.Exit(4)
		}
		return
	case "child":
		waitForCommandTreeStop(os.Getenv("ZERO_SUCCESSFUL_UNIX_COMMAND_TREE_STOP_FILE"), 30*time.Second)
		return
	}

	root := t.TempDir()
	pidFile := filepath.Join(root, "child.pid")
	stopFile := filepath.Join(root, "stop")
	child := ownHelperProcess(t, pidFile, stopFile)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunCommandPreservesRedirectedChildAfterSuccessfulExit$")
	command.Env = append(os.Environ(),
		"ZERO_SUCCESSFUL_UNIX_COMMAND_TREE_HELPER=root",
		"ZERO_SUCCESSFUL_UNIX_COMMAND_TREE_PID_FILE="+pidFile,
		"ZERO_SUCCESSFUL_UNIX_COMMAND_TREE_STOP_FILE="+stopFile,
	)
	result := runCommandAsync(ctx, command)
	pid := child.waitReady(t, 2*time.Second)
	if err := waitForRunCommand(t, result, 4*time.Second); err != nil {
		t.Fatalf("RunCommand failed: %v", err)
	}
	if !signalTargetRunning(pid) {
		t.Fatalf("successful RunCommand terminated redirected child %d", pid)
	}
}

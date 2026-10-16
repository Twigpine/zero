package execution

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestRunCommandCanceledBeforePlainCommandStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	command := exec.Command(os.Args[0], "-test.run=^$")
	err := RunCommand(ctx, command)
	if !errors.Is(err, context.Canceled) || command.Process != nil {
		t.Fatalf("canceled command must not start: err=%v process=%v", err, command.Process)
	}
}

func TestRunCommandKillsDescendantAfterRootExit(t *testing.T) {
	switch os.Getenv("ZERO_COMMAND_TREE_HELPER") {
	case "root":
		child := exec.Command(os.Args[0], "-test.run=^TestRunCommandKillsDescendantAfterRootExit$")
		child.Env = append(os.Environ(),
			"ZERO_COMMAND_TREE_HELPER=child",
			"ZERO_COMMAND_TREE_STOP_FILE="+os.Getenv("ZERO_COMMAND_TREE_STOP_FILE"),
		)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv("ZERO_COMMAND_TREE_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = os.WriteFile(os.Getenv("ZERO_COMMAND_TREE_STOP_FILE"), nil, 0o600)
			_ = child.Wait()
			os.Exit(3)
		}
		return
	case "child":
		waitForCommandTreeStop(os.Getenv("ZERO_COMMAND_TREE_STOP_FILE"), 30*time.Second)
		return
	}

	root := t.TempDir()
	pidFile := root + string(os.PathSeparator) + "child.pid"
	stopFile := root + string(os.PathSeparator) + "stop"
	child := ownHelperProcess(t, pidFile, stopFile)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunCommandKillsDescendantAfterRootExit$")
	cmd.Env = append(os.Environ(),
		"ZERO_COMMAND_TREE_HELPER=root",
		"ZERO_COMMAND_TREE_PID_FILE="+pidFile,
		"ZERO_COMMAND_TREE_STOP_FILE="+stopFile,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	result := runCommandAsync(ctx, cmd)
	child.waitReady(t, 2*time.Second)
	cancel()
	err := waitForRunCommand(t, result, 4*time.Second)
	if err == nil {
		t.Fatal("timed-out command unexpectedly succeeded")
	}
	child.awaitExit(t)
}

func TestRunCommandKillsDescendantWhenWaitDelayExpires(t *testing.T) {
	switch os.Getenv("ZERO_WAIT_DELAY_TREE_HELPER") {
	case "root":
		child := exec.Command(os.Args[0], "-test.run=^TestRunCommandKillsDescendantWhenWaitDelayExpires$")
		child.Env = append(os.Environ(),
			"ZERO_WAIT_DELAY_TREE_HELPER=child",
			"ZERO_WAIT_DELAY_TREE_STOP_FILE="+os.Getenv("ZERO_WAIT_DELAY_TREE_STOP_FILE"),
		)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv("ZERO_WAIT_DELAY_TREE_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = os.WriteFile(os.Getenv("ZERO_WAIT_DELAY_TREE_STOP_FILE"), nil, 0o600)
			_ = child.Wait()
			os.Exit(3)
		}
		if os.Getenv("ZERO_WAIT_DELAY_TREE_EXIT_CODE") == "7" {
			os.Exit(7)
		}
		return
	case "child":
		waitForCommandTreeStop(os.Getenv("ZERO_WAIT_DELAY_TREE_STOP_FILE"), 30*time.Second)
		return
	}

	root := t.TempDir()
	pidFile := root + string(os.PathSeparator) + "child.pid"
	stopFile := root + string(os.PathSeparator) + "stop"
	child := ownHelperProcess(t, pidFile, stopFile)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunCommandKillsDescendantWhenWaitDelayExpires$")
	cmd.Env = append(os.Environ(),
		"ZERO_WAIT_DELAY_TREE_HELPER=root",
		"ZERO_WAIT_DELAY_TREE_PID_FILE="+pidFile,
		"ZERO_WAIT_DELAY_TREE_STOP_FILE="+stopFile,
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	result := runCommandAsync(ctx, cmd)
	child.waitReady(t, 2*time.Second)
	err := waitForRunCommand(t, result, 4*time.Second)
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("RunCommand error = %v, want exec.ErrWaitDelay", err)
	}
	child.awaitExit(t)
}

func TestRunCommandPreservesWaitDelayAfterNonzeroRootExit(t *testing.T) {
	root := t.TempDir()
	pidFile := root + string(os.PathSeparator) + "child.pid"
	stopFile := root + string(os.PathSeparator) + "stop"
	child := ownHelperProcess(t, pidFile, stopFile)
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCommandKillsDescendantWhenWaitDelayExpires$")
	cmd.Env = append(os.Environ(),
		"ZERO_WAIT_DELAY_TREE_HELPER=root",
		"ZERO_WAIT_DELAY_TREE_PID_FILE="+pidFile,
		"ZERO_WAIT_DELAY_TREE_STOP_FILE="+stopFile,
		"ZERO_WAIT_DELAY_TREE_EXIT_CODE=7",
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	result := runCommandAsync(context.Background(), cmd)
	child.waitReady(t, 2*time.Second)
	err := waitForRunCommand(t, result, 4*time.Second)
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("RunCommand error = %v, want exec.ErrWaitDelay", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("RunCommand error = %v, want exit code 7", err)
	}
	child.awaitExit(t)
}

func TestRunCommandKillsDescendantAfterNonzeroRootExit(t *testing.T) {
	switch os.Getenv("ZERO_NONZERO_TREE_HELPER") {
	case "root":
		nullFile, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
		if err != nil {
			os.Exit(2)
		}
		defer nullFile.Close()
		child := exec.Command(os.Args[0], "-test.run=^TestRunCommandKillsDescendantAfterNonzeroRootExit$")
		child.Env = append(os.Environ(),
			"ZERO_NONZERO_TREE_HELPER=child",
			"ZERO_NONZERO_TREE_STOP_FILE="+os.Getenv("ZERO_NONZERO_TREE_STOP_FILE"),
		)
		child.Stdin = nullFile
		child.Stdout = nullFile
		child.Stderr = nullFile
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		if err := os.WriteFile(os.Getenv("ZERO_NONZERO_TREE_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = os.WriteFile(os.Getenv("ZERO_NONZERO_TREE_STOP_FILE"), nil, 0o600)
			_ = child.Wait()
			os.Exit(4)
		}
		// Leave time for the parent test to retain an independent cleanup handle
		// before the root's abnormal exit triggers production tree cleanup.
		if waitForCommandTreeStop(os.Getenv("ZERO_NONZERO_TREE_STOP_FILE"), 500*time.Millisecond) {
			_ = child.Wait()
			return
		}
		os.Exit(7)
	case "child":
		waitForCommandTreeStop(os.Getenv("ZERO_NONZERO_TREE_STOP_FILE"), 30*time.Second)
		return
	}

	root := t.TempDir()
	pidFile := root + string(os.PathSeparator) + "child.pid"
	stopFile := root + string(os.PathSeparator) + "stop"
	child := ownHelperProcess(t, pidFile, stopFile)
	ctx := context.Background()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunCommandKillsDescendantAfterNonzeroRootExit$")
	cmd.Env = append(os.Environ(),
		"ZERO_NONZERO_TREE_HELPER=root",
		"ZERO_NONZERO_TREE_PID_FILE="+pidFile,
		"ZERO_NONZERO_TREE_STOP_FILE="+stopFile,
	)
	result := runCommandAsync(ctx, cmd)
	child.waitReady(t, 2*time.Second)
	err := waitForRunCommand(t, result, 4*time.Second)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
		t.Fatalf("RunCommand error = %v, want exit code 7", err)
	}
	child.awaitExit(t)
}

func waitForCommandTreeStop(stopFile string, lifetime time.Duration) bool {
	deadline := time.Now().Add(lifetime)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stopFile); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func runCommandAsync(ctx context.Context, command *exec.Cmd) <-chan error {
	result := make(chan error, 1)
	go func() { result <- RunCommand(ctx, command) }()
	return result
}

func waitForRunCommand(t *testing.T, result <-chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(timeout):
		t.Fatalf("RunCommand did not return within %s", timeout)
		return nil
	}
}

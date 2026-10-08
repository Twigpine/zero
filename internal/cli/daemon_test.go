package cli

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/background"
)

// isolateDaemonPaths points DefaultPaths at a temp dir so the test never touches
// a real daemon on the dev machine.
func isolateDaemonPaths(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
}

func runDaemonCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runDaemon(args, &out, &errb, appDeps{})
	return code, out.String(), errb.String()
}

func TestDaemonUsage(t *testing.T) {
	code, _, _ := runDaemonCLI(t)
	if code != exitUsage {
		t.Fatalf("no-args exit = %d, want exitUsage", code)
	}
	code, out, _ := runDaemonCLI(t, "--help")
	if code != exitSuccess || !strings.Contains(out, "Usage: zero daemon") {
		t.Fatalf("--help exit=%d out=%q", code, out)
	}
}

func TestDaemonUnknownSubcommand(t *testing.T) {
	code, _, errb := runDaemonCLI(t, "frobnicate")
	if code != exitUsage {
		t.Fatalf("unknown subcommand exit = %d, want exitUsage", code)
	}
	if !strings.Contains(errb, "unknown daemon subcommand") {
		t.Fatalf("stderr = %q, want unknown-subcommand message", errb)
	}
}

func TestDaemonRunRequiresSession(t *testing.T) {
	isolateDaemonPaths(t)
	code, _, errb := runDaemonCLI(t, "run", "--prompt", "hi")
	if code != exitUsage {
		t.Fatalf("run without --session exit = %d, want exitUsage", code)
	}
	if !strings.Contains(errb, "--session") {
		t.Fatalf("stderr = %q, want a --session hint", errb)
	}
}

func TestDaemonRunRequiresPromptOrArgs(t *testing.T) {
	isolateDaemonPaths(t)
	code, _, errb := runDaemonCLI(t, "run", "--session", "s1")
	if code != exitUsage {
		t.Fatalf("run without prompt/args exit = %d, want exitUsage", code)
	}
	if !strings.Contains(errb, "--prompt") {
		t.Fatalf("stderr = %q, want a --prompt hint", errb)
	}
}

func TestDaemonStopWhenNotRunning(t *testing.T) {
	isolateDaemonPaths(t)
	code, out, _ := runDaemonCLI(t, "stop")
	if code != exitSuccess {
		t.Fatalf("stop (not running) exit = %d, want exitSuccess", code)
	}
	if !strings.Contains(out, "not running") {
		t.Fatalf("stop output = %q, want 'not running'", out)
	}
}

func TestDaemonStatusWhenNotRunning(t *testing.T) {
	isolateDaemonPaths(t)
	code, out, _ := runDaemonCLI(t, "status")
	if code != exitSuccess {
		t.Fatalf("status (not running) exit = %d, want exitSuccess", code)
	}
	if !strings.Contains(out, "not running") {
		t.Fatalf("status output = %q, want 'not running'", out)
	}
}

func TestDaemonAttachRequiresSession(t *testing.T) {
	isolateDaemonPaths(t)
	code, _, errb := runDaemonCLI(t, "attach")
	if code != exitUsage {
		t.Fatalf("attach without session exit = %d, want exitUsage", code)
	}
	if !strings.Contains(errb, "session") {
		t.Fatalf("stderr = %q, want a session hint", errb)
	}
}

func TestDaemonRunWhenNotRunning(t *testing.T) {
	isolateDaemonPaths(t)
	code, _, errb := runDaemonCLI(t, "run", "--session", "s1", "--prompt", "hello")
	if code != exitCrash {
		t.Fatalf("run (no daemon) exit = %d, want exitCrash", code)
	}
	if !strings.Contains(errb, "not running") {
		t.Fatalf("stderr = %q, want 'not running'", errb)
	}
}

func TestDaemonSubcommandsRejectExtraArgs(t *testing.T) {
	isolateDaemonPaths(t)
	cases := [][]string{
		{"stop", "oops"},
		{"status", "oops"},
		{"attach", "s1", "extra"},
	}
	for _, args := range cases {
		code, _, errb := runDaemonCLI(t, args...)
		if code != exitUsage {
			t.Fatalf("%v exit = %d, want exitUsage (reject extra args); stderr=%q", args, code, errb)
		}
	}
}

func TestWaitForDaemonReadinessChecksTimeoutBoundary(t *testing.T) {
	checks := 0
	ready := waitForDaemonReadiness(func() bool {
		checks++
		return checks == 2
	}, 0, time.Millisecond)
	if !ready {
		t.Fatal("daemon that became reachable at the timeout boundary was not detected")
	}
	if checks != 2 {
		t.Fatalf("reachability checks = %d, want initial and final checks", checks)
	}
}

func TestTerminateAndReapDaemonProcess(t *testing.T) {
	cmd := daemonDetachedChildCommand("hang")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}

	if err := terminateAndReapDaemonProcess(cmd); err != nil {
		t.Fatalf("terminate and reap helper process: %v", err)
	}
	if cmd.ProcessState == nil {
		t.Fatalf("helper process was not reaped: state=%v", cmd.ProcessState)
	}
	if cmd.ProcessState.Success() {
		t.Fatalf("helper process unexpectedly exited successfully: %v", cmd.ProcessState)
	}
}

func TestStartAndAwaitDaemonProcessTimeoutTerminatesAndReaps(t *testing.T) {
	cmd := daemonDetachedChildCommand("hang")
	if err := startAndAwaitDaemonProcess(cmd, func() bool { return false }, 0, time.Millisecond); !errors.Is(err, errDaemonStartTimeout) {
		t.Fatalf("start and await error = %v, want timeout", err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("timed-out helper process was not reaped")
	}
}

func TestStartAndAwaitDaemonProcessReleasesReadyChild(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "child-finished")
	cmd := daemonDetachedChildCommand("mark", "ZERO_TEST_DAEMON_CHILD_MARKER="+marker)
	if err := startAndAwaitDaemonProcess(cmd, func() bool { return true }, time.Second, time.Millisecond); err != nil {
		t.Fatalf("start and await ready helper: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("released helper process did not remain alive to write its marker")
}

func daemonDetachedChildCommand(mode string, extraEnv ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=TestDaemonDetachedChildProcess")
	cmd.Env = append(os.Environ(), "ZERO_TEST_DAEMON_DETACHED_CHILD="+mode)
	cmd.Env = append(cmd.Env, extraEnv...)
	background.ConfigureChildProcessGroup(cmd)
	return cmd
}

func TestDaemonDetachedChildProcess(t *testing.T) {
	switch os.Getenv("ZERO_TEST_DAEMON_DETACHED_CHILD") {
	case "":
		return
	case "mark":
		if err := os.WriteFile(os.Getenv("ZERO_TEST_DAEMON_CHILD_MARKER"), []byte("ready"), 0o600); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	for {
		time.Sleep(time.Hour)
	}
}

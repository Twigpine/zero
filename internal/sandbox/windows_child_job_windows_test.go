//go:build windows

package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/Twigpine/zero/internal/execution"
)

// THE OWNERSHIP HAS TO BE TESTED FROM OUTSIDE THE PROCESS THAT OWNS IT.
//
// joinWindowsChildKillJob puts the CALLING process into a job with
// kill-on-close, which is the whole point: when the caller is terminated its
// handle closes and the kernel takes the children down with it. A test that
// calls it in-process and then closes the handle therefore terminates the test
// binary. That is exactly what the first version of this file did: `go test`
// printed "ok", exited 0, and the assertions after the close were never
// reached. Worse, the package's remaining tests never ran either, because the
// binary was gone, and nothing anywhere said so.
//
// So everything below drives a SEPARATE process: the test binary re-executed as
// the helper below. It calls the production joinWindowsChildKillJob, creates a
// suspended child the way the sandbox helper does, and parks. The test is then
// free to kill it and ask what happened, which is the question.

const (
	suspendedHelperModeEnv    = "ZERO_TEST_SUSPENDED_CHILD_MODE"
	suspendedHelperPIDFileEnv = "ZERO_TEST_SUSPENDED_CHILD_PIDFILE"

	// suspendedHelperModeJob is the production shape: own the children before
	// creating any. suspendedHelperModeNoJob is the same helper WITHOUT that
	// ownership, which is what this code did before the fix.
	suspendedHelperModeJob   = "job"
	suspendedHelperModeNoJob = "nojob"
	// suspendedHelperModeRunner runs the production command runner itself and
	// exits with what it returned.
	suspendedHelperModeRunner = "runner"
)

// TestWindowsSuspendedChildHelperProcess is not a test. It is the stand-in for
// the sandbox command helper, re-executed from the test binary, parked in the
// one window this contract is about: after CreateProcessAsUser has returned a
// suspended child and before ResumeThread.
//
// It stands in for the helper rather than being it, because the real one mints a
// restricted token against an elevated setup marker. What it does NOT stand in
// for is the ownership: it calls the production joinWindowsChildKillJob, in the
// production order, before any child can exist.
//
// The child inherits this process's stdout, which is the pipe the captured
// runner is reading. That is what makes a surviving child a hang and not just a
// leak: the reader waits for an EOF that a process which can never execute will
// never produce.
func TestWindowsSuspendedChildHelperProcess(t *testing.T) {
	mode := os.Getenv(suspendedHelperModeEnv)
	if mode == "" {
		return
	}
	fail := func(code int, format string, args ...any) {
		fmt.Fprintf(os.Stderr, "suspended-child helper: "+format+"\n", args...)
		os.Exit(code)
	}
	if mode == suspendedHelperModeJob {
		// BEFORE ANY CHILD EXISTS, exactly where runWindowsSandboxCommand does
		// it, so there is no interval in which one is alive and unowned. The
		// handle is deliberately never closed: it is released when this process
		// is terminated, which is the event the guarantee is about.
		if _, err := joinWindowsChildKillJob(); err != nil {
			fail(3, "join the kill job: %v", err)
		}
	}
	if mode == suspendedHelperModeRunner {
		// The production entry point, refused at its own earliest return so the
		// exit code below is the one runWindowsSandboxCommand chose and nothing
		// else has run. An unsupported level is the cheapest such refusal.
		os.Exit(runWindowsSandboxCommand(WindowsSandboxCommandConfig{SandboxLevel: "not-a-level"}, os.Stderr))
	}
	child := exec.Command(comspec(), "/c", "ver")
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_SUSPENDED}
	if err := child.Start(); err != nil {
		fail(4, "start the suspended child: %v", err)
	}
	if err := publishHelperChildPID(os.Getenv(suspendedHelperPIDFileEnv), child.Process.Pid); err != nil {
		_ = child.Process.Kill()
		fail(5, "publish the child pid: %v", err)
	}
	// Parked. Production reaches ResumeThread a few instructions later; this
	// process never does, so it holds the window open for as long as the test
	// needs. Nothing below ever runs: the test kills this process.
	time.Sleep(10 * time.Minute)
	fail(6, "the test never terminated this helper")
}

func comspec() string {
	if shell := strings.TrimSpace(os.Getenv("COMSPEC")); shell != "" {
		return shell
	}
	return "cmd.exe"
}

// publishHelperChildPID writes the pid through a rename, so the test never
// reads a half-written file and never mistakes "not created yet" for "created".
func publishHelperChildPID(path string, pid int) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("no pid file was requested")
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}

// suspendedChildRun is one captured run of the helper, parked with its child
// created.
type suspendedChildRun struct {
	childPID uint32
	done     chan struct{}
	result   execution.CapturedResult
	cancel   context.CancelFunc
}

// startParkedSuspendedChildRun runs the helper through the REAL captured
// execution path, the one hook and plugin commands use: execution.Runner over
// the sandbox Engine, whose CommandContext builds an exec.CommandContext. It
// returns once the helper has created its child, which is the barrier.
//
// The plan is deliberately a direct command. The wrapper is not what this is
// about: the hang lives in the captured runner, which sets Stdout and Stderr to
// buffers, so Wait cannot finish until both pipes reach EOF, and sets no
// WaitDelay.
func startParkedSuspendedChildRun(t *testing.T, ctx context.Context, cancel context.CancelFunc, mode string) *suspendedChildRun {
	t.Helper()
	workspace := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child.pid")

	engine := NewEngine(EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        DefaultPolicy(),
		Backend:       Backend{Name: BackendUnavailable, Message: "test backend unavailable"},
	})
	runner := execution.NewRunner(engine)

	run := &suspendedChildRun{done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(run.done)
		run.result = runner.ExecuteCaptured(ctx, execution.CapturedRequest{Request: execution.Request{
			Origin: execution.OriginHook,
			Mode:   execution.ModeCaptured,
			Command: execution.Command{
				Name: os.Args[0],
				Args: []string{"-test.run=TestWindowsSuspendedChildHelperProcess"},
				Env: append(os.Environ(),
					suspendedHelperModeEnv+"="+mode,
					suspendedHelperPIDFileEnv+"="+pidFile,
				),
			},
			WorkingDirectory: workspace,
			WorkspaceRoots:   []string{workspace},
			Approval:         execution.ApprovalContext{PolicyVersion: execution.PolicyVersion},
		}})
	}()

	run.childPID = awaitHelperChildPID(t, run, pidFile)
	// INDEPENDENT OF EVERY ASSERTION BELOW. A failure after this point must not
	// leave a suspended process on the machine, and a suspended process cannot
	// exit by itself.
	t.Cleanup(func() {
		cancel()
		terminateTestProcess(run.childPID)
		select {
		case <-run.done:
		case <-time.After(30 * time.Second):
			t.Errorf("the captured call never returned, even after the child was killed")
		}
	})
	if alive, err := windowsProcessAlive(run.childPID); err != nil || !alive {
		t.Fatalf("SETUP INVALID: the helper's child is not alive at the barrier: alive=%v err=%v", alive, err)
	}
	return run
}

// awaitHelperChildPID waits for the helper to say it has created its child. It
// fails rather than timing out silently, and it gives up early if the helper
// died, so a broken helper reads as a broken helper.
func awaitHelperChildPID(t *testing.T, run *suspendedChildRun, pidFile string) uint32 {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		if data, err := os.ReadFile(pidFile); err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if convErr == nil && pid > 0 {
				return uint32(pid)
			}
		}
		select {
		case <-run.done:
			t.Fatalf("SETUP INVALID: the helper exited before creating its child: %#v\nstderr: %s", run.result.Outcome, run.result.Stderr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("SETUP INVALID: the helper never reported a child pid")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func terminateTestProcess(pid uint32) {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	_ = windows.TerminateProcess(handle, 1)
}

func awaitProcessGone(t *testing.T, pid uint32, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		alive, err := windowsProcessAlive(pid)
		if err != nil {
			t.Fatalf("query the child: %v", err)
		}
		if !alive {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// CANCELLATION IN THE CREATE-TO-RESUME WINDOW MUST NOT ORPHAN THE CHILD, AND
// MUST NOT HANG THE CALLER.
//
// Hook and plugin commands reach the sandbox through Engine.CommandContext,
// which uses the default exec.CommandContext cancellation: on a timeout or a
// cancel the helper is killed outright, so it never reaches its own cleanup,
// never reaches terminateSuspendedWindowsChild, and never resumes. Windows does
// not terminate a child because its parent died. The suspended child then
// survives holding the inherited pipe write ends, and ExecuteCaptured, which is
// copying from those pipes with no WaitDelay, waits for an EOF that a process
// which cannot execute will never send.
//
// Both halves are asserted, because either one alone is the wrong fix: a pipe
// deadline would release the caller and keep the orphan, and killing the child
// without the caller returning would still hang the hook.
func TestCancellingACapturedRunTakesDownTheSuspendedChild(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deadline bool
	}{
		{"parent cancellation", false},
		{"deadline expiry", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if tc.deadline {
				ctx, cancel = context.WithTimeout(context.Background(), 3*time.Second)
			}
			run := startParkedSuspendedChildRun(t, ctx, cancel, suspendedHelperModeJob)
			if !tc.deadline {
				cancel()
			}

			select {
			case <-run.done:
			case <-time.After(60 * time.Second):
				t.Fatal("the captured call did not return after cancellation; the hook or plugin that made it is blocked on a pipe the orphaned child still holds")
			}
			if !awaitProcessGone(t, run.childPID, 30*time.Second) {
				t.Fatal("the suspended child outlived the cancelled helper, so it is an orphan holding the inherited pipes")
			}
			if state := run.result.Outcome.State; state != execution.StateCancelled && state != execution.StateFailed {
				t.Errorf("outcome state = %q, want the run reported as cancelled or failed", state)
			}
		})
	}
}

// THE CONTROL, and the reason the job is the fix rather than a nicety.
//
// The same helper WITHOUT the ownership step is what this code did before: the
// child survives its terminated parent, and the captured call stays blocked on
// the pipes it holds. Asserting that shape keeps the test above from passing for
// some unrelated reason, such as the child exiting on its own.
//
// It cleans up after itself by killing the orphan, which is also what releases
// the blocked call.
func TestWithoutTheKillJobACancelledHelperLeavesAnOrphan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	run := startParkedSuspendedChildRun(t, ctx, cancel, suspendedHelperModeNoJob)
	cancel()

	if awaitProcessGone(t, run.childPID, 5*time.Second) {
		t.Fatal("SETUP INVALID: the child died without the kill job, so this control proves nothing about the job")
	}
	select {
	case <-run.done:
		t.Error("SETUP INVALID: the captured call returned while the orphan still held the pipes, so the hang this pins is not reproduced here")
	case <-time.After(3 * time.Second):
	}
	// The cleanup registered by startParkedSuspendedChildRun kills the orphan,
	// which closes the pipes and lets the captured call return.
}

// A SUCCESSFUL RUN IS STILL A SUCCESSFUL RUN. The job is joined on every command,
// so an ordinary one has to come back with its output and its exit code intact.
func TestJoiningTheKillJobLeavesAnOrdinaryRunAlone(t *testing.T) {
	workspace := t.TempDir()
	engine := NewEngine(EngineOptions{
		WorkspaceRoot: workspace,
		Policy:        DefaultPolicy(),
		Backend:       Backend{Name: BackendUnavailable, Message: "test backend unavailable"},
	})
	runner := execution.NewRunner(engine)
	result := runner.ExecuteCaptured(context.Background(), execution.CapturedRequest{Request: execution.Request{
		Origin:           execution.OriginHook,
		Mode:             execution.ModeCaptured,
		Command:          execution.Command{Name: comspec(), Args: []string{"/c", "echo", "still-here"}},
		WorkingDirectory: workspace,
		WorkspaceRoots:   []string{workspace},
		Approval:         execution.ApprovalContext{PolicyVersion: execution.PolicyVersion},
	}})
	if result.Outcome.Kind != execution.OutcomeSuccess {
		t.Fatalf("outcome = %#v, stderr=%q", result.Outcome, result.Stderr)
	}
	if !strings.Contains(result.Stdout, "still-here") {
		t.Errorf("stdout = %q, want the command's output", result.Stdout)
	}
}

// windowsProcessAlive reports whether pid still exists. A suspended process is
// alive, so WaitForSingleObject returning WAIT_TIMEOUT is the positive answer.
func windowsProcessAlive(pid uint32) (bool, error) {
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		// The process is gone, which is what the caller is asking about.
		return false, nil
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	state, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return false, err
	}
	return state == uint32(windows.WAIT_TIMEOUT), nil
}

// TAKING OWNERSHIP MUST NOT COST THE EXIT CODE.
//
// The helper is a MEMBER of the job it creates, so closing the last handle to
// that job terminates the helper itself. The first version of this fix closed
// it in a defer, which fired during runWindowsSandboxCommand's own return: the
// process was killed before its exit code could be handed back, and every
// sandboxed command reported 0 no matter how it really ended, including the
// refusals a few lines below the join.
//
// This runs the production entry point in a separate process and asks for the
// code it returned. The refusal it drives returns 1, so a 0 here is the bug
// coming back rather than a command that happened to succeed.
func TestOwningTheChildrenKeepsTheCommandRunnersExitCode(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=TestWindowsSuspendedChildHelperProcess")
	command.Env = append(os.Environ(), suspendedHelperModeEnv+"="+suspendedHelperModeRunner)
	output, err := command.CombinedOutput()

	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run the command runner: %v\n%s", err, output)
	}
	if code != 1 {
		t.Fatalf("the command runner exited %d, want the 1 it returned; the job handle is being closed on the way out and killing this process before its exit code survives\n%s", code, output)
	}
	if !strings.Contains(string(output), "unsupported Windows sandbox level") {
		t.Errorf("the runner did not reach its refusal, so the exit code above is not the one under test:\n%s", output)
	}
}

// AND OWNERSHIP IS TAKEN ON EVERY PATH, including the ones that refuse the
// command before any child could exist. The point of joining first is that
// there is no interval in which a child is alive and unowned, and a join that
// moved below one of these returns would leave exactly that interval on the
// paths that do go on to launch.
func TestTheCommandRunnerTakesOwnershipBeforeItRefuses(t *testing.T) {
	joined := 0
	previous := windowsJoinChildKillJob
	t.Cleanup(func() { windowsJoinChildKillJob = previous })
	windowsJoinChildKillJob = func() (windows.Handle, error) {
		joined++
		return 0, fmt.Errorf("declined for the test")
	}

	// The EARLIEST refusal in the function, so a join that slipped below any of
	// the checks would be measured as not having happened. A DenyRead profile on
	// a restricted-token tier is refused before the level switch, before setup
	// validation, and long before a token or a child exists.
	var stderr strings.Builder
	config := WindowsSandboxCommandConfig{
		SandboxLevel:      WindowsSandboxLevelRestrictedToken,
		PermissionProfile: PermissionProfile{FileSystem: FileSystemPolicy{DenyRead: []string{`C:\secrets`}}},
	}
	if code := runWindowsSandboxCommand(config, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1 (%s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "DenyRead is not supported") {
		t.Fatalf("SETUP INVALID: the runner did not stop at its earliest refusal, so a later join would still count: %s", stderr.String())
	}
	if joined != 1 {
		t.Fatalf("the runner took ownership %d times before its earliest refusal, want exactly once", joined)
	}
}

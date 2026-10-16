package hooks

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/execution"
)

type hookExecutionPreparer struct {
	request execution.Request
	report  func() (execution.AdapterReport, error)
	cleanup func()
}

func (preparer *hookExecutionPreparer) PrepareExecution(ctx context.Context, request execution.Request) (execution.PreparedCommand, error) {
	preparer.request = request
	command := exec.CommandContext(ctx, request.Command.Name, request.Command.Args...)
	command.Env = request.Command.Env
	command.Dir = request.WorkingDirectory
	return execution.PreparedCommand{Command: command, Report: preparer.report, Cleanup: preparer.cleanup}, nil
}

func beforeToolConfig(hooks ...Definition) Config {
	return Config{Enabled: true, Hooks: hooks}
}

func TestExecCommandRunnerTimeoutKillsGrandchildHoldingOutput(t *testing.T) {
	switch os.Getenv("ZERO_HOOK_TREE_HELPER") {
	case "parent":
		if err := os.WriteFile(os.Getenv("ZERO_HOOK_TREE_PARENT_PID_FILE"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(2)
		}
		child := exec.Command(os.Args[0], "-test.run=^TestExecCommandRunnerTimeoutKillsGrandchildHoldingOutput$")
		child.Env = append(os.Environ(),
			"ZERO_HOOK_TREE_HELPER=grandchild",
			"ZERO_HOOK_TREE_STOP_FILE="+os.Getenv("ZERO_HOOK_TREE_STOP_FILE"),
		)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		if err := os.WriteFile(os.Getenv("ZERO_HOOK_TREE_GRANDCHILD_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = os.WriteFile(os.Getenv("ZERO_HOOK_TREE_STOP_FILE"), nil, 0o600)
			_ = child.Wait()
			os.Exit(4)
		}
		if err := os.WriteFile(os.Getenv("ZERO_HOOK_TREE_READY_FILE"), []byte("ready"), 0o600); err != nil {
			_ = os.WriteFile(os.Getenv("ZERO_HOOK_TREE_STOP_FILE"), nil, 0o600)
			_ = child.Wait()
			os.Exit(5)
		}
		if exitFile := os.Getenv("ZERO_HOOK_TREE_EXIT_FILE"); exitFile != "" {
			input, err := io.ReadAll(os.Stdin)
			if err != nil {
				os.Exit(6)
			}
			fmt.Fprintln(os.Stdout, string(input))
			fmt.Fprintln(os.Stderr, "hook diagnostic")
			waitForHookTreeStop(exitFile, 30*time.Second)
			os.Exit(0)
		}
		waitForHookTreeStop(os.Getenv("ZERO_HOOK_TREE_STOP_FILE"), 30*time.Second)
		_ = child.Wait()
		return
	case "grandchild":
		waitForHookTreeStop(os.Getenv("ZERO_HOOK_TREE_STOP_FILE"), 30*time.Second)
		os.Exit(0)
	}

	root := t.TempDir()
	parentPIDFile := filepath.Join(root, "parent.pid")
	grandchildPIDFile := filepath.Join(root, "grandchild.pid")
	readyFile := filepath.Join(root, "ready")
	stopFile := filepath.Join(root, "stop")
	owner := newHookTestProcessOwner(t, stopFile, parentPIDFile, grandchildPIDFile)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resultChannel := make(chan commandResult, 1)
	go func() {
		resultChannel <- execCommandRunner(
			ctx,
			os.Args[0],
			[]string{"-test.run=^TestExecCommandRunnerTimeoutKillsGrandchildHoldingOutput$"},
			nil,
			"",
			append(os.Environ(),
				"ZERO_HOOK_TREE_HELPER=parent",
				"ZERO_HOOK_TREE_PARENT_PID_FILE="+parentPIDFile,
				"ZERO_HOOK_TREE_GRANDCHILD_PID_FILE="+grandchildPIDFile,
				"ZERO_HOOK_TREE_READY_FILE="+readyFile,
				"ZERO_HOOK_TREE_STOP_FILE="+stopFile,
			),
		)
	}()
	parentPID, grandchildPID := awaitHookTreeReady(t, owner, readyFile, parentPIDFile, grandchildPIDFile)
	started := time.Now()
	var result commandResult
	select {
	case result = <-resultChannel:
	case <-time.After(6 * time.Second):
		cancel()
		t.Fatal("execCommandRunner did not return within six seconds after its timeout")
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("command remained blocked by grandchild output handles for %s", elapsed)
	}
	if result.Err == nil && result.ExitCode == 0 {
		t.Fatalf("timed-out command unexpectedly succeeded: %#v", result)
	}
	for role, pid := range map[string]int{"parent": parentPID, "grandchild": grandchildPID} {
		if err := owner.awaitExit(pid, 2*time.Second); err != nil {
			t.Fatalf("%s process %d survived hook cancellation: %v", role, pid, err)
		}
	}
}

// The test injects deadline expiry only after the process handoffs, independently
// of startup speed and of the watchdog that bounds a broken execution runner.
type hookDeadlineContext struct{ context.Context }

// Prevent context.WithTimeout from bypassing Err via the embedded cancelCtx.
func (hookDeadlineContext) Value(any) any { return nil }

func (ctx hookDeadlineContext) Err() error {
	if ctx.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}

func TestDispatchConfiguredRunnerTimeoutKillsGrandchildHoldingOutput(t *testing.T) {
	root := t.TempDir()
	parentFile, childFile := filepath.Join(root, "parent.pid"), filepath.Join(root, "child.pid")
	ready, stop, exit := filepath.Join(root, "ready"), filepath.Join(root, "stop"), filepath.Join(root, "exit")
	owner := newHookTestProcessOwner(t, stop, parentFile, childFile)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// Registered before launch: release the root and pipe holder even when the
	// production lifecycle regresses or a readiness assertion aborts the test.
	t.Cleanup(func() {
		cancel()
		for _, path := range []string{exit, stop} {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Error(err)
			}
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("dispatch did not finish after independent fixture cleanup")
		}
	})
	audit, err := NewAuditStore(AuditStoreOptions{AuditPath: filepath.Join(root, "audit.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	reported, cleaned := false, false
	preparer := &hookExecutionPreparer{
		report: func() (execution.AdapterReport, error) {
			reported = true
			return execution.AdapterReport{}, nil
		},
		cleanup: func() { cleaned = true },
	}
	dispatcher := NewDispatcher(DispatcherOptions{
		Config: beforeToolConfig(Definition{ID: "tree", Event: EventBeforeTool, Enabled: true,
			Command: os.Args[0], Args: []string{"-test.run=^TestExecCommandRunnerTimeoutKillsGrandchildHoldingOutput$"}}),
		Execution: execution.NewRunner(preparer), Audit: audit, Cwd: root,
		Timeout: time.Minute,
		Env: append(os.Environ(), "ZERO_HOOK_TREE_HELPER=parent",
			"ZERO_HOOK_TREE_PARENT_PID_FILE="+parentFile, "ZERO_HOOK_TREE_GRANDCHILD_PID_FILE="+childFile,
			"ZERO_HOOK_TREE_READY_FILE="+ready, "ZERO_HOOK_TREE_STOP_FILE="+stop, "ZERO_HOOK_TREE_EXIT_FILE="+exit),
	})
	results := make(chan DispatchOutcome, 1)
	go func() {
		defer close(done)
		results <- dispatcher.Dispatch(hookDeadlineContext{ctx}, DispatchInput{Event: EventBeforeTool, Payload: "payload"})
	}()
	parentPID, childPID := awaitHookTreeReady(t, owner, ready, parentFile, childFile)
	if err := os.WriteFile(exit, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := owner.awaitExit(parentPID, 2*time.Second); err != nil {
		t.Fatalf("root did not exit before deadline: %v", err)
	}
	cancel()
	var outcome DispatchOutcome
	select {
	case outcome = <-results:
	case <-time.After(4 * time.Second):
		t.Fatal("configured hook remained blocked by inherited output after deadline")
	}
	if !outcome.Blocked || outcome.Ran != 1 || !strings.Contains(outcome.Reason, "hook timed out") {
		t.Fatalf("deadline not reported: %#v", outcome)
	}
	if len(outcome.Messages) != 1 || outcome.Messages[0] != `"payload"` {
		t.Fatalf("stdin/output not preserved: %#v", outcome)
	}
	if !reported || !cleaned {
		t.Fatalf("adapter callbacks not preserved: report=%v cleanup=%v", reported, cleaned)
	}
	if err := owner.awaitExit(childPID, 2*time.Second); err != nil {
		t.Fatalf("grandchild survived configured hook deadline: %v", err)
	}
	events, err := audit.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != "hook_execution_started" || events[1].Status != AuditBlocked ||
		len(events[1].Results) != 1 || strings.TrimSpace(events[1].Results[0].Stdout) != `"payload"` ||
		strings.TrimSpace(events[1].Results[0].Stderr) != "hook diagnostic" {
		t.Fatalf("audit/output not preserved: %#v", events)
	}
}

func waitForHookTreeStop(stopFile string, lifetime time.Duration) {
	deadline := time.Now().Add(lifetime)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stopFile); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func awaitHookTreeReady(t *testing.T, owner *hookTestProcessOwner, readyFile string, pidFiles ...string) (int, int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			pids := make([]int, 0, len(pidFiles))
			for _, path := range pidFiles {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read helper PID handoff %q: %v", path, err)
				}
				pid, err := strconv.Atoi(string(data))
				if err != nil {
					t.Fatalf("parse helper PID handoff %q: %v", data, err)
				}
				if err := owner.retain(pid); err != nil {
					t.Fatalf("retain helper process %d: %v", pid, err)
				}
				pids = append(pids, pid)
			}
			return pids[0], pids[1]
		}
		if time.Now().After(deadline) {
			t.Fatal("process-tree helper did not hand off parent and grandchild identities")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDispatchRunsMatchingHooksAndRecordsAudit(t *testing.T) {
	var calls []string
	runner := func(ctx context.Context, command string, args []string, stdin []byte, cwd string, env []string) commandResult {
		calls = append(calls, command)
		return commandResult{ExitCode: 0, Stdout: "ok"}
	}
	audit, err := NewAuditStore(AuditStoreOptions{AuditPath: filepath.Join(t.TempDir(), "audit.jsonl")})
	if err != nil {
		t.Fatalf("NewAuditStore: %v", err)
	}
	config := beforeToolConfig(
		Definition{ID: "h1", Event: EventBeforeTool, Matcher: "bash", Command: "guard", Enabled: true},
		Definition{ID: "h2", Event: EventBeforeTool, Command: "log", Enabled: true}, // no matcher = always
		Definition{ID: "h3", Event: EventBeforeTool, Matcher: "read_file", Command: "skip", Enabled: true},
	)
	dispatcher := NewDispatcher(DispatcherOptions{Config: config, Audit: audit, run: runner})

	outcome := dispatcher.Dispatch(context.Background(), DispatchInput{Event: EventBeforeTool, ToolName: "bash", ToolCallID: "call_1"})
	if outcome.Blocked {
		t.Fatalf("unexpected block: %#v", outcome)
	}
	if outcome.Ran != 2 {
		t.Fatalf("Ran = %d, want 2 (h1 matcher + h2 unmatched), calls=%v", outcome.Ran, calls)
	}
	events, err := audit.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents: %v", err)
	}
	started, completed := 0, 0
	for _, event := range events {
		switch event.Type {
		case "hook_execution_started":
			started++
		case "hook_execution_completed":
			completed++
			if event.Status != AuditCompleted {
				t.Fatalf("status = %q, want completed", event.Status)
			}
		}
	}
	if started != 2 || completed != 2 {
		t.Fatalf("audit events: started=%d completed=%d, want 2/2", started, completed)
	}
}

func TestDispatchBeforeToolBlocksOnNonZeroExitAndStops(t *testing.T) {
	ran := 0
	runner := func(ctx context.Context, command string, args []string, stdin []byte, cwd string, env []string) commandResult {
		ran++
		if command == "deny" {
			return commandResult{ExitCode: 2, Stderr: "policy violation"}
		}
		return commandResult{ExitCode: 0}
	}
	config := beforeToolConfig(
		Definition{ID: "deny", Event: EventBeforeTool, Command: "deny", Enabled: true},
		Definition{ID: "after-deny", Event: EventBeforeTool, Command: "second", Enabled: true},
	)
	dispatcher := NewDispatcher(DispatcherOptions{Config: config, run: runner})

	outcome := dispatcher.Dispatch(context.Background(), DispatchInput{Event: EventBeforeTool, ToolName: "bash"})
	if !outcome.Blocked || outcome.BlockedBy != "deny" {
		t.Fatalf("outcome = %#v, want blocked by deny", outcome)
	}
	if outcome.Reason != "policy violation" {
		t.Fatalf("reason = %q, want hook stderr", outcome.Reason)
	}
	if ran != 1 {
		t.Fatalf("ran %d hooks, want 1 (must stop after the first veto)", ran)
	}
}

func TestDispatchNonBlockingEventDoesNotVetoOnNonZero(t *testing.T) {
	runner := func(ctx context.Context, command string, args []string, stdin []byte, cwd string, env []string) commandResult {
		return commandResult{ExitCode: 1, Stderr: "noisy"}
	}
	config := Config{Enabled: true, Hooks: []Definition{
		{ID: "notify", Event: EventAfterTool, Command: "notify", Enabled: true},
	}}
	dispatcher := NewDispatcher(DispatcherOptions{Config: config, run: runner})

	outcome := dispatcher.Dispatch(context.Background(), DispatchInput{Event: EventAfterTool, ToolName: "bash"})
	if outcome.Blocked {
		t.Fatalf("afterTool must not block: %#v", outcome)
	}
	if outcome.Ran != 1 {
		t.Fatalf("Ran = %d, want 1", outcome.Ran)
	}
}

func TestDispatchCollectsHookOutputMessages(t *testing.T) {
	runner := func(ctx context.Context, command string, args []string, stdin []byte, cwd string, env []string) commandResult {
		switch command {
		case "fmt":
			return commandResult{ExitCode: 0, Stdout: "  reformatted main.go  "}
		case "vet":
			return commandResult{ExitCode: 1, Stderr: "vet: suspicious construct"} // stdout empty → stderr surfaces
		case "quiet":
			return commandResult{ExitCode: 0} // no output → omitted
		}
		return commandResult{}
	}
	config := Config{Enabled: true, Hooks: []Definition{
		{ID: "fmt", Event: EventAfterTool, Command: "fmt", Enabled: true},
		{ID: "vet", Event: EventAfterTool, Command: "vet", Enabled: true},
		{ID: "quiet", Event: EventAfterTool, Command: "quiet", Enabled: true},
	}}
	dispatcher := NewDispatcher(DispatcherOptions{Config: config, run: runner})

	outcome := dispatcher.Dispatch(context.Background(), DispatchInput{Event: EventAfterTool, ToolName: "write_file"})
	if outcome.Blocked {
		t.Fatalf("afterTool must not block: %#v", outcome)
	}
	if outcome.Ran != 3 {
		t.Fatalf("Ran = %d, want 3", outcome.Ran)
	}
	want := []string{"reformatted main.go", "vet: suspicious construct"}
	if strings.Join(outcome.Messages, "|") != strings.Join(want, "|") {
		t.Fatalf("Messages = %#v, want trimmed stdout then stderr-fallback, quiet omitted: %#v", outcome.Messages, want)
	}
}

func TestDispatchSkipsWhenDisabledOrUnmatched(t *testing.T) {
	runner := func(ctx context.Context, command string, args []string, stdin []byte, cwd string, env []string) commandResult {
		t.Fatal("runner must not be called")
		return commandResult{}
	}
	disabled := Config{Enabled: false, Hooks: []Definition{{ID: "h", Event: EventBeforeTool, Command: "x", Enabled: true}}}
	if outcome := NewDispatcher(DispatcherOptions{Config: disabled, run: runner}).Dispatch(context.Background(), DispatchInput{Event: EventBeforeTool, ToolName: "bash"}); outcome.Ran != 0 {
		t.Fatalf("disabled config ran hooks: %#v", outcome)
	}
	unmatched := beforeToolConfig(Definition{ID: "h", Event: EventBeforeTool, Matcher: "read_file", Command: "x", Enabled: true})
	if outcome := NewDispatcher(DispatcherOptions{Config: unmatched, run: runner}).Dispatch(context.Background(), DispatchInput{Event: EventBeforeTool, ToolName: "bash"}); outcome.Ran != 0 {
		t.Fatalf("unmatched matcher ran hooks: %#v", outcome)
	}
}

func TestDispatchDeliversJSONPayloadOnStdin(t *testing.T) {
	var gotStdin string
	runner := func(ctx context.Context, command string, args []string, stdin []byte, cwd string, env []string) commandResult {
		gotStdin = string(stdin)
		return commandResult{ExitCode: 0}
	}
	config := beforeToolConfig(Definition{ID: "h", Event: EventBeforeTool, Command: "x", Enabled: true})
	dispatcher := NewDispatcher(DispatcherOptions{Config: config, run: runner})

	dispatcher.Dispatch(context.Background(), DispatchInput{
		Event:    EventBeforeTool,
		ToolName: "bash",
		Payload:  map[string]any{"tool": "bash", "args": map[string]any{"command": "ls"}},
	})
	if !strings.Contains(gotStdin, `"tool":"bash"`) || !strings.Contains(gotStdin, `"command":"ls"`) {
		t.Fatalf("stdin payload = %q, want serialized tool call", gotStdin)
	}
}

func TestExecCommandRunnerCapturesExitAndStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	// Echoes stdin to stderr and exits non-zero so we exercise both paths.
	result := execCommandRunner(context.Background(), "/bin/sh", []string{"-c", "cat 1>&2; exit 4"}, []byte("payload-123"), t.TempDir(), nil)
	if result.Err != nil {
		t.Fatalf("unexpected launch error: %v", result.Err)
	}
	if result.ExitCode != 4 {
		t.Fatalf("exit code = %d, want 4", result.ExitCode)
	}
	if !strings.Contains(result.Stderr, "payload-123") {
		t.Fatalf("stderr = %q, want stdin echoed", result.Stderr)
	}
}

func TestDispatcherRoutesHookThroughTypedExecutionOrigin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	preparer := &hookExecutionPreparer{}
	dispatcher := NewDispatcher(DispatcherOptions{
		Config:    beforeToolConfig(Definition{ID: "typed", Event: EventBeforeTool, Command: "/bin/sh", Args: []string{"-c", "cat"}, Enabled: true}),
		Cwd:       t.TempDir(),
		Execution: execution.NewRunner(preparer),
	})
	outcome := dispatcher.Dispatch(context.Background(), DispatchInput{Event: EventBeforeTool, ToolName: "bash", Payload: map[string]any{"ok": true}})
	if outcome.Blocked || outcome.Ran != 1 {
		t.Fatalf("dispatch outcome = %#v", outcome)
	}
	if preparer.request.Origin != execution.OriginHook || preparer.request.Mode != execution.ModeCaptured {
		t.Fatalf("execution request = %#v", preparer.request)
	}
}

func TestExecCommandRunnerReportsLaunchFailureFailsClosedForBeforeTool(t *testing.T) {
	result := execCommandRunner(context.Background(), "definitely-not-a-real-binary-zzz", nil, nil, t.TempDir(), nil)
	if result.Err == nil {
		t.Fatal("expected launch error for a missing binary")
	}
	// A launch failure for beforeTool fails closed (vetoes/blocks).
	if status, blocked := classifyResult(EventBeforeTool, result); !blocked || status != AuditBlocked {
		t.Fatalf("beforeTool classify = (%q, %v), want (blocked, true) for a launch failure", status, blocked)
	}
	// An observational afterTool hook still fails open (does not block).
	if status, blocked := classifyResult(EventAfterTool, result); blocked || status != AuditError {
		t.Fatalf("afterTool classify = (%q, %v), want (error, false) for a launch failure", status, blocked)
	}
}

func TestClassifyResultTimedOutFailsClosedForBeforeTool(t *testing.T) {
	// Unlike a launch failure, a hook that STARTED but was killed by its deadline
	// gave no verdict — a beforeTool policy hook must fail CLOSED (veto), or a hung
	// hook would silently wave the tool through.
	timedOut := commandResult{TimedOut: true, Err: context.DeadlineExceeded}
	if status, blocked := classifyResult(EventBeforeTool, timedOut); !blocked || status != AuditBlocked {
		t.Fatalf("beforeTool timeout classify = (%q, %v), want (blocked, true)", status, blocked)
	}
	// Observational events still never veto, even on timeout.
	if status, blocked := classifyResult(EventAfterTool, timedOut); blocked || status != AuditError {
		t.Fatalf("afterTool timeout classify = (%q, %v), want (error, false)", status, blocked)
	}
	if reason := blockReason(timedOut); !strings.Contains(reason, "timed out") {
		t.Fatalf("blockReason = %q, want a timeout message", reason)
	}
}

func TestDispatchBeforeToolFailsClosedWhenHookTimesOut(t *testing.T) {
	// End-to-end: a beforeTool hook that hangs past the dispatcher timeout vetoes
	// the tool. The injected runner blocks until its context is cancelled.
	runner := func(ctx context.Context, _ string, _ []string, _ []byte, _ string, _ []string) commandResult {
		<-ctx.Done()
		return commandResult{Err: ctx.Err()}
	}
	config := beforeToolConfig(Definition{ID: "slow-guard", Event: EventBeforeTool, Command: "hang", Enabled: true})
	dispatcher := NewDispatcher(DispatcherOptions{Config: config, run: runner, Timeout: 20 * time.Millisecond})

	outcome := dispatcher.Dispatch(context.Background(), DispatchInput{Event: EventBeforeTool, ToolName: "bash"})
	if !outcome.Blocked {
		t.Fatal("a timed-out beforeTool hook must fail closed (block the tool)")
	}
	if outcome.BlockedBy != "slow-guard" {
		t.Fatalf("BlockedBy = %q, want slow-guard", outcome.BlockedBy)
	}
	if !strings.Contains(outcome.Reason, "timed out") {
		t.Fatalf("Reason = %q, want a timeout reason", outcome.Reason)
	}
}

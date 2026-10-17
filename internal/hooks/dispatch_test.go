package hooks

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/execution"
)

type hookExecutionPreparer struct {
	request execution.Request
}

func (preparer *hookExecutionPreparer) PrepareExecution(_ context.Context, request execution.Request) (execution.PreparedCommand, error) {
	preparer.request = request
	return execution.PreparedCommand{Command: exec.Command(request.Command.Name, request.Command.Args...)}, nil
}

func beforeToolConfig(hooks ...Definition) Config {
	return Config{Enabled: true, Hooks: hooks}
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

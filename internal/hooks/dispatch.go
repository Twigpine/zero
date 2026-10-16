package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/execution"
)

// defaultHookTimeout bounds a single hook command so a hung or slow hook cannot
// stall the agent indefinitely.
const defaultHookTimeout = 30 * time.Second

// DispatchInput describes one lifecycle point at which hooks may run.
type DispatchInput struct {
	Event      Event
	ToolName   string // for beforeTool/afterTool matching
	ToolCallID string
	// Payload is serialized to JSON and written to each hook command's stdin so a
	// hook can inspect the tool call, its result, or session context.
	Payload any
}

// DispatchOutcome reports what happened for one Dispatch call.
type DispatchOutcome struct {
	Ran       int    // hooks that executed
	Blocked   bool   // a blocking-event hook exited non-zero, vetoing the action
	BlockedBy string // ID of the hook that blocked (empty unless Blocked)
	Reason    string // the blocking hook's stderr/stdout, for surfacing to the model
	// Messages collects the output (stdout, else stderr) of each hook that
	// produced any, in run order. afterTool validators use this to feed results
	// (e.g. a formatter diff or vet warning) back to the model on the tool result.
	Messages []string
}

type commandResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Err      error // set when the command could not be executed (not a non-zero exit)
	TimedOut bool  // the hook started but its deadline/cancellation fired before it returned
}

// commandRunner executes one hook command. It is injectable so the dispatch
// logic can be tested without spawning processes.
type commandRunner func(ctx context.Context, command string, args []string, stdin []byte, cwd string, env []string) commandResult

// DispatcherOptions configures a Dispatcher.
type DispatcherOptions struct {
	Config    Config
	Audit     *AuditStore       // optional; when set, every run is recorded
	Cwd       string            // working directory for hook commands
	Env       []string          // extra environment entries appended to the process env
	Timeout   time.Duration     // per-command timeout (defaults to defaultHookTimeout)
	Execution *execution.Runner // routes hook subprocesses through sandbox policy
	now       func() time.Time
	run       commandRunner
}

// Dispatcher selects and runs the hooks configured for a lifecycle event,
// recording each run to the audit store. A beforeTool hook that exits non-zero
// blocks the tool; hooks for other events are advisory (failures are recorded
// but do not interrupt the run).
type Dispatcher struct {
	config  Config
	audit   *AuditStore
	cwd     string
	env     []string
	timeout time.Duration
	now     func() time.Time
	run     commandRunner
}

// NewDispatcher builds a Dispatcher. A zero/empty config yields a dispatcher that
// runs nothing, so callers can always construct one unconditionally.
func NewDispatcher(options DispatcherOptions) *Dispatcher {
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultHookTimeout
	}
	now := options.now
	if now == nil {
		now = time.Now
	}
	run := options.run
	if run == nil {
		if options.Execution != nil {
			run = executionCommandRunner(options.Execution)
		} else {
			run = execCommandRunner
		}
	}
	return &Dispatcher{
		config:  options.Config,
		audit:   options.Audit,
		cwd:     options.Cwd,
		env:     options.Env,
		timeout: timeout,
		now:     now,
		run:     run,
	}
}

func executionCommandRunner(runner *execution.Runner) commandRunner {
	return func(ctx context.Context, command string, args []string, stdin []byte, cwd string, env []string) commandResult {
		result := runner.ExecuteCaptured(ctx, execution.CapturedRequest{
			Request: execution.Request{
				Origin:           execution.OriginHook,
				Mode:             execution.ModeCaptured,
				Command:          execution.Command{Name: command, Args: append([]string(nil), args...), Env: append([]string(nil), env...)},
				WorkingDirectory: cwd,
				WorkspaceRoots:   []string{cwd},
				Approval:         execution.ApprovalContext{PolicyVersion: execution.PolicyVersion},
			},
			Stdin: stdin,
		})
		exitCode := -1
		if result.Outcome.Exit != nil {
			exitCode = result.Outcome.Exit.Code
		}
		stderr := result.Stderr
		if stderr == "" && result.Outcome.Denial != nil {
			stderr = result.Outcome.Denial.Reason
		}
		commandErr := error(nil)
		switch result.Outcome.Kind {
		case execution.OutcomeSandboxSetupFailure, execution.OutcomeExecutableNotFound:
			commandErr = result.Err
		}
		return commandResult{
			ExitCode: exitCode,
			Stdout:   result.Stdout,
			Stderr:   stderr,
			Err:      commandErr,
			TimedOut: result.Outcome.Kind == execution.OutcomeTimedOut,
		}
	}
}

// blocksOn reports whether a non-zero exit for this event should veto the action.
// Only beforeTool gates the tool; other events are observational.
func blocksOn(event Event) bool {
	return event == EventBeforeTool
}

// Dispatch runs every enabled hook configured for the input event (and matcher,
// for tool events). It returns once all hooks have run, or early if a blocking
// hook vetoes the action.
func (dispatcher *Dispatcher) Dispatch(ctx context.Context, input DispatchInput) DispatchOutcome {
	outcome := DispatchOutcome{}
	if dispatcher == nil {
		return outcome
	}
	selected := Select(dispatcher.config, SelectInput{Event: input.Event, ToolName: input.ToolName})
	if len(selected) == 0 {
		return outcome
	}

	payload, err := json.Marshal(input.Payload)
	if err != nil {
		payload = nil
	}

	for _, hook := range selected {
		command := Command{Command: hook.Command, Args: hook.Args}
		dispatcher.recordStarted(hook, input, command)

		started := dispatcher.now()
		result := dispatcher.runWithTimeout(ctx, hook, payload)
		durationMs := int(dispatcher.now().Sub(started) / time.Millisecond)
		outcome.Ran++

		status, blocked := classifyResult(input.Event, result)
		dispatcher.recordCompleted(hook, input, status, result, durationMs)

		if message := hookMessage(result); message != "" {
			outcome.Messages = append(outcome.Messages, message)
		}

		if blocked {
			outcome.Blocked = true
			outcome.BlockedBy = hook.ID
			outcome.Reason = blockReason(result)
			// Stop on the first veto: the action is already denied.
			return outcome
		}
	}
	return outcome
}

func (dispatcher *Dispatcher) runWithTimeout(ctx context.Context, hook Definition, stdin []byte) commandResult {
	runCtx, cancel := context.WithTimeout(ctx, dispatcher.timeout)
	defer cancel()
	env := os.Environ()
	if len(dispatcher.env) > 0 {
		env = append(env, dispatcher.env...)
	}
	result := dispatcher.run(runCtx, hook.Command, hook.Args, stdin, dispatcher.cwd, env)
	// A deadline or cancellation that fired while the hook was running is distinct
	// from a launch failure: the hook DID start, we just never got its verdict.
	// classifyResult must fail CLOSED on this for a blocking event — a hung policy
	// hook cannot be allowed to wave the tool through.
	if runCtx.Err() != nil {
		result.TimedOut = true
	}
	return result
}

// classifyResult maps a command result to an audit status and whether it vetoes
// the action. A command that ran and exited non-zero blocks a beforeTool hook; a
// command that could not be executed at all is an error but never blocks (a
// missing hook binary must not wedge every tool call).
func classifyResult(event Event, result commandResult) (AuditStatus, bool) {
	// A hook that started but was killed by its deadline (or a cancellation) never
	// returned a verdict. For a blocking event we must fail CLOSED and veto: a
	// hung beforeTool policy hook cannot be treated as approval.
	if result.TimedOut {
		if blocksOn(event) {
			return AuditBlocked, true
		}
		return AuditError, false
	}
	if result.Err != nil {
		if blocksOn(event) {
			return AuditBlocked, true
		}
		return AuditError, false
	}
	if result.ExitCode != 0 {
		if blocksOn(event) {
			return AuditBlocked, true
		}
		return AuditError, false
	}
	return AuditCompleted, false
}

// hookMessage returns the output worth surfacing from a hook run: stdout when
// present, else stderr. Empty when the hook produced no output.
func hookMessage(result commandResult) string {
	if trimmed := strings.TrimSpace(result.Stdout); trimmed != "" {
		return trimmed
	}
	return strings.TrimSpace(result.Stderr)
}

func blockReason(result commandResult) string {
	if result.TimedOut {
		if trimmed := strings.TrimSpace(result.Stderr); trimmed != "" {
			return "hook timed out: " + trimmed
		}
		return "hook timed out before returning a verdict"
	}
	for _, candidate := range []string{result.Stderr, result.Stdout} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return trimmed
		}
	}
	return "hook exited non-zero"
}

func (dispatcher *Dispatcher) recordStarted(hook Definition, input DispatchInput, command Command) {
	if dispatcher.audit == nil {
		return
	}
	_, _ = dispatcher.audit.AppendStarted(AppendStartedInput{
		HookID:     hook.ID,
		Event:      input.Event,
		Matcher:    hook.Matcher,
		ToolCallID: input.ToolCallID,
		Commands:   []AuditCommand{command},
	})
}

func (dispatcher *Dispatcher) recordCompleted(hook Definition, input DispatchInput, status AuditStatus, result commandResult, durationMs int) {
	if dispatcher.audit == nil {
		return
	}
	_, _ = dispatcher.audit.AppendCompleted(AppendCompletedInput{
		HookID:     hook.ID,
		Event:      input.Event,
		Matcher:    hook.Matcher,
		ToolCallID: input.ToolCallID,
		Status:     status,
		Results:    []AuditResult{{ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr}},
		DurationMs: durationMs,
	})
}

// execCommandRunner runs a hook command directly (no shell), feeding the JSON
// payload on stdin and capturing stdout/stderr. A non-zero exit is reported via
// ExitCode (not Err); Err is reserved for commands that could not be launched.
func execCommandRunner(ctx context.Context, command string, args []string, stdin []byte, cwd string, env []string) commandResult {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = cwd
	cmd.Env = env
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := execution.RunCommand(ctx, cmd)
	result := commandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return result
	}
	if exitErr, ok := execution.AsPureExitError(err); ok {
		result.ExitCode = exitErr.ExitCode()
		return result
	}
	// Could not launch (binary missing, timeout, etc.).
	result.ExitCode = -1
	result.Err = err
	return result
}

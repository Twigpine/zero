package execution

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

const maxCapturedStreamBytes = 1 << 20

// Preparer is the platform-adapter seam. It turns a platform-neutral request
// into one enforceable OS command while keeping native sandbox mechanics out of
// every caller.
type Preparer interface {
	PrepareExecution(context.Context, Request) (PreparedCommand, error)
}

type PreparedCommand struct {
	Command     *exec.Cmd
	Enforcement Enforcement
	Report      func() (AdapterReport, error)
	Cleanup     func()
}

type CapturedRequest struct {
	Request Request
	Stdin   []byte
}

type CapturedResult struct {
	Stdout    string
	Stderr    string
	Truncated bool
	Outcome   Outcome
	Err       error
}

// Runner is the deep execution module used by Zero-owned subprocess launchers.
// The preparer may be installed after tool registration, but execution fails
// closed until an adapter is present.
type Runner struct {
	mu       sync.RWMutex
	preparer Preparer
}

func NewRunner(preparer Preparer) *Runner {
	return &Runner{preparer: preparer}
}

func (runner *Runner) SetPreparer(preparer Preparer) {
	if runner == nil {
		return
	}
	runner.mu.Lock()
	runner.preparer = preparer
	runner.mu.Unlock()
}

func (runner *Runner) ExecuteCaptured(ctx context.Context, input CapturedRequest) CapturedResult {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := input.Request.Validate(); err != nil {
		return capturedSetupFailure("invalid execution request: "+err.Error(), err, Enforcement{})
	}
	if input.Request.Mode != ModeCaptured {
		err := fmt.Errorf("captured execution requires mode %q", ModeCaptured)
		return capturedSetupFailure(err.Error(), err, Enforcement{})
	}
	prepared, err := runner.Prepare(ctx, input.Request)
	if err != nil {
		return capturedSetupFailure("prepare execution: "+err.Error(), err, prepared.Enforcement)
	}
	if prepared.Cleanup != nil {
		defer prepared.Cleanup()
	}
	if prepared.Command == nil {
		err := errors.New("execution adapter returned no command")
		return capturedSetupFailure(err.Error(), err, prepared.Enforcement)
	}
	if len(input.Stdin) > 0 {
		prepared.Command.Stdin = bytes.NewReader(input.Stdin)
	}
	stdout := &capturedBuffer{limit: maxCapturedStreamBytes}
	stderr := &capturedBuffer{limit: maxCapturedStreamBytes}
	prepared.Command.Stdout = stdout
	prepared.Command.Stderr = stderr
	runErr := RunCommand(ctx, prepared.Command)
	report, reportErr := AdapterReport{}, error(nil)
	if prepared.Report != nil {
		report, reportErr = prepared.Report()
	}
	result := CapturedResult{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		Truncated: stdout.Truncated() || stderr.Truncated(),
		Err:       runErr,
		Outcome: Outcome{
			Enforcement: prepared.Enforcement,
		},
	}
	exitCode := commandExitCode(runErr)
	result.Outcome.Exit = &Exit{Code: exitCode}
	switch {
	case reportErr != nil:
		result.Outcome.State = StateFailed
		result.Outcome.Kind = OutcomeSandboxSetupFailure
		result.Err = reportErr
	case report.Denial != nil:
		denial := *report.Denial
		result.Outcome.State = StateDenied
		result.Outcome.Kind = OutcomeEnforcementDenied
		result.Outcome.Denial = &denial
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		result.Outcome.State = StateFailed
		result.Outcome.Kind = OutcomeTimedOut
	case errors.Is(ctx.Err(), context.Canceled):
		result.Outcome.State = StateCancelled
		result.Outcome.Kind = OutcomeCancelled
	case runErr == nil:
		result.Outcome.State = StateCompleted
		result.Outcome.Kind = OutcomeSuccess
	case executableNotFound(runErr):
		result.Outcome.State = StateFailed
		result.Outcome.Kind = OutcomeExecutableNotFound
	default:
		result.Outcome.State = StateFailed
		result.Outcome.Kind = OutcomeApplicationFailure
	}
	return result
}

// Prepare evaluates and prepares a typed request for callers that require
// streaming pipes or protocol-specific lifecycle handling, such as stdio MCP.
// The returned cleanup remains mandatory; ordinary captured commands should use
// ExecuteCaptured so the Runner owns it automatically.
func (runner *Runner) Prepare(ctx context.Context, request Request) (PreparedCommand, error) {
	if runner == nil {
		return PreparedCommand{}, errors.New("execution runner is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := request.Validate(); err != nil {
		return PreparedCommand{}, err
	}
	runner.mu.RLock()
	preparer := runner.preparer
	runner.mu.RUnlock()
	if preparer == nil {
		return PreparedCommand{}, errors.New("execution adapter is not configured")
	}
	return preparer.PrepareExecution(ctx, request)
}

func capturedSetupFailure(message string, err error, enforcement Enforcement) CapturedResult {
	return CapturedResult{
		Stderr: message,
		Err:    err,
		Outcome: Outcome{
			State:       StateFailed,
			Kind:        OutcomeSandboxSetupFailure,
			Exit:        &Exit{Code: -1},
			Enforcement: enforcement,
		},
	}
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := AsPureExitError(err); ok {
		return exitErr.ExitCode()
	}
	return -1
}

func executableNotFound(err error) bool {
	var execErr *exec.Error
	return errors.As(err, &execErr)
}

type capturedBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *capturedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining > 0 {
		_, _ = buffer.buffer.Write(data[:min(len(data), remaining)])
	}
	if len(data) > remaining {
		buffer.truncated = true
	}
	return len(data), nil
}

func (buffer *capturedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func (buffer *capturedBuffer) Truncated() bool {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.truncated
}

var _ io.Writer = (*capturedBuffer)(nil)

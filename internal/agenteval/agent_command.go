package agenteval

import (
	"bytes"
	"context"
	"os/exec"
	"strings"

	"github.com/Gitlawb/zero/internal/execution"
)

type AgentRunInput struct {
	TaskID        string
	Prompt        string
	WorkspacePath string
	Model         string
}

type AgentRunResult struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	Error    string `json:"error,omitempty"`
	// Truncated is set when captured stdout/stderr exceeded the runner's
	// OutputLimit and some output was dropped.
	Truncated bool `json:"truncated,omitempty"`
}

type AgentRunner interface {
	Run(context.Context, AgentRunInput) AgentRunResult
}

type AgentRunnerFunc func(context.Context, AgentRunInput) AgentRunResult

// defaultAgentOutputLimit caps captured stdout/stderr per stream so a chatty or
// runaway agent cannot exhaust memory or bloat the benchmark report.
const defaultAgentOutputLimit = 1 << 20 // 1 MiB per stream

type CommandAgentRunner struct {
	Command []string
	// OutputLimit caps captured stdout/stderr per stream in bytes. Zero applies
	// defaultAgentOutputLimit; a negative value disables the cap.
	OutputLimit int
}

func (runner CommandAgentRunner) Run(ctx context.Context, input AgentRunInput) AgentRunResult {
	result := AgentRunResult{ExitCode: -1}
	if len(runner.Command) == 0 || strings.TrimSpace(runner.Command[0]) == "" {
		result.Error = "agent command is required"
		return result
	}
	if strings.TrimSpace(input.WorkspacePath) == "" {
		result.Error = "workspace path is required"
		return result
	}
	if ctx == nil {
		ctx = context.Background()
	}
	limit := runner.OutputLimit
	if limit == 0 {
		limit = defaultAgentOutputLimit
	}
	command := expandAgentCommand(runner.Command, input)
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = input.WorkspacePath
	stdout := &capWriter{limit: limit}
	stderr := &capWriter{limit: limit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := execution.RunCommand(ctx, cmd)
	result.Stdout = stdout.buf.String()
	result.Stderr = stderr.buf.String()
	result.Truncated = stdout.truncated || stderr.truncated
	if err == nil {
		result.ExitCode = 0
		return result
	}
	// A canceled or timed-out context kills the process, surfacing as a signal
	// exit; report the context error explicitly instead of "exited with code -1".
	if ctxErr := ctx.Err(); ctxErr != nil {
		result.Error = ctxErr.Error()
		return result
	}
	if exitErr, ok := execution.AsPureExitError(err); ok {
		result.ExitCode = exitErr.ExitCode()
		return result
	}
	result.Error = err.Error()
	return result
}

func expandAgentCommand(command []string, input AgentRunInput) []string {
	// strings.NewReplacer performs a single left-to-right pass and never
	// re-scans replaced text, so a placeholder value that itself contains
	// "{workspace}"/"{task_id}"/"{model}" is not re-expanded.
	replacer := strings.NewReplacer(
		"{prompt}", input.Prompt,
		"{workspace}", input.WorkspacePath,
		"{task_id}", input.TaskID,
		"{model}", input.Model,
	)
	expanded := make([]string, len(command))
	for i, arg := range command {
		expanded[i] = replacer.Replace(arg)
	}
	return expanded
}

// capWriter buffers writes up to limit bytes and records whether any data was
// dropped. A non-positive limit means unbounded.
type capWriter struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		return w.buf.Write(p)
	}
	remaining := w.limit - w.buf.Len()
	if remaining <= 0 {
		w.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		if _, err := w.buf.Write(p[:remaining]); err != nil {
			return 0, err
		}
		w.truncated = true
		return len(p), nil
	}
	return w.buf.Write(p)
}

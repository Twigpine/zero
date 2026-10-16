package agenteval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/execution"
)

// defaultCommandTimeout bounds a single verification command so a hung command
// (e.g. a test that waits on input) cannot run unbounded. Generous enough for
// real build/test commands; overridable per run via RunInput.CommandTimeout.
const defaultCommandTimeout = 10 * time.Minute

type RunInput struct {
	TaskID        string
	WorkspacePath string
	TraceStdout   string
	// CommandTimeout bounds each verification command. Non-positive applies
	// defaultCommandTimeout.
	CommandTimeout time.Duration
}

type CommandRunner func(context.Context, string, Command) CommandResult

type ChangedFilesFunc func(context.Context, string) ([]string, error)

type Runner struct {
	RunCommand   CommandRunner
	ChangedFiles ChangedFilesFunc

	runGit func(context.Context, string, ...string) ([]byte, error)
}

func (runner Runner) Run(ctx context.Context, suite Suite, input RunInput) Report {
	if ctx == nil {
		ctx = context.Background()
	}
	task, err := selectTask(suite, input.TaskID)
	if err != nil {
		return Score(suite, ScoreInput{TaskID: input.TaskID})
	}
	workspace, err := verifyWorkspace(input.WorkspacePath)
	if err != nil {
		return runner.blocked(suite, task.ID, fmt.Sprintf("workspace setup failed: %v", err), nil)
	}
	if err := ctx.Err(); err != nil {
		return runner.blocked(suite, task.ID, err.Error(), nil)
	}
	var contextResult *ContextCheckResult
	var contextError string
	if len(task.ContextChecks.RequiredFiles) > 0 || len(task.ContextChecks.ForbiddenFiles) > 0 {
		result, err := task.ContextChecks.CheckWorkspace(workspace)
		if err != nil {
			contextError = err.Error()
		} else {
			contextResult = &result
		}
	}

	timeout := input.CommandTimeout
	if timeout <= 0 {
		timeout = defaultCommandTimeout
	}
	results := make([]CommandResult, 0, len(task.VerificationCommands))
	for _, command := range task.VerificationCommands {
		if err := ctx.Err(); err != nil {
			return runner.blocked(suite, task.ID, err.Error(), results)
		}
		result := runner.runCommand(ctx, workspace, command, timeout)
		if result.ID == "" {
			result.ID = command.ID
		}
		results = append(results, result)
	}

	files, err := runner.changedFiles(ctx, workspace)
	if err != nil {
		reason := fmt.Sprintf("changed files collection failed: %v", err)
		return runner.blocked(suite, task.ID, reason, results)
	}

	return Score(suite, ScoreInput{
		TaskID:             task.ID,
		CommandResults:     results,
		ChangedFiles:       files,
		ContextCheckResult: contextResult,
		ContextCheckError:  contextError,
		TraceStdout:        input.TraceStdout,
	})
}

func (runner Runner) blocked(suite Suite, taskID string, reason string, results []CommandResult) Report {
	return Score(suite, ScoreInput{
		TaskID:         taskID,
		CommandResults: results,
		Blocked:        true,
		BlockReason:    reason,
	})
}

func (runner Runner) runCommand(ctx context.Context, workspace string, command Command, timeout time.Duration) CommandResult {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if runner.RunCommand != nil {
		return runner.RunCommand(ctx, workspace, command)
	}
	return execCommand(ctx, workspace, command)
}

func (runner Runner) changedFiles(ctx context.Context, workspace string) ([]string, error) {
	if runner.ChangedFiles != nil {
		return runner.ChangedFiles(ctx, workspace)
	}
	runGit := runner.runGit
	if runGit == nil {
		runGit = defaultRunGit
	}
	output, err := runGit(ctx, workspace, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	return parseGitStatusPorcelain(output), nil
}

func execCommand(ctx context.Context, workspace string, command Command) CommandResult {
	result := CommandResult{ID: command.ID, ExitCode: -1}
	if emptyCommand(command.Command) {
		result.Error = "command must not be empty"
		return result
	}
	args := trimCommand(command.Command)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = workspace
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := execution.RunCommand(ctx, cmd)
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
	if err == nil {
		result.ExitCode = 0
		return result
	}
	if exitErr, ok := execution.AsPureExitError(err); ok {
		result.ExitCode = exitErr.ExitCode()
		return result
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		result.Error = ctxErr.Error()
		return result
	}
	result.Error = err.Error()
	return result
}

func defaultRunGit(ctx context.Context, workspace string, args ...string) ([]byte, error) {
	allArgs := append([]string{"-C", workspace}, args...)
	cmd := exec.CommandContext(ctx, "git", allArgs...)
	var output bytes.Buffer
	cmd.Stdout = &output
	err := execution.RunCommand(ctx, cmd)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	return output.Bytes(), nil
}

func parseGitStatusPorcelain(output []byte) []string {
	lines := strings.Split(string(output), "\n")
	files := make([]string, 0, len(lines))
	for _, line := range lines {
		if len(line) < 4 {
			continue
		}
		file := strings.TrimSpace(line[3:])
		if arrow := strings.LastIndex(file, " -> "); arrow >= 0 {
			file = file[arrow+4:]
		}
		files = append(files, file)
	}
	return normalizeFiles(files)
}

func verifyWorkspace(workspace string) (string, error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return "", errors.New("workspace path is required")
	}
	clean, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(clean)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("workspace path must be a directory")
	}
	return clean, nil
}

func trimCommand(command []string) []string {
	trimmed := make([]string, 0, len(command))
	for _, part := range command {
		part = strings.TrimSpace(part)
		if part != "" {
			trimmed = append(trimmed, part)
		}
	}
	return trimmed
}

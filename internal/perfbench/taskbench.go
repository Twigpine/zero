package perfbench

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/execution"
)

// TaskSchemaVersion is the schema version of a published task-benchmark result.
// Bump it whenever the TaskRunResult shape changes so consumers (docs/BENCHMARK.md
// tooling, dashboards) can detect format drift.
const TaskSchemaVersion = 1

// TaskSet is a reproducible benchmark task list. It mirrors the shape of a
// Terminal-Bench task manifest: each task is an isolated workspace plus a prompt
// ZERO must satisfy. The set is recorded by ID with every result so a published
// number is traceable to the exact tasks that produced it.
type TaskSet struct {
	ID          string      `json:"id"`
	Name        string      `json:"name,omitempty"`
	Description string      `json:"description,omitempty"`
	Tasks       []BenchTask `json:"tasks"`
	// BuildOnlyClasses lists task classes whose verificationCommand is a
	// non-positive build check (e.g. refactor's `go build ./...`): it proves the
	// edit compiles, not that the refactor achieved its goal. The turn benchmark
	// reports these separately from correctness oracles so a build-pass cannot be
	// misread as a correctness pass. Classes with a verificationCommand that are
	// NOT listed here are treated as correctness classes; classes whose tasks
	// carry no verificationCommand are latency-only regardless of this list.
	BuildOnlyClasses []string `json:"buildOnlyClasses,omitempty"`
}

// BenchTask is one benchmark task. WorkspaceFixture is the relative path of the
// task's starting workspace; VerificationCommand (optional) is the command the
// default runner executes to decide pass/fail after ZERO finishes. Class groups
// the task for the turn-benchmark's per-group latency breakdown (e.g. "nav",
// "edit", "fix"); it is optional and ignored by the pass/fail runner.
type BenchTask struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name,omitempty"`
	Class               string   `json:"class,omitempty"`
	Prompt              string   `json:"prompt"`
	WorkspaceFixture    string   `json:"workspaceFixture,omitempty"`
	VerificationCommand []string `json:"verificationCommand,omitempty"`
	// OracleTest is Go source stamped into <fixtureCopy>/oracle_test.go AFTER
	// the agent run (so it can't interfere with the agent's own go build/test
	// during the task and can't be pre-seen). The verificationCommand then runs
	// `go test ./...`, making the Go compiler the structural verifier: a
	// compile-time reference (e.g. `var _ = Config{}.Label`) or a behavioral call
	// fails to compile/run when the task wasn't actually done. Empty for tasks
	// whose verificationCommand is a plain grep/build with no stamped test.
	OracleTest string `json:"oracleTest,omitempty"`
}

// TaskConfig is the recorded configuration of a benchmark run. The honest
// framing matters: the model, mode, and self-correct flag are captured because
// the score is largely model-bounded — the point of the benchmark is to show the
// self-correct loop's contribution (the with/without delta), not the raw digit.
type TaskConfig struct {
	Model       string
	Mode        string
	SelfCorrect bool
	Version     string
	Commit      string
	// Now overrides the clock for the recorded date (tests inject a fixed time).
	Now func() time.Time
	// Runner executes one task and reports the outcome. Required. The default
	// production runner (NewExecRunner) invokes headless `zero exec`.
	Runner TaskRunner
}

// RunContext is the immutable per-run context handed to the runner for each
// task, so a runner records the same model/mode/self-correct values the result
// is stamped with.
type RunContext struct {
	Model       string
	Mode        string
	SelfCorrect bool
	// ExecProfile, when non-empty, is forwarded to the agent invocation as
	// --exec-profile (turn benchmark runner; the task benchmark leaves it
	// empty).
	ExecProfile string
}

// TaskRunner runs a single benchmark task and returns its outcome.
type TaskRunner func(ctx context.Context, task BenchTask, rc RunContext) TaskOutcome

// TaskOutcome is what a runner reports for one task. Err is reserved for the run
// failing to execute (e.g. the agent process crashed); Passed reports whether
// the task's verification succeeded. A non-nil Err is always a non-pass.
type TaskOutcome struct {
	Passed bool
	Detail string
	Err    error
}

// TaskResult is the per-task entry in a TaskRunResult.
type TaskResult struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Passed  bool   `json:"passed"`
	Errored bool   `json:"errored,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// TaskRunResult is the publishable benchmark record. It is intentionally
// self-describing: model + version + commit + self-correct flag + date are all
// embedded so a copy of this JSON is reproducible and auditable on its own.
type TaskRunResult struct {
	SchemaVersion  int          `json:"schemaVersion"`
	Suite          string       `json:"suite"`
	SuiteName      string       `json:"suiteName,omitempty"`
	Model          string       `json:"model"`
	Mode           string       `json:"mode,omitempty"`
	SelfCorrect    bool         `json:"selfCorrect"`
	Version        string       `json:"version,omitempty"`
	Commit         string       `json:"commit,omitempty"`
	Date           string       `json:"date"`
	TasksAttempted int          `json:"tasksAttempted"`
	TasksPassed    int          `json:"tasksPassed"`
	Errors         int          `json:"errors"`
	PassRate       float64      `json:"passRate"`
	Tasks          []TaskResult `json:"tasks"`
}

// LoadTaskSet reads and validates a benchmark task set from a JSON file. Unknown
// fields are rejected so a malformed manifest fails loudly rather than silently
// dropping tasks.
func LoadTaskSet(path string) (TaskSet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return TaskSet{}, fmt.Errorf("load task set: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var set TaskSet
	if err := decoder.Decode(&set); err != nil {
		return TaskSet{}, fmt.Errorf("parse task set: %w", err)
	}
	// DisallowUnknownFields only validates the first top-level value, so a file
	// like `{...}\nnot-json` would still load. Reject any trailing content so a
	// malformed manifest fails loudly rather than silently using only the prefix.
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return TaskSet{}, errors.New("parse task set: unexpected trailing content")
	}
	if strings.TrimSpace(set.ID) == "" {
		return TaskSet{}, errors.New("parse task set: id is required")
	}
	if len(set.Tasks) == 0 {
		return TaskSet{}, errors.New("parse task set: tasks must not be empty")
	}
	// Resolve relative workspace fixtures against the manifest's directory so the
	// runner's cmd.Dir does not depend on the caller's cwd (the documented sample
	// uses paths like ./hello relative to the manifest, not the repo root).
	base := filepath.Dir(path)
	for index, task := range set.Tasks {
		if strings.TrimSpace(task.ID) == "" {
			return TaskSet{}, fmt.Errorf("parse task set: tasks[%d] id is required", index)
		}
		if strings.TrimSpace(task.Prompt) == "" {
			return TaskSet{}, fmt.Errorf("parse task set: tasks[%d] prompt is required", index)
		}
		if fixture := strings.TrimSpace(task.WorkspaceFixture); fixture != "" && !filepath.IsAbs(fixture) {
			set.Tasks[index].WorkspaceFixture = filepath.Clean(filepath.Join(base, fixture))
		}
	}
	return set, nil
}

// SkipRunner is a runner that performs no work and records every task as skipped
// (not passed, no error). It backs the harness's --dry-run mode, which exercises
// the full record path without invoking an agent or a model.
func SkipRunner(context.Context, BenchTask, RunContext) TaskOutcome {
	return TaskOutcome{Passed: false, Detail: "skipped (dry run)"}
}

// RunTasks executes every task in the set with the configured runner and returns
// a self-describing result record. It never aborts on a single task failure or
// runner error — every task is attempted and recorded — so one bad task can't
// hide the rest of the run. The context cancels the whole run.
func RunTasks(ctx context.Context, set TaskSet, config TaskConfig) (TaskRunResult, error) {
	if len(set.Tasks) == 0 {
		return TaskRunResult{}, errors.New("task set has no tasks")
	}
	if strings.TrimSpace(config.Model) == "" {
		return TaskRunResult{}, errors.New("benchmark requires a model")
	}
	if config.Runner == nil {
		return TaskRunResult{}, errors.New("benchmark requires a task runner")
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	rc := RunContext{Model: config.Model, Mode: config.Mode, SelfCorrect: config.SelfCorrect}

	result := TaskRunResult{
		SchemaVersion: TaskSchemaVersion,
		Suite:         strings.TrimSpace(set.ID),
		SuiteName:     strings.TrimSpace(set.Name),
		Model:         strings.TrimSpace(config.Model),
		Mode:          strings.TrimSpace(config.Mode),
		SelfCorrect:   config.SelfCorrect,
		Version:       strings.TrimSpace(config.Version),
		Commit:        strings.TrimSpace(config.Commit),
		Date:          now().UTC().Format(time.RFC3339),
		Tasks:         make([]TaskResult, 0, len(set.Tasks)),
	}

	for _, task := range set.Tasks {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		outcome := config.Runner(ctx, task, rc)
		entry := TaskResult{ID: task.ID, Name: task.Name}
		result.TasksAttempted++
		switch {
		case outcome.Err != nil:
			entry.Errored = true
			entry.Detail = outcome.Err.Error()
			result.Errors++
		case outcome.Passed:
			entry.Passed = true
			entry.Detail = strings.TrimSpace(outcome.Detail)
			result.TasksPassed++
		default:
			entry.Detail = strings.TrimSpace(outcome.Detail)
		}
		result.Tasks = append(result.Tasks, entry)
	}
	if result.TasksAttempted > 0 {
		result.PassRate = RoundMetric(float64(result.TasksPassed) / float64(result.TasksAttempted))
	}
	return result, nil
}

// FormatTaskSummary renders a human-readable summary that foregrounds the honest
// framing: the model and the self-correct setting are printed alongside the
// score, because the number is model-bounded.
func FormatTaskSummary(result TaskRunResult) string {
	selfCorrect := "off"
	if result.SelfCorrect {
		selfCorrect = "on"
	}
	build := strings.TrimSpace(result.Version)
	if commit := strings.TrimSpace(result.Commit); commit != "" {
		if build != "" {
			build += " (commit " + commit + ")"
		} else {
			build = "commit " + commit
		}
	}
	lines := []string{
		"Zero task benchmark: " + displayOrUnknown(result.Suite),
		"model: " + displayOrUnknown(result.Model),
	}
	if mode := strings.TrimSpace(result.Mode); mode != "" {
		lines = append(lines, "mode: "+mode)
	}
	lines = append(lines, "self-correct: "+selfCorrect)
	if build != "" {
		lines = append(lines, "build: "+build)
	}
	lines = append(lines, fmt.Sprintf("passed: %d/%d (%.0f%%)", result.TasksPassed, result.TasksAttempted, result.PassRate*100))
	if result.Errors > 0 {
		lines = append(lines, fmt.Sprintf("errors: %d", result.Errors))
	}
	for _, task := range result.Tasks {
		lines = append(lines, "- "+formatTaskLine(task))
	}
	return strings.Join(lines, "\n")
}

// WriteTaskJSON writes the indented JSON form of a benchmark result.
func WriteTaskJSON(w io.Writer, result TaskRunResult) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func formatTaskLine(task TaskResult) string {
	status := "pass"
	if task.Errored {
		status = "error"
	} else if !task.Passed {
		status = "fail"
	}
	label := strings.TrimSpace(task.Name)
	if label == "" {
		label = task.ID
	} else {
		label = task.ID + " " + label
	}
	line := fmt.Sprintf("[%s] %s", status, label)
	if detail := strings.TrimSpace(task.Detail); detail != "" {
		line += ": " + detail
	}
	return line
}

func displayOrUnknown(value string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return "unknown"
}

// NewExecRunner builds the production runner: it invokes headless `zero exec`
// with stream-json output for each task and decides pass/fail. binary is the path
// to the `zero` binary; extraArgs are appended to every invocation (e.g. sandbox
// flags). The self-correct flag from RunContext is translated into the exec
// invocation so the recorded config matches what actually ran.
//
// Pass/fail is decided from the stream-json run_end event's exit code: a zero
// exit means the agent completed the task without error. When a task carries a
// VerificationCommand, that command is run after the agent and its exit status
// is authoritative (this mirrors Terminal-Bench's external verifier model).
func NewExecRunner(binary string, extraArgs ...string) TaskRunner {
	return func(ctx context.Context, task BenchTask, rc RunContext) TaskOutcome {
		args := buildExecArgs(task, rc, extraArgs)
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = appendNoColor(os.Environ())
		if dir := strings.TrimSpace(task.WorkspaceFixture); dir != "" {
			cmd.Dir = dir
		}
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		runErr := execution.RunCommand(ctx, cmd)
		if !runEndCanReconcile(runErr) {
			if errors.Is(runErr, exec.ErrWaitDelay) {
				return TaskOutcome{Err: fmt.Errorf("zero exec output cleanup failed: %w", runErr)}
			}
			return TaskOutcome{Err: fmt.Errorf("zero exec command failed: %w", runErr)}
		}

		// The terminal run_end exit code is authoritative for pass/fail: a non-zero
		// agent exit is a normal task failure, not a harness error, even though
		// cmd.Run() reports any non-zero process exit as an *exec.ExitError. Only when
		// no terminal run_end event was parsed do we fall back to treating the run as
		// a harness error — failing closed so a missing terminal event never reads as
		// a pass.
		exitCode, haveExit := streamJSONExitCode(stdout.Bytes())
		if haveExit && exitCode != 0 {
			return TaskOutcome{Passed: false, Detail: fmt.Sprintf("agent run_end exit code %d", exitCode)}
		}
		if !haveExit {
			detail := strings.TrimSpace(stderr.String())
			if detail == "" && runErr != nil {
				detail = runErr.Error()
			}
			if detail == "" {
				detail = "missing terminal run_end event"
			}
			return TaskOutcome{Err: fmt.Errorf("zero exec failed: %s", detail)}
		}

		if len(task.VerificationCommand) > 0 {
			return runVerification(ctx, task)
		}
		return TaskOutcome{Passed: true}
	}
}

// runEndCanReconcile reports whether a command result contains only an ordinary
// process exit status. A run_end may explain success or an *exec.ExitError, but
// it must not hide cancellation, startup, process-tree, or output-cleanup errors.
func runEndCanReconcile(err error) bool {
	if err == nil {
		return true
	}
	_, ok := execution.AsPureExitError(err)
	return ok
}

func buildExecArgs(task BenchTask, rc RunContext, extraArgs []string) []string {
	args := []string{"exec", "--output-format", "stream-json"}
	if model := strings.TrimSpace(rc.Model); model != "" {
		args = append(args, "--model", model)
	}
	if mode := strings.TrimSpace(rc.Mode); mode != "" {
		args = append(args, "--mode", mode)
	}
	if rc.SelfCorrect {
		args = append(args, "--self-correct")
	}
	args = append(args, extraArgs...)
	args = append(args, task.Prompt)
	return args
}

func runVerification(ctx context.Context, task BenchTask) TaskOutcome {
	cmd := exec.CommandContext(ctx, task.VerificationCommand[0], task.VerificationCommand[1:]...)
	// Inherit the environment so the verifier sees PATH, HOME, language toolchain
	// vars, etc. — the same surface a maintainer gets running the command by hand
	// (matching the agent run above). NO_COLOR is appended for stable output.
	cmd.Env = appendNoColor(os.Environ())
	if dir := strings.TrimSpace(task.WorkspaceFixture); dir != "" {
		cmd.Dir = dir
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := execution.RunCommand(ctx, cmd)
	if err != nil {
		detail := strings.TrimSpace(output.String())
		if detail == "" {
			detail = err.Error()
		}
		return TaskOutcome{Passed: false, Detail: "verification failed: " + firstLine(detail)}
	}
	return TaskOutcome{Passed: true}
}

// streamJSONResult holds the terminal events parsed from one pass over a
// stream-json output: the last run_end exit code (if any) and the last final
// event's text (the agent's answer, if any). streamJSONExitCode and
// streamJSONFinalText both consume this so the scanner limits and
// malformed-event handling live in exactly one place.
type streamJSONResult struct {
	exitCode  int
	haveExit  bool
	finalText string
}

// parseStreamJSON scans stream-json output once and returns the terminal
// run_end exit code and the final event's text. ZERO emits one JSON object per
// line; the last run_end carries the exit code and the last final carries the
// answer. Lines that don't parse are skipped. The scanner buffer is raised so a
// large single-line event (e.g. a big final answer) doesn't error out.
func parseStreamJSON(output []byte) streamJSONResult {
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var r streamJSONResult
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			ExitCode *int   `json:"exitCode"`
			Text     string `json:"text"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		if event.Type == "run_end" && event.ExitCode != nil {
			r.exitCode = *event.ExitCode
			r.haveExit = true
		}
		if event.Type == "final" {
			r.finalText = event.Text
		}
	}
	return r
}

// streamJSONExitCode scans stream-json output for the terminal run_end event and
// returns its exit code. The last run_end event carries the exit code. Lines
// that don't parse are skipped.
func streamJSONExitCode(output []byte) (int, bool) {
	r := parseStreamJSON(output)
	return r.exitCode, r.haveExit
}

// streamJSONFinalText scans stream-json output for the terminal "final" event
// and returns its text (the agent's final answer). ZERO emits one final event
// per run on both the success and incomplete paths (exec.go writer.final), so
// the last one wins. The turn benchmark writes this to .zero-answer.txt in the
// fixture copy so nav answer-oracles can grep the captured answer rather than
// the raw stream. Returns "" when no final event was emitted (e.g. a provider
// error path) — the caller writes an empty file and a nav grep then fails,
// which is the correct outcome for a run that produced no answer.
func streamJSONFinalText(output []byte) string {
	return parseStreamJSON(output).finalText
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return strings.TrimSpace(text[:index])
	}
	return text
}

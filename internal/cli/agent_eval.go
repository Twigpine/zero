package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/agenteval"
)

type agentEvalOptions struct {
	Mode           string        `json:"mode"`
	SuitePath      string        `json:"suite_path"`
	TaskID         string        `json:"task_id,omitempty"`
	WorkspacePath  string        `json:"workspace_path,omitempty"`
	WorkRoot       string        `json:"work_root,omitempty"`
	AgentCommand   []string      `json:"agent_command,omitempty"`
	Models         []string      `json:"models,omitempty"`
	KeepWorkspaces bool          `json:"keep_workspaces,omitempty"`
	Timeout        time.Duration `json:"timeout,omitempty"`
	ReportDir      string        `json:"report_dir,omitempty"`
	JSON           bool          `json:"json"`
}

// agentEvalRuntimeError marks an eval failure that is a runtime/environment
// problem (e.g. failing to create a work root) rather than a usage error, so
// the command can exit with a crash code instead of the usage code.
type agentEvalRuntimeError struct{ err error }

func (e agentEvalRuntimeError) Error() string { return e.err.Error() }

func (e agentEvalRuntimeError) Unwrap() error { return e.err }

type agentEvalReport struct {
	Suite      string                     `json:"suite"`
	Name       string                     `json:"name,omitempty"`
	TaskID     string                     `json:"task_id,omitempty"`
	Status     string                     `json:"status,omitempty"`
	OK         bool                       `json:"ok"`
	Tasks      int                        `json:"tasks"`
	Checks     int                        `json:"checks"`
	Total      int                        `json:"total"`
	Passed     int                        `json:"passed"`
	Failed     int                        `json:"failed"`
	Blocked    int                        `json:"blocked"`
	Errors     int                        `json:"errors"`
	Truncated  bool                       `json:"truncated,omitempty"`
	WorkRoot   string                     `json:"work_root,omitempty"`
	ReportPath string                     `json:"report_path,omitempty"`
	Failures   []agentEvalFailure         `json:"failures,omitempty"`
	Benchmark  *agenteval.BenchmarkReport `json:"benchmark,omitempty"`
}

type agentEvalFailure struct {
	ID      string `json:"id,omitempty"`
	Message string `json:"message,omitempty"`
}

func runAgentEvalCommand(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseAgentEvalArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeAgentEvalHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	// Run under a signal-aware context so Ctrl+C / SIGTERM cancels in-flight
	// verification commands (which honor context cancellation) instead of leaking them.
	ctx, stop := signalContext()
	defer stop()
	report, err := deps.runAgentEval(ctx, options)
	if err != nil {
		var runtimeErr agentEvalRuntimeError
		if errors.As(err, &runtimeErr) {
			return writeAppError(stderr, runtimeErr.Error(), exitCrash)
		}
		return writeExecUsageError(stderr, err.Error())
	}
	if options.ReportDir != "" {
		report.ReportPath = filepath.Join(options.ReportDir, "agent-eval-report.json")
		if err := writeAgentEvalReportFile(options.ReportDir, report); err != nil {
			return writeAppError(stderr, "failed to write eval report: "+err.Error(), exitCrash)
		}
	}
	if options.JSON {
		if err := writePrettyJSON(stdout, report); err != nil {
			return exitCrash
		}
	} else if _, err := fmt.Fprintln(stdout, formatAgentEvalReport(report)); err != nil {
		return exitCrash
	}
	if !report.OK {
		return exitProvider
	}
	return exitSuccess
}

func parseAgentEvalArgs(args []string) (agentEvalOptions, bool, error) {
	options := agentEvalOptions{Mode: "validate"}
	if len(args) > 0 {
		switch args[0] {
		case "bench", "run", "validate":
			options.Mode = args[0]
			args = args[1:]
		}
	}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--json":
			options.JSON = true
		case arg == "--suite":
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.SuitePath = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--suite="):
			options.SuitePath = strings.TrimSpace(strings.TrimPrefix(arg, "--suite="))
		case arg == "--task":
			if options.Mode != "run" && options.Mode != "bench" {
				return options, false, execUsageError{"--task is only valid for eval run or eval bench"}
			}
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.TaskID = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--task="):
			if options.Mode != "run" && options.Mode != "bench" {
				return options, false, execUsageError{"--task is only valid for eval run or eval bench"}
			}
			value, err := requiredInlineFlagValue(arg, "--task")
			if err != nil {
				return options, false, err
			}
			options.TaskID = strings.TrimSpace(value)
		case arg == "--workspace":
			if options.Mode != "run" {
				return options, false, execUsageError{"--workspace is only valid for eval run"}
			}
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.WorkspacePath = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--workspace="):
			if options.Mode != "run" {
				return options, false, execUsageError{"--workspace is only valid for eval run"}
			}
			value, err := requiredInlineFlagValue(arg, "--workspace")
			if err != nil {
				return options, false, err
			}
			options.WorkspacePath = strings.TrimSpace(value)
		case arg == "--work-root":
			if options.Mode != "bench" {
				return options, false, execUsageError{"--work-root is only valid for eval bench"}
			}
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.WorkRoot = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--work-root="):
			if options.Mode != "bench" {
				return options, false, execUsageError{"--work-root is only valid for eval bench"}
			}
			value, err := requiredInlineFlagValue(arg, "--work-root")
			if err != nil {
				return options, false, err
			}
			options.WorkRoot = strings.TrimSpace(value)
		case arg == "--model":
			if options.Mode != "bench" {
				return options, false, execUsageError{"--model is only valid for eval bench"}
			}
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.Models = appendAgentEvalModel(options.Models, value)
			index = next
		case strings.HasPrefix(arg, "--model="):
			if options.Mode != "bench" {
				return options, false, execUsageError{"--model is only valid for eval bench"}
			}
			value, err := requiredInlineFlagValue(arg, "--model")
			if err != nil {
				return options, false, err
			}
			options.Models = appendAgentEvalModel(options.Models, value)
		case arg == "--models":
			if options.Mode != "bench" {
				return options, false, execUsageError{"--models is only valid for eval bench"}
			}
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.Models = appendAgentEvalModels(options.Models, value)
			index = next
		case strings.HasPrefix(arg, "--models="):
			if options.Mode != "bench" {
				return options, false, execUsageError{"--models is only valid for eval bench"}
			}
			value, err := requiredInlineFlagValue(arg, "--models")
			if err != nil {
				return options, false, err
			}
			options.Models = appendAgentEvalModels(options.Models, value)
		case arg == "--timeout":
			if options.Mode != "bench" {
				return options, false, execUsageError{"--timeout is only valid for eval bench"}
			}
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			duration, err := parseEvalTimeout(value)
			if err != nil {
				return options, false, err
			}
			options.Timeout = duration
			index = next
		case strings.HasPrefix(arg, "--timeout="):
			if options.Mode != "bench" {
				return options, false, execUsageError{"--timeout is only valid for eval bench"}
			}
			value, err := requiredInlineFlagValue(arg, "--timeout")
			if err != nil {
				return options, false, err
			}
			duration, err := parseEvalTimeout(value)
			if err != nil {
				return options, false, err
			}
			options.Timeout = duration
		case arg == "--agent-command":
			if options.Mode != "bench" {
				return options, false, execUsageError{"--agent-command is only valid for eval bench"}
			}
			if index+1 >= len(args) {
				return options, false, execUsageError{"--agent-command requires at least one argument"}
			}
			options.AgentCommand = append([]string{}, args[index+1:]...)
			index = len(args)
		case strings.HasPrefix(arg, "--agent-command="):
			if options.Mode != "bench" {
				return options, false, execUsageError{"--agent-command is only valid for eval bench"}
			}
			value, err := requiredInlineFlagValue(arg, "--agent-command")
			if err != nil {
				return options, false, err
			}
			options.AgentCommand = []string{value}
		case arg == "--keep-workspaces":
			if options.Mode != "bench" {
				return options, false, execUsageError{"--keep-workspaces is only valid for eval bench"}
			}
			options.KeepWorkspaces = true
		case arg == "--report-dir":
			if options.Mode != "run" && options.Mode != "bench" {
				return options, false, execUsageError{"--report-dir is only valid for eval run or eval bench"}
			}
			value, next, err := nextFlagValue(args, index, arg)
			if err != nil {
				return options, false, err
			}
			options.ReportDir = strings.TrimSpace(value)
			index = next
		case strings.HasPrefix(arg, "--report-dir="):
			if options.Mode != "run" && options.Mode != "bench" {
				return options, false, execUsageError{"--report-dir is only valid for eval run or eval bench"}
			}
			value, err := requiredInlineFlagValue(arg, "--report-dir")
			if err != nil {
				return options, false, err
			}
			options.ReportDir = strings.TrimSpace(value)
		case strings.HasPrefix(arg, "-"):
			return options, false, execUsageError{fmt.Sprintf("unknown eval flag %q", arg)}
		default:
			return options, false, execUsageError{fmt.Sprintf("unexpected eval argument %q", arg)}
		}
	}
	if options.SuitePath == "" {
		return options, false, execUsageError{"--suite requires a path"}
	}
	if options.Mode == "run" && options.WorkspacePath == "" {
		// Require an explicit workspace: defaulting to "." would run the suite's
		// verification commands (go test/git) against the real working tree instead
		// of a staged fixture.
		return options, false, execUsageError{"--workspace requires a path for eval run"}
	}
	return options, false, nil
}

func appendAgentEvalModel(models []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return models
	}
	for _, model := range models {
		if model == value {
			return models
		}
	}
	return append(models, value)
}

func appendAgentEvalModels(models []string, value string) []string {
	for _, model := range strings.Split(value, ",") {
		models = appendAgentEvalModel(models, model)
	}
	return models
}

func parseEvalTimeout(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, execUsageError{fmt.Sprintf("--timeout must be a Go duration such as 90s or 5m: %v", err)}
	}
	if duration < 0 {
		return 0, execUsageError{"--timeout must not be negative"}
	}
	return duration, nil
}

func formatAgentEvalReport(report agentEvalReport) string {
	lines := []string{
		"Zero agent eval",
		"suite: " + report.Suite,
	}
	if report.Name != "" {
		lines = append(lines, "name: "+report.Name)
	}
	if report.TaskID != "" {
		lines = append(lines, "task: "+report.TaskID)
	}
	// Only validate mode reports a static check count; run and bench modes carry
	// scored pass/fail/blocked/error tallies that must be surfaced instead.
	if report.Checks > 0 {
		lines = append(lines, fmt.Sprintf("summary: %d tasks, %d checks", report.Tasks, report.Checks))
	} else {
		lines = append(lines, fmt.Sprintf("summary: %d total, %d passed, %d failed, %d blocked, %d errors", report.Total, report.Passed, report.Failed, report.Blocked, report.Errors))
	}
	status := strings.TrimSpace(report.Status)
	if status == "" {
		if report.OK {
			status = "passed"
		} else {
			status = "failed"
		}
	}
	lines = append(lines, "status: "+status)
	if report.WorkRoot != "" {
		lines = append(lines, "work-root: "+report.WorkRoot)
	}
	if report.ReportPath != "" {
		lines = append(lines, "report: "+report.ReportPath)
	}
	if report.Truncated {
		lines = append(lines, "note: agent output was truncated")
	}
	if len(report.Failures) > 0 {
		lines = append(lines, "failures:")
	}
	for _, failure := range report.Failures {
		detail := strings.TrimSpace(failure.ID)
		message := strings.TrimSpace(failure.Message)
		if detail == "" {
			detail = "failure"
		}
		if message != "" {
			detail += " - " + message
		}
		lines = append(lines, "  "+detail)
	}
	return strings.Join(lines, "\n")
}

func writeAgentEvalHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero eval --suite <path> [flags]
  zero eval run --suite <path> [flags]
  zero eval bench --suite <path> [flags] [--model <id>] [--agent-command <argv...>]

Validates offline agent eval suites, scores an existing workspace, or benchmarks an agent command against fixture workspaces.

Flags:
      --suite <path>          Eval suite JSON file
      --task <id>             Run one task (eval run or eval bench)
      --workspace <path>      Workspace path for local scoring (eval run only, required)
      --work-root <path>      Work root for materialized benchmark workspaces (eval bench only)
      --model <id>            Model id for eval bench model-matrix runs; repeatable
      --models <ids>          Comma-separated model ids for eval bench model-matrix runs
      --timeout <duration>    Per-task timeout for eval bench (Go duration, e.g. 90s, 5m)
      --keep-workspaces       Keep materialized benchmark workspaces (eval bench only)
      --report-dir <path>     Write agent-eval-report.json (eval run or eval bench)
      --agent-command <argv>  Agent command argv for eval bench; may include {prompt},
                              {workspace}, {task_id}, and {model}; must be last as it
                              consumes all remaining arguments
      --json                  Print JSON output
  -h, --help                  Show this help
`)
	return err
}

func defaultRunAgentEval(ctx context.Context, options agentEvalOptions) (agentEvalReport, error) {
	suite, err := agenteval.LoadSuite(options.SuitePath)
	if err != nil {
		return agentEvalReport{}, err
	}
	if options.Mode == "run" {
		report := (agenteval.Runner{}).Run(ctx, suite, agenteval.RunInput{
			TaskID:        options.TaskID,
			WorkspacePath: options.WorkspacePath,
		})
		return agentEvalReportFromRunner(suite, report), nil
	}
	if options.Mode == "bench" {
		workRoot, cleanup, err := agentEvalBenchWorkRoot(options)
		if err != nil {
			return agentEvalReport{}, err
		}
		if cleanup != "" {
			defer func() { _ = os.RemoveAll(cleanup) }()
		}
		harness := agenteval.Harness{}
		if len(options.AgentCommand) > 0 {
			harness.Agent = agenteval.CommandAgentRunner{Command: options.AgentCommand}
		}
		report := harness.Run(ctx, options.SuitePath, suite, agenteval.BenchmarkInput{
			TaskID:         options.TaskID,
			WorkRoot:       workRoot,
			Models:         options.Models,
			KeepWorkspaces: options.KeepWorkspaces,
			Timeout:        options.Timeout,
		})
		converted := agentEvalReportFromBenchmark(suite, report)
		if options.KeepWorkspaces {
			// Surface where the kept workspaces live, especially when the work
			// root was an unnamed temp dir the caller never specified.
			converted.WorkRoot = workRoot
		}
		return converted, nil
	}
	checks := 0
	for _, task := range suite.Tasks {
		checks += agentEvalTaskCheckCount(task)
	}
	return agentEvalReport{
		Suite:  suite.ID,
		Name:   suite.Name,
		Status: "valid",
		OK:     true,
		Tasks:  len(suite.Tasks),
		Checks: checks,
	}, nil
}

func agentEvalBenchWorkRoot(options agentEvalOptions) (string, string, error) {
	workRoot := strings.TrimSpace(options.WorkRoot)
	if workRoot != "" {
		return workRoot, "", nil
	}
	created, err := os.MkdirTemp("", "zero-eval-")
	if err != nil {
		return "", "", agentEvalRuntimeError{fmt.Errorf("create benchmark work root: %w", err)}
	}
	if options.KeepWorkspaces {
		return created, "", nil
	}
	return created, created, nil
}

func agentEvalReportFromBenchmark(suite agenteval.Suite, report agenteval.BenchmarkReport) agentEvalReport {
	converted := agentEvalReport{
		Suite:     report.SuiteID,
		Name:      suite.Name,
		Status:    benchmarkStatus(report),
		OK:        report.OK,
		Tasks:     report.Summary.TotalTasks,
		Total:     report.Summary.TotalTasks,
		Passed:    report.Summary.PassedTasks,
		Failed:    report.Summary.FailedTasks,
		Blocked:   report.Summary.BlockedTasks,
		Errors:    report.Summary.ErrorTasks,
		Benchmark: &report,
	}
	if converted.Suite == "" {
		converted.Suite = suite.ID
	}
	if len(report.Tasks) == 1 {
		converted.TaskID = report.Tasks[0].TaskID
	}
	for _, task := range report.Tasks {
		if task.Agent.Truncated {
			converted.Truncated = true
		}
		converted.Failures = append(converted.Failures, agentEvalFailuresFromTaskReport(task)...)
	}
	return converted
}

func benchmarkStatus(report agenteval.BenchmarkReport) string {
	switch {
	case report.Summary.BlockedTasks > 0:
		return string(agenteval.StatusBlocked)
	case report.Summary.ErrorTasks > 0:
		return string(agenteval.StatusError)
	case report.Summary.FailedTasks > 0:
		return string(agenteval.StatusFail)
	default:
		return string(agenteval.StatusPass)
	}
}

func agentEvalFailuresFromTaskReport(task agenteval.BenchmarkTaskReport) []agentEvalFailure {
	failures := []agentEvalFailure{}
	seen := map[string]struct{}{}
	appendUnique := func(id string, message string) {
		key := strings.TrimSpace(id) + "\x00" + strings.TrimSpace(message)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		failures = append(failures, agentEvalFailure{ID: id, Message: message})
	}
	taskID := benchmarkFailureTaskID(task)
	if task.Agent.Error != "" {
		appendUnique(taskID, task.Agent.Error)
	}
	for _, result := range task.Report.Results {
		if result.Status == agenteval.StatusPass {
			continue
		}
		id := taskID
		if result.ID != "" {
			id += "." + result.ID
		}
		appendUnique(id, agentEvalResultMessage(result))
	}
	if task.Report.Error != "" {
		appendUnique(taskID, task.Report.Error)
	}
	return failures
}

func benchmarkFailureTaskID(task agenteval.BenchmarkTaskReport) string {
	if task.Model == "" {
		return task.TaskID
	}
	return task.Model + "." + task.TaskID
}

func agentEvalReportFromRunner(suite agenteval.Suite, report agenteval.Report) agentEvalReport {
	converted := agentEvalReport{
		Suite:   report.SuiteID,
		Name:    suite.Name,
		TaskID:  report.TaskID,
		Status:  string(report.Status),
		OK:      report.OK,
		Total:   report.Summary.Total,
		Passed:  report.Summary.Passed,
		Failed:  report.Summary.Failed,
		Blocked: report.Summary.Blocked,
		Errors:  report.Summary.Errors,
	}
	if converted.Suite == "" {
		converted.Suite = suite.ID
	}
	if report.Error != "" {
		converted.Failures = append(converted.Failures, agentEvalFailure{
			ID:      "task",
			Message: report.Error,
		})
	}
	for _, result := range report.Results {
		if result.Status == agenteval.StatusPass {
			continue
		}
		converted.Failures = append(converted.Failures, agentEvalFailure{
			ID:      result.ID,
			Message: agentEvalResultMessage(result),
		})
	}
	return converted
}

func agentEvalResultMessage(result agenteval.Result) string {
	for _, value := range []string{result.Message, result.Stderr, string(result.Status)} {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func writeAgentEvalReportFile(reportDir string, report agentEvalReport) error {
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(report.ReportPath, append(data, '\n'), 0o644)
}

func agentEvalTaskCheckCount(task agenteval.Task) int {
	checks := len(task.VerificationCommands) + 1
	if len(task.ForbiddenChangedFiles) > 0 {
		checks++
	}
	if len(task.ContextChecks.RequiredFiles) > 0 || len(task.ContextChecks.ForbiddenFiles) > 0 {
		checks++
	}
	if len(task.RequiredTraceEvents) > 0 {
		checks++
	}
	return checks
}

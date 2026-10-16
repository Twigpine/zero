package verify

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/redaction"
	"github.com/Gitlawb/zero/internal/testrunner"
)

type Status string

const (
	StatusPass  Status = "pass"
	StatusFail  Status = "fail"
	StatusError Status = "error"
)

type Check struct {
	ID        string               `json:"id"`
	Name      string               `json:"name"`
	Command   []string             `json:"command"`
	Kind      testrunner.Kind      `json:"kind,omitempty"`
	Framework testrunner.Framework `json:"framework,omitempty"`
}

type Plan struct {
	Root   string  `json:"root"`
	Checks []Check `json:"checks"`
}

type Summary struct {
	Total  int `json:"total"`
	Passed int `json:"passed"`
	Failed int `json:"failed"`
	Errors int `json:"errors"`
}

type Result struct {
	ID            string               `json:"id"`
	Name          string               `json:"name"`
	Command       []string             `json:"command"`
	Kind          testrunner.Kind      `json:"kind,omitempty"`
	Framework     testrunner.Framework `json:"framework,omitempty"`
	Status        Status               `json:"status"`
	ExitCode      int                  `json:"exitCode"`
	Stdout        string               `json:"stdout,omitempty"`
	Stderr        string               `json:"stderr,omitempty"`
	StartedAt     string               `json:"startedAt"`
	EndedAt       string               `json:"endedAt"`
	DurationMs    int                  `json:"durationMs"`
	Error         string               `json:"error,omitempty"`
	OutputSummary *OutputSummary       `json:"outputSummary,omitempty"`
	TestSummary   *testrunner.Summary  `json:"testSummary,omitempty"`
}

type Report struct {
	Root      string   `json:"root"`
	StartedAt string   `json:"startedAt"`
	EndedAt   string   `json:"endedAt"`
	OK        bool     `json:"ok"`
	Summary   Summary  `json:"summary"`
	Results   []Result `json:"results"`
}

type OutputSummary struct {
	Lines     []string `json:"lines,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

type Attempt struct {
	Number int    `json:"number"`
	Report Report `json:"report"`
}

type LoopReport struct {
	StartedAt string    `json:"startedAt"`
	EndedAt   string    `json:"endedAt"`
	OK        bool      `json:"ok"`
	Summary   Summary   `json:"summary"`
	Attempts  []Attempt `json:"attempts"`
	Error     string    `json:"error,omitempty"`
}

type CommandResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

type Runner func(context.Context, string, []string, time.Duration) (CommandResult, error)

type RunOptions struct {
	Only      []string
	TimeoutMS int
	Runner    Runner
	Now       func() time.Time
}

type LoopOptions struct {
	RunOptions
	MaxAttempts int
	OnFailure   func(context.Context, Attempt) error
}

const defaultTimeoutMS = 120000
const maxOutputSummaryLines = 8

func DetectPlan(root string) (Plan, error) {
	resolvedRoot, err := resolveRoot(root)
	if err != nil {
		return Plan{}, err
	}
	detected, err := testrunner.Detect(resolvedRoot)
	if err != nil {
		return Plan{}, err
	}
	checks := make([]Check, 0, len(detected))
	for _, check := range detected {
		checks = append(checks, Check{
			ID:        check.ID,
			Name:      check.Name,
			Command:   append([]string{}, check.Command...),
			Kind:      check.Kind,
			Framework: check.Framework,
		})
	}
	return Plan{Root: resolvedRoot, Checks: checks}, nil
}

func Run(ctx context.Context, plan Plan, options RunOptions) Report {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	runner := options.Runner
	if runner == nil {
		runner = defaultRunner
	}
	timeout := time.Duration(firstPositive(options.TimeoutMS, defaultTimeoutMS)) * time.Millisecond
	start := now()
	report := Report{
		Root:      plan.Root,
		StartedAt: formatTime(start),
		OK:        true,
	}
	checks, unknownChecks := filterChecks(plan.Checks, options.Only)
	for _, check := range checks {
		checkStart := now()
		result := Result{
			ID:        check.ID,
			Name:      check.Name,
			Command:   append([]string{}, check.Command...),
			Kind:      check.Kind,
			Framework: check.Framework,
			StartedAt: formatTime(checkStart),
		}
		commandResult, err := runner(ctx, plan.Root, check.Command, timeout)
		checkEnd := now()
		result.EndedAt = formatTime(checkEnd)
		result.DurationMs = int(checkEnd.Sub(checkStart).Milliseconds())
		result.Stdout = redaction.RedactString(commandResult.Stdout, redaction.Options{})
		result.Stderr = redaction.RedactString(commandResult.Stderr, redaction.Options{})
		result.ExitCode = commandResult.ExitCode
		if shouldParseTestSummary(check) {
			result.TestSummary = testrunner.ParseSummary(testrunner.Check{
				ID:        check.ID,
				Name:      check.Name,
				Command:   append([]string{}, check.Command...),
				Kind:      check.Kind,
				Framework: check.Framework,
			}, result.Stdout, result.Stderr)
		}
		if err != nil {
			result.Status = StatusError
			result.Error = redaction.RedactString(err.Error(), redaction.Options{})
			result.OutputSummary = summarizeOutput(result.Stdout, result.Stderr, result.Error)
			report.Summary.Errors++
		} else if commandResult.ExitCode == 0 {
			result.Status = StatusPass
			report.Summary.Passed++
		} else {
			result.Status = StatusFail
			result.OutputSummary = summarizeOutput(result.Stdout, result.Stderr)
			report.Summary.Failed++
		}
		report.Results = append(report.Results, result)
	}
	for _, id := range unknownChecks {
		at := formatTime(now())
		report.Results = append(report.Results, Result{
			ID:        id,
			Name:      "Unknown verification check",
			Status:    StatusError,
			ExitCode:  -1,
			StartedAt: at,
			EndedAt:   at,
			Error:     fmt.Sprintf("unknown verification check %q", id),
		})
		report.Summary.Errors++
	}
	report.Summary.Total = len(report.Results)
	report.OK = report.Summary.Failed == 0 && report.Summary.Errors == 0
	report.EndedAt = formatTime(now())
	return report
}

func shouldParseTestSummary(check Check) bool {
	if check.Kind == testrunner.KindTest {
		return true
	}
	if check.Kind != "" {
		return false
	}
	id := strings.ToLower(strings.TrimSpace(check.ID))
	if strings.Contains(id, ".test") || strings.HasSuffix(id, "test") || strings.Contains(id, "pytest") {
		return true
	}
	for _, part := range check.Command {
		normalized := strings.ToLower(strings.TrimSpace(part))
		if normalized == "test" || normalized == "pytest" {
			return true
		}
	}
	return false
}

func summarizeOutput(values ...string) *OutputSummary {
	lines := []string{}
	fallback := ""
	contextLines := 0
	truncated := false
	for _, value := range values {
		for _, line := range strings.Split(value, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			if fallback == "" {
				fallback = trimmed
			}
			include := false
			if isFailureLine(trimmed) {
				include = true
				contextLines = 2
			} else if contextLines > 0 {
				include = true
				contextLines--
			}
			if !include {
				continue
			}
			if len(lines) >= maxOutputSummaryLines {
				truncated = true
				break
			}
			lines = append(lines, trimmed)
		}
		if truncated {
			break
		}
	}
	if len(lines) == 0 {
		if fallback != "" {
			return &OutputSummary{Lines: []string{fallback}}
		}
		return nil
	}
	return &OutputSummary{Lines: lines, Truncated: truncated}
}

func isFailureLine(line string) bool {
	lower := strings.ToLower(line)
	return strings.HasPrefix(line, "--- FAIL:") ||
		strings.HasPrefix(line, "FAIL") ||
		strings.HasPrefix(lower, "panic:") ||
		strings.HasPrefix(lower, "error:") ||
		strings.HasPrefix(lower, "not ok") ||
		strings.Contains(lower, " assertion") ||
		strings.Contains(lower, " failed")
}

func defaultRunner(ctx context.Context, dir string, command []string, timeout time.Duration) (CommandResult, error) {
	if len(command) == 0 {
		return CommandResult{ExitCode: -1}, fmt.Errorf("verify command is empty")
	}
	commandCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		commandCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()

	cmd := exec.CommandContext(commandCtx, command[0], command[1:]...)
	cmd.Dir = dir
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := execution.RunCommand(commandCtx, cmd)
	exitCode := 0
	if err != nil {
		exitCode = -1
		if exitError, ok := execution.AsPureExitError(err); ok {
			exitCode = exitError.ExitCode()
			err = nil
		}
	}
	if commandCtx.Err() == context.DeadlineExceeded {
		return CommandResult{
			ExitCode: -1,
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
		}, fmt.Errorf("command timed out after %dms", timeout.Milliseconds())
	}
	return CommandResult{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, err
}

func resolveRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve verify root: %w", err)
		}
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve verify root: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("verify root must be an existing directory: %s", absolute)
	}
	return filepath.Clean(absolute), nil
}

func filterChecks(checks []Check, only []string) ([]Check, []string) {
	if len(only) == 0 {
		return append([]Check{}, checks...), nil
	}
	allowed := map[string]bool{}
	for _, id := range only {
		trimmed := strings.TrimSpace(id)
		if trimmed != "" {
			allowed[trimmed] = true
		}
	}
	filtered := []Check{}
	seen := map[string]bool{}
	for _, check := range checks {
		if allowed[check.ID] {
			filtered = append(filtered, check)
			seen[check.ID] = true
		}
	}
	unknown := []string{}
	for id := range allowed {
		if !seen[id] {
			unknown = append(unknown, id)
		}
	}
	sort.Strings(unknown)
	return filtered, unknown
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

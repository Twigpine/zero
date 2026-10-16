package verify

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/testrunner"
)

func TestRunDefaultRunnerTimeoutKillsGrandchildHoldingOutput(t *testing.T) {
	switch os.Getenv("ZERO_VERIFY_TREE_HELPER") {
	case "parent":
		if err := os.WriteFile(os.Getenv("ZERO_VERIFY_TREE_PARENT_PID_FILE"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(2)
		}
		child := exec.Command(os.Args[0], "-test.run=^TestRunDefaultRunnerTimeoutKillsGrandchildHoldingOutput$")
		child.Env = append(os.Environ(),
			"ZERO_VERIFY_TREE_HELPER=grandchild",
			"ZERO_VERIFY_TREE_STOP_FILE="+os.Getenv("ZERO_VERIFY_TREE_STOP_FILE"),
		)
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		if err := os.WriteFile(os.Getenv("ZERO_VERIFY_TREE_GRANDCHILD_PID_FILE"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = os.WriteFile(os.Getenv("ZERO_VERIFY_TREE_STOP_FILE"), nil, 0o600)
			_ = child.Wait()
			os.Exit(4)
		}
		if err := os.WriteFile(os.Getenv("ZERO_VERIFY_TREE_READY_FILE"), []byte("ready"), 0o600); err != nil {
			_ = os.WriteFile(os.Getenv("ZERO_VERIFY_TREE_STOP_FILE"), nil, 0o600)
			_ = child.Wait()
			os.Exit(5)
		}
		waitForVerifyTreeStop(os.Getenv("ZERO_VERIFY_TREE_STOP_FILE"), 30*time.Second)
		_ = child.Wait()
		return
	case "grandchild":
		waitForVerifyTreeStop(os.Getenv("ZERO_VERIFY_TREE_STOP_FILE"), 30*time.Second)
		return
	}

	root := t.TempDir()
	parentPIDFile := filepath.Join(root, "parent.pid")
	grandchildPIDFile := filepath.Join(root, "grandchild.pid")
	readyFile := filepath.Join(root, "ready")
	stopFile := filepath.Join(root, "stop")
	owner := newVerifyTestProcessOwner(t, stopFile, parentPIDFile, grandchildPIDFile)
	t.Setenv("ZERO_VERIFY_TREE_HELPER", "parent")
	t.Setenv("ZERO_VERIFY_TREE_PARENT_PID_FILE", parentPIDFile)
	t.Setenv("ZERO_VERIFY_TREE_GRANDCHILD_PID_FILE", grandchildPIDFile)
	t.Setenv("ZERO_VERIFY_TREE_READY_FILE", readyFile)
	t.Setenv("ZERO_VERIFY_TREE_STOP_FILE", stopFile)
	plan := Plan{Root: root, Checks: []Check{{
		ID:      "tree.timeout",
		Name:    "process tree timeout",
		Command: []string{os.Args[0], "-test.run=^TestRunDefaultRunnerTimeoutKillsGrandchildHoldingOutput$"},
	}}}
	reportChannel := make(chan Report, 1)
	go func() {
		reportChannel <- Run(context.Background(), plan, RunOptions{TimeoutMS: 3000})
	}()
	parentPID, grandchildPID := awaitVerifyTreeReady(t, owner, readyFile, parentPIDFile, grandchildPIDFile)
	started := time.Now()
	var report Report
	select {
	case report = <-reportChannel:
	case <-time.After(6 * time.Second):
		t.Fatal("defaultRunner did not return within six seconds after its timeout")
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("defaultRunner remained blocked by grandchild output handles for %s", elapsed)
	}
	if report.OK || len(report.Results) != 1 || report.Results[0].Status == StatusPass {
		t.Fatalf("timed-out defaultRunner command unexpectedly passed: %#v", report)
	}
	for role, pid := range map[string]int{"parent": parentPID, "grandchild": grandchildPID} {
		if err := owner.awaitExit(pid, 2*time.Second); err != nil {
			t.Fatalf("%s process %d survived verify cancellation: %v", role, pid, err)
		}
	}
}

func waitForVerifyTreeStop(stopFile string, lifetime time.Duration) {
	deadline := time.Now().Add(lifetime)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stopFile); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func awaitVerifyTreeReady(t *testing.T, owner *verifyTestProcessOwner, readyFile string, pidFiles ...string) (int, int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
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

func TestDetectPlanFindsBunAndGoChecks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/zero\n")
	writeFile(t, filepath.Join(root, "bun.lock"), "")
	writeFile(t, filepath.Join(root, "package.json"), `{
		"scripts": {
			"test": "bun test ./tests",
			"typecheck": "tsc --noEmit",
			"build": "go run ./cmd/zero-release build",
			"lint": "eslint ."
		}
	}`)

	plan, err := DetectPlan(root)
	if err != nil {
		t.Fatalf("DetectPlan returned error: %v", err)
	}

	ids := checkIDs(plan.Checks)
	for _, want := range []string{"go.test", "bun.typecheck", "bun.test", "bun.build", "bun.lint"} {
		if !contains(ids, want) {
			t.Fatalf("expected check %q in %#v", want, ids)
		}
	}
	if plan.Checks[0].Command[0] != "go" || strings.Join(plan.Checks[0].Command, " ") != "go test ./..." {
		t.Fatalf("first check = %#v, want go test ./...", plan.Checks[0])
	}
	byID := checksByID(plan.Checks)
	if byID["go.test"].Kind != testrunner.KindTest || byID["go.test"].Framework != testrunner.FrameworkGo {
		t.Fatalf("go.test metadata = %#v", byID["go.test"])
	}
	if byID["bun.typecheck"].Kind != testrunner.KindTypecheck || byID["bun.test"].Framework != testrunner.FrameworkBun {
		t.Fatalf("bun metadata = typecheck %#v test %#v", byID["bun.typecheck"], byID["bun.test"])
	}
}

func TestRunExecutesPlanAndRedactsOutput(t *testing.T) {
	root := t.TempDir()
	plan := Plan{Root: root, Checks: []Check{
		{ID: "go.test", Name: "Go tests", Command: []string{"go", "test", "./..."}},
		{ID: "bun.test", Name: "Bun tests", Command: []string{"bun", "test"}},
	}}
	runner := &fakeCommandRunner{results: []CommandResult{
		{ExitCode: 0, Stdout: "ok\n"},
		{ExitCode: 1, Stdout: "token sk-proj-secret1234567890", Stderr: "fail\n"},
	}}

	report := Run(context.Background(), plan, RunOptions{
		Runner:    runner.Run,
		Now:       fixedVerifyTime("2026-06-05T10:45:00Z"),
		TimeoutMS: 5000,
	})

	if report.OK {
		t.Fatalf("report.OK = true, want false")
	}
	if report.Summary.Total != 2 || report.Summary.Passed != 1 || report.Summary.Failed != 1 {
		t.Fatalf("unexpected summary: %#v", report.Summary)
	}
	if report.Results[1].Status != StatusFail || report.Results[1].ExitCode != 1 {
		t.Fatalf("unexpected failing result: %#v", report.Results[1])
	}
	if strings.Contains(report.Results[1].Stdout, "sk-proj-secret") || !strings.Contains(report.Results[1].Stdout, "[REDACTED]") {
		t.Fatalf("expected redacted stdout, got %q", report.Results[1].Stdout)
	}
	if got := runner.calls[0].dir; got != root {
		t.Fatalf("runner dir = %q, want %q", got, root)
	}
}

func TestRunParsesStructuredFailureSummary(t *testing.T) {
	root := t.TempDir()
	plan := Plan{Root: root, Checks: []Check{
		{ID: "go.test", Name: "Go tests", Command: []string{"go", "test", "./..."}, Kind: testrunner.KindTest, Framework: testrunner.FrameworkGo},
	}}
	runner := &fakeCommandRunner{results: []CommandResult{{
		ExitCode: 1,
		Stdout: strings.Join([]string{
			"--- FAIL: TestSecret (0.00s)",
			"    secret_test.go:12: token sk-proj-abcdefghijklmnopqrstuvwxyz",
			"FAIL",
		}, "\n"),
	}}}

	report := Run(context.Background(), plan, RunOptions{
		Runner: runner.Run,
		Now:    fixedVerifyTime("2026-06-05T11:15:00Z"),
	})

	if report.Results[0].OutputSummary == nil {
		t.Fatalf("expected output summary, got %#v", report.Results[0])
	}
	lines := strings.Join(report.Results[0].OutputSummary.Lines, "\n")
	if !strings.Contains(lines, "TestSecret") || !strings.Contains(lines, "[REDACTED]") {
		t.Fatalf("expected redacted failure summary lines, got %q", lines)
	}
	if strings.Contains(lines, "sk-proj-abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("failure summary leaked secret: %q", lines)
	}
	if report.Results[0].TestSummary == nil {
		t.Fatalf("expected parsed test summary, got %#v", report.Results[0])
	}
	if report.Results[0].TestSummary.Total != 1 || report.Results[0].TestSummary.Failed != 1 {
		t.Fatalf("unexpected parsed test summary: %#v", report.Results[0].TestSummary)
	}
	if got := report.Results[0].TestSummary.Failures[0]; got.Name != "TestSecret" || got.File != "secret_test.go:12" || !strings.Contains(got.Message, "[REDACTED]") {
		t.Fatalf("unexpected parsed failure detail: %#v", got)
	}
}

func TestRunSkipsStructuredSummaryForNonTestChecks(t *testing.T) {
	root := t.TempDir()
	plan := Plan{Root: root, Checks: []Check{
		{ID: "bun.typecheck", Name: "Bun typecheck", Command: []string{"bun", "run", "typecheck"}, Kind: testrunner.KindTypecheck, Framework: testrunner.FrameworkBun},
	}}
	runner := &fakeCommandRunner{results: []CommandResult{{
		ExitCode: 1,
		Stdout:   "1 failed, 2 passed while checking types\n",
	}}}

	report := Run(context.Background(), plan, RunOptions{
		Runner: runner.Run,
		Now:    fixedVerifyTime("2026-06-05T11:15:30Z"),
	})

	if report.Results[0].TestSummary != nil {
		t.Fatalf("non-test check should not have a parsed test summary: %#v", report.Results[0].TestSummary)
	}
	if report.Results[0].OutputSummary == nil {
		t.Fatalf("expected ordinary failure output summary, got %#v", report.Results[0])
	}
}

func TestRunSummarizesPlainCommandErrors(t *testing.T) {
	root := t.TempDir()
	plan := Plan{Root: root, Checks: []Check{
		{ID: "bun.test", Name: "Bun tests", Command: []string{"bun", "test"}},
	}}

	report := Run(context.Background(), plan, RunOptions{
		Runner: func(context.Context, string, []string, time.Duration) (CommandResult, error) {
			return CommandResult{}, errors.New(`exec: "bun": executable file not found in $PATH`)
		},
		Now: fixedVerifyTime("2026-06-05T11:16:00Z"),
	})

	summary := report.Results[0].OutputSummary
	if summary == nil {
		t.Fatalf("expected plain command error summary, got %#v", report.Results[0])
	}
	if len(summary.Lines) != 1 || !strings.Contains(summary.Lines[0], "executable file not found") {
		t.Fatalf("unexpected plain command error summary: %#v", summary)
	}
	if summary.Truncated {
		t.Fatalf("plain command error summary should not be truncated: %#v", summary)
	}
}

func TestRunFailureSummaryTruncatesOnlyOnOverflow(t *testing.T) {
	lines := []string{}
	for index := 0; index < maxOutputSummaryLines; index++ {
		lines = append(lines, "--- FAIL: TestExact (0.00s)")
	}
	exact := summarizeOutput(strings.Join(lines, "\n"))
	if exact == nil || len(exact.Lines) != maxOutputSummaryLines {
		t.Fatalf("expected exact-limit summary, got %#v", exact)
	}
	if exact.Truncated {
		t.Fatalf("exact-limit summary should not be truncated: %#v", exact)
	}

	overflowLines := append(append([]string{}, lines...), "--- FAIL: TestOverflow (0.00s)")
	overflow := summarizeOutput(strings.Join(overflowLines, "\n"))
	if overflow == nil || len(overflow.Lines) != maxOutputSummaryLines {
		t.Fatalf("expected capped overflow summary, got %#v", overflow)
	}
	if !overflow.Truncated {
		t.Fatalf("overflow summary should be truncated: %#v", overflow)
	}
}

func TestRunLoopRetriesUntilVerificationPasses(t *testing.T) {
	root := t.TempDir()
	plan := Plan{Root: root, Checks: []Check{
		{ID: "go.test", Name: "Go tests", Command: []string{"go", "test", "./..."}},
	}}
	runner := &fakeCommandRunner{results: []CommandResult{
		{ExitCode: 1, Stdout: "--- FAIL: TestOne\nFAIL\n"},
		{ExitCode: 0, Stdout: "ok\n"},
	}}
	retryCount := 0

	report := RunLoop(context.Background(), plan, LoopOptions{
		RunOptions: RunOptions{
			Runner: runner.Run,
			Now:    fixedVerifyTime("2026-06-05T11:20:00Z"),
		},
		MaxAttempts: 2,
		OnFailure: func(ctx context.Context, attempt Attempt) error {
			retryCount++
			if attempt.Number != 1 || attempt.Report.OK {
				t.Fatalf("unexpected failed attempt passed to hook: %#v", attempt)
			}
			return nil
		},
	})

	if !report.OK {
		t.Fatalf("expected loop to pass, got %#v", report)
	}
	if len(report.Attempts) != 2 || !report.Attempts[1].Report.OK {
		t.Fatalf("unexpected attempts: %#v", report.Attempts)
	}
	if retryCount != 1 {
		t.Fatalf("retry hook called %d times, want 1", retryCount)
	}
}

func TestRunFiltersChecksByID(t *testing.T) {
	root := t.TempDir()
	plan := Plan{Root: root, Checks: []Check{
		{ID: "go.test", Name: "Go tests", Command: []string{"go", "test", "./..."}},
		{ID: "bun.test", Name: "Bun tests", Command: []string{"bun", "test"}},
	}}
	runner := &fakeCommandRunner{results: []CommandResult{{ExitCode: 0, Stdout: "ok\n"}}}

	report := Run(context.Background(), plan, RunOptions{
		Only:   []string{"bun.test"},
		Runner: runner.Run,
		Now:    fixedVerifyTime("2026-06-05T10:50:00Z"),
	})

	if report.Summary.Total != 1 || report.Results[0].ID != "bun.test" {
		t.Fatalf("unexpected filtered report: %#v", report)
	}
	if got := strings.Join(runner.calls[0].args, " "); got != "bun test" {
		t.Fatalf("runner command = %q, want bun test", got)
	}
}

func TestRunReportsUnknownOnlyChecks(t *testing.T) {
	root := t.TempDir()
	plan := Plan{Root: root, Checks: []Check{
		{ID: "go.test", Name: "Go tests", Command: []string{"go", "test", "./..."}},
	}}

	report := Run(context.Background(), plan, RunOptions{
		Only: []string{"missing.check"},
		Now:  fixedVerifyTime("2026-06-05T10:55:00Z"),
	})

	if report.OK {
		t.Fatalf("report.OK = true, want false")
	}
	if report.Summary.Total != 1 || report.Summary.Errors != 1 {
		t.Fatalf("unexpected summary for unknown check: %#v", report.Summary)
	}
	if report.Results[0].Status != StatusError || !strings.Contains(report.Results[0].Error, "unknown verification check") {
		t.Fatalf("unexpected unknown check result: %#v", report.Results[0])
	}
}

func TestRunReportsUnknownOnlyChecksInStableOrder(t *testing.T) {
	root := t.TempDir()
	plan := Plan{Root: root}

	report := Run(context.Background(), plan, RunOptions{
		Only: []string{"missing.z", "missing.a"},
		Now:  fixedVerifyTime("2026-06-05T10:56:00Z"),
	})

	if len(report.Results) != 2 {
		t.Fatalf("expected two unknown check results, got %#v", report.Results)
	}
	if report.Results[0].ID != "missing.a" || report.Results[1].ID != "missing.z" {
		t.Fatalf("unknown checks are not stable sorted: %#v", report.Results)
	}
}

func TestDetectPlanRejectsMissingRoot(t *testing.T) {
	_, err := DetectPlan(filepath.Join(t.TempDir(), "missing"))
	if err == nil || !strings.Contains(err.Error(), "verify root must be an existing directory") {
		t.Fatalf("expected missing root error, got %v", err)
	}
}

type fakeCommandRunner struct {
	calls   []commandCall
	results []CommandResult
}

func (runner *fakeCommandRunner) Run(ctx context.Context, dir string, command []string, timeout time.Duration) (CommandResult, error) {
	runner.calls = append(runner.calls, commandCall{dir: dir, args: append([]string{}, command...)})
	if len(runner.results) == 0 {
		return CommandResult{}, nil
	}
	result := runner.results[0]
	runner.results = runner.results[1:]
	return result, nil
}

type commandCall struct {
	dir  string
	args []string
}

func checkIDs(checks []Check) []string {
	ids := make([]string, 0, len(checks))
	for _, check := range checks {
		ids = append(ids, check.ID)
	}
	return ids
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func checksByID(checks []Check) map[string]Check {
	byID := map[string]Check{}
	for _, check := range checks {
		byID[check.ID] = check
	}
	return byID
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func fixedVerifyTime(value string) func() time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return func() time.Time { return parsed }
}

package perfbench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func sampleTaskSet() TaskSet {
	return TaskSet{
		ID:   "terminal-bench-mini",
		Name: "Terminal-Bench (mini)",
		Tasks: []BenchTask{
			{ID: "t1", Name: "fix failing test", Prompt: "make the test pass", WorkspaceFixture: "fixtures/t1"},
			{ID: "t2", Name: "add flag", Prompt: "add a --json flag", WorkspaceFixture: "fixtures/t2"},
		},
	}
}

func TestRunTasksRecordsModelCommitAndSelfCorrect(t *testing.T) {
	config := TaskConfig{
		Model:       "test-model",
		Mode:        "build",
		SelfCorrect: true,
		Version:     "1.2.3",
		Commit:      "abc1234",
		Runner: func(_ context.Context, task BenchTask, rc RunContext) TaskOutcome {
			if !rc.SelfCorrect {
				t.Fatalf("runner should see SelfCorrect=true from config")
			}
			if rc.Model != "test-model" {
				t.Fatalf("runner model = %q, want test-model", rc.Model)
			}
			return TaskOutcome{Passed: true}
		},
	}

	result, err := RunTasks(context.Background(), sampleTaskSet(), config)
	if err != nil {
		t.Fatalf("RunTasks returned error: %v", err)
	}

	if result.Model != "test-model" {
		t.Fatalf("result.Model = %q, want test-model", result.Model)
	}
	if result.Commit != "abc1234" {
		t.Fatalf("result.Commit = %q, want abc1234", result.Commit)
	}
	if result.Version != "1.2.3" {
		t.Fatalf("result.Version = %q, want 1.2.3", result.Version)
	}
	if !result.SelfCorrect {
		t.Fatalf("result.SelfCorrect = false, want true")
	}
	if result.Mode != "build" {
		t.Fatalf("result.Mode = %q, want build", result.Mode)
	}
	if result.TasksAttempted != 2 || result.TasksPassed != 2 {
		t.Fatalf("attempted/passed = %d/%d, want 2/2", result.TasksAttempted, result.TasksPassed)
	}
	if result.Suite != "terminal-bench-mini" {
		t.Fatalf("result.Suite = %q, want terminal-bench-mini", result.Suite)
	}
	if strings.TrimSpace(result.Date) == "" {
		t.Fatalf("result.Date must be recorded, got empty")
	}
	if result.SchemaVersion != TaskSchemaVersion {
		t.Fatalf("schema version = %d, want %d", result.SchemaVersion, TaskSchemaVersion)
	}
	if len(result.Tasks) != 2 {
		t.Fatalf("per-task results = %d, want 2", len(result.Tasks))
	}
}

func TestRunTasksCountsPassesAndFailures(t *testing.T) {
	config := TaskConfig{
		Model: "m",
		Runner: func(_ context.Context, task BenchTask, _ RunContext) TaskOutcome {
			if task.ID == "t1" {
				return TaskOutcome{Passed: true}
			}
			return TaskOutcome{Passed: false, Detail: "verification failed"}
		},
	}

	result, err := RunTasks(context.Background(), sampleTaskSet(), config)
	if err != nil {
		t.Fatalf("RunTasks returned error: %v", err)
	}
	if result.TasksAttempted != 2 || result.TasksPassed != 1 {
		t.Fatalf("attempted/passed = %d/%d, want 2/1", result.TasksAttempted, result.TasksPassed)
	}
	if result.PassRate < 0.49 || result.PassRate > 0.51 {
		t.Fatalf("pass rate = %v, want ~0.5", result.PassRate)
	}
	var t2 *TaskResult
	for i := range result.Tasks {
		if result.Tasks[i].ID == "t2" {
			t2 = &result.Tasks[i]
		}
	}
	if t2 == nil || t2.Passed || t2.Detail != "verification failed" {
		t.Fatalf("t2 result = %#v, want failed with detail", t2)
	}
}

func TestRunTasksRecordsRunnerError(t *testing.T) {
	config := TaskConfig{
		Model: "m",
		Runner: func(_ context.Context, task BenchTask, _ RunContext) TaskOutcome {
			return TaskOutcome{Err: errors.New("zero exec exited 1")}
		},
	}

	result, err := RunTasks(context.Background(), sampleTaskSet(), config)
	if err != nil {
		t.Fatalf("RunTasks must not abort on a single task error: %v", err)
	}
	// A runner error counts as a non-pass (attempted but not passed) and is
	// recorded as the task detail, never silently dropped.
	if result.TasksPassed != 0 {
		t.Fatalf("errored tasks must not count as passed: %#v", result)
	}
	if result.Errors != 2 {
		t.Fatalf("errors = %d, want 2", result.Errors)
	}
	if !strings.Contains(result.Tasks[0].Detail, "zero exec exited 1") {
		t.Fatalf("task detail should carry the runner error, got %q", result.Tasks[0].Detail)
	}
}

func TestRunTasksRejectsEmptyTaskSet(t *testing.T) {
	_, err := RunTasks(context.Background(), TaskSet{ID: "empty"}, TaskConfig{Model: "m", Runner: passingRunner})
	if err == nil {
		t.Fatalf("RunTasks should reject an empty task set")
	}
}

func TestRunTasksRequiresModel(t *testing.T) {
	_, err := RunTasks(context.Background(), sampleTaskSet(), TaskConfig{Runner: passingRunner})
	if err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("RunTasks should require a model, got err=%v", err)
	}
}

func TestRunTasksRequiresRunner(t *testing.T) {
	_, err := RunTasks(context.Background(), sampleTaskSet(), TaskConfig{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "runner") {
		t.Fatalf("RunTasks should require a runner, got err=%v", err)
	}
}

func TestFormatTaskSummaryIsHonestAboutModelAndSelfCorrect(t *testing.T) {
	result := TaskRunResult{
		SchemaVersion:  TaskSchemaVersion,
		Suite:          "terminal-bench-mini",
		Model:          "test-model",
		Mode:           "build",
		SelfCorrect:    true,
		Version:        "1.2.3",
		Commit:         "abc1234",
		Date:           "2026-06-12T00:00:00Z",
		TasksAttempted: 2,
		TasksPassed:    1,
		PassRate:       0.5,
		Tasks: []TaskResult{
			{ID: "t1", Name: "fix", Passed: true},
			{ID: "t2", Name: "flag", Passed: false, Detail: "nope"},
		},
	}

	summary := FormatTaskSummary(result)
	for _, want := range []string{
		"terminal-bench-mini",
		"model: test-model",
		"self-correct: on",
		"commit abc1234",
		"1/2",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestFormatTaskSummarySelfCorrectOff(t *testing.T) {
	summary := FormatTaskSummary(TaskRunResult{
		Suite: "s", Model: "m", SelfCorrect: false,
		TasksAttempted: 1, TasksPassed: 0,
		Tasks: []TaskResult{{ID: "t1", Passed: false}},
	})
	if !strings.Contains(summary, "self-correct: off") {
		t.Fatalf("summary should report self-correct off:\n%s", summary)
	}
}

func passingRunner(context.Context, BenchTask, RunContext) TaskOutcome {
	return TaskOutcome{Passed: true}
}

func writeManifest(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "suite.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return path
}

func TestLoadTaskSetRejectsTrailingContent(t *testing.T) {
	// A "fail loudly" manifest loader must reject extra JSON after the object, not
	// silently load only the first value.
	path := writeManifest(t, t.TempDir(), `{"id":"s","tasks":[{"id":"t1","prompt":"p"}]}
not-json`)
	if _, err := LoadTaskSet(path); err == nil {
		t.Fatalf("LoadTaskSet should reject trailing content after the manifest object")
	}
}

func TestLoadTaskSetResolvesFixturesRelativeToManifest(t *testing.T) {
	// Relative workspaceFixture paths must be anchored at the manifest's directory,
	// not the caller's cwd, so the documented repo-root command points at the right
	// directories.
	dir := t.TempDir()
	path := writeManifest(t, dir, `{"id":"s","tasks":[
		{"id":"t1","prompt":"p","workspaceFixture":"./hello"},
		{"id":"t2","prompt":"p","workspaceFixture":"sub/cli"},
		{"id":"t3","prompt":"p"}
	]}`)
	set, err := LoadTaskSet(path)
	if err != nil {
		t.Fatalf("LoadTaskSet returned error: %v", err)
	}
	wantT1 := filepath.Join(dir, "hello")
	if set.Tasks[0].WorkspaceFixture != wantT1 {
		t.Fatalf("t1 fixture = %q, want %q", set.Tasks[0].WorkspaceFixture, wantT1)
	}
	wantT2 := filepath.Join(dir, "sub", "cli")
	if set.Tasks[1].WorkspaceFixture != wantT2 {
		t.Fatalf("t2 fixture = %q, want %q", set.Tasks[1].WorkspaceFixture, wantT2)
	}
	if set.Tasks[2].WorkspaceFixture != "" {
		t.Fatalf("t3 fixture = %q, want empty (no fixture)", set.Tasks[2].WorkspaceFixture)
	}
}

func TestLoadTaskSetKeepsAbsoluteFixtures(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "elsewhere")
	path := writeManifest(t, dir, `{"id":"s","tasks":[{"id":"t1","prompt":"p","workspaceFixture":`+strconv.Quote(abs)+`}]}`)
	set, err := LoadTaskSet(path)
	if err != nil {
		t.Fatalf("LoadTaskSet returned error: %v", err)
	}
	if set.Tasks[0].WorkspaceFixture != abs {
		t.Fatalf("absolute fixture = %q, want unchanged %q", set.Tasks[0].WorkspaceFixture, abs)
	}
}

func writeExecStub(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("exec stub uses a POSIX shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "zero-stub.sh")
	script := "#!/bin/sh\n" + body
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write exec stub: %v", err)
	}
	return path
}

func writeBlockingExecStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	if err := os.WriteFile(source, []byte(`package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	fmt.Println("{\"type\":\"run_end\",\"exitCode\":0}")
	if ready := os.Getenv("PERFBENCH_BLOCKING_STUB_READY"); ready != "" {
		if err := os.WriteFile(ready, nil, 0600); err != nil {
			panic(err)
		}
	}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat(os.Getenv("PERFBENCH_BLOCKING_STUB_STOP")); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
`), 0o600); err != nil {
		t.Fatalf("write blocking exec stub: %v", err)
	}
	binary := filepath.Join(dir, "zero-stub")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if output, err := exec.Command("go", "build", "-o", binary, source).CombinedOutput(); err != nil {
		t.Fatalf("build blocking exec stub: %v\n%s", err, output)
	}
	return binary
}

// Override Err only after Done closes, allowing both context failures to be
// injected at the readiness handoff rather than racing process startup.
type stubFailureContext struct {
	context.Context
	failure error
}

func (ctx stubFailureContext) Err() error {
	if ctx.Context.Err() != nil {
		return ctx.failure
	}
	return nil
}

func runAfterStubReady[T any](t *testing.T, failure error, run func(context.Context) T) T {
	t.Helper()
	root := t.TempDir()
	ready, stop := filepath.Join(root, "ready"), filepath.Join(root, "stop")
	t.Setenv("PERFBENCH_BLOCKING_STUB_READY", ready)
	t.Setenv("PERFBENCH_BLOCKING_STUB_STOP", stop)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan T, 1)
	done := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		// Independent of the production process-tree cleanup being tested.
		if err := os.WriteFile(stop, nil, 0o600); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("stub did not stop after independent cleanup")
		}
	})
	go func() {
		defer close(done)
		result <- run(stubFailureContext{Context: ctx, failure: failure})
	}()
	watchdog := time.NewTimer(15 * time.Second)
	defer watchdog.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case <-result:
			t.Fatal("runner returned before run_end readiness handoff")
		case <-watchdog.C:
			t.Fatal("stub did not emit run_end before watchdog")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case outcome := <-result:
		return outcome
	case <-time.After(4 * time.Second):
		t.Fatal("runner did not return after context failure")
	}
	var zero T
	return zero
}

func TestNewExecRunnerNonZeroRunEndIsFailNotError(t *testing.T) {
	// A non-zero run_end exit code is a normal task failure, not a harness error,
	// even though the process itself exits non-zero.
	stub := writeExecStub(t, `echo '{"type":"run_end","exitCode":1}'
exit 1
`)
	runner := NewExecRunner(stub)
	outcome := runner(context.Background(), BenchTask{ID: "t1", Prompt: "p"}, RunContext{Model: "m"})
	if outcome.Err != nil {
		t.Fatalf("non-zero run_end must not be a harness error, got Err=%v", outcome.Err)
	}
	if outcome.Passed {
		t.Fatalf("non-zero run_end must be a failed task, got Passed=true")
	}
}

func TestRunEndCanReconcile(t *testing.T) {
	exitErr := &exec.ExitError{}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "success", want: true},
		{name: "exit error", err: exitErr, want: true},
		{name: "joined exit errors", err: errors.Join(exitErr, &exec.ExitError{}), want: true},
		{name: "ordinary error", err: errors.New("startup failed")},
		{name: "canceled", err: context.Canceled},
		{name: "deadline", err: context.DeadlineExceeded},
		{name: "wait delay", err: exec.ErrWaitDelay},
		{name: "exit plus cancellation", err: errors.Join(exitErr, context.Canceled)},
		{name: "wrapped exit error", err: fmt.Errorf("attachment failed: %w", exitErr)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := runEndCanReconcile(test.err); got != test.want {
				t.Fatalf("runEndCanReconcile(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}

func TestNewExecRunnerRunEndCannotHideContextFailure(t *testing.T) {
	stub := writeBlockingExecStub(t)
	tests := []struct {
		name    string
		wantErr error
	}{
		{
			name:    "cancellation",
			wantErr: context.Canceled,
		},
		{
			name:    "deadline",
			wantErr: context.DeadlineExceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			outcome := runAfterStubReady(t, test.wantErr, func(ctx context.Context) TaskOutcome {
				return NewExecRunner(stub)(ctx, BenchTask{ID: "t1", Prompt: "p"}, RunContext{Model: "m"})
			})
			if outcome.Err == nil || !errors.Is(outcome.Err, test.wantErr) {
				t.Fatalf("run_end must not hide %v, got %#v", test.wantErr, outcome)
			}
			if outcome.Passed {
				t.Fatal("context failure must not reach task pass accounting")
			}
		})
	}
}

func TestNewExecRunnerMissingRunEndFailsClosed(t *testing.T) {
	// A clean exit with no terminal run_end event is a harness error: we cannot
	// claim the task passed when the agent never reported a terminal event.
	stub := writeExecStub(t, `echo '{"type":"text","text":"hi"}'
exit 0
`)
	runner := NewExecRunner(stub)
	outcome := runner(context.Background(), BenchTask{ID: "t1", Prompt: "p"}, RunContext{Model: "m"})
	if outcome.Err == nil {
		t.Fatalf("missing terminal run_end must be a harness error, got Passed=%v", outcome.Passed)
	}
}

func TestNewExecRunnerZeroRunEndPasses(t *testing.T) {
	stub := writeExecStub(t, `echo '{"type":"run_end","exitCode":0}'
exit 0
`)
	runner := NewExecRunner(stub)
	outcome := runner(context.Background(), BenchTask{ID: "t1", Prompt: "p"}, RunContext{Model: "m"})
	if outcome.Err != nil {
		t.Fatalf("zero run_end with no verification must pass, got Err=%v", outcome.Err)
	}
	if !outcome.Passed {
		t.Fatalf("zero run_end with no verification must pass, got Passed=false")
	}
}

func TestNewExecRunnerWaitDelayCannotPassWithRunEnd(t *testing.T) {
	stub := writeExecStub(t, `sleep 3 &
echo '{"type":"run_end","exitCode":0}'
exit 0
`)
	runner := NewExecRunner(stub)
	outcome := runner(context.Background(), BenchTask{ID: "t1", Prompt: "p"}, RunContext{Model: "m"})
	if outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "output cleanup failed") {
		t.Fatalf("inherited output pipe must be a harness error, got %#v", outcome)
	}
	if outcome.Passed {
		t.Fatal("run_end must not bypass an output cleanup failure")
	}
}

func TestNewExecRunnerNonzeroWaitDelayCannotReconcileRunEnd(t *testing.T) {
	stub := writeExecStub(t, `sleep 3 &
echo '{"type":"run_end","exitCode":4}'
exit 4
`)
	runner := NewExecRunner(stub)
	outcome := runner(context.Background(), BenchTask{ID: "t1", Prompt: "p"}, RunContext{Model: "m"})
	if outcome.Err == nil || !errors.Is(outcome.Err, exec.ErrWaitDelay) {
		t.Fatalf("nonzero inherited output pipe must retain its cleanup failure, got %#v", outcome)
	}
	if outcome.Passed {
		t.Fatal("run_end must not reconcile a nonzero command with an output cleanup failure")
	}
}

func TestNewExecRunnerLaunchFailureIsHarnessError(t *testing.T) {
	// A binary that cannot be launched (no terminal event, process error) is a
	// genuine harness error.
	runner := NewExecRunner(filepath.Join(t.TempDir(), "does-not-exist"))
	outcome := runner(context.Background(), BenchTask{ID: "t1", Prompt: "p"}, RunContext{Model: "m"})
	if outcome.Err == nil {
		t.Fatalf("a launch failure must be a harness error")
	}
}

func TestRunVerificationInheritsEnvironment(t *testing.T) {
	// The verifier must see the inherited environment (PATH, HOME, toolchain
	// vars, ...) the same way a maintainer running the command by hand would.
	// A command that only succeeds when it can read an inherited var proves the
	// environment is passed through rather than reset to NO_COLOR only.
	if runtime.GOOS == "windows" {
		t.Skip("verification probe uses a POSIX shell")
	}
	t.Setenv("PERFBENCH_VERIFY_PROBE", "present")
	task := BenchTask{
		ID:                  "t1",
		VerificationCommand: []string{"sh", "-c", `[ "$PERFBENCH_VERIFY_PROBE" = present ]`},
	}
	outcome := runVerification(context.Background(), task)
	if !outcome.Passed {
		t.Fatalf("verifier should see the inherited env var, got Passed=false detail=%q", outcome.Detail)
	}
}

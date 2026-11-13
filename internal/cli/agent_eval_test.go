package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/agenteval"
)

func TestRunEvalHelpIsListed(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"--help"}, &stdout, &stderr, appDeps{})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "eval") || !strings.Contains(stdout.String(), "offline agent eval") {
		t.Fatalf("expected eval command in help, got %q", stdout.String())
	}
}

func TestRunEvalRequiresSuitePath(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"eval"}, &stdout, &stderr, appDeps{
		runAgentEval: func(context.Context, agentEvalOptions) (agentEvalReport, error) {
			t.Fatal("runAgentEval should not be called without --suite")
			return agentEvalReport{}, nil
		},
	})

	if exitCode != exitUsage {
		t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--suite requires a path") {
		t.Fatalf("expected missing suite error, got %q", stderr.String())
	}
}

func TestRunEvalJSONMode(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	report := agentEvalReport{
		Suite:  "evals/context.yaml",
		OK:     true,
		Total:  2,
		Passed: 2,
	}

	exitCode := runWithDeps([]string{"eval", "--suite", "evals/context.yaml", "--json"}, &stdout, &stderr, appDeps{
		runAgentEval: func(ctx context.Context, options agentEvalOptions) (agentEvalReport, error) {
			if options.Mode != "validate" || options.SuitePath != "evals/context.yaml" || !options.JSON {
				t.Fatalf("unexpected eval options: %#v", options)
			}
			return report, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	var decoded agentEvalReport
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decode eval JSON: %v\n%s", err, stdout.String())
	}
	if decoded.Suite != report.Suite || !decoded.OK || decoded.Passed != 2 {
		t.Fatalf("unexpected eval JSON: %#v", decoded)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw eval JSON: %v", err)
	}
	for _, key := range []string{"tasks", "checks", "total", "passed", "failed", "errors"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("expected JSON key %q in %s", key, stdout.String())
		}
	}
	for _, key := range []string{"tasks", "checks", "failed", "blocked", "errors"} {
		if string(raw[key]) != "0" {
			t.Fatalf("expected JSON key %q to be zero, got %s", key, string(raw[key]))
		}
	}
}

func TestRunEvalRunJSONModePassesRunnerOptions(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{
		"eval", "run",
		"--suite", "evals/context.json",
		"--task", "edit-reader",
		"--workspace", "D:\\work\\zero-fixture",
		"--json",
	}, &stdout, &stderr, appDeps{
		runAgentEval: func(ctx context.Context, options agentEvalOptions) (agentEvalReport, error) {
			if options.Mode != "run" || options.SuitePath != "evals/context.json" || options.TaskID != "edit-reader" || options.WorkspacePath != "D:\\work\\zero-fixture" || !options.JSON {
				t.Fatalf("unexpected eval run options: %#v", options)
			}
			return agentEvalReport{
				Suite:  "quality-context",
				TaskID: "edit-reader",
				Status: "pass",
				OK:     true,
				Total:  2,
				Passed: 2,
			}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	var decoded agentEvalReport
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decode eval run JSON: %v\n%s", err, stdout.String())
	}
	if decoded.TaskID != "edit-reader" || decoded.Status != "pass" || decoded.Blocked != 0 {
		t.Fatalf("unexpected eval run JSON: %#v", decoded)
	}
}

func TestRunEvalBenchJSONModePassesHarnessOptions(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{
		"eval", "bench",
		"--suite", "evals/context.json",
		"--task", "edit-reader",
		"--work-root", "D:\\tmp\\zero-evals",
		"--json",
		"--agent-command", "zero", "exec", "{prompt}",
	}, &stdout, &stderr, appDeps{
		runAgentEval: func(ctx context.Context, options agentEvalOptions) (agentEvalReport, error) {
			if options.Mode != "bench" || options.SuitePath != "evals/context.json" || options.TaskID != "edit-reader" || options.WorkRoot != "D:\\tmp\\zero-evals" || !options.JSON {
				t.Fatalf("unexpected eval bench options: %#v", options)
			}
			if got, want := strings.Join(options.AgentCommand, "\x00"), strings.Join([]string{"zero", "exec", "{prompt}"}, "\x00"); got != want {
				t.Fatalf("agent command = %#v, want zero exec {prompt}", options.AgentCommand)
			}
			return agentEvalReport{
				Suite:  "quality-context",
				TaskID: "edit-reader",
				Status: "pass",
				OK:     true,
				Total:  1,
				Passed: 1,
			}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	var decoded agentEvalReport
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decode eval bench JSON: %v\n%s", err, stdout.String())
	}
	if decoded.TaskID != "edit-reader" || decoded.Status != "pass" || decoded.Passed != 1 {
		t.Fatalf("unexpected eval bench JSON: %#v", decoded)
	}
}

func TestRunEvalBenchModelFlagsDeduplicateAndPreserveOrder(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	wantModels := []string{"gpt-5", "o4-mini", "claude-sonnet-4.5", "o3"}

	exitCode := runWithDeps([]string{
		"eval", "bench",
		"--suite", "evals/context.json",
		"--model", " gpt-5 ",
		"--models", "o4-mini,,gpt-5, claude-sonnet-4.5 ",
		"--model=o3",
		"--models=o4-mini",
		"--json",
	}, &stdout, &stderr, appDeps{
		runAgentEval: func(ctx context.Context, options agentEvalOptions) (agentEvalReport, error) {
			if options.Mode != "bench" || options.SuitePath != "evals/context.json" || !options.JSON {
				t.Fatalf("unexpected eval bench options: %#v", options)
			}
			if got, want := strings.Join(options.Models, "\x00"), strings.Join(wantModels, "\x00"); got != want {
				t.Fatalf("models = %#v, want %#v", options.Models, wantModels)
			}
			return agentEvalReport{
				Suite:  "quality-context",
				Status: "pass",
				OK:     true,
				Total:  1,
				Passed: 1,
			}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunEvalBenchReportDirAndKeepWorkspacesPassHarnessOptions(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	reportDir := t.TempDir()

	exitCode := runWithDeps([]string{
		"eval", "bench",
		"--suite=evals/context.json",
		"--task=edit-reader",
		"--keep-workspaces",
		"--report-dir", reportDir,
	}, &stdout, &stderr, appDeps{
		runAgentEval: func(ctx context.Context, options agentEvalOptions) (agentEvalReport, error) {
			if options.Mode != "bench" || !options.KeepWorkspaces || options.ReportDir != reportDir {
				t.Fatalf("unexpected eval bench options: %#v", options)
			}
			if options.WorkRoot != "" {
				t.Fatalf("default work root should remain empty at parse layer: %#v", options)
			}
			return agentEvalReport{
				Suite:   "quality-context",
				TaskID:  "edit-reader",
				Status:  "blocked",
				OK:      false,
				Total:   1,
				Blocked: 1,
			}, nil
		},
	})

	if exitCode != exitProvider {
		t.Fatalf("expected provider-style failure exit %d, got %d", exitProvider, exitCode)
	}
	reportPath := filepath.Join(reportDir, "agent-eval-report.json")
	if _, err := os.Stat(reportPath); err != nil {
		t.Fatalf("expected report file: %v", err)
	}
	if !strings.Contains(stdout.String(), "report: "+reportPath) {
		t.Fatalf("expected text output to mention report path, got:\n%s", stdout.String())
	}
}

func TestRunEvalBenchDefaultHarnessBlocksWithoutAgentCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	suitePath := filepath.Join("..", "agenteval", "testdata", "sample_suite.json")

	exitCode := runWithDeps([]string{
		"eval", "bench",
		"--suite", suitePath,
		"--task", "document-stream-json-verify-events",
		"--json",
	}, &stdout, &stderr, appDeps{})

	if exitCode != exitProvider {
		t.Fatalf("expected provider-style failure exit %d, got %d: %s", exitProvider, exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	var decoded agentEvalReport
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decode eval bench JSON: %v\n%s", err, stdout.String())
	}
	if decoded.Status != "blocked" || decoded.Blocked != 1 {
		t.Fatalf("expected blocked benchmark report, got %#v", decoded)
	}
	if len(decoded.Failures) == 0 || !strings.Contains(decoded.Failures[0].Message, "agent command is required") {
		t.Fatalf("expected agent command failure, got %#v", decoded.Failures)
	}
}

func TestRunEvalBenchDefaultHarnessRunsModelMatrix(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	suitePath := filepath.Join("..", "agenteval", "testdata", "sample_suite.json")

	exitCode := runWithDeps([]string{
		"eval", "bench",
		"--suite", suitePath,
		"--task", "document-stream-json-verify-events",
		"--models", "model-a,model-b",
		"--json",
	}, &stdout, &stderr, appDeps{})

	if exitCode != exitProvider {
		t.Fatalf("expected provider-style failure exit %d, got %d: %s", exitProvider, exitCode, stderr.String())
	}
	var decoded agentEvalReport
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decode eval bench JSON: %v\n%s", err, stdout.String())
	}
	if decoded.Total != 2 || decoded.Blocked != 2 {
		t.Fatalf("expected two blocked model runs, got %#v", decoded)
	}
	if decoded.Benchmark == nil || len(decoded.Benchmark.Tasks) != 2 {
		t.Fatalf("expected nested benchmark detail, got %#v", decoded.Benchmark)
	}
	if decoded.Benchmark.Tasks[0].Model != "model-a" || decoded.Benchmark.Tasks[1].Model != "model-b" {
		t.Fatalf("benchmark model order = %#v", decoded.Benchmark.Tasks)
	}
	if len(decoded.Failures) < 2 ||
		!strings.HasPrefix(decoded.Failures[0].ID, "model-a.document-stream-json-verify-events") ||
		!strings.HasPrefix(decoded.Failures[len(decoded.Failures)-1].ID, "model-b.document-stream-json-verify-events") {
		t.Fatalf("model-prefixed failures = %#v", decoded.Failures)
	}
}

func TestAgentEvalFailuresFromTaskReportDeduplicatesTaskLevelFailures(t *testing.T) {
	failures := agentEvalFailuresFromTaskReport(agenteval.BenchmarkTaskReport{
		TaskID: "task-a",
		Model:  "model-a",
		Agent:  agenteval.AgentRunResult{Error: "boom"},
		Report: agenteval.Report{
			Error: "boom",
			Results: []agenteval.Result{
				{Status: agenteval.StatusError, Message: "boom"},
				{ID: "verify", Status: agenteval.StatusFail, Message: "boom"},
			},
		},
	})

	if len(failures) != 2 {
		t.Fatalf("failures = %#v, want two unique entries", failures)
	}
	if failures[0] != (agentEvalFailure{ID: "model-a.task-a", Message: "boom"}) {
		t.Fatalf("first failure = %#v", failures[0])
	}
	if failures[1] != (agentEvalFailure{ID: "model-a.task-a.verify", Message: "boom"}) {
		t.Fatalf("second failure = %#v", failures[1])
	}
}

func TestRunEvalBenchTextOutputShowsScores(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{
		"eval", "bench",
		"--suite", "evals/context.json",
		"--agent-command", "zero", "exec", "{prompt}",
	}, &stdout, &stderr, appDeps{
		runAgentEval: func(_ context.Context, _ agentEvalOptions) (agentEvalReport, error) {
			// agentEvalReportFromBenchmark populates both Tasks and Total; the
			// text formatter must still surface the scored tallies.
			return agentEvalReport{
				Suite:  "quality-context",
				Status: "fail",
				OK:     false,
				Tasks:  2,
				Total:  2,
				Passed: 1,
				Failed: 1,
			}, nil
		},
	})

	if exitCode != exitProvider {
		t.Fatalf("expected provider-style failure exit %d, got %d: %s", exitProvider, exitCode, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "summary: 2 total, 1 passed, 1 failed, 0 blocked, 0 errors") {
		t.Fatalf("expected scored summary in bench text output, got:\n%s", out)
	}
	if strings.Contains(out, "tasks, 0 checks") {
		t.Fatalf("bench text output must not hide scores behind the validate-style summary:\n%s", out)
	}
}

func TestRunEvalBenchTextOutputSurfacesWorkRoot(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{
		"eval", "bench",
		"--suite", "evals/context.json",
		"--keep-workspaces",
		"--agent-command", "zero", "exec", "{prompt}",
	}, &stdout, &stderr, appDeps{
		runAgentEval: func(_ context.Context, options agentEvalOptions) (agentEvalReport, error) {
			if !options.KeepWorkspaces {
				t.Fatalf("expected keep-workspaces option: %#v", options)
			}
			return agentEvalReport{
				Suite:    "quality-context",
				Status:   "pass",
				OK:       true,
				Total:    1,
				Passed:   1,
				WorkRoot: "/tmp/zero-eval-abc",
			}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected success exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "work-root: /tmp/zero-eval-abc") {
		t.Fatalf("expected kept work root in text output, got:\n%s", stdout.String())
	}
}

func TestRunEvalBenchKeepWorkspacesSurfacesRealWorkRoot(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	suitePath := filepath.Join("..", "agenteval", "testdata", "sample_suite.json")
	workRoot := t.TempDir()

	// Drive the real defaultRunAgentEval (appDeps{}) so the keep-workspaces
	// WorkRoot wiring is exercised, not just the formatter.
	exitCode := runWithDeps([]string{
		"eval", "bench",
		"--suite", suitePath,
		"--task", "document-stream-json-verify-events",
		"--work-root", workRoot,
		"--keep-workspaces",
	}, &stdout, &stderr, appDeps{})

	if exitCode != exitProvider {
		t.Fatalf("expected provider-style exit %d, got %d: %s", exitProvider, exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "work-root: "+workRoot) {
		t.Fatalf("expected real work root surfaced in text output, got:\n%s", stdout.String())
	}
}

func TestAgentEvalReportFromBenchmarkSurfacesTruncation(t *testing.T) {
	report := agenteval.BenchmarkReport{
		SuiteID: "s",
		OK:      true,
		Summary: agenteval.BenchmarkSummary{TotalTasks: 1, PassedTasks: 1},
		Tasks: []agenteval.BenchmarkTaskReport{{
			TaskID: "t1",
			Agent:  agenteval.AgentRunResult{ExitCode: 0, Truncated: true},
			Report: agenteval.Report{Status: agenteval.StatusPass, OK: true},
		}},
	}

	converted := agentEvalReportFromBenchmark(agenteval.Suite{ID: "s", Name: "S"}, report)

	if !converted.Truncated {
		t.Fatal("expected Truncated to be surfaced from the task agent result")
	}
	if text := formatAgentEvalReport(converted); !strings.Contains(text, "note: agent output was truncated") {
		t.Fatalf("expected truncation note in text output:\n%s", text)
	}
}

func TestRunEvalBenchParsesTimeout(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{
		"eval", "bench",
		"--suite", "evals/context.json",
		"--timeout", "90s",
		"--json",
		"--agent-command", "zero", "exec", "{prompt}",
	}, &stdout, &stderr, appDeps{
		runAgentEval: func(_ context.Context, options agentEvalOptions) (agentEvalReport, error) {
			if options.Timeout != 90*time.Second {
				t.Fatalf("Timeout = %s, want 90s", options.Timeout)
			}
			return agentEvalReport{Suite: "quality-context", OK: true, Total: 1, Passed: 1}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected success exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
}

func TestRunEvalTimeoutValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "run mode rejected",
			args: []string{"eval", "run", "--suite", "evals/context.json", "--timeout", "5s"},
			want: "--timeout is only valid for eval bench",
		},
		{
			name: "invalid duration",
			args: []string{"eval", "bench", "--suite", "evals/context.json", "--timeout=soon"},
			want: "--timeout must be a Go duration",
		},
		{
			name: "negative duration",
			args: []string{"eval", "bench", "--suite", "evals/context.json", "--timeout=-5s"},
			want: "--timeout must not be negative",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := runWithDeps(tt.args, &stdout, &stderr, appDeps{
				runAgentEval: func(context.Context, agentEvalOptions) (agentEvalReport, error) {
					t.Fatal("runAgentEval should not be called for invalid --timeout")
					return agentEvalReport{}, nil
				},
			})

			if exitCode != exitUsage {
				t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("expected %q, got %q", tt.want, stderr.String())
			}
		})
	}
}

func TestRunEvalRuntimeErrorReturnsCrashExit(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"eval", "bench", "--suite", "evals/context.json"}, &stdout, &stderr, appDeps{
		runAgentEval: func(context.Context, agentEvalOptions) (agentEvalReport, error) {
			return agentEvalReport{}, agentEvalRuntimeError{errors.New("create benchmark work root: disk full")}
		},
	})

	if exitCode != exitCrash {
		t.Fatalf("expected crash exit %d, got %d", exitCrash, exitCode)
	}
	if !strings.Contains(stderr.String(), "disk full") {
		t.Fatalf("expected runtime error message in stderr, got %q", stderr.String())
	}
}

func TestRunEvalBenchRejectsRunOnlyWorkspaceFlag(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"eval", "bench", "--suite", "evals/context.json", "--workspace", "."}, &stdout, &stderr, appDeps{
		runAgentEval: func(context.Context, agentEvalOptions) (agentEvalReport, error) {
			t.Fatal("runAgentEval should not be called for invalid bench flags")
			return agentEvalReport{}, nil
		},
	})

	if exitCode != exitUsage {
		t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
	}
	if !strings.Contains(stderr.String(), "--workspace is only valid for eval run") {
		t.Fatalf("expected workspace mode error, got %q", stderr.String())
	}
}

func TestRunEvalRunRejectsBenchOnlyFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "work root",
			args: []string{"eval", "run", "--suite", "evals/context.json", "--work-root", "D:\\tmp\\zero-evals"},
			want: "--work-root is only valid for eval bench",
		},
		{
			name: "keep workspaces",
			args: []string{"eval", "run", "--suite", "evals/context.json", "--keep-workspaces"},
			want: "--keep-workspaces is only valid for eval bench",
		},
		{
			name: "agent command",
			args: []string{"eval", "run", "--suite", "evals/context.json", "--agent-command", "zero", "exec", "{prompt}"},
			want: "--agent-command is only valid for eval bench",
		},
		{
			name: "model",
			args: []string{"eval", "run", "--suite", "evals/context.json", "--model", "gpt-5"},
			want: "--model is only valid for eval bench",
		},
		{
			name: "models",
			args: []string{"eval", "run", "--suite", "evals/context.json", "--models=gpt-5,o4-mini"},
			want: "--models is only valid for eval bench",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := runWithDeps(tt.args, &stdout, &stderr, appDeps{
				runAgentEval: func(context.Context, agentEvalOptions) (agentEvalReport, error) {
					t.Fatal("runAgentEval should not be called for invalid run flags")
					return agentEvalReport{}, nil
				},
			})

			if exitCode != exitUsage {
				t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("expected %q, got %q", tt.want, stderr.String())
			}
		})
	}
}

func TestRunEvalHelpMentionsBenchModelFlags(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"eval", "bench", "--help"}, &stdout, &stderr, appDeps{})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	for _, want := range []string{"--model <id>", "--models <ids>", "{model}"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("expected help to contain %q, got:\n%s", want, stdout.String())
		}
	}
}

func TestRunEvalRunRequiresExplicitWorkspace(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"eval", "run", "--suite", "evals/context.json", "--task", "edit-reader"}, &stdout, &stderr, appDeps{
		runAgentEval: func(context.Context, agentEvalOptions) (agentEvalReport, error) {
			t.Fatal("eval run must not execute without an explicit --workspace (would run against the real working tree)")
			return agentEvalReport{}, nil
		},
	})

	if exitCode != exitUsage {
		t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
	}
	if !strings.Contains(stderr.String(), "--workspace") {
		t.Fatalf("expected a --workspace required error, got %q", stderr.String())
	}
}

func TestRunEvalRunsUnderCancellableContext(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cancellable := false

	runWithDeps([]string{"eval", "--suite", "evals/context.json"}, &stdout, &stderr, appDeps{
		runAgentEval: func(ctx context.Context, _ agentEvalOptions) (agentEvalReport, error) {
			// signal.NotifyContext yields a cancellable context (non-nil Done);
			// context.Background().Done() is nil.
			cancellable = ctx.Done() != nil
			return agentEvalReport{Suite: "quality-context", OK: true}, nil
		},
	})

	if !cancellable {
		t.Fatal("eval must run under a cancellable signal context, not context.Background()")
	}
}

func TestRunEvalRunFailureTextShowsSummaryAndFailures(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"eval", "run", "--suite=evals/context.json", "--task=edit-reader", "--workspace", "fixture-dir"}, &stdout, &stderr, appDeps{
		runAgentEval: func(ctx context.Context, options agentEvalOptions) (agentEvalReport, error) {
			if options.Mode != "run" || options.TaskID != "edit-reader" || options.WorkspacePath != "fixture-dir" {
				t.Fatalf("unexpected eval run options: %#v", options)
			}
			return agentEvalReport{
				Suite:   "quality-context",
				TaskID:  "edit-reader",
				Status:  "fail",
				OK:      false,
				Total:   3,
				Passed:  1,
				Failed:  1,
				Blocked: 1,
				Failures: []agentEvalFailure{{
					ID:      "test",
					Message: "go test ./... exited 1",
				}},
			}, nil
		},
	})

	if exitCode != exitProvider {
		t.Fatalf("expected provider-style failure exit %d, got %d", exitProvider, exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	for _, want := range []string{
		"suite: quality-context",
		"task: edit-reader",
		"status: fail",
		"summary: 3 total, 1 passed, 1 failed, 1 blocked, 0 errors",
		"failures:",
		"test - go test ./... exited 1",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, stdout.String())
		}
	}
}

func TestRunEvalRunReportDirWritesJSONReport(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	reportDir := t.TempDir()

	exitCode := runWithDeps([]string{"eval", "run", "--suite", "evals/context.json", "--workspace", "fixture-dir", "--report-dir", reportDir}, &stdout, &stderr, appDeps{
		runAgentEval: func(ctx context.Context, options agentEvalOptions) (agentEvalReport, error) {
			if options.ReportDir != reportDir {
				t.Fatalf("unexpected report dir: %#v", options)
			}
			return agentEvalReport{
				Suite:  "quality-context",
				TaskID: "edit-reader",
				Status: "pass",
				OK:     true,
				Total:  1,
				Passed: 1,
			}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	reportPath := filepath.Join(reportDir, "agent-eval-report.json")
	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var decoded agentEvalReport
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode report: %v\n%s", err, string(data))
	}
	if decoded.ReportPath != reportPath || decoded.Suite != "quality-context" {
		t.Fatalf("unexpected written report: %#v", decoded)
	}
	if !strings.Contains(stdout.String(), "report: "+reportPath) {
		t.Fatalf("expected text output to mention report path, got:\n%s", stdout.String())
	}
}

func TestRunEvalRunDefaultRunnerUsesAgentEvalRunner(t *testing.T) {
	workspace := t.TempDir()
	if output, err := exec.Command("git", "-C", workspace, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, string(output))
	}
	if err := os.WriteFile(filepath.Join(workspace, "expected.txt"), []byte("changed"), 0o600); err != nil {
		t.Fatalf("write expected file: %v", err)
	}
	suitePath := filepath.Join(t.TempDir(), "suite.json")
	if err := os.WriteFile(suitePath, []byte(`{
		"id": "runner-cli",
		"name": "Runner CLI",
		"tasks": [{
			"id": "local-score",
			"name": "Local score",
			"prompt": "Touch the expected file.",
			"workspaceFixture": "fixtures/runner",
			"verificationCommands": [
				{"id": "go-version", "name": "Go version", "command": ["go", "version"]}
			],
			"expectedChangedFiles": ["expected.txt"]
		}]
	}`), 0o600); err != nil {
		t.Fatalf("write suite: %v", err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"eval", "run", "--suite", suitePath, "--workspace", workspace}, &stdout, &stderr, appDeps{})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s\n%s", exitSuccess, exitCode, stderr.String(), stdout.String())
	}
	for _, want := range []string{
		"suite: runner-cli",
		"name: Runner CLI",
		"task: local-score",
		"summary: 2 total, 2 passed, 0 failed, 0 blocked, 0 errors",
		"status: pass",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, stdout.String())
		}
	}
}

func TestRunEvalDefaultRunnerLoadsSuite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "suite.json")
	if err := os.WriteFile(path, []byte(`{
		"id": "quality-foundation",
		"name": "Quality foundation",
		"tasks": [{
			"id": "prompt-discipline",
			"name": "Prompt discipline",
			"prompt": "Improve the system prompt.",
			"workspaceFixture": "fixtures/zero",
			"verificationCommands": [
				{"id": "test", "name": "Tests", "command": ["go", "test", "./internal/agent"]}
			],
			"expectedChangedFiles": ["internal/agent/system_prompt.md"]
		}]
	}`), 0o600); err != nil {
		t.Fatalf("write suite: %v", err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"eval", "--suite", path}, &stdout, &stderr, appDeps{})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	for _, want := range []string{
		"suite: quality-foundation",
		"name: Quality foundation",
		"summary: 1 tasks, 2 checks",
		"status: valid",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("expected output to contain %q, got:\n%s", want, stdout.String())
		}
	}
}

func TestRunEvalFailingSuiteReturnsProviderExit(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"eval", "--suite=evals/failing.yaml"}, &stdout, &stderr, appDeps{
		runAgentEval: func(context.Context, agentEvalOptions) (agentEvalReport, error) {
			return agentEvalReport{
				Suite:  "evals/failing.yaml",
				OK:     false,
				Total:  2,
				Passed: 1,
				Failed: 1,
				Failures: []agentEvalFailure{{
					ID:      "context.recall",
					Message: "expected answer to cite loaded context",
				}},
			}, nil
		},
	})

	if exitCode != exitProvider {
		t.Fatalf("expected provider-style failure exit %d, got %d", exitProvider, exitCode)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Zero agent eval") || !strings.Contains(stdout.String(), "context.recall") {
		t.Fatalf("unexpected eval text output: %q", stdout.String())
	}
}

func TestRunEvalRunnerErrorReturnsUsage(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"eval", "--suite", "missing.yaml"}, &stdout, &stderr, appDeps{
		runAgentEval: func(context.Context, agentEvalOptions) (agentEvalReport, error) {
			return agentEvalReport{}, errors.New("suite file not found")
		},
	})

	if exitCode != exitUsage {
		t.Fatalf("expected usage exit %d, got %d", exitUsage, exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "suite file not found") {
		t.Fatalf("expected runner error, got %q", stderr.String())
	}
}

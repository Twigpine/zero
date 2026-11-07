package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/perfbench"
)

func TestParseArgsUsesCliAndEnvOverrides(t *testing.T) {
	env := func(key string) string {
		values := map[string]string{
			"ZERO_PERF_COLD_START_WARN_MS":      "250",
			"ZERO_PERF_HARNESS_END_RSS_WARN_MB": "384",
		}
		return values[key]
	}

	options, err := parseArgs([]string{
		"--iterations=3",
		"--warmup",
		"0",
		"--first-output-warn-ms=600",
		"--output=dist/perf/report.json",
		"--ci",
		"--fail-on-warning",
	}, env)
	if err != nil {
		t.Fatalf("parseArgs returned error: %v", err)
	}

	if options.Iterations != 3 || options.WarmupIterations != 0 {
		t.Fatalf("iterations = %d warmup = %d", options.Iterations, options.WarmupIterations)
	}
	if options.Thresholds.ColdStartP95Ms != 250 || options.Thresholds.FirstOutputP95Ms != 600 || options.Thresholds.HarnessEndRssMaxMb != 384 {
		t.Fatalf("thresholds = %#v", options.Thresholds)
	}
	if options.Output != "dist/perf/report.json" || !options.CI || !options.FailOnWarning {
		t.Fatalf("CLI options = %#v", options)
	}
}

func TestParseArgsUsesEnvOnlyThreshold(t *testing.T) {
	options, err := parseArgs(nil, func(key string) string {
		if key == "ZERO_PERF_FIRST_OUTPUT_WARN_MS" {
			return "610"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("parseArgs returned error: %v", err)
	}
	if options.Thresholds.FirstOutputP95Ms != 610 {
		t.Fatalf("first output threshold = %v", options.Thresholds.FirstOutputP95Ms)
	}
}

func TestParseArgsRejectsInvalidValues(t *testing.T) {
	if _, err := parseArgs([]string{"--iterations=0"}, emptyEnv); err == nil || !strings.Contains(err.Error(), "--iterations must be a positive integer") {
		t.Fatalf("iterations error = %v", err)
	}
	if _, err := parseArgs([]string{"--warmup=-1"}, emptyEnv); err == nil || !strings.Contains(err.Error(), "--warmup must be a non-negative integer") {
		t.Fatalf("warmup error = %v", err)
	}
	if _, err := parseArgs([]string{"--output="}, emptyEnv); err == nil || !strings.Contains(err.Error(), "--output requires a value") {
		t.Fatalf("output error = %v", err)
	}
	if _, err := parseArgs(nil, func(key string) string {
		if key == "ZERO_PERF_ITERATIONS" {
			return "nope"
		}
		return ""
	}); err == nil || !strings.Contains(err.Error(), "ZERO_PERF_ITERATIONS must be a positive integer") {
		t.Fatalf("env error = %v", err)
	}
}

func TestHelpText(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"--help"}, emptyEnv, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run --help code = %d stderr = %q", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"Usage: zero-perf-bench", "--iterations", "ZERO_PERF_ITERATIONS"} {
		if !strings.Contains(output, want) {
			t.Fatalf("help missing %q:\n%s", want, output)
		}
	}
}

func emptyEnv(string) string {
	return ""
}

func TestTurnExitCodeFailsWhenEveryTaskErrored(t *testing.T) {
	var stderr bytes.Buffer
	allErrored := perfbench.TurnBenchResult{TasksAttempted: 3, TasksErrored: 3}
	if code := turnExitCode(allErrored, &stderr); code != 1 {
		t.Fatalf("exit=%d, want 1 when every task errored", code)
	}
	if !strings.Contains(stderr.String(), "no accepted benchmark sample") {
		t.Fatalf("stderr missing explanation: %q", stderr.String())
	}
	partial := perfbench.TurnBenchResult{TasksAttempted: 3, TasksErrored: 2}
	if code := turnExitCode(partial, &stderr); code != 0 {
		t.Fatalf("exit=%d, want 0 for partial errors (summary surfaces them)", code)
	}
}

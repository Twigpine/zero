package perfbench

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSummarizeSamplesSortsAndRounds(t *testing.T) {
	stats := SummarizeSamples([]float64{30.333, 10.111, 20.222, 40.444})

	if !equalFloatSlices(stats.Samples, []float64{10.11, 20.22, 30.33, 40.44}) {
		t.Fatalf("samples = %#v", stats.Samples)
	}
	if stats.Min != 10.11 || stats.Median != 25.28 || stats.P95 != 40.44 || stats.Max != 40.44 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestEvaluateWarningsClassifiesThresholds(t *testing.T) {
	metrics := Metrics{
		ColdStartMs:          SummarizeSamples([]float64{120, 301}),
		FirstOutputMs:        SummarizeSamples([]float64{12, 18}),
		ProcessDrainMs:       SummarizeSamples([]float64{0.2, 0.4}),
		HarnessEndRssMb:      SummarizeSamples([]float64{80, 90}),
		HarnessEndRssDeltaMb: SummarizeSamples([]float64{1, 3}),
	}

	warnings := EvaluateWarnings(metrics, DefaultThresholds)

	if len(warnings) != 1 {
		t.Fatalf("warnings = %#v, want one", warnings)
	}
	if warnings[0].Metric != "coldStartMs" || warnings[0].Statistic != "p95" || warnings[0].Observed != 301 || warnings[0].Threshold != 300 || warnings[0].Unit != "ms" {
		t.Fatalf("warning = %#v", warnings[0])
	}
}

func TestRunMinimalBenchmarkEndToEnd(t *testing.T) {
	t.Setenv("ZERO_PERF_HELPER", "1")
	command := []string{os.Args[0], "-test.run=TestPerfBenchHelperProcess"}

	result, err := Run(context.Background(), Options{
		Iterations:         1,
		WarmupIterations:   0,
		ColdStartCommand:   command,
		FirstOutputCommand: command,
		Thresholds: Thresholds{
			ColdStartP95Ms:     60_000,
			FirstOutputP95Ms:   60_000,
			HarnessEndRssMaxMb: 4096,
		},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if result.SchemaVersion != SchemaVersion || result.Iterations != 1 {
		t.Fatalf("result metadata = %#v", result)
	}
	if len(result.Metrics.ColdStartMs.Samples) != 1 || len(result.Metrics.FirstOutputMs.Samples) != 1 {
		t.Fatalf("metrics = %#v", result.Metrics)
	}
	if result.Metrics.HarnessEndRssMb.Max <= 0 {
		t.Fatalf("harness RSS = %#v", result.Metrics.HarnessEndRssMb)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
}

func TestFormatSummaryIncludesWarnings(t *testing.T) {
	metrics := Metrics{
		ColdStartMs:          SummarizeSamples([]float64{100, 110}),
		FirstOutputMs:        SummarizeSamples([]float64{20, 30}),
		ProcessDrainMs:       SummarizeSamples([]float64{0.1, 0.2}),
		HarnessEndRssMb:      SummarizeSamples([]float64{300, 310}),
		HarnessEndRssDeltaMb: SummarizeSamples([]float64{2, 4}),
	}
	warnings := EvaluateWarnings(metrics, DefaultThresholds)
	result := Result{
		SchemaVersion:       SchemaVersion,
		Timestamp:           "2026-06-03T00:00:00Z",
		Platform:            Platform{OS: "linux", Arch: "amd64", GoVersion: "go1.24.4", Harness: "go"},
		ColdStartCommand:    []string{"/repo/zero", "--version"},
		FirstOutputCommand:  []string{"/repo/zero", "--version"},
		Iterations:          2,
		WarmupIterations:    1,
		Thresholds:          DefaultThresholds,
		Metrics:             metrics,
		BenchmarkDurationMs: 50,
		Warnings:            warnings,
	}

	summary := FormatSummary(result)

	for _, want := range []string{
		"Zero performance benchmark",
		"cold start: median 105.00 ms",
		"harness end RSS: max 310.00 MB",
		"warnings:",
		"Benchmark harness end RSS 310.00 MB",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestEscapeActionCommand(t *testing.T) {
	if got := EscapeActionCommand("a%b\r\nc"); got != "a%25b%0D%0Ac" {
		t.Fatalf("EscapeActionCommand = %q", got)
	}
}

func TestPerfBenchHelperProcess(t *testing.T) {
	if os.Getenv("ZERO_PERF_HELPER") != "1" {
		return
	}
	if stream := os.Getenv("ZERO_PERF_HELPER_STREAM"); stream != "" {
		if stream == "stderr" {
			fmt.Fprintln(os.Stderr, "zero 0.1.0")
		} else {
			fmt.Println("zero 0.1.0")
		}
		time.Sleep(400 * time.Millisecond)
	} else {
		fmt.Println("zero 0.1.0")
	}
	os.Exit(0)
}

func TestMeasureFirstOutputCapturesFirstByteTiming(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			command := []string{os.Args[0], "-test.run=^TestPerfBenchHelperProcess$"}
			t.Setenv("ZERO_PERF_HELPER", "1")
			t.Setenv("ZERO_PERF_HELPER_STREAM", stream)
			sample, err := MeasureFirstOutput(context.Background(), command)
			if err != nil {
				t.Fatalf("MeasureFirstOutput returned error: %v", err)
			}
			if sample.FirstOutputMs >= 250 {
				t.Fatalf("first output = %fms, want before delayed exit", sample.FirstOutputMs)
			}
			if sample.ProcessDrainMs < 250 {
				t.Fatalf("process drain = %fms, want delayed exit time", sample.ProcessDrainMs)
			}
		})
	}
}

func equalFloatSlices(left []float64, right []float64) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

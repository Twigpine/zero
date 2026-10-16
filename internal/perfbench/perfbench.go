package perfbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/release"
)

const SchemaVersion = 3

type Thresholds struct {
	ColdStartP95Ms     float64 `json:"coldStartP95Ms"`
	FirstOutputP95Ms   float64 `json:"firstOutputP95Ms"`
	HarnessEndRssMaxMb float64 `json:"harnessEndRssMaxMb"`
}

type NumericStats struct {
	Samples []float64 `json:"samples"`
	Min     float64   `json:"min"`
	Median  float64   `json:"median"`
	Average float64   `json:"average"`
	P95     float64   `json:"p95"`
	Max     float64   `json:"max"`
}

type Metrics struct {
	ColdStartMs          NumericStats `json:"coldStartMs"`
	FirstOutputMs        NumericStats `json:"firstOutputMs"`
	ProcessDrainMs       NumericStats `json:"processDrainMs"`
	HarnessEndRssMb      NumericStats `json:"harnessEndRssMb"`
	HarnessEndRssDeltaMb NumericStats `json:"harnessEndRssDeltaMb"`
}

type Warning struct {
	Metric    string  `json:"metric"`
	Statistic string  `json:"statistic"`
	Observed  float64 `json:"observed"`
	Threshold float64 `json:"threshold"`
	Unit      string  `json:"unit"`
	Message   string  `json:"message"`
}

type Platform struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	GoVersion string `json:"goVersion"`
	Harness   string `json:"harness"`
}

type Result struct {
	SchemaVersion       int        `json:"schemaVersion"`
	Timestamp           string     `json:"timestamp"`
	Platform            Platform   `json:"platform"`
	ColdStartCommand    []string   `json:"coldStartCommand"`
	FirstOutputCommand  []string   `json:"firstOutputCommand"`
	Iterations          int        `json:"iterations"`
	WarmupIterations    int        `json:"warmupIterations"`
	Thresholds          Thresholds `json:"thresholds"`
	Metrics             Metrics    `json:"metrics"`
	BenchmarkDurationMs float64    `json:"benchmarkDurationMs"`
	Warnings            []Warning  `json:"warnings"`
}

type Options struct {
	Iterations         int
	WarmupIterations   int
	Thresholds         Thresholds
	ColdStartCommand   []string
	FirstOutputCommand []string
}

type firstOutputSample struct {
	FirstOutputMs        float64
	ProcessDrainMs       float64
	HarnessEndRssMb      float64
	HarnessEndRssDeltaMb float64
}

var DefaultThresholds = Thresholds{
	ColdStartP95Ms:     300,
	FirstOutputP95Ms:   500,
	HarnessEndRssMaxMb: 256,
}

func Run(ctx context.Context, options Options) (Result, error) {
	if options.Iterations < 1 {
		return Result{}, errors.New("iterations must be a positive integer")
	}
	if options.WarmupIterations < 0 {
		return Result{}, errors.New("warmup iterations must be a non-negative integer")
	}
	startedAt := time.Now()
	coldStartCommand := options.ColdStartCommand
	if len(coldStartCommand) == 0 {
		var err error
		coldStartCommand, err = ResolveColdStartCommand("")
		if err != nil {
			return Result{}, err
		}
	}
	firstOutputCommand := options.FirstOutputCommand
	if len(firstOutputCommand) == 0 {
		var err error
		firstOutputCommand, err = ResolveFirstOutputCommand("")
		if err != nil {
			return Result{}, err
		}
	}

	coldStartSamples := []float64{}
	firstOutputSamples := []firstOutputSample{}

	for i := 0; i < options.WarmupIterations; i++ {
		if _, err := MeasureColdStart(ctx, coldStartCommand); err != nil {
			return Result{}, err
		}
		if _, err := MeasureFirstOutput(ctx, firstOutputCommand); err != nil {
			return Result{}, err
		}
	}

	for i := 0; i < options.Iterations; i++ {
		coldStart, err := MeasureColdStart(ctx, coldStartCommand)
		if err != nil {
			return Result{}, err
		}
		firstOutput, err := MeasureFirstOutput(ctx, firstOutputCommand)
		if err != nil {
			return Result{}, err
		}
		coldStartSamples = append(coldStartSamples, coldStart)
		firstOutputSamples = append(firstOutputSamples, firstOutput)
	}

	firstOutputMs := []float64{}
	processDrainMs := []float64{}
	harnessEndRssMb := []float64{}
	harnessEndRssDeltaMb := []float64{}
	for _, sample := range firstOutputSamples {
		firstOutputMs = append(firstOutputMs, sample.FirstOutputMs)
		processDrainMs = append(processDrainMs, sample.ProcessDrainMs)
		harnessEndRssMb = append(harnessEndRssMb, sample.HarnessEndRssMb)
		harnessEndRssDeltaMb = append(harnessEndRssDeltaMb, sample.HarnessEndRssDeltaMb)
	}

	metrics := Metrics{
		ColdStartMs:          SummarizeSamples(coldStartSamples),
		FirstOutputMs:        SummarizeSamples(firstOutputMs),
		ProcessDrainMs:       SummarizeSamples(processDrainMs),
		HarnessEndRssMb:      SummarizeSamples(harnessEndRssMb),
		HarnessEndRssDeltaMb: SummarizeSamples(harnessEndRssDeltaMb),
	}
	result := Result{
		SchemaVersion:       SchemaVersion,
		Timestamp:           time.Now().UTC().Format(time.RFC3339Nano),
		Platform:            CurrentPlatform(),
		ColdStartCommand:    coldStartCommand,
		FirstOutputCommand:  firstOutputCommand,
		Iterations:          options.Iterations,
		WarmupIterations:    options.WarmupIterations,
		Thresholds:          options.Thresholds,
		Metrics:             metrics,
		BenchmarkDurationMs: RoundMetric(float64(time.Since(startedAt).Microseconds()) / 1000),
	}
	result.Warnings = EvaluateWarnings(metrics, options.Thresholds)
	return result, nil
}

func ResolveColdStartCommand(rootDir string) ([]string, error) {
	return resolveZeroVersionCommand(rootDir)
}

func ResolveFirstOutputCommand(rootDir string) ([]string, error) {
	return resolveZeroVersionCommand(rootDir)
}

func SummarizeSamples(samples []float64) NumericStats {
	if len(samples) == 0 {
		panic("cannot summarize an empty sample set")
	}
	sorted := append([]float64(nil), samples...)
	for i, sample := range sorted {
		sorted[i] = RoundMetric(sample)
	}
	sort.Float64s(sorted)
	total := 0.0
	for _, sample := range sorted {
		total += sample
	}
	return NumericStats{
		Samples: sorted,
		Min:     sorted[0],
		Median:  median(sorted),
		Average: RoundMetric(total / float64(len(sorted))),
		P95:     percentile(sorted, 95),
		Max:     sorted[len(sorted)-1],
	}
}

func EvaluateWarnings(metrics Metrics, thresholds Thresholds) []Warning {
	warnings := []Warning{}
	if warning, ok := createWarning("coldStartMs", "p95", metrics.ColdStartMs.P95, thresholds.ColdStartP95Ms, "ms", "Cold start p95"); ok {
		warnings = append(warnings, warning)
	}
	if warning, ok := createWarning("firstOutputMs", "p95", metrics.FirstOutputMs.P95, thresholds.FirstOutputP95Ms, "ms", "Binary first-output p95"); ok {
		warnings = append(warnings, warning)
	}
	if warning, ok := createWarning("harnessEndRssMb", "max", metrics.HarnessEndRssMb.Max, thresholds.HarnessEndRssMaxMb, "MB", "Benchmark harness end RSS"); ok {
		warnings = append(warnings, warning)
	}
	return warnings
}

func FormatSummary(result Result) string {
	lines := []string{
		fmt.Sprintf("Zero performance benchmark (%s/%s, Go %s)", result.Platform.OS, result.Platform.Arch, result.Platform.GoVersion),
		"command: " + FormatCommand(result.ColdStartCommand),
		"first-output command: " + FormatCommand(result.FirstOutputCommand),
		fmt.Sprintf("iterations: %d measured, %d warmup", result.Iterations, result.WarmupIterations),
		fmt.Sprintf("cold start: median %s, p95 %s (warn > %s)", FormatMetric(result.Metrics.ColdStartMs.Median, "ms"), FormatMetric(result.Metrics.ColdStartMs.P95, "ms"), FormatMetric(result.Thresholds.ColdStartP95Ms, "ms")),
		fmt.Sprintf("binary first output: median %s, p95 %s (warn > %s)", FormatMetric(result.Metrics.FirstOutputMs.Median, "ms"), FormatMetric(result.Metrics.FirstOutputMs.P95, "ms"), FormatMetric(result.Thresholds.FirstOutputP95Ms, "ms")),
		fmt.Sprintf("process drain: median %s, p95 %s", FormatMetric(result.Metrics.ProcessDrainMs.Median, "ms"), FormatMetric(result.Metrics.ProcessDrainMs.P95, "ms")),
		fmt.Sprintf("harness end RSS: max %s, max end delta %s (warn > %s)", FormatMetric(result.Metrics.HarnessEndRssMb.Max, "MB"), FormatMetric(result.Metrics.HarnessEndRssDeltaMb.Max, "MB"), FormatMetric(result.Thresholds.HarnessEndRssMaxMb, "MB")),
	}
	if len(result.Warnings) > 0 {
		lines = append(lines, "warnings:")
		for _, warning := range result.Warnings {
			lines = append(lines, "- "+warning.Message)
		}
	} else {
		lines = append(lines, "warnings: none")
	}
	return strings.Join(lines, "\n")
}

func EmitWarnings(w io.Writer, result Result) {
	for _, warning := range result.Warnings {
		_, _ = fmt.Fprintf(w, "::warning title=Zero performance::%s\n", EscapeActionCommand(warning.Message))
	}
}

func WriteJSON(w io.Writer, result Result) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func MeasureColdStart(ctx context.Context, command []string) (float64, error) {
	if len(command) == 0 {
		return 0, errors.New("cold-start command is empty")
	}
	startedAt := time.Now()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = appendNoColor(os.Environ())
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := execution.RunCommand(ctx, cmd)
	durationMs := RoundMetric(float64(time.Since(startedAt).Microseconds()) / 1000)
	if err != nil {
		return 0, commandError(command, err, output.String(), "")
	}
	return durationMs, nil
}

func MeasureFirstOutput(ctx context.Context, command []string) (firstOutputSample, error) {
	if len(command) == 0 {
		return firstOutputSample{}, errors.New("first-output command is empty")
	}
	rssBefore := readHarnessMemoryMb()
	startedAt := time.Now()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = offlineBenchmarkEnv(os.Environ())

	var once sync.Once
	var firstOutputAt time.Time
	markFirstOutput := func() {
		once.Do(func() {
			firstOutputAt = time.Now()
		})
	}
	stdout := &timedBuffer{onFirstWrite: markFirstOutput}
	stderr := &timedBuffer{onFirstWrite: markFirstOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	waitErr := execution.RunCommand(ctx, cmd)
	finishedAt := time.Now()

	if firstOutputAt.IsZero() {
		firstOutputAt = finishedAt
	}
	rssAfter := readHarnessMemoryMb()
	if waitErr != nil {
		return firstOutputSample{}, commandError(command, waitErr, stdout.String(), stderr.String())
	}
	return firstOutputSample{
		FirstOutputMs:        RoundMetric(float64(firstOutputAt.Sub(startedAt).Microseconds()) / 1000),
		ProcessDrainMs:       RoundMetric(math.Max(0, float64(finishedAt.Sub(firstOutputAt).Microseconds())/1000)),
		HarnessEndRssMb:      RoundMetric(rssAfter),
		HarnessEndRssDeltaMb: RoundMetric(math.Max(0, rssAfter-rssBefore)),
	}, nil
}

func CurrentPlatform() Platform {
	return Platform{
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		GoVersion: runtime.Version(),
		Harness:   "go",
	}
}

func RoundMetric(value float64) float64 {
	return math.Round(value*100) / 100
}

func FormatMetric(value float64, unit string) string {
	return fmt.Sprintf("%.2f %s", RoundMetric(value), unit)
}

func FormatCommand(command []string) string {
	parts := make([]string, 0, len(command))
	for _, part := range command {
		if strings.ContainsAny(part, " \t\r\n") {
			parts = append(parts, strconv.Quote(part))
		} else {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, " ")
}

func EscapeActionCommand(value string) string {
	replacer := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")
	return replacer.Replace(value)
}

func resolveZeroVersionCommand(rootDir string) ([]string, error) {
	if strings.TrimSpace(rootDir) == "" {
		var err error
		rootDir, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	binaryName := release.ZeroArtifactName(runtime.GOOS)
	binaryPath := filepath.Join(rootDir, binaryName)
	if _, err := os.Stat(binaryPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no %s binary found; run `go run ./cmd/zero-release build` before running the performance benchmark", binaryName)
		}
		return nil, err
	}
	return []string{binaryPath, "--version"}, nil
}

func createWarning(metric string, statistic string, observed float64, threshold float64, unit string, label string) (Warning, bool) {
	if observed <= threshold {
		return Warning{}, false
	}
	return Warning{
		Metric:    metric,
		Statistic: statistic,
		Observed:  observed,
		Threshold: threshold,
		Unit:      unit,
		Message:   fmt.Sprintf("%s %s exceeded warning threshold %s", label, FormatMetric(observed, unit), FormatMetric(threshold, unit)),
	}, true
}

func percentile(sortedSamples []float64, percentileValue float64) float64 {
	index := int(math.Ceil((percentileValue/100)*float64(len(sortedSamples)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sortedSamples) {
		index = len(sortedSamples) - 1
	}
	return sortedSamples[index]
}

func median(sortedSamples []float64) float64 {
	middle := len(sortedSamples) / 2
	if len(sortedSamples)%2 == 1 {
		return sortedSamples[middle]
	}
	return RoundMetric((sortedSamples[middle-1] + sortedSamples[middle]) / 2)
}

type timedBuffer struct {
	buffer       bytes.Buffer
	onFirstWrite func()
}

func (buffer *timedBuffer) Write(data []byte) (int, error) {
	if len(data) > 0 {
		buffer.onFirstWrite()
	}
	return buffer.buffer.Write(data)
}

func (buffer *timedBuffer) String() string {
	return buffer.buffer.String()
}

func commandError(command []string, err error, stdout string, stderr string) error {
	message := strings.TrimSpace(stderr)
	if message == "" {
		message = strings.TrimSpace(stdout)
	}
	if message == "" {
		message = "no output"
	}
	if exitErr := new(exec.ExitError); errors.As(err, &exitErr) {
		return fmt.Errorf("%s exited with %d: %s", FormatCommand(command), exitErr.ExitCode(), message)
	}
	return fmt.Errorf("%s failed: %s", FormatCommand(command), message)
}

func appendNoColor(env []string) []string {
	return setEnv(env, "NO_COLOR", "1")
}

func offlineBenchmarkEnv(env []string) []string {
	remove := map[string]bool{
		"ZERO_PROVIDER_COMMAND": true,
		"ZERO_PROVIDER":         true,
		"OPENAI_API_KEY":        true,
		"OPENAI_BASE_URL":       true,
		"OPENAI_MODEL":          true,
		"ANTHROPIC_API_KEY":     true,
		"ANTHROPIC_BASE_URL":    true,
		"ANTHROPIC_MODEL":       true,
		"GEMINI_API_KEY":        true,
		"GEMINI_BASE_URL":       true,
		"GEMINI_MODEL":          true,
		"GOOGLE_API_KEY":        true,
		"GOOGLE_BASE_URL":       true,
		"GOOGLE_MODEL":          true,
	}
	filtered := make([]string, 0, len(env)+1)
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if ok && remove[name] {
			continue
		}
		filtered = append(filtered, entry)
	}
	return setEnv(filtered, "NO_COLOR", "1")
}

func setEnv(env []string, name string, value string) []string {
	prefix := name + "="
	next := make([]string, 0, len(env)+1)
	replaced := false
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			next = append(next, prefix+value)
			replaced = true
			continue
		}
		next = append(next, entry)
	}
	if !replaced {
		next = append(next, prefix+value)
	}
	return next
}

func readHarnessMemoryMb() float64 {
	if rss, ok := readLinuxRSSMb(); ok {
		return rss
	}
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return float64(stats.Sys) / 1024 / 1024
}

func readLinuxRSSMb() (float64, bool) {
	bytes, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(bytes))
	if len(fields) < 2 {
		return 0, false
	}
	residentPages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return float64(residentPages*uint64(os.Getpagesize())) / 1024 / 1024, true
}

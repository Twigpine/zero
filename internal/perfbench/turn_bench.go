package perfbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/execprofile"
	"github.com/Gitlawb/zero/internal/trace"
)

// TurnSchemaVersion is the schema version of a published turn-benchmark result.
// Bump when the TurnBenchResult shape changes so consumers can detect drift.
//
// v2 splits pass/fail into three oracle tiers so the report cannot be misread as
// a blanket correctness verdict: tasksVerified/tasksPassed/correctnessPassRate
// cover only the positive-oracle classes (edit/fix); buildCheckedTasks/
// buildPassedTasks/buildPassRate cover the non-positive build-check classes
// (refactor); latencyOnlyTasks covers the no-oracle classes (nav/longproc/
// longctx/parallel). tasksAttempted is still every task run.
//
// v3 (Phase 0 oracle hardening) strengthens the oracles so the tiers are honest: edit/refactor
// now carry a stamped oracle_test.go compiled by `go test ./...` (the Go
// compiler is the structural verifier — a no-op refactor or a missing field
// fails to compile), and nav carries an answer-oracle grepping the agent's
// captured final answer. refactor graduates from build to correctness (its
// oracle is now positive), nav graduates from latency-only to correctness, and
// the build tier is empty (nothing is only-build-checked anymore). The result
// SHAPE is unchanged — only class-list membership shifts — but
// correctnessPassRate now means something strictly stronger, so the bump
// prevents a v2→v2 cross-version comparison from misreading the jump as a model
// improvement. New counts: correctness 34 (edit 10 + fix 8 + nav 10 + refactor
// 6), build 0, latency 14 (longproc 4 + longctx 4 + parallel 6).
//
// v4 adds tasksErrored: the number of tasks whose every iteration produced no
// accepted benchmark sample. That covers pre-run failures (missing binary,
// spawn error, process crash) and harness errors after a successful agent run
// (e.g. oracle stamping failed) alike — in every case the iteration yields no
// usable measurement. Errored tasks were previously visible only in warnings,
// so a run where nothing was actually measured still printed a clean-looking
// 0% pass summary and exited 0. tasksErrored makes that state first-class:
// the summary surfaces it, and the turn command exits nonzero when every
// attempted task errored. Accounting: an errored oracle task stays in its
// tier denominator as a failure; an errored latency-only task counts in
// latencyOnlyTasks and, as always, in no pass rate.
//
// v5 is ADDITIVE ONLY: execProfile records the execution profile the run was
// benchmarked under (omitted when none was selected), so profile A/B captures
// are self-describing and two reports can't be compared without noticing they
// ran different postures. No existing field changed shape or meaning; a v4
// consumer reading a v5 report misses only the new field.
const TurnSchemaVersion = 5

// TurnRunner runs one benchmark task and reports its outcome plus the captured
// per-turn trace. A non-nil Err means the run failed to execute (process crash);
// Passed reflects the verification result. Trace is the parsed NDJSON trace
// (nil when the run errored before emitting one).
type TurnRunner func(ctx context.Context, task BenchTask, rc RunContext) TurnTaskOutcome

// TurnTaskOutcome is what a TurnRunner reports for one task iteration.
type TurnTaskOutcome struct {
	Passed    bool
	VerifyErr string
	WallMs    float64
	Trace     *trace.TurnTrace
	// TraceIssue, when non-empty, explains why the per-turn trace could not be
	// parsed (file missing or malformed). The run still has a pass/fail verdict,
	// but an incomplete measurement is surfaced as a result warning so it can't
	// masquerade as a clean attribution sample.
	TraceIssue string
	Err        error
}

// TurnBenchConfig configures a turn-benchmark run.
type TurnBenchConfig struct {
	Model       string
	Mode        string
	SelfCorrect bool
	// ExecProfile, when non-empty, runs every task under the named execution
	// profile (zero exec --exec-profile) and stamps it into the result so the
	// report is self-describing for profile A/B comparisons.
	ExecProfile string
	Version     string
	Commit      string
	// Iterations is how many times each task is run. The per-process `zero exec`
	// runner is inherently cold-start, so this is the sample count for per-span
	// median/P95 — a genuine warm path needs an in-process runner (future work).
	Iterations int
	// Runner executes one task iteration. Required.
	Runner TurnRunner
	// Now overrides the clock for the recorded date (tests inject a fixed time).
	Now func() time.Time
}

// SpanStats summarizes one span's duration across all measured task iterations.
type SpanStats struct {
	Count    int     `json:"count"`
	TotalMs  float64 `json:"totalMs"`
	MedianMs float64 `json:"medianMs"`
	P95Ms    float64 `json:"p95Ms"`
	MaxMs    float64 `json:"maxMs"`
}

// LatencySource is one of the top controllable latency sources, ranked by total
// exclusive time across the whole run. Share is its fraction of total exclusive
// span time. Because exclusive time subtracts nested children, concurrent and
// nested spans no longer double-count, so shares of the top sources sum to ~1
// for a well-instrumented run.
type LatencySource struct {
	Span    string  `json:"span"`
	TotalMs float64 `json:"totalMs"`
	Share   float64 `json:"share"`
}

// ClassSummary is the per-class (task group) roll-up.
//
// Passed is the count that passed THIS class's oracle: a correctness pass for
// the positive-oracle classes (edit/fix/nav/refactor), and always 0 for the
// no-oracle classes (longproc/longctx/parallel) — the build tier is empty in v3
// (refactor graduated to correctness). Verified is how many tasks in the class
// carry an oracle; LatencyOnly is how many carry none. A latency-only class
// therefore reports Passed=0, Verified=0, LatencyOnly=Tasks.
type ClassSummary struct {
	Tasks       int                `json:"tasks"`
	Verified    int                `json:"verified"`
	Passed      int                `json:"passed"`
	LatencyOnly int                `json:"latencyOnly"`
	WallMs      NumericStats       `json:"wallMs"`
	SpanTotals  map[string]float64 `json:"spanTotals"`
}

// TurnBenchResult is the publishable turn-benchmark record.
//
// Pass/fail is split into three oracle tiers so it cannot be misread as a
// blanket correctness verdict:
//   - Correctness (tasksVerified / tasksPassed / correctnessPassRate): tasks
//     with a positive oracle (edit/fix/nav/refactor). This is the only pass
//     rate that can move with model quality.
//   - Build-only (buildCheckedTasks / buildPassedTasks / buildPassRate): the
//     non-positive build-check tier. Empty in v3 — refactor graduated to
//     correctness (its stamped oracle is positive) — so buildCheckedTasks is
//     always 0 and buildPassRate is 0. The fields remain for schema stability.
//   - Latency-only (latencyOnlyTasks): tasks with no oracle (longproc/longctx/
//     parallel). They ran for latency and span attribution only and are
//     excluded from every pass rate.
//
// tasksAttempted is still the total number of tasks run across all three tiers.
// The tier class lists are echoed so a consumer can see exactly which classes
// each rate is computed over.
type TurnBenchResult struct {
	SchemaVersion       int                     `json:"schemaVersion"`
	Suite               string                  `json:"suite"`
	Model               string                  `json:"model"`
	Mode                string                  `json:"mode,omitempty"`
	ExecProfile         string                  `json:"execProfile,omitempty"`
	SelfCorrect         bool                    `json:"selfCorrect"`
	Version             string                  `json:"version,omitempty"`
	Commit              string                  `json:"commit,omitempty"`
	Date                string                  `json:"date"`
	TasksAttempted      int                     `json:"tasksAttempted"`
	TasksErrored        int                     `json:"tasksErrored"`
	TasksVerified       int                     `json:"tasksVerified"`
	TasksPassed         int                     `json:"tasksPassed"`
	LatencyOnlyTasks    int                     `json:"latencyOnlyTasks"`
	BuildCheckedTasks   int                     `json:"buildCheckedTasks"`
	BuildPassedTasks    int                     `json:"buildPassedTasks"`
	CorrectnessPassRate float64                 `json:"correctnessPassRate"`
	BuildPassRate       float64                 `json:"buildPassRate"`
	CorrectnessClasses  []string                `json:"correctnessClasses,omitempty"`
	BuildOnlyClasses    []string                `json:"buildOnlyClasses,omitempty"`
	LatencyOnlyClasses  []string                `json:"latencyOnlyClasses,omitempty"`
	Iterations          int                     `json:"iterations"`
	PerSpan             map[string]SpanStats    `json:"perSpan"`
	TopLatency          []LatencySource         `json:"topLatency"`
	PerClass            map[string]ClassSummary `json:"perClass"`
	Totals              TurnBenchTotals         `json:"totals"`
	Warnings            []Warning               `json:"warnings,omitempty"`
}

// TurnBenchTotals aggregates token and count totals across the whole run.
type TurnBenchTotals struct {
	InputTokens       int64 `json:"inputTokens"`
	CachedInputTokens int64 `json:"cachedInputTokens"`
	CacheWriteTokens  int64 `json:"cacheWriteTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	ModelRequests     int64 `json:"modelRequests"`
	ToolCalls         int64 `json:"toolCalls"`
	Retries           int64 `json:"retries"`
	Reconnects        int64 `json:"reconnects"`
	Compactions       int64 `json:"compactions"`
}

// RunTurnBench executes every task in the set with the configured runner and
// returns a self-describing per-turn result. It never aborts on a single task
// failure — every task is attempted and recorded. Per-span stats aggregate
// across iterations; the top three controllable latency sources are ranked by
// total attributed time.
func RunTurnBench(ctx context.Context, set TaskSet, cfg TurnBenchConfig) (TurnBenchResult, error) {
	if len(set.Tasks) == 0 {
		return TurnBenchResult{}, errors.New("task set has no tasks")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return TurnBenchResult{}, errors.New("turn benchmark requires a model")
	}
	if cfg.Runner == nil {
		return TurnBenchResult{}, errors.New("turn benchmark requires a runner")
	}
	// Canonicalize the profile once at the boundary so every consumer — the
	// child exec args, the stamped result, the summary — carries the same
	// catalog name regardless of the caller's casing/whitespace, and a direct
	// library caller cannot slip an unknown name into a report the way the CLI
	// (which validates at parse time) cannot.
	benchProfile := strings.TrimSpace(cfg.ExecProfile)
	if benchProfile != "" {
		profile, ok := execprofile.Lookup(benchProfile)
		if !ok {
			return TurnBenchResult{}, fmt.Errorf("unknown execution profile %q (valid: %s)", cfg.ExecProfile, strings.Join(execprofile.Names(), ", "))
		}
		benchProfile = profile.Name
	}
	iterations := cfg.Iterations
	if iterations < 1 {
		iterations = 1
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	rc := RunContext{Model: cfg.Model, Mode: cfg.Mode, SelfCorrect: cfg.SelfCorrect, ExecProfile: benchProfile}

	perSpanSamples := map[string][]float64{}
	classWalls := map[string][]float64{}
	classSpanTotals := map[string]map[string]float64{}
	classTasks := map[string]int{}
	classVerified := map[string]int{}
	classPassed := map[string]int{}
	classLatencyOnly := map[string]int{}
	correctnessClasses := map[string]bool{}
	buildOnlyClasses := map[string]bool{}
	latencyOnlyClasses := map[string]bool{}
	var totals TurnBenchTotals

	// A class is build-only when the manifest declares it in BuildOnlyClasses.
	// A task's tier is then decided per-task: no verificationCommand => latency-
	// only; otherwise build-only if its class is declared, else correctness. The
	// latency-only tier is always driven by oracle presence, never by the
	// declared list, so a declared build-only class with a missing oracle still
	// counts as latency-only rather than silently passing on exit 0.
	buildOnly := map[string]bool{}
	for _, c := range set.BuildOnlyClasses {
		buildOnly[strings.TrimSpace(c)] = true
	}

	result := TurnBenchResult{
		SchemaVersion: TurnSchemaVersion,
		Suite:         strings.TrimSpace(set.ID),
		Model:         strings.TrimSpace(cfg.Model),
		Mode:          strings.TrimSpace(cfg.Mode),
		ExecProfile:   benchProfile,
		SelfCorrect:   cfg.SelfCorrect,
		Version:       strings.TrimSpace(cfg.Version),
		Commit:        strings.TrimSpace(cfg.Commit),
		Date:          now().UTC().Format(time.RFC3339),
		Iterations:    iterations,
		PerSpan:       map[string]SpanStats{},
		PerClass:      map[string]ClassSummary{},
	}

	for _, task := range set.Tasks {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		class := strings.TrimSpace(task.Class)
		if class == "" {
			class = "default"
		}
		classTasks[class]++
		// A task counts as passed only when every iteration passed. The per-process
		// runner is cold-start, so a flaky pass on one iteration and a fail on
		// another is a real regression signal, not noise to average away.
		passedForTask := true
		erroredIterations := 0
		for iter := 0; iter < iterations; iter++ {
			outcome := cfg.Runner(ctx, task, rc)
			if outcome.TraceIssue != "" {
				result.Warnings = append(result.Warnings, Warning{
					Metric:  "trace",
					Message: fmt.Sprintf("task %s: %s", task.ID, outcome.TraceIssue),
				})
			}
			if outcome.Err != nil {
				// A crashed run must not look like a normal sample that's merely
				// absent — surface it as a warning so a run that died every
				// iteration can't pass as "fewer measurements."
				result.Warnings = append(result.Warnings, Warning{
					Metric:  "run",
					Message: fmt.Sprintf("task %s: %v", task.ID, outcome.Err),
				})
				passedForTask = false
				erroredIterations++
				continue
			}
			if !outcome.Passed {
				passedForTask = false
			}
			wall := outcome.WallMs
			if wall <= 0 && outcome.Trace != nil {
				wall = float64(outcome.Trace.WallDuration().Microseconds()) / 1000
			}
			if wall > 0 {
				classWalls[class] = append(classWalls[class], wall)
			}
			if outcome.Trace != nil {
				aggregateTotals(&totals, outcome.Trace)
				for _, span := range outcome.Trace.Spans {
					// Rank by exclusive time (duration minus nested children) so
					// concurrent/nested spans do not double-count: provider_connect
					// inside generation and permission_wait inside tool_execution
					// each contribute their own exclusive time, not their parent's.
					// A span whose exclusive is legitimately zero (a parent fully
					// covered by its children) contributes zero on purpose — do
					// NOT fall back to Duration, which would re-introduce the
					// double-count. ReadNDJSON preserves a written exclusive_ms:0
					// as 0 and only falls back to Duration when the key is absent,
					// so span.Exclusive is always populated here.
					ms := float64(span.Exclusive.Microseconds()) / 1000
					perSpanSamples[span.Name] = append(perSpanSamples[span.Name], ms)
					if classSpanTotals[class] == nil {
						classSpanTotals[class] = map[string]float64{}
					}
					classSpanTotals[class][span.Name] += ms
				}
			}
		}
		result.TasksAttempted++

		// Classify the task into an oracle tier and update only that tier's
		// counters. A latency-only task (no verificationCommand) is never counted
		// in any pass rate even when the runner reports Passed — an exit-0
		// read-only run proves the turn ran, not that the answer was right.
		if erroredIterations == iterations {
			// Every iteration produced no accepted sample (spawn failure or a
			// harness error after the run). The task still counts in its tier
			// below — an oracle task as a failure, a latency-only task in no
			// pass rate as always — but the errored state is first-class so a
			// broken run can never print a clean-looking summary.
			result.TasksErrored++
		}
		hasOracle := len(task.VerificationCommand) > 0
		switch {
		case !hasOracle:
			result.LatencyOnlyTasks++
			classLatencyOnly[class]++
			latencyOnlyClasses[class] = true
		case buildOnly[class]:
			result.BuildCheckedTasks++
			classVerified[class]++
			buildOnlyClasses[class] = true
			if passedForTask {
				result.BuildPassedTasks++
				classPassed[class]++
			}
		default:
			result.TasksVerified++
			classVerified[class]++
			correctnessClasses[class] = true
			if passedForTask {
				result.TasksPassed++
				classPassed[class]++
			}
		}
	}

	for name, samples := range perSpanSamples {
		result.PerSpan[name] = summarizeSpan(samples)
	}
	result.TopLatency = topLatencySources(result.PerSpan, 3)
	for class := range classTasks {
		walls := classWalls[class]
		var wallStats NumericStats
		if len(walls) > 0 {
			wallStats = SummarizeSamples(walls)
		}
		result.PerClass[class] = ClassSummary{
			Tasks:       classTasks[class],
			Verified:    classVerified[class],
			Passed:      classPassed[class],
			LatencyOnly: classLatencyOnly[class],
			WallMs:      wallStats,
			SpanTotals:  classSpanTotals[class],
		}
	}
	result.CorrectnessClasses = sortedSetKeys(correctnessClasses)
	result.BuildOnlyClasses = sortedSetKeys(buildOnlyClasses)
	result.LatencyOnlyClasses = sortedSetKeys(latencyOnlyClasses)
	result.CorrectnessPassRate = passRate(result.TasksPassed, result.TasksVerified)
	result.BuildPassRate = passRate(result.BuildPassedTasks, result.BuildCheckedTasks)
	result.Totals = totals
	return result, nil
}

// passRate is passed/total rounded to the benchmark's metric precision, or 0
// when the denominator is zero (no tasks in that tier were run).
func passRate(passed, total int) float64 {
	if total <= 0 {
		return 0
	}
	return RoundMetric(float64(passed) / float64(total))
}

func sortedSetKeys(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func summarizeSpan(samples []float64) SpanStats {
	if len(samples) == 0 {
		return SpanStats{}
	}
	stats := SummarizeSamples(samples)
	total := 0.0
	for _, s := range samples {
		total += s
	}
	return SpanStats{
		Count:    len(samples),
		TotalMs:  RoundMetric(total),
		MedianMs: stats.Median,
		P95Ms:    stats.P95,
		MaxMs:    stats.Max,
	}
}

func topLatencySources(perSpan map[string]SpanStats, top int) []LatencySource {
	sources := make([]LatencySource, 0, len(perSpan))
	totalAttributed := 0.0
	for _, s := range perSpan {
		totalAttributed += s.TotalMs
	}
	for name, s := range perSpan {
		share := 0.0
		if totalAttributed > 0 {
			share = s.TotalMs / totalAttributed
		}
		sources = append(sources, LatencySource{
			Span:    name,
			TotalMs: s.TotalMs,
			Share:   RoundMetric(share),
		})
	}
	sort.SliceStable(sources, func(i, j int) bool {
		if sources[i].TotalMs != sources[j].TotalMs {
			return sources[i].TotalMs > sources[j].TotalMs
		}
		return sources[i].Span < sources[j].Span
	})
	if top > 0 && len(sources) > top {
		sources = sources[:top]
	}
	return sources
}

func aggregateTotals(totals *TurnBenchTotals, tr *trace.TurnTrace) {
	totals.InputTokens += tr.Counter(trace.CounterInputTokens)
	totals.CachedInputTokens += tr.Counter(trace.CounterCachedInputTokens)
	totals.CacheWriteTokens += tr.Counter(trace.CounterCacheWriteTokens)
	totals.OutputTokens += tr.Counter(trace.CounterOutputTokens)
	totals.ModelRequests += tr.Counter(trace.CounterModelRequests)
	totals.ToolCalls += tr.Counter(trace.CounterToolCalls)
	totals.Retries += tr.Counter(trace.CounterRetryCount)
	totals.Reconnects += tr.Counter(trace.CounterReconnectCount)
	totals.Compactions += tr.Counter(trace.CounterCompactionCount)
}

// FormatTurnBenchSummary renders a human-readable turn-benchmark summary that
// names the top controllable latency sources — the baseline's "do not proceed
// until" criterion.
func FormatTurnBenchSummary(result TurnBenchResult) string {
	lines := []string{
		"Zero turn benchmark: " + displayOrUnknown(result.Suite),
		"model: " + displayOrUnknown(result.Model),
		// The headline separates the three oracle tiers so an exit-0 read-only
		// task can never inflate a "pass rate" that reads as correctness:
		// correctness (positive oracle: edit/fix/nav/refactor), build (empty in
		// v3 — refactor graduated to correctness), and latency-only (no oracle:
		// longproc/longctx/parallel).
		fmt.Sprintf("tasks: %d total | correctness %d/%d (%.0f%%) | build %d/%d (%.0f%%) | latency-only %d | %d iter",
			result.TasksAttempted,
			result.TasksPassed, result.TasksVerified, result.CorrectnessPassRate*100,
			result.BuildPassedTasks, result.BuildCheckedTasks, result.BuildPassRate*100,
			result.LatencyOnlyTasks, result.Iterations),
	}
	if result.TasksErrored > 0 {
		lines = append(lines, fmt.Sprintf("ERRORED: %d task(s) produced no accepted benchmark sample (spawn/crash or harness error) — errored oracle tasks count as failures in their tier's pass rate; latency-only tasks stay out of pass rates as always", result.TasksErrored))
		shown := 0
		for _, warning := range result.Warnings {
			if warning.Metric != "run" {
				continue
			}
			lines = append(lines, "  "+warning.Message)
			shown++
			if shown == 3 {
				break
			}
		}
	}
	if result.Mode != "" {
		lines = append(lines, "mode: "+result.Mode)
	}
	if result.ExecProfile != "" {
		lines = append(lines, "exec-profile: "+result.ExecProfile)
	}
	if len(result.TopLatency) > 0 {
		lines = append(lines, "top latency sources:")
		for _, src := range result.TopLatency {
			lines = append(lines, fmt.Sprintf("  %-18s %10s  %5.1f%%", src.Span, FormatMetric(src.TotalMs, "ms"), src.Share*100))
		}
	}
	lines = append(lines, fmt.Sprintf("totals: in=%d (cache-read %d, cache-write %d, uncached %d) out=%d | requests=%d tools=%d retries=%d reconnects=%d compactions=%d",
		result.Totals.InputTokens, result.Totals.CachedInputTokens, result.Totals.CacheWriteTokens,
		uncachedInputTokens(result.Totals), result.Totals.OutputTokens,
		result.Totals.ModelRequests, result.Totals.ToolCalls, result.Totals.Retries,
		result.Totals.Reconnects, result.Totals.Compactions))
	for _, class := range sortedClasses(result.PerClass) {
		summary := result.PerClass[class]
		median := FormatMetric(summary.WallMs.Median, "ms")
		tier := classTier(result, class)
		lines = append(lines, fmt.Sprintf("  [%s/%s] %d/%d passed, %d latency-only, wall median %s",
			class, tier, summary.Passed, summary.Verified, summary.LatencyOnly, median))
	}
	return strings.Join(lines, "\n")
}

func uncachedInputTokens(totals TurnBenchTotals) int64 {
	remaining := totals.InputTokens
	if remaining <= 0 {
		return 0
	}
	for _, cached := range []int64{totals.CachedInputTokens, totals.CacheWriteTokens} {
		if cached <= 0 {
			continue
		}
		if cached >= remaining {
			return 0
		}
		remaining -= cached
	}
	return remaining
}

// classTier returns the oracle tier label a class was classified into, so the
// per-class roll-up states explicitly which oracle (if any) its "passed" count
// is measured against.
func classTier(result TurnBenchResult, class string) string {
	for _, c := range result.BuildOnlyClasses {
		if c == class {
			return "build"
		}
	}
	for _, c := range result.LatencyOnlyClasses {
		if c == class {
			return "latency"
		}
	}
	return "correctness"
}

func sortedClasses(perClass map[string]ClassSummary) []string {
	classes := make([]string, 0, len(perClass))
	for c := range perClass {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	return classes
}

// WriteTurnBenchJSON writes the indented JSON form of a turn-benchmark result.
func WriteTurnBenchJSON(w io.Writer, result TurnBenchResult) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

// execExitIncomplete mirrors internal/cli.exitIncomplete (the `zero exec` exit
// code, 4) for a headless run the completion gate marked INCOMPLETE: the run
// stopped without a completion signal (e.g. it couldn't self-verify because its
// sandboxed shell was unavailable under --auto member), which is distinct from a
// crash (1), usage error (2), provider failure (3), or interruption (130). It is
// the ONLY nonzero exit an oracle-bearing task may defer to the oracle for; every
// other nonzero exit stays authoritative. Kept as a local literal so perfbench
// doesn't import the cli package; must stay in sync with that constant.
const execExitIncomplete = 4

// NewTurnExecRunner builds the production turn-benchmark runner: it invokes
// headless `zero exec` with stream-json output AND `--trace <tmpfile>`, then
// parses the emitted NDJSON trace into a *trace.TurnTrace. binary is the path to
// the `zero` binary; extraArgs are appended to every invocation. Pass/fail is
// decided by the task's oracle when it has one. The oracle that gates the verdict
// is the VerificationCommand (which compiles and runs the stamped OracleTest via
// `go test`, plus any greps against the fixture); it is ground truth, so an
// oracle-bearing task that applied the right edit but exited INCOMPLETE (exit 4,
// its sandboxed self-verification couldn't run) still passes. Any OTHER nonzero
// exit (crash/usage/provider/interrupt) stays authoritative and fails even an
// oracle-bearing task. A latency-only task has no VerificationCommand, so any
// nonzero exit is its only signal and fails it.
func NewTurnExecRunner(binary string, extraArgs ...string) TurnRunner {
	return func(ctx context.Context, task BenchTask, rc RunContext) TurnTaskOutcome {
		// buildTurnExecArgs grants the write + sandboxed-shell tool set to EVERY
		// invocation, so a task MUST run inside an isolated fixture copy — never the
		// caller's cwd. The grant is unconditional, so the isolation can't be
		// optional: a fixtureless task would turn an agent with write/shell tools
		// loose in the caller's working directory (their real repo). Enforce the
		// invariant instead of trusting it — reject a fixtureless task up front,
		// before anything is launched, so the grant and the guard can't drift apart.
		// Every shipped task declares a fixture, so this only fires on a malformed or
		// newly added task, and it fails loudly rather than silently.
		if strings.TrimSpace(task.WorkspaceFixture) == "" {
			return TurnTaskOutcome{Err: errors.New("benchmark task has no workspaceFixture: the run grants write/shell tools and must execute in an isolated fixture copy, not the caller's cwd")}
		}
		// Isolate the workspace: copy the fixture into a fresh temp dir so a
		// mutating task (edit/fix/refactor) can't dirty the shared, checked-in
		// fixture or bleed into a later iteration of the same task. The guard above
		// guarantees WorkspaceFixture is set, so this always runs.
		copyDir, parent, cerr := copyFixture(strings.TrimSpace(task.WorkspaceFixture))
		if cerr != nil {
			return TurnTaskOutcome{Err: fmt.Errorf("isolate fixture: %w", cerr)}
		}
		// Clean the whole unique parent (which owns copyDir) so the per-invocation
		// scratch dir never leaks.
		defer os.RemoveAll(parent)
		task.WorkspaceFixture = copyDir

		traceFile, err := os.CreateTemp("", "zero-turn-trace-*.ndjson")
		if err != nil {
			return TurnTaskOutcome{Err: fmt.Errorf("create trace file: %w", err)}
		}
		_ = traceFile.Close()
		tracePath := traceFile.Name()
		defer os.Remove(tracePath)

		args := buildTurnExecArgs(task, rc, tracePath, extraArgs)
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = appendNoColor(os.Environ())
		if dir := strings.TrimSpace(task.WorkspaceFixture); dir != "" {
			cmd.Dir = dir
		}
		var outBuf, errBuf bytes.Buffer
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		start := time.Now()
		runErr := cmd.Run()
		wallMs := float64(time.Since(start).Microseconds()) / 1000

		exitCode, haveExit := streamJSONExitCode(outBuf.Bytes())
		outcome := TurnTaskOutcome{WallMs: wallMs}
		if haveExit && exitCode != 0 {
			outcome.VerifyErr = fmt.Sprintf("agent run_end exit code %d", exitCode)
		} else if !haveExit {
			detail := strings.TrimSpace(errBuf.String())
			// If the process never produced a run_end event, the actual failure
			// is in runErr (binary missing, exec permission denied, etc.) —
			// prefer it over the generic fallback so the real reason survives.
			if detail == "" && runErr != nil {
				detail = runErr.Error()
			}
			if detail == "" {
				detail = "missing terminal run_end event"
			}
			outcome.Err = fmt.Errorf("zero exec failed: %s", detail)
			return outcome
		}

		// Parse the captured trace. The trace is the attribution layer, so a
		// missing/malformed file is not fatal — but it IS surfaced as a warning
		// (via outcome.TraceIssue) so an incomplete measurement can't look valid.
		if f, ferr := os.Open(tracePath); ferr != nil {
			outcome.TraceIssue = fmt.Sprintf("trace open failed: %v", ferr)
		} else {
			tr, perr := trace.ReadNDJSON(f)
			_ = f.Close()
			if perr != nil {
				outcome.TraceIssue = fmt.Sprintf("trace parse failed: %v", perr)
			} else {
				outcome.Trace = tr
			}
		}

		// Decide what a nonzero exit means. We defer to the oracle for exactly ONE
		// case: an oracle-bearing task that exited INCOMPLETE (exit 4). That is the
		// completion gate downgrading a run whose edits may be correct but that
		// couldn't emit a completion signal (e.g. its sandboxed self-verify couldn't
		// run under --auto member) — the oracle below (the compiled test / grep run
		// against the actual fixture files) is ground truth, so if the edit really
		// landed it passes, and if the run abandoned the task the oracle fails it.
		// Every OTHER nonzero exit stays authoritative: a crash (1), usage error
		// (2), provider failure (3), or interruption (130) is a genuine failure, so
		// a partial edit that happens to satisfy the oracle can't launder it into a
		// pass. A latency-only task has no oracle to defer to, so any nonzero exit
		// fails it.
		if outcome.VerifyErr != "" {
			if len(task.VerificationCommand) == 0 || exitCode != execExitIncomplete {
				return outcome
			}
			outcome.VerifyErr = ""
		}
		// Stamp the compiler-backed oracle and capture the agent's final answer
		// BEFORE running the verification command. Both write into the fixture
		// copy (task.WorkspaceFixture, set to copyDir above) so runVerification —
		// which uses that same dir as cmd.Dir — sees them. Stamping happens only
		// after the agent run so the oracle test can't interfere with the agent's
		// own go build/test during the task (e.g. refactor-03's package-zeroapp
		// test would break a pre-rename build) and can't be pre-seen or tampered
		// with.
		if err := stampOracleAndAnswer(task, outBuf.Bytes()); err != nil {
			// Preserve the already-parsed trace, trace issue, and wall time: a
			// stamp failure is a harness error, but the run still produced a
			// trace worth attributing, so mutate the existing outcome rather than
			// returning a fresh one that drops Trace/TraceIssue.
			outcome.Err = fmt.Errorf("stamp oracle: %w", err)
			return outcome
		}
		if len(task.VerificationCommand) > 0 {
			if vOutcome := runVerification(ctx, task); !vOutcome.Passed {
				outcome.VerifyErr = strings.TrimSpace(vOutcome.Detail)
				return outcome
			}
			// A positive oracle (grep/test/build) passed: this is the only path
			// that sets Passed. A task with no verificationCommand is latency-only
			// (read-only longproc/longctx/parallel): its exit 0 proves the turn
			// ran, not that the answer was right, so it never reports Passed and the
			// harness counts it in latencyOnlyTasks rather than any pass rate.
			outcome.Passed = true
		}
		return outcome
	}
}

// copyFixture copies the fixture directory at src into a fresh temp dir and
// returns the copy's path (dst) plus the unique parent dir that owns it (parent,
// which the caller must RemoveAll to clean up dst and its sibling scratch space
// together). Used to give each benchmark invocation an isolated workspace so
// mutating tasks (edit/fix/refactor) can't dirty the checked-in fixture or a
// later iteration.
//
// The copy is placed two levels BELOW the system temp root: a unique 0700
// parent (created fresh per invocation via os.MkdirTemp, so concurrent runs and
// different users can't share or traverse a predictable scratch dir) and the
// fixture copy beneath it. Go ignores go.mod files in DIRECT children of the
// system temp dir (a hijack guard), which would make the fixture's go.mod —
// and thus every `go test ./...`/`go build ./...` oracle — fail with "cannot
// find main module". Keeping the copy a grandchild of the temp root makes Go
// respect the copied go.mod so the compiler-backed oracles actually run.
func copyFixture(src string) (dst, parent string, err error) {
	info, err := os.Stat(src)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("fixture %q is not a directory", src)
	}
	// Unique, 0700-per-invocation parent directly under the temp root. The
	// fixture copy is created BENEATH it, so the copy is a grandchild of the
	// temp root and its go.mod is respected (see the doc comment above).
	parent, err = os.MkdirTemp(os.TempDir(), "zero-turn-bench-*")
	if err != nil {
		return "", "", err
	}
	dst, err = os.MkdirTemp(parent, "zero-turn-fixture-*")
	if err != nil {
		os.RemoveAll(parent)
		return "", "", err
	}
	werr := filepath.WalkDir(src, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(target, data, 0o644)
	})
	if werr != nil {
		os.RemoveAll(parent) // removes dst too
		return "", "", werr
	}
	return dst, parent, nil
}

// stampOracleAndAnswer writes the compiler-backed oracle test (when configured)
// and the agent's captured final answer into the fixture copy, so the
// verification command can compile the oracle and grep the answer. Both files
// land in task.WorkspaceFixture, which the runner has already set to the
// fixture copy and which runVerification uses as cmd.Dir. The answer file is
// always written (empty when no final event was captured) so a nav answer-oracle
// fails cleanly on a run that produced no answer rather than reading a stale or
// absent file.
//
// A task that needs an oracle — a stamped oracle_test.go OR a verification
// command that reads .zero-answer.txt — but has no workspace fixture is
// rejected: there is nowhere safe to stamp, so runVerification would otherwise
// run in the caller's cwd with the oracle never compiled (or the answer never
// captured), which can read as a false pass. A fixtureless task with no oracle
// and no verification (a pure latency-only run in the caller's cwd) is left
// alone.
func stampOracleAndAnswer(task BenchTask, outBuf []byte) error {
	dir := strings.TrimSpace(task.WorkspaceFixture)
	if dir == "" {
		if task.OracleTest != "" || len(task.VerificationCommand) > 0 {
			return fmt.Errorf("stamp oracle: task %q has an oracle or verification but no workspace fixture to stamp into", task.ID)
		}
		return nil
	}
	if ot := task.OracleTest; ot != "" {
		if err := os.WriteFile(filepath.Join(dir, "oracle_test.go"), []byte(ot), 0o644); err != nil {
			return fmt.Errorf("write oracle_test.go: %w", err)
		}
	}
	answer := streamJSONFinalText(outBuf)
	if err := os.WriteFile(filepath.Join(dir, ".zero-answer.txt"), []byte(answer), 0o644); err != nil {
		return fmt.Errorf("write .zero-answer.txt: %w", err)
	}
	return nil
}

func buildTurnExecArgs(task BenchTask, rc RunContext, tracePath string, extraArgs []string) []string {
	// --auto member is REQUIRED, not optional: without a permission grant `zero
	// exec` runs in its default read-only posture, which exposes no write or shell
	// tools (no edit_file/apply_patch/write_file/exec_command/bash). The mutating
	// task classes (edit/fix/refactor) then cannot apply any change, so every run
	// grinds to the turn ceiling or reports a no-tool blocker, and the only tasks
	// that "pass" are ones whose oracle grep matches the stamped answer text rather
	// than a real edit. member-auto grants the write + sandboxed-shell tools the
	// benchmark needs while keeping the workspace/network/destructive safeguards, so
	// it is preferred over the broader --skip-permissions-unsafe (which also drops
	// those guards and auto-retries blocked commands unsandboxed). Note the shell it
	// grants is sandboxed: on a host without sandbox setup the agent's own
	// self-verification (go test/build) can't run and the turn exits INCOMPLETE even
	// after a correct edit — the runner treats the stamped oracle, not that exit
	// code, as ground truth for oracle-bearing tasks (see NewTurnExecRunner), so a
	// correct edit still passes. Each task also runs in an isolated, throwaway
	// fixture copy, so the granted tools have nothing outside the task to harm.
	args := []string{"exec", "--auto", "member", "--output-format", "stream-json", "--trace", tracePath}
	if model := strings.TrimSpace(rc.Model); model != "" {
		args = append(args, "--model", model)
	}
	if mode := strings.TrimSpace(rc.Mode); mode != "" {
		args = append(args, "--mode", mode)
	}
	if rc.SelfCorrect {
		args = append(args, "--self-correct")
	}
	if profile := strings.TrimSpace(rc.ExecProfile); profile != "" {
		args = append(args, "--exec-profile", profile)
	}
	args = append(args, extraArgs...)
	args = append(args, task.Prompt)
	return args
}

// ResolveBinary locates the zero binary for a benchmark run: an explicit path
// when provided, else a `zero` (or zero.exe) on PATH, else a binary built into
// the repo root. Returns an error when none is found.
func ResolveBinary(explicit string) (string, error) {
	if v := strings.TrimSpace(explicit); v != "" {
		if _, err := os.Stat(v); err != nil {
			return "", fmt.Errorf("trace binary not found: %w", err)
		}
		// Pin the explicit path to an absolute one: the turn runner sets each
		// child's cmd.Dir to a per-task fixture copy, so a relative path
		// (./zero, the Makefile default) that stats fine here would fail to
		// spawn from inside every fixture dir, silently erroring all tasks.
		absolute, err := filepath.Abs(v)
		if err != nil {
			return "", fmt.Errorf("resolve trace binary path: %w", err)
		}
		return absolute, nil
	}
	if path, err := exec.LookPath("zero"); err == nil {
		return path, nil
	}
	if path, err := exec.LookPath("zero.exe"); err == nil {
		return path, nil
	}
	cwd, err := os.Getwd()
	if err == nil {
		for _, name := range []string{"zero", "zero.exe"} {
			candidate := filepath.Join(cwd, name)
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
	}
	return "", errors.New("zero binary not found; build it first or pass an explicit path")
}

package perfbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/trace"
)

// fakeTurnRunner returns a canned *trace.TurnTrace per task so the harness's
// aggregation logic can be exercised without a model or a binary. Each task
// yields one generation span (the dominant cost), one tool_execution span, and
// the counters the totals aggregate.
func fakeTurnRunner(canned map[string]*trace.TurnTrace) TurnRunner {
	return func(ctx context.Context, task BenchTask, rc RunContext) TurnTaskOutcome {
		tr, ok := canned[task.ID]
		if !ok {
			return TurnTaskOutcome{Err: errNoCanned}
		}
		wallMs := float64(tr.WallDuration().Microseconds()) / 1000
		return TurnTaskOutcome{Passed: true, WallMs: wallMs, Trace: tr}
	}
}

var errNoCanned = &cannedError{}

type cannedError struct{}

func (*cannedError) Error() string { return "no canned trace for task" }

// cannedTrace builds a deterministic *trace.TurnTrace with the given spans and
// counters. Spans are recorded as fixed durations (no real timing) so the
// aggregation math is predictable in assertions.
func cannedTrace(genMs, toolMs int, tokens int64) *trace.TurnTrace {
	r := trace.NewRecorder("sess", "run-1", "test")
	r.Start()
	r.RecordSpan(trace.SpanGeneration, time.Duration(genMs)*time.Millisecond)
	r.RecordSpan(trace.SpanToolExecution, time.Duration(toolMs)*time.Millisecond)
	r.Counter(trace.CounterInputTokens, tokens)
	r.Counter(trace.CounterOutputTokens, tokens/2)
	r.Counter(trace.CounterModelRequests, 1)
	r.Counter(trace.CounterToolCalls, 1)
	r.StampFirstToken()
	return r.Finish()
}

func TestRunTurnBenchAggregation(t *testing.T) {
	// v3 tier model: nav and refactor both carry a verificationCommand and are
	// NOT in buildOnlyClasses, so they count as correctness; longproc has no
	// oracle and is latency-only. The build tier is empty (buildOnlyClasses is
	// nil), matching the v3 baseline manifest where refactor graduated from
	// build to correctness.
	set := TaskSet{
		ID: "fake-suite",
		Tasks: []BenchTask{
			{ID: "t1", Class: "nav", Prompt: "p1", VerificationCommand: []string{"true"}},      // correctness
			{ID: "t2", Class: "edit", Prompt: "p2", VerificationCommand: []string{"true"}},     // correctness
			{ID: "t3", Class: "refactor", Prompt: "p3", VerificationCommand: []string{"true"}}, // correctness
			{ID: "t4", Class: "longproc", Prompt: "p4"},                                        // latency-only
		},
		BuildOnlyClasses: nil,
	}
	canned := map[string]*trace.TurnTrace{
		"t1": cannedTrace(100, 10, 1000),
		"t2": cannedTrace(200, 50, 2000),
		"t3": cannedTrace(150, 20, 1500),
		"t4": cannedTrace(300, 10, 1000),
	}
	canned["t1"].Counters = append(canned["t1"].Counters, trace.Counter{Name: trace.CounterCacheWriteTokens, Value: 125})
	cfg := TurnBenchConfig{
		Model:      "fake-model",
		Iterations: 1,
		Runner:     fakeTurnRunner(canned),
		Now:        func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	}
	result, err := RunTurnBench(context.Background(), set, cfg)
	if err != nil {
		t.Fatalf("RunTurnBench: %v", err)
	}
	// 4 tasks attempted; the v3 tier split is 3 correctness (nav, edit, refactor),
	// 1 latency-only (longproc), and 0 build (the build tier is empty).
	if result.TasksAttempted != 4 {
		t.Fatalf("attempted=%d, want 4", result.TasksAttempted)
	}
	if result.TasksVerified != 3 || result.TasksPassed != 3 {
		t.Fatalf("correctness verified=%d passed=%d, want 3/3", result.TasksVerified, result.TasksPassed)
	}
	if result.LatencyOnlyTasks != 1 {
		t.Fatalf("latencyOnly=%d, want 1", result.LatencyOnlyTasks)
	}
	if result.BuildCheckedTasks != 0 || result.BuildPassedTasks != 0 {
		t.Fatalf("build checked=%d passed=%d, want 0/0 (empty build tier)", result.BuildCheckedTasks, result.BuildPassedTasks)
	}
	if result.CorrectnessPassRate != 1.0 {
		t.Fatalf("correctnessPassRate=%v, want 1.0", result.CorrectnessPassRate)
	}
	// passRate returns 0 for a 0/0 denominator, so an empty build tier reports 0
	// (not NaN) and cannot be misread as a perfect build score.
	if result.BuildPassRate != 0 {
		t.Fatalf("buildPassRate=%v, want 0 (empty build tier is 0/0 -> 0)", result.BuildPassRate)
	}
	if len(result.CorrectnessClasses) != 3 || result.CorrectnessClasses[0] != "edit" || result.CorrectnessClasses[1] != "nav" || result.CorrectnessClasses[2] != "refactor" {
		t.Fatalf("correctnessClasses=%v, want [edit nav refactor]", result.CorrectnessClasses)
	}
	if len(result.BuildOnlyClasses) != 0 {
		t.Fatalf("buildOnlyClasses=%v, want empty (v3: build tier empty)", result.BuildOnlyClasses)
	}
	if len(result.LatencyOnlyClasses) != 1 || result.LatencyOnlyClasses[0] != "longproc" {
		t.Fatalf("latencyOnlyClasses=%v, want [longproc]", result.LatencyOnlyClasses)
	}
	if result.SchemaVersion != TurnSchemaVersion {
		t.Fatalf("schemaVersion = %d, want %d", result.SchemaVersion, TurnSchemaVersion)
	}
	if result.Date != "2026-01-02T03:04:05Z" {
		t.Fatalf("date = %q", result.Date)
	}

	// Per-span: generation appears in all four (100+200+150+300=750ms), tool in
	// all four (10+50+20+10=90ms). Count must equal the number of tasks * iterations.
	gen := result.PerSpan[trace.SpanGeneration]
	if gen.Count != 4 {
		t.Fatalf("generation count = %d, want 4", gen.Count)
	}
	if gen.TotalMs != 750 {
		t.Fatalf("generation totalMs = %v, want 750", gen.TotalMs)
	}
	tool := result.PerSpan[trace.SpanToolExecution]
	if tool.TotalMs != 90 {
		t.Fatalf("tool totalMs = %v, want 90", tool.TotalMs)
	}

	// Top latency: generation (750) ranks above tool (90). Exactly two spans
	// here, so both appear and generation is first.
	if len(result.TopLatency) != 2 || result.TopLatency[0].Span != trace.SpanGeneration {
		t.Fatalf("topLatency = %+v", result.TopLatency)
	}
	if result.TopLatency[0].Share <= result.TopLatency[1].Share {
		t.Fatalf("top latency not ranked by share: %+v", result.TopLatency)
	}

	// Totals: 4 model requests, 4 tool calls, input tokens 1000+2000+1500+1000=5500.
	if result.Totals.ModelRequests != 4 {
		t.Fatalf("modelRequests = %d, want 4", result.Totals.ModelRequests)
	}
	if result.Totals.ToolCalls != 4 {
		t.Fatalf("toolCalls = %d, want 4", result.Totals.ToolCalls)
	}
	if result.Totals.InputTokens != 5500 {
		t.Fatalf("inputTokens = %d, want 5500", result.Totals.InputTokens)
	}
	if result.Totals.CacheWriteTokens != 125 {
		t.Fatalf("cacheWriteTokens = %d, want 125", result.Totals.CacheWriteTokens)
	}
	if result.Totals.OutputTokens != 2750 {
		t.Fatalf("outputTokens = %d, want 2750", result.Totals.OutputTokens)
	}

	// Per-class tier roll-up: nav/edit/refactor are correctness (1/1 verified
	// passed, 0 latency-only); longproc is latency-only (0 verified, 1 latency).
	nav := result.PerClass["nav"]
	if nav.Tasks != 1 || nav.Verified != 1 || nav.Passed != 1 || nav.LatencyOnly != 0 {
		t.Fatalf("nav class = %+v", nav)
	}
	edit := result.PerClass["edit"]
	if edit.Tasks != 1 || edit.Verified != 1 || edit.Passed != 1 || edit.LatencyOnly != 0 {
		t.Fatalf("edit class = %+v", edit)
	}
	refactor := result.PerClass["refactor"]
	if refactor.Tasks != 1 || refactor.Verified != 1 || refactor.Passed != 1 || refactor.LatencyOnly != 0 {
		t.Fatalf("refactor class = %+v", refactor)
	}
	longproc := result.PerClass["longproc"]
	if longproc.Tasks != 1 || longproc.Verified != 0 || longproc.Passed != 0 || longproc.LatencyOnly != 1 {
		t.Fatalf("longproc class = %+v", longproc)
	}
}

// TestRunTurnBenchLatencyOnlyNeverPassed asserts the honesty gate: a task with
// no verificationCommand reports Passed=true from the (stub) runner, yet the
// harness counts it ONLY in latencyOnlyTasks — never in tasksPassed or any pass
// rate — so an exit-0 read-only run cannot inflate a correctness number.
func TestRunTurnBenchLatencyOnlyNeverPassed(t *testing.T) {
	set := TaskSet{
		ID: "lo-suite",
		Tasks: []BenchTask{
			{ID: "n1", Class: "nav", Prompt: "p"}, // no oracle — runner still says Passed=true
		},
	}
	cfg := TurnBenchConfig{
		Model:  "fake-model",
		Runner: fakeTurnRunner(map[string]*trace.TurnTrace{"n1": cannedTrace(50, 5, 100)}),
		Now:    func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	}
	result, err := RunTurnBench(context.Background(), set, cfg)
	if err != nil {
		t.Fatalf("RunTurnBench: %v", err)
	}
	if result.TasksPassed != 0 || result.TasksVerified != 0 {
		t.Fatalf("latency-only leaked into pass: passed=%d verified=%d, want 0/0",
			result.TasksPassed, result.TasksVerified)
	}
	if result.CorrectnessPassRate != 0 || result.BuildPassRate != 0 {
		t.Fatalf("pass rates should be 0 with no oracle tasks: c=%v b=%v",
			result.CorrectnessPassRate, result.BuildPassRate)
	}
	if result.LatencyOnlyTasks != 1 {
		t.Fatalf("latencyOnlyTasks=%d, want 1", result.LatencyOnlyTasks)
	}
	if result.PerClass["nav"].Passed != 0 || result.PerClass["nav"].LatencyOnly != 1 {
		t.Fatalf("nav class = %+v, want passed=0 latencyOnly=1", result.PerClass["nav"])
	}
}

func TestRunTurnBenchIterationsAggregates(t *testing.T) {
	set := TaskSet{
		ID:    "iter-suite",
		Tasks: []BenchTask{{ID: "t1", Class: "nav", Prompt: "p1"}},
	}
	canned := map[string]*trace.TurnTrace{"t1": cannedTrace(100, 10, 500)}
	cfg := TurnBenchConfig{
		Model:      "fake-model",
		Iterations: 3,
		Runner:     fakeTurnRunner(canned),
		Now:        func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	}
	result, err := RunTurnBench(context.Background(), set, cfg)
	if err != nil {
		t.Fatalf("RunTurnBench: %v", err)
	}
	if result.PerSpan[trace.SpanGeneration].Count != 3 {
		t.Fatalf("generation count = %d, want 3 (one per iteration)", result.PerSpan[trace.SpanGeneration].Count)
	}
	if result.PerSpan[trace.SpanGeneration].TotalMs != 300 {
		t.Fatalf("generation totalMs = %v, want 300", result.PerSpan[trace.SpanGeneration].TotalMs)
	}
}

func TestRunTurnBenchRequiresModelAndRunner(t *testing.T) {
	set := TaskSet{ID: "s", Tasks: []BenchTask{{ID: "t", Prompt: "p"}}}
	if _, err := RunTurnBench(context.Background(), set, TurnBenchConfig{Runner: fakeTurnRunner(nil)}); err == nil {
		t.Fatal("expected error for missing model")
	}
	if _, err := RunTurnBench(context.Background(), set, TurnBenchConfig{Model: "m"}); err == nil {
		t.Fatal("expected error for missing runner")
	}
	if _, err := RunTurnBench(context.Background(), TaskSet{ID: "empty"}, TurnBenchConfig{Model: "m", Runner: fakeTurnRunner(nil)}); err == nil {
		t.Fatal("expected error for empty task set")
	}
}

func TestTopLatencySourcesRanksByTotalAndCapsTopN(t *testing.T) {
	perSpan := map[string]SpanStats{
		"a": {TotalMs: 100},
		"b": {TotalMs: 500},
		"c": {TotalMs: 300},
		"d": {TotalMs: 50},
	}
	top := topLatencySources(perSpan, 3)
	if len(top) != 3 {
		t.Fatalf("len = %d, want 3", len(top))
	}
	wantOrder := []string{"b", "c", "a"}
	for i, w := range wantOrder {
		if top[i].Span != w {
			t.Fatalf("top[%d] = %q, want %q", i, top[i].Span, w)
		}
	}
	// Shares sum to 1 across all four (100+500+300+50=950); the top-3 retain
	// their global share (not renormalized to the top-3).
	// Share is rounded to 2 decimals by RoundMetric (500/950 -> 0.53), so compare
	// against the rounded value with a small tolerance.
	if got, want := top[0].Share, RoundMetric(500.0/950.0); !approxEqual(got, want, 0.001) {
		t.Fatalf("top[0] share = %v, want %v", got, want)
	}
}

func TestWriteTurnBenchJSONRoundTrip(t *testing.T) {
	set := TaskSet{ID: "json-suite", Tasks: []BenchTask{
		{ID: "t1", Class: "edit", Prompt: "p1", VerificationCommand: []string{"true"}},
	}}
	canned := map[string]*trace.TurnTrace{"t1": cannedTrace(150, 20, 800)}
	result, err := RunTurnBench(context.Background(), set, TurnBenchConfig{
		Model:  "fake-model",
		Runner: fakeTurnRunner(canned),
		Now:    func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("RunTurnBench: %v", err)
	}
	var buf bytes.Buffer
	if err := WriteTurnBenchJSON(&buf, result); err != nil {
		t.Fatalf("WriteTurnBenchJSON: %v", err)
	}
	var decoded TurnBenchResult
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if decoded.SchemaVersion != TurnSchemaVersion {
		t.Fatalf("schemaVersion = %d, want %d", decoded.SchemaVersion, TurnSchemaVersion)
	}
	if decoded.TasksVerified != 1 || decoded.TasksPassed != 1 || decoded.LatencyOnlyTasks != 0 {
		t.Fatalf("decoded tier counts = verified=%d passed=%d latency=%d, want 1/1/0",
			decoded.TasksVerified, decoded.TasksPassed, decoded.LatencyOnlyTasks)
	}
	if decoded.CorrectnessPassRate != 1.0 {
		t.Fatalf("decoded correctnessPassRate = %v, want 1.0", decoded.CorrectnessPassRate)
	}
	if decoded.PerSpan[trace.SpanGeneration].TotalMs != 150 {
		t.Fatalf("decoded generation totalMs = %v, want 150", decoded.PerSpan[trace.SpanGeneration].TotalMs)
	}
}

func TestFormatTurnBenchSummaryNamesTopSources(t *testing.T) {
	set := TaskSet{ID: "fmt-suite", Tasks: []BenchTask{{ID: "t1", Class: "nav", Prompt: "p1"}}}
	canned := map[string]*trace.TurnTrace{"t1": cannedTrace(150, 20, 800)}
	result, err := RunTurnBench(context.Background(), set, TurnBenchConfig{
		Model:  "fake-model",
		Runner: fakeTurnRunner(canned),
		Now:    func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("RunTurnBench: %v", err)
	}
	summary := FormatTurnBenchSummary(result)
	if !strings.Contains(summary, "top latency sources") {
		t.Fatalf("summary missing top-latency header:\n%s", summary)
	}
	if !strings.Contains(summary, trace.SpanGeneration) {
		t.Fatalf("summary missing top span name %q:\n%s", trace.SpanGeneration, summary)
	}
}

func TestUncachedInputTokensClampsInconsistentTotals(t *testing.T) {
	tests := []struct {
		name   string
		totals TurnBenchTotals
		want   int64
	}{
		{name: "normal", totals: TurnBenchTotals{InputTokens: 100, CachedInputTokens: 30, CacheWriteTokens: 20}, want: 50},
		{name: "cached exceeds input", totals: TurnBenchTotals{InputTokens: 100, CachedInputTokens: 120}, want: 0},
		{name: "write exceeds remainder", totals: TurnBenchTotals{InputTokens: 100, CachedInputTokens: 20, CacheWriteTokens: 90}, want: 0},
		{name: "overflowing inconsistent counters", totals: TurnBenchTotals{CachedInputTokens: 1<<63 - 1, CacheWriteTokens: 1<<63 - 1}, want: 0},
		{name: "overflowing counters with input", totals: TurnBenchTotals{InputTokens: 100, CachedInputTokens: 1<<63 - 1, CacheWriteTokens: 1<<63 - 1}, want: 0},
		{name: "negative cache counters ignored", totals: TurnBenchTotals{InputTokens: 100, CachedInputTokens: -20, CacheWriteTokens: -10}, want: 100},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := uncachedInputTokens(test.totals); got != test.want {
				t.Fatalf("uncachedInputTokens(%+v) = %d, want %d", test.totals, got, test.want)
			}
		})
	}
	summary := FormatTurnBenchSummary(TurnBenchResult{Totals: TurnBenchTotals{
		CachedInputTokens: 1<<63 - 1,
		CacheWriteTokens:  1<<63 - 1,
	}})
	if !strings.Contains(summary, "uncached 0") {
		t.Fatalf("summary did not clamp inconsistent cache totals:\n%s", summary)
	}
}

func TestLoadBaselineManifest(t *testing.T) {
	path := filepath.Join("manifests", "baseline.json")
	set, err := LoadTaskSet(path)
	if err != nil {
		t.Fatalf("LoadTaskSet: %v", err)
	}
	if set.ID == "" {
		t.Fatal("manifest has no id")
	}
	// The baseline must clear the "do not proceed until ≥30 tasks" gate.
	if len(set.Tasks) < 30 {
		t.Fatalf("baseline has %d tasks, want >= 30", len(set.Tasks))
	}
	// The six required classes must all be present and non-empty.
	wantClasses := map[string]bool{
		"nav": false, "edit": false, "fix": false,
		"refactor": false, "longproc": false, "longctx": false, "parallel": false,
	}
	counts := map[string]int{}
	for _, task := range set.Tasks {
		class := strings.TrimSpace(task.Class)
		if class == "" {
			t.Fatalf("task %q has no class", task.ID)
		}
		if _, ok := wantClasses[class]; !ok {
			t.Fatalf("unexpected class %q on task %q", class, task.ID)
		}
		wantClasses[class] = true
		counts[class]++
	}
	for class, present := range wantClasses {
		if !present {
			t.Fatalf("manifest missing required class %q", class)
		}
		if counts[class] == 0 {
			t.Fatalf("class %q has zero tasks", class)
		}
	}
	// Every task must have a prompt and a workspace fixture pointing under testdata,
	// and that fixture must actually exist on disk — a manifest referencing a
	// missing fixture would make every task in its class error out at run time, so
	// catch it at load time instead.
	seen := map[string]bool{}
	for _, task := range set.Tasks {
		if strings.TrimSpace(task.Prompt) == "" {
			t.Fatalf("task %q has empty prompt", task.ID)
		}
		if strings.TrimSpace(task.WorkspaceFixture) == "" {
			t.Fatalf("task %q has no workspace fixture", task.ID)
		}
		if !strings.Contains(task.WorkspaceFixture, "testdata") {
			t.Fatalf("task %q fixture %q not under testdata", task.ID, task.WorkspaceFixture)
		}
		if seen[task.WorkspaceFixture] {
			continue
		}
		seen[task.WorkspaceFixture] = true
		info, err := os.Stat(task.WorkspaceFixture)
		if err != nil {
			t.Fatalf("task %q fixture %q does not exist: %v", task.ID, task.WorkspaceFixture, err)
		}
		if !info.IsDir() {
			t.Fatalf("task %q fixture %q is not a directory", task.ID, task.WorkspaceFixture)
		}
	}
}

func approxEqual(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < tol
}

// TestCopyFixtureIsolatesSourceFromMutation asserts the property that lets a
// mutating task run twice (or across iterations) without poisoning the next
// sample: the runner operates on a per-invocation copy, so mutating the copy
// leaves the checked-in source fixture byte-identical. This is verified by
// reading the code today; a test makes it durable against future refactors of
// the runner's isolation path.
func TestCopyFixtureIsolatesSourceFromMutation(t *testing.T) {
	src, err := os.MkdirTemp("", "zero-fixture-src-*")
	if err != nil {
		t.Fatalf("mkdtemp src: %v", err)
	}
	defer os.RemoveAll(src)
	orig := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(orig), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "a.go"), []byte("package sub\n"), 0o644); err != nil {
		t.Fatalf("write sub: %v", err)
	}

	// Snapshot the source tree before any mutation of the copy.
	wantMain, err := os.ReadFile(filepath.Join(src, "main.go"))
	if err != nil {
		t.Fatalf("read source main.go: %v", err)
	}

	copyDir, parent, err := copyFixture(src)
	if err != nil {
		t.Fatalf("copyFixture: %v", err)
	}
	defer os.RemoveAll(parent) // cleans copyDir too, since copyDir lives under parent

	// The copy must be a real copy, not a symlink/alias of the source, and
	// mutating it must not touch the source.
	if copyDir == src {
		t.Fatalf("copyFixture returned the source dir, not a copy: %s", copyDir)
	}
	// The copy must be a grandchild of the system temp root (parent is a direct
	// child of Temp, copyDir is a direct child of parent). Go ignores go.mod in
	// direct children of Temp (a hijack guard), so this nesting is what lets the
	// compiler-backed oracles actually run in the copy. Compare via filepath.Clean
	// because os.TempDir() may carry a trailing slash (macOS $TMPDIR ends in
	// "/T/") while filepath.Dir strips it; the raw != would false-fail on macOS
	// even though the parent is genuinely a direct child.
	tempRoot := filepath.Clean(os.TempDir())
	if filepath.Dir(parent) != tempRoot {
		t.Fatalf("parent %q is not a direct child of temp root %q", parent, tempRoot)
	}
	if filepath.Dir(copyDir) != parent {
		t.Fatalf("copyDir %q is not nested under parent %q", copyDir, parent)
	}
	if err := os.WriteFile(filepath.Join(copyDir, "main.go"), []byte("package main\n\n// mutated\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("mutate copy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(copyDir, "new.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("add file to copy: %v", err)
	}

	gotMain, err := os.ReadFile(filepath.Join(src, "main.go"))
	if err != nil {
		t.Fatalf("re-read source main.go: %v", err)
	}
	if string(gotMain) != string(wantMain) {
		t.Fatalf("source fixture mutated by copy: got %q, want %q", gotMain, wantMain)
	}
	if _, err := os.Stat(filepath.Join(src, "new.go")); !os.IsNotExist(err) {
		t.Fatalf("file added to copy appeared in source: %v", err)
	}
}

// TestRunTurnBenchBuildOnlyMechanism exercises the build-only tier code path in
// isolation. The v3 baseline manifest leaves this tier empty (refactor graduated
// to correctness), but the mechanism is still supported for future use, so a
// focused test keeps it covered: a class listed in buildOnlyClasses with a
// verificationCommand counts in buildCheckedTasks/buildPassedTasks — never in
// tasksVerified/correctnessPassRate — and a build-pass cannot leak into the
// correctness number.
func TestRunTurnBenchBuildOnlyMechanism(t *testing.T) {
	set := TaskSet{
		ID: "build-suite",
		Tasks: []BenchTask{
			{ID: "b1", Class: "buildcheck", Prompt: "p", VerificationCommand: []string{"true"}},
		},
		BuildOnlyClasses: []string{"buildcheck"},
	}
	canned := map[string]*trace.TurnTrace{"b1": cannedTrace(120, 30, 900)}
	result, err := RunTurnBench(context.Background(), set, TurnBenchConfig{
		Model:  "fake-model",
		Runner: fakeTurnRunner(canned),
		Now:    func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("RunTurnBench: %v", err)
	}
	if result.BuildCheckedTasks != 1 || result.BuildPassedTasks != 1 {
		t.Fatalf("build checked=%d passed=%d, want 1/1", result.BuildCheckedTasks, result.BuildPassedTasks)
	}
	if result.BuildPassRate != 1.0 {
		t.Fatalf("buildPassRate=%v, want 1.0", result.BuildPassRate)
	}
	// A build-only task must NOT count toward correctness.
	if result.TasksVerified != 0 || result.TasksPassed != 0 {
		t.Fatalf("build-only leaked into correctness: verified=%d passed=%d, want 0/0",
			result.TasksVerified, result.TasksPassed)
	}
	if result.CorrectnessPassRate != 0 {
		t.Fatalf("correctnessPassRate=%v, want 0 with only build-only tasks", result.CorrectnessPassRate)
	}
	if len(result.BuildOnlyClasses) != 1 || result.BuildOnlyClasses[0] != "buildcheck" {
		t.Fatalf("buildOnlyClasses=%v, want [buildcheck]", result.BuildOnlyClasses)
	}
	if len(result.CorrectnessClasses) != 0 {
		t.Fatalf("correctnessClasses=%v, want empty", result.CorrectnessClasses)
	}
	bc := result.PerClass["buildcheck"]
	if bc.Tasks != 1 || bc.Verified != 1 || bc.Passed != 1 || bc.LatencyOnly != 0 {
		t.Fatalf("buildcheck class = %+v, want 1/1 verified passed", bc)
	}
}

// TestTurnSchemaVersion pins the schema bump so a shape change is a conscious
// decision. v3 recorded the tier reclassification (refactor structural-positive
// and nav answer-oracles moved into correctnessPassRate). v4 adds tasksErrored:
// tasks whose every iteration died before the agent produced a run are now
// first-class in the report instead of visible only in warnings, so a
// spawn-broken run cannot print a clean-looking summary or exit 0. v5 is
// additive only: execProfile stamps the execution profile the run was
// benchmarked under so profile A/B reports are self-describing.
func TestTurnSchemaVersion(t *testing.T) {
	if TurnSchemaVersion != 5 {
		t.Fatalf("TurnSchemaVersion = %d, want 5", TurnSchemaVersion)
	}
}

// TestStreamJSONFinalTextExtractsAnswer, TestStreamJSONFinalTextEmptyWhenNoFinal,
// and TestStreamJSONFinalTextLastWins cover the pure capture helper that the
// nav answer-oracle depends on: it scans stream-json for the terminal "final"
// event and returns its text, "" when none was emitted, and the last when
// multiple appear (the success path emits exactly one, but the incomplete path
// can also emit one, so last-wins matches streamJSONExitCode's tie-break).
func TestStreamJSONFinalTextExtractsAnswer(t *testing.T) {
	out := []byte(`{"type":"text","text":"thinking..."}
{"type":"final","text":"the keys are port, name, and retries"}
{"type":"run_end","exitCode":0}
`)
	if got := streamJSONFinalText(out); got != "the keys are port, name, and retries" {
		t.Fatalf("streamJSONFinalText = %q, want the final text", got)
	}
}

func TestStreamJSONFinalTextEmptyWhenNoFinal(t *testing.T) {
	out := []byte(`{"type":"text","text":"hi"}
{"type":"run_end","exitCode":0}
`)
	if got := streamJSONFinalText(out); got != "" {
		t.Fatalf("streamJSONFinalText = %q, want empty when no final event", got)
	}
}

func TestStreamJSONFinalTextLastWins(t *testing.T) {
	out := []byte(`{"type":"final","text":"first"}
{"type":"final","text":"second"}
`)
	if got := streamJSONFinalText(out); got != "second" {
		t.Fatalf("streamJSONFinalText = %q, want second (last final wins)", got)
	}
}

// loadBaselineTask returns the named task from the checked-in baseline manifest.
// The stub-binary oracle tests below run the REAL manifest oracles against the
// REAL fixtures through the REAL NewTurnExecRunner, so a manifest edit that
// breaks an oracle is caught here rather than only in production.
func loadBaselineTask(t *testing.T, id string) BenchTask {
	t.Helper()
	set, err := LoadTaskSet(filepath.Join("manifests", "baseline.json"))
	if err != nil {
		t.Fatalf("LoadTaskSet: %v", err)
	}
	for _, task := range set.Tasks {
		if task.ID == id {
			return task
		}
	}
	t.Fatalf("manifest has no task %q", id)
	return BenchTask{}
}

// runTurnStub runs one manifest task through the production NewTurnExecRunner
// with a stub "zero" binary whose body is a POSIX sh script (writeExecStub
// skips on Windows). The stub is invoked with cmd.Dir set to the fixture copy,
// so it can both emit canned stream-json AND mutate the copy (apply or omit the
// fix) before the runner stamps the oracle and runs the verification command.
func runTurnStub(t *testing.T, task BenchTask, stubBody string) TurnTaskOutcome {
	t.Helper()
	stub := writeExecStub(t, stubBody)
	return NewTurnExecRunner(stub)(context.Background(), task, RunContext{Model: "fake-model"})
}

// assertVerifyFailed asserts an outcome failed specifically because the oracle
// rejected the work — Passed is false, there is no harness error (Err nil), and
// VerifyErr carries the surfaced failure detail. This is stronger than merely
// checking Passed=false: it distinguishes a genuine oracle rejection from a
// harness error (Err set) or a silent non-pass (Passed=false with no VerifyErr,
// which would mean the failure never went through verification at all). The
// gating stubs all emit a clean run_end (exitCode 0), so a non-empty VerifyErr
// here can only come from the verification command failing.
func assertVerifyFailed(t *testing.T, label string, outcome TurnTaskOutcome) {
	t.Helper()
	if outcome.Err != nil {
		t.Fatalf("%s: want a verify fail, got harness error: %v", label, outcome.Err)
	}
	if outcome.Passed {
		t.Fatalf("%s: want verify fail, got Passed=true", label)
	}
	if strings.TrimSpace(outcome.VerifyErr) == "" {
		t.Fatalf("%s: want verify fail, but VerifyErr is empty (failure did not come from verification): %+v", label, outcome)
	}
}

// --- Gating tests: the oracle FAILS the wrong thing (no-op / wrong answer) ---

// TestStampedOracleRejectsNoOpRefactor is the core #712 fix: refactor used to
// live in the build tier where a no-op `go build ./...` passed. Now a no-op
// agent (the stub emits a clean run_end but touches nothing) leaves the fixture
// with no formatGreeting helper, so the stamped `var _ = formatGreeting` fails
// to compile and `go test ./...` fails — the task is failed, not passed.
func TestStampedOracleRejectsNoOpRefactor(t *testing.T) {
	task := loadBaselineTask(t, "refactor-01")
	outcome := runTurnStub(t, task, `echo '{"type":"run_end","exitCode":0}'
`)
	assertVerifyFailed(t, "no-op refactor", outcome)
}

// TestStampedOracleRejectsMissingField proves edit-03's oracle is structural: a
// no-op leaves Config with no Label field, so `var _ string = Config{}.Label`
// fails to compile and the task fails.
func TestStampedOracleRejectsMissingField(t *testing.T) {
	task := loadBaselineTask(t, "edit-03")
	outcome := runTurnStub(t, task, `echo '{"type":"run_end","exitCode":0}'
`)
	assertVerifyFailed(t, "missing-field edit-03", outcome)
}

// TestNavAnswerOracleRejectsWrongAnswer proves the nav oracle greps the CAPTURED
// answer, not the raw stream: the stub emits a final answer missing the
// required keys, the harness writes it to .zero-answer.txt, and the compound
// grep fails — so a plausible-but-wrong answer cannot pass nav-09.
func TestNavAnswerOracleRejectsWrongAnswer(t *testing.T) {
	task := loadBaselineTask(t, "nav-09")
	outcome := runTurnStub(t, task, `echo '{"type":"final","text":"the keys are foo and bar"}'
echo '{"type":"run_end","exitCode":0}'
`)
	assertVerifyFailed(t, "wrong nav-09 answer", outcome)
}

// TestNavNoFinalTextFails proves a run that produced no answer fails nav: with
// no "final" event, .zero-answer.txt is empty and the grep finds nothing.
func TestNavNoFinalTextFails(t *testing.T) {
	task := loadBaselineTask(t, "nav-09")
	outcome := runTurnStub(t, task, `echo '{"type":"run_end","exitCode":0}'
`)
	assertVerifyFailed(t, "nav-09 no final", outcome)
}

// TestEdit05RejectsRewordedDebugPrint proves edit-05's oracle catches a reword,
// not just a deletion: changing `fmt.Println("debug: starting")` to
// `fmt.Println("starting up")` leaves a fmt.Println string-literal call, so the
// stamped oracle's `strings.Contains(main.go, fmt.Println(")` check fails. A
// plain `! grep 'debug: starting'` would have rubber-stamped this reword.
func TestEdit05RejectsRewordedDebugPrint(t *testing.T) {
	task := loadBaselineTask(t, "edit-05")
	outcome := runTurnStub(t, task, `sed 's/debug: starting/starting up/' main.go > .zero-tmp && mv .zero-tmp main.go
echo '{"type":"run_end","exitCode":0}'
`)
	assertVerifyFailed(t, "reworded edit-05 debug print", outcome)
}

// TestEdit05RejectsRespelledDebugPrint proves the second edit-05 check (the
// message text "debug: starting" must be absent) catches a respell onto a
// different call form that the fmt.Println(" string-literal check misses: the
// agent rewrites the line to fmt.Printf("debug: starting\n"), which still prints
// the message but is no longer a fmt.Println string-literal. The
// strings.Contains("debug: starting") assertion fails the task.
func TestEdit05RejectsRespelledDebugPrint(t *testing.T) {
	task := loadBaselineTask(t, "edit-05")
	outcome := runTurnStub(t, task, `sed 's/fmt.Println("debug: starting")/fmt.Printf("debug: starting\\n")/' main.go > .zero-tmp && mv .zero-tmp main.go
echo '{"type":"run_end","exitCode":0}'
`)
	assertVerifyFailed(t, "respelled edit-05 debug print", outcome)
}

// TestRefactor05RejectsStubbedCaller proves refactor-05's behavioral oracle
// catches a wrong edit the negative grep alone would pass: the agent deletes
// Wrapper (so ! grep 'func Wrapper' succeeds) but stubs greetWrapped to return
// "" instead of inlining. The stamped TestGreetWrapped runs greetWrapped("x"),
// gets "" not "hello, x", and fails — so deleting the target along with its
// caller (or stubbing the caller) can no longer pass the oracle.
func TestRefactor05RejectsStubbedCaller(t *testing.T) {
	task := loadBaselineTask(t, "refactor-05")
	outcome := runTurnStub(t, task, `sed -e 's/return Wrapper(name)/return ""/' -e '/^func Wrapper(name string) string {/,/^}/d' main.go > .zero-tmp && mv .zero-tmp main.go
echo '{"type":"run_end","exitCode":0}'
`)
	assertVerifyFailed(t, "stubbed refactor-05 caller", outcome)
}

// TestRefactor04RejectsBareMapKept proves refactor-04's file-reading oracle
// catches a no-op that the old exact-text shell grep would miss: the agent adds
// an unused `type Stats map[string]int` (so the compile-time `var _ Stats`
// passes) but keeps the bare `var stats = map[string]int{}` under the original
// spacing. The spacing-tolerant regex in TestStatsNamedType still matches the
// bare-map declaration, so the task fails — Record/Lookup never moved onto the
// named type.
func TestRefactor04RejectsBareMapKept(t *testing.T) {
	task := loadBaselineTask(t, "refactor-04")
	outcome := runTurnStub(t, task, `sed 's/type Config struct {/type Stats map[string]int\n\ntype Config struct {/' main.go > .zero-tmp && mv .zero-tmp main.go
echo '{"type":"run_end","exitCode":0}'
`)
	assertVerifyFailed(t, "refactor-04 bare-map kept", outcome)
}

// TestEdit01IgnoresDocComment proves edit-01's scoped negative grep does NOT
// false-fail a correct rename that leaves the doc comment mentioning the old
// name. The stub renames the declaration (`MaxRetries =` -> `RetryLimit =`) but
// leaves `// MaxRetries is the maximum...`; the scoped `! grep -RIn 'MaxRetries ='`
// matches declaration sites only, so the comment is ignored, `var _ = RetryLimit`
// compiles, and the task passes.
func TestEdit01IgnoresDocComment(t *testing.T) {
	task := loadBaselineTask(t, "edit-01")
	outcome := runTurnStub(t, task, `sed 's/const MaxRetries = 3/const RetryLimit = 3/' main.go > .zero-tmp && mv .zero-tmp main.go
echo '{"type":"run_end","exitCode":0}'
`)
	if outcome.Err != nil {
		t.Fatalf("correct rename with kept doc comment should pass, got harness error: %v", outcome.Err)
	}
	if !outcome.Passed {
		t.Fatalf("edit-01 oracle must not false-fail a correct rename that leaves the doc comment: %+v", outcome)
	}
}

// TestOracleAuthoritativeOnIncompleteExit is the companion to the --auto member
// switch: under member-auto the agent's shell is sandboxed, so on a host without
// sandbox setup its self-verification (go test/build) can't run and the turn
// exits INCOMPLETE (exit 4) even after a correct edit. For an ORACLE-bearing task
// the runner must treat the stamped oracle — not that INCOMPLETE exit — as ground
// truth: here the stub applies the real edit-01 rename but reports exitCode 4, and
// the task must still pass because the fixture is correct. This defers ONLY for
// exit 4; TestNonIncompleteExitStaysAuthoritative pins the other side.
func TestOracleAuthoritativeOnIncompleteExit(t *testing.T) {
	task := loadBaselineTask(t, "edit-01")
	outcome := runTurnStub(t, task, `sed 's/const MaxRetries = 3/const RetryLimit = 3/' main.go > .zero-tmp && mv .zero-tmp main.go
echo '{"type":"run_end","exitCode":4}'
`)
	if outcome.Err != nil {
		t.Fatalf("incomplete-exit with a correct edit should pass, got harness error: %v", outcome.Err)
	}
	if !outcome.Passed {
		t.Fatalf("a correct edit that exited INCOMPLETE (exit 4) must still pass its oracle: %+v", outcome)
	}
	if strings.TrimSpace(outcome.VerifyErr) != "" {
		t.Fatalf("a passing oracle must clear the exit-code VerifyErr, got %q", outcome.VerifyErr)
	}
}

// TestNonIncompleteExitStaysAuthoritative is the guard the oracle-authoritative
// change MUST NOT weaken: only INCOMPLETE (exit 4) defers to the oracle. A crash
// (1), provider failure (3), or interruption (130) is a genuine failure, so it
// stays authoritative even for an oracle-bearing task — otherwise a partial edit
// that happens to satisfy the oracle would launder a crashed or interrupted run
// into a pass. Each case applies the CORRECT edit-01 rename (so the oracle WOULD
// pass) but reports the failing exit code; the task must still fail, and the
// failure must be attributed to the exit code, not the oracle.
func TestNonIncompleteExitStaysAuthoritative(t *testing.T) {
	for _, code := range []int{1, 3, 130} {
		t.Run(fmt.Sprintf("exit%d", code), func(t *testing.T) {
			task := loadBaselineTask(t, "edit-01")
			outcome := runTurnStub(t, task, fmt.Sprintf(`sed 's/const MaxRetries = 3/const RetryLimit = 3/' main.go > .zero-tmp && mv .zero-tmp main.go
echo '{"type":"run_end","exitCode":%d}'
`, code))
			if outcome.Err != nil {
				t.Fatalf("a nonzero exit should be a task fail, not a harness error: %v", outcome.Err)
			}
			if outcome.Passed {
				t.Fatalf("a run that exited %d must not pass even when the edit satisfies the oracle: %+v", code, outcome)
			}
			if want := fmt.Sprintf("exit code %d", code); !strings.Contains(outcome.VerifyErr, want) {
				t.Fatalf("a non-INCOMPLETE nonzero exit must stay authoritative and surface its code, got VerifyErr=%q", outcome.VerifyErr)
			}
		})
	}
}

// TestIncompleteExitStillFailsWhenOracleFails pins the OTHER half of the exit-4
// deferral: deferring to the oracle on INCOMPLETE must still MEAN the oracle is
// consulted, not a blanket pass. Here the stub does no real work and exits
// INCOMPLETE (4); the edit-01 rename never happened, so the oracle fails and the
// task fails. Without this, a regression that turned exit 4 into an unconditional
// pass would slip through, because its sibling TestOracleAuthoritativeOnIncomplete-
// Exit applies a CORRECT edit and would pass either way.
func TestIncompleteExitStillFailsWhenOracleFails(t *testing.T) {
	task := loadBaselineTask(t, "edit-01")
	outcome := runTurnStub(t, task, `echo '{"type":"run_end","exitCode":4}'
`)
	assertVerifyFailed(t, "incomplete exit with no edit applied", outcome)
}

// TestNonzeroExitStillFailsLatencyOnly is the guard on the other side of that
// change: a latency-only task has no oracle to appeal to, so the exit code is
// the ONLY correctness signal and a nonzero exit must still fail it. Without
// this, dropping the exit-code gate for oracle tasks could be misread as
// dropping it everywhere.
func TestNonzeroExitStillFailsLatencyOnly(t *testing.T) {
	task := loadBaselineTask(t, "longproc-01")
	outcome := runTurnStub(t, task, `echo '{"type":"run_end","exitCode":4}'
`)
	if outcome.Err != nil {
		t.Fatalf("latency-only nonzero exit should be a verify fail, not a harness error: %v", outcome.Err)
	}
	if outcome.Passed {
		t.Fatalf("a latency-only task that exited nonzero must not pass: %+v", outcome)
	}
	if strings.TrimSpace(outcome.VerifyErr) == "" {
		t.Fatalf("a latency-only nonzero exit must surface a VerifyErr, got none: %+v", outcome)
	}
}

// --- Satisfiable tests: the oracle PASSES the right thing (real fix applied) ---

// TestStampedOraclePassesWhenRefactorHappened proves the refactor-01 oracle is
// not an always-fail gate: when the stub applies the real refactor (extract
// formatGreeting, call it from both callers), `var _ = formatGreeting` compiles
// and the `hello, %s` grep-count is 1 (only the helper holds the literal), so
// the task passes.
func TestStampedOraclePassesWhenRefactorHappened(t *testing.T) {
	task := loadBaselineTask(t, "refactor-01")
	outcome := runTurnStub(t, task, `sed -e 's/return fmt.Sprintf("hello, %s", c.Name)/return formatGreeting(c.Name)/' -e 's/return fmt.Sprintf("hello, %s", name)/return formatGreeting(name)/' main.go > .zero-tmp
cat >> .zero-tmp <<'EOF'

func formatGreeting(name string) string {
return fmt.Sprintf("hello, %s", name)
}
EOF
mv .zero-tmp main.go
echo '{"type":"run_end","exitCode":0}'
`)
	if outcome.Err != nil {
		t.Fatalf("real refactor should pass, got harness error: %v", outcome.Err)
	}
	if !outcome.Passed {
		t.Fatalf("refactor-01 oracle must pass when formatGreeting is extracted: %+v", outcome)
	}
}

// TestEdit03PassesWhenFieldAdded proves edit-03's oracle passes when the field
// is actually added: Config gets a Label string field, so `var _ string =
// Config{}.Label` compiles and the task passes.
func TestEdit03PassesWhenFieldAdded(t *testing.T) {
	task := loadBaselineTask(t, "edit-03")
	outcome := runTurnStub(t, task, `awk '/Name string/ && !d {print; print "Label string"; d=1; next} {print}' main.go > .zero-tmp && mv .zero-tmp main.go
echo '{"type":"run_end","exitCode":0}'
`)
	if outcome.Err != nil {
		t.Fatalf("real field-add should pass, got harness error: %v", outcome.Err)
	}
	if !outcome.Passed {
		t.Fatalf("edit-03 oracle must pass when Label is added: %+v", outcome)
	}
}

// TestNav09PassesWithCorrectAnswer proves the nav-09 oracle passes when the
// captured answer names all three config keys.
func TestNav09PassesWithCorrectAnswer(t *testing.T) {
	task := loadBaselineTask(t, "nav-09")
	outcome := runTurnStub(t, task, `echo '{"type":"final","text":"config.json has three keys: port, name, and retries."}'
echo '{"type":"run_end","exitCode":0}'
`)
	if outcome.Err != nil {
		t.Fatalf("correct nav answer should pass, got harness error: %v", outcome.Err)
	}
	if !outcome.Passed {
		t.Fatalf("nav-09 oracle must pass when the answer names port/name/retries: %+v", outcome)
	}
}

// TestRefactor05PassesWhenInlined proves refactor-05's oracle is not an
// always-fail gate: when the stub applies the real inline (greetWrapped calls
// GreetByName directly, Wrapper removed), TestGreetWrapped gets "hello, x" and
// the ! grep 'func Wrapper' check passes, so the task passes.
func TestRefactor05PassesWhenInlined(t *testing.T) {
	task := loadBaselineTask(t, "refactor-05")
	outcome := runTurnStub(t, task, `sed -e 's/return Wrapper(name)/return GreetByName(name)/' -e '/^func Wrapper(name string) string {/,/^}/d' main.go > .zero-tmp && mv .zero-tmp main.go
echo '{"type":"run_end","exitCode":0}'
`)
	if outcome.Err != nil {
		t.Fatalf("real inline should pass, got harness error: %v", outcome.Err)
	}
	if !outcome.Passed {
		t.Fatalf("refactor-05 oracle must pass when Wrapper is inlined into greetWrapped: %+v", outcome)
	}
}

// TestRefactor04PassesWhenTyped proves refactor-04's oracle passes when the
// bare map is actually replaced by the named type: the stub introduces
// `type Stats map[string]int` and retypes the var to `var stats = Stats{}`,
// so the bare-map regex no longer matches, Stats is referenced, `var _ Stats`
// compiles, and go test passes.
func TestRefactor04PassesWhenTyped(t *testing.T) {
	task := loadBaselineTask(t, "refactor-04")
	outcome := runTurnStub(t, task, `sed -e 's/type Config struct {/type Stats map[string]int\n\ntype Config struct {/' -e 's/var stats = map\[string\]int{}/var stats = Stats{}/' main.go > .zero-tmp && mv .zero-tmp main.go
echo '{"type":"run_end","exitCode":0}'
`)
	if outcome.Err != nil {
		t.Fatalf("real typed refactor should pass, got harness error: %v", outcome.Err)
	}
	if !outcome.Passed {
		t.Fatalf("refactor-04 oracle must pass when stats is typed as Stats: %+v", outcome)
	}
}

// TestStampOracleRejectsFixturelessOracleTask covers the fail-closed path in
// stampOracleAndAnswer: a task that carries an oracle (a stamped OracleTest or a
// verification command) but has no workspace fixture has nowhere safe to stamp,
// so it must error rather than let runVerification run in the caller's cwd with
// the oracle never compiled (or the answer never captured). A fixtureless task
// with no oracle and no verification (a pure latency-only run in the caller's
// cwd) is left alone. This is a direct unit test (no stub), so it runs on all
// platforms.
func TestStampOracleRejectsFixturelessOracleTask(t *testing.T) {
	// OracleTest set, no verificationCommand: isolates the `OracleTest != ""`
	// reject branch so a regression that only checked VerificationCommand would
	// fail here (the both-set case below would still pass it).
	withOracleTest := BenchTask{
		ID:         "t",
		OracleTest: "package x\n\nvar _ = Missing\n",
	}
	if err := stampOracleAndAnswer(withOracleTest, nil); err == nil {
		t.Fatal("fixtureless task with an OracleTest must be rejected, got nil error")
	}
	// verificationCommand set, no OracleTest: isolates the other reject branch.
	withVerify := BenchTask{
		ID:                  "t",
		VerificationCommand: []string{"bash", "-c", "grep x .zero-answer.txt"},
	}
	if err := stampOracleAndAnswer(withVerify, nil); err == nil {
		t.Fatal("fixtureless task with a verificationCommand must be rejected, got nil error")
	}
	// Both set: the `||` rejects, and this guards against a regression that
	// inverted the logic to `&&`.
	withBoth := BenchTask{
		ID:                  "t",
		OracleTest:          "package x\n\nvar _ = Missing\n",
		VerificationCommand: []string{"go", "test", "./..."},
	}
	if err := stampOracleAndAnswer(withBoth, nil); err == nil {
		t.Fatal("fixtureless task with both an OracleTest and a verificationCommand must be rejected, got nil error")
	}
	latencyOnly := BenchTask{ID: "t"}
	if err := stampOracleAndAnswer(latencyOnly, nil); err != nil {
		t.Fatalf("fixtureless latency-only task must not be rejected, got: %v", err)
	}
}

// --- Count-oracle tests: anchored `^count: N$` with the exact expected count ---

// TestNav01CountOracleAcceptsExactCount proves nav-01's anchored count oracle
// passes when the answer names the files AND states the exact count (5 — the
// fixture has README.md, config.json, go.mod, main.go, main_test.go) on its own
// line. printf emits the JSON with a literal \n escape so the captured answer
// has "count: 5" on its own line.
func TestNav01CountOracleAcceptsExactCount(t *testing.T) {
	task := loadBaselineTask(t, "nav-01")
	outcome := runTurnStub(t, task, `printf '%s\n' '{"type":"final","text":"main.go, config.json, go.mod, README.md, main_test.go\ncount: 5"}'
printf '%s\n' '{"type":"run_end","exitCode":0}'
`)
	if outcome.Err != nil {
		t.Fatalf("correct nav-01 answer should pass, got harness error: %v", outcome.Err)
	}
	if !outcome.Passed {
		t.Fatalf("nav-01 oracle must pass when the answer names the files and states count: 5: %+v", outcome)
	}
}

// TestNav01CountOracleRejectsWrongCount proves the anchored count oracle rejects
// a wrong count: the file names are all present, but "count: 3" does not match
// `^count: 5$`, so the task fails at verification.
func TestNav01CountOracleRejectsWrongCount(t *testing.T) {
	task := loadBaselineTask(t, "nav-01")
	outcome := runTurnStub(t, task, `printf '%s\n' '{"type":"final","text":"main.go, config.json, go.mod, README.md, main_test.go\ncount: 3"}'
printf '%s\n' '{"type":"run_end","exitCode":0}'
`)
	assertVerifyFailed(t, "nav-01 wrong count", outcome)
}

// TestNav04CountOracleRejectsSubstring proves the line anchor rejects a
// substring: "the count: 1 is the number" contains "count: 1" (the right
// value) but the line does not start with "count:", so `^count:[[:space:]]*1$`
// does not match. A pre-anchor oracle (`count:\s*1`) would have rubber-stamped
// this.
func TestNav04CountOracleRejectsSubstring(t *testing.T) {
	task := loadBaselineTask(t, "nav-04")
	outcome := runTurnStub(t, task, `printf '%s\n' '{"type":"final","text":"the count: 1 is the number of test functions"}'
printf '%s\n' '{"type":"run_end","exitCode":0}'
`)
	assertVerifyFailed(t, "nav-04 substring count", outcome)
}

// TestNav04CountOracleAcceptsExactCount proves the nav-04 oracle passes for a
// real answer: the count is anchored on its own line AND the answer names the
// one Test* function (TestGreet, from main_test.go). The fixture's ground truth
// is exactly one test function, so count: 1 is right, and naming TestGreet is
// the inspection-proof that the agent actually read main_test.go.
func TestNav04CountOracleAcceptsExactCount(t *testing.T) {
	task := loadBaselineTask(t, "nav-04")
	outcome := runTurnStub(t, task, `printf '%s\n' '{"type":"final","text":"There is one test function: TestGreet in main_test.go.\ncount: 1"}'
printf '%s\n' '{"type":"run_end","exitCode":0}'
`)
	if outcome.Err != nil {
		t.Fatalf("correct nav-04 answer should pass, got harness error: %v", outcome.Err)
	}
	if !outcome.Passed {
		t.Fatalf("nav-04 oracle must pass when the answer names TestGreet and states count: 1 on its own line: %+v", outcome)
	}
}

// TestNav04CountOracleRejectsZeroGuess is the count=0 gameability gate: an agent
// that never opens the workspace and always emits "count: 0" used to pass nav-04
// when the ground truth was 0. The fixture now has one real Test* function, so
// the ground truth is 1 and a clean "count: 0" fails.
func TestNav04CountOracleRejectsZeroGuess(t *testing.T) {
	task := loadBaselineTask(t, "nav-04")
	outcome := runTurnStub(t, task, `printf '%s\n' '{"type":"final","text":"count: 0"}'
printf '%s\n' '{"type":"run_end","exitCode":0}'
`)
	assertVerifyFailed(t, "nav-04 zero-guess", outcome)
}

// TestNav04CountOracleRejectsBlindCountOne is the count=1 gameability gate: once
// the ground truth became 1, an agent that never opens the workspace and always
// emits "count: 1" would pass a count-only oracle. nav-04's oracle also requires
// the answer to name the test function (TestGreet), so a bare "count: 1" with no
// named fact fails — proving the blind-count-one surface is closed.
func TestNav04CountOracleRejectsBlindCountOne(t *testing.T) {
	task := loadBaselineTask(t, "nav-04")
	outcome := runTurnStub(t, task, `printf '%s\n' '{"type":"final","text":"count: 1"}'
printf '%s\n' '{"type":"run_end","exitCode":0}'
`)
	assertVerifyFailed(t, "nav-04 blind-count-one", outcome)
}

// TestNav05CountOracleAcceptsExactCount proves nav-05's oracle passes for a real
// answer: the count is anchored on its own line AND the answer names the file
// holding the one TODO (main.go). Naming main.go is the inspection-proof that the
// agent actually located the TODO rather than guessing the count.
func TestNav05CountOracleAcceptsExactCount(t *testing.T) {
	task := loadBaselineTask(t, "nav-05")
	outcome := runTurnStub(t, task, `printf '%s\n' '{"type":"final","text":"One TODO in main.go.\ncount: 1"}'
printf '%s\n' '{"type":"run_end","exitCode":0}'
`)
	if outcome.Err != nil {
		t.Fatalf("correct nav-05 answer should pass, got harness error: %v", outcome.Err)
	}
	if !outcome.Passed {
		t.Fatalf("nav-05 oracle must pass when the answer names main.go and states count: 1: %+v", outcome)
	}
}

// TestNav05CountOracleRejectsZeroGuess is nav-05's count=0 gameability gate: the
// fixture now has one real TODO, so a clean "count: 0" (the always-guess-zero
// answer) fails.
func TestNav05CountOracleRejectsZeroGuess(t *testing.T) {
	task := loadBaselineTask(t, "nav-05")
	outcome := runTurnStub(t, task, `printf '%s\n' '{"type":"final","text":"count: 0"}'
printf '%s\n' '{"type":"run_end","exitCode":0}'
`)
	assertVerifyFailed(t, "nav-05 zero-guess", outcome)
}

// TestNav05CountOracleRejectsBlindCountOne is nav-05's count=1 gameability gate:
// once the ground truth became 1, an agent that never opens the workspace and
// always emits "count: 1" would pass a count-only oracle. nav-05's oracle also
// requires the answer to name the file (main.go), so a bare "count: 1" with no
// named file fails — proving the blind-count-one surface is closed.
func TestNav05CountOracleRejectsBlindCountOne(t *testing.T) {
	task := loadBaselineTask(t, "nav-05")
	outcome := runTurnStub(t, task, `printf '%s\n' '{"type":"final","text":"count: 1"}'
printf '%s\n' '{"type":"run_end","exitCode":0}'
`)
	assertVerifyFailed(t, "nav-05 blind-count-one", outcome)
}

// TestNav05CountOracleRejectsSubstring proves the line anchor still bites even
// when the named fact is present: "the count: 1 is the number of TODOs in
// main.go" names main.go and contains "count: 1" (the right value), but the
// count does not stand on its own line, so `^count:[[:space:]]*1$` does not
// match and the oracle fails. A pre-anchor oracle (`count:\s*1`) would have
// rubber-stamped this. Mirrors TestNav04CountOracleRejectsSubstring.
func TestNav05CountOracleRejectsSubstring(t *testing.T) {
	task := loadBaselineTask(t, "nav-05")
	outcome := runTurnStub(t, task, `printf '%s\n' '{"type":"final","text":"the count: 1 is the number of TODOs in main.go"}'
printf '%s\n' '{"type":"run_end","exitCode":0}'
`)
	assertVerifyFailed(t, "nav-05 substring count", outcome)
}

// TestNav08CountOracleAcceptsStdlibAnswer proves nav-08's oracle passes when the
// agent enumerated the imports across BOTH .go files and named the stdlib ones
// (fmt from main.go, testing from main_test.go): the answer states the only
// imports are stdlib so there are 0 third-party, with "count: 0" on its own
// line. The fmt+testing requirement is the inspection-proof — it forces the
// agent to have read all the files rather than blindly emitting "count: 0".
func TestNav08CountOracleAcceptsStdlibAnswer(t *testing.T) {
	task := loadBaselineTask(t, "nav-08")
	outcome := runTurnStub(t, task, `printf '%s\n' '{"type":"final","text":"The only imports are fmt (main.go) and testing (main_test.go), both standard library, so there are no third-party imports.\ncount: 0"}'
printf '%s\n' '{"type":"run_end","exitCode":0}'
`)
	if outcome.Err != nil {
		t.Fatalf("correct nav-08 answer should pass, got harness error: %v", outcome.Err)
	}
	if !outcome.Passed {
		t.Fatalf("nav-08 oracle must pass when the answer names fmt+testing and states count: 0: %+v", outcome)
	}
}

// TestNav08CountOracleRejectsAnswerWithoutFmt proves the inspection-proof bites:
// an answer that states the right count (0) but never names the stdlib imports it
// examined ("No third-party imports. count: 0") fails, so an agent that didn't
// actually look at the imports can't pass by guessing the count alone.
func TestNav08CountOracleRejectsAnswerWithoutFmt(t *testing.T) {
	task := loadBaselineTask(t, "nav-08")
	outcome := runTurnStub(t, task, `printf '%s\n' '{"type":"final","text":"No third-party imports.\ncount: 0"}'
printf '%s\n' '{"type":"run_end","exitCode":0}'
`)
	assertVerifyFailed(t, "nav-08 no-fmt", outcome)
}

// TestNav08CountOracleRejectsFmtOnlyNoTesting proves the tighter inspection-proof:
// an answer that names fmt (from main.go) but misses testing (from main_test.go)
// fails, so an agent that only read main.go — or a guesser that names the one
// most-common stdlib import blind — can't pass. The agent must enumerate every
// file's imports, which is what the prompt asks for.
func TestNav08CountOracleRejectsFmtOnlyNoTesting(t *testing.T) {
	task := loadBaselineTask(t, "nav-08")
	outcome := runTurnStub(t, task, `printf '%s\n' '{"type":"final","text":"The only import is fmt, which is standard library. count: 0"}'
printf '%s\n' '{"type":"run_end","exitCode":0}'
`)
	assertVerifyFailed(t, "nav-08 fmt-only-no-testing", outcome)
}

func TestResolveBinaryAbsolutizesExplicitPath(t *testing.T) {
	dir := t.TempDir()
	name := "zero-probe.exe"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	resolved, err := ResolveBinary("./" + name)
	if err != nil {
		t.Fatalf("ResolveBinary: %v", err)
	}
	if !filepath.IsAbs(resolved) {
		t.Fatalf("ResolveBinary returned relative path %q; the turn runner sets cmd.Dir per task, so a relative binary fails to spawn from every fixture copy", resolved)
	}
}

func TestRunTurnBenchCountsErroredTasks(t *testing.T) {
	set := TaskSet{
		ID: "errored-suite",
		Tasks: []BenchTask{
			{ID: "t1", Class: "nav", Prompt: "p1", VerificationCommand: []string{"true"}},
			{ID: "t2", Class: "longproc", Prompt: "p2"},
		},
	}
	cfg := TurnBenchConfig{
		Model:      "fake-model",
		Iterations: 1,
		Runner: func(context.Context, BenchTask, RunContext) TurnTaskOutcome {
			return TurnTaskOutcome{Err: errors.New("fork/exec ./zero: file does not exist")}
		},
		Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	}
	result, err := RunTurnBench(context.Background(), set, cfg)
	if err != nil {
		t.Fatalf("RunTurnBench: %v", err)
	}
	if result.TasksErrored != 2 {
		t.Fatalf("TasksErrored=%d, want 2 (every iteration of both tasks errored)", result.TasksErrored)
	}
	if len(result.Warnings) != 2 {
		t.Fatalf("warnings=%d, want 2 run warnings", len(result.Warnings))
	}
	summary := FormatTurnBenchSummary(result)
	if !strings.Contains(summary, "ERRORED: 2 task(s)") {
		t.Fatalf("summary does not surface the errored tasks:\n%s", summary)
	}
	if !strings.Contains(summary, "fork/exec ./zero") {
		t.Fatalf("summary does not echo the underlying run error:\n%s", summary)
	}
}

func TestRunTurnBenchPartialErrorStillCounts(t *testing.T) {
	set := TaskSet{
		ID: "partial-suite",
		Tasks: []BenchTask{
			{ID: "ok", Class: "nav", Prompt: "p", VerificationCommand: []string{"true"}},
			{ID: "dead", Class: "nav", Prompt: "p", VerificationCommand: []string{"true"}},
		},
	}
	canned := map[string]*trace.TurnTrace{"ok": cannedTrace(100, 10, 1000)}
	inner := fakeTurnRunner(canned)
	cfg := TurnBenchConfig{
		Model:      "fake-model",
		Iterations: 1,
		Runner: func(ctx context.Context, task BenchTask, rc RunContext) TurnTaskOutcome {
			if task.ID == "dead" {
				return TurnTaskOutcome{Err: errors.New("spawn failed")}
			}
			return inner(ctx, task, rc)
		},
		Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	}
	result, err := RunTurnBench(context.Background(), set, cfg)
	if err != nil {
		t.Fatalf("RunTurnBench: %v", err)
	}
	if result.TasksErrored != 1 {
		t.Fatalf("TasksErrored=%d, want 1", result.TasksErrored)
	}
	if result.TasksAttempted != 2 {
		t.Fatalf("TasksAttempted=%d, want 2", result.TasksAttempted)
	}
}

// The exec-profile passthrough: the profile must reach every task's zero exec
// invocation as --exec-profile (with the prompt staying last) and stay out of
// the args entirely when unset.
func TestBuildTurnExecArgsIncludesExecProfile(t *testing.T) {
	task := BenchTask{ID: "t", Prompt: "do the thing"}
	args := buildTurnExecArgs(task, RunContext{Model: "m", ExecProfile: "fast"}, "trace.ndjson", nil)
	found := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--exec-profile" && args[i+1] == "fast" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("args must carry --exec-profile fast, got %v", args)
	}
	if args[len(args)-1] != "do the thing" {
		t.Fatalf("prompt must stay the last argument, got %v", args)
	}

	args = buildTurnExecArgs(task, RunContext{Model: "m"}, "trace.ndjson", nil)
	for _, arg := range args {
		if arg == "--exec-profile" {
			t.Fatalf("no profile configured, but args carry --exec-profile: %v", args)
		}
	}
}

// Every benchmark invocation MUST grant the write/shell tool set via --auto
// member. Without a permission grant the agent runs read-only and cannot apply
// any edit, so the mutating classes measure nothing but oracle/answer
// contamination. member-auto is the RIGHT grant: it exposes the write +
// sandboxed-shell tools the benchmark needs while keeping the workspace/network/
// destructive safeguards, so the args must NOT reach for the broader
// --skip-permissions-unsafe (which drops those guards). This is a correctness
// contract, not a preference.
func TestBuildTurnExecArgsGrantsWriteTools(t *testing.T) {
	args := buildTurnExecArgs(BenchTask{ID: "t", Prompt: "edit the file"}, RunContext{Model: "m"}, "trace.ndjson", nil)
	autoMember := false
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--auto" && args[i+1] == "member" {
			autoMember = true
			break
		}
	}
	if !autoMember {
		t.Fatalf("benchmark exec args must include --auto member so the agent can apply edits, got %v", args)
	}
	for _, arg := range args {
		if arg == "--skip-permissions-unsafe" {
			t.Fatalf("benchmark exec args must NOT use --skip-permissions-unsafe (member-auto keeps the sandbox/network guards), got %v", args)
		}
	}
	if args[len(args)-1] != "edit the file" {
		t.Fatalf("prompt must stay the last argument, got %v", args)
	}
}

// TestTurnRunnerRejectsFixturelessTask is the other half of the write-tools
// contract: because buildTurnExecArgs grants the write + sandboxed-shell tool
// set to every invocation, a task with no fixture would run an agent with those
// tools in the caller's cwd. The runner must reject such a task BEFORE launching
// zero exec. Passing a binary path that does not exist proves the rejection is
// pre-launch — the outcome carries the fixture error, not a spawn error — so this
// runs on every OS (it never reaches the POSIX-only exec stub).
func TestTurnRunnerRejectsFixturelessTask(t *testing.T) {
	task := BenchTask{ID: "no-fixture", Prompt: "do a thing"} // no WorkspaceFixture set
	outcome := NewTurnExecRunner(filepath.Join(t.TempDir(), "does-not-exist-zero"))(context.Background(), task, RunContext{Model: "m"})
	if outcome.Err == nil {
		t.Fatalf("a fixtureless task must be rejected before launch, got %+v", outcome)
	}
	if !strings.Contains(outcome.Err.Error(), "workspaceFixture") {
		t.Fatalf("rejection must name the missing workspaceFixture, got: %v", outcome.Err)
	}
	if outcome.Passed {
		t.Fatalf("a rejected task must not pass, got %+v", outcome)
	}
}

// The configured profile must reach the runner's RunContext and be stamped
// into the result, so a profile A/B report is self-describing. The boundary
// canonicalizes (case/whitespace) so equivalent postures always carry the same
// label, and rejects unknown names so a direct library caller cannot slip an
// unvalidated profile into a report the way the CLI (parse-time check) cannot.
func TestRunTurnBenchStampsExecProfile(t *testing.T) {
	set := TaskSet{
		ID:    "profile-suite",
		Tasks: []BenchTask{{ID: "t1", Class: "longproc", Prompt: "p"}},
	}
	var gotProfile string
	cfg := TurnBenchConfig{
		Model:       "fake-model",
		ExecProfile: " FAST ",
		Iterations:  1,
		Runner: func(_ context.Context, _ BenchTask, rc RunContext) TurnTaskOutcome {
			gotProfile = rc.ExecProfile
			return TurnTaskOutcome{Passed: true, WallMs: 10, Trace: cannedTrace(10, 1, 100)}
		},
		Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	}
	result, err := RunTurnBench(context.Background(), set, cfg)
	if err != nil {
		t.Fatalf("RunTurnBench: %v", err)
	}
	if gotProfile != "fast" {
		t.Fatalf("runner RunContext.ExecProfile = %q, want the canonical fast", gotProfile)
	}
	if result.ExecProfile != "fast" {
		t.Fatalf("result.ExecProfile = %q, want the canonical fast", result.ExecProfile)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if !strings.Contains(string(raw), `"execProfile":"fast"`) {
		t.Fatal("published JSON must carry execProfile")
	}
}

func TestRunTurnBenchRejectsUnknownExecProfile(t *testing.T) {
	set := TaskSet{
		ID:    "profile-suite",
		Tasks: []BenchTask{{ID: "t1", Class: "longproc", Prompt: "p"}},
	}
	ran := false
	cfg := TurnBenchConfig{
		Model:       "fake-model",
		ExecProfile: "blanced",
		Iterations:  1,
		Runner: func(context.Context, BenchTask, RunContext) TurnTaskOutcome {
			ran = true
			return TurnTaskOutcome{Passed: true, WallMs: 10, Trace: cannedTrace(10, 1, 100)}
		},
	}
	_, err := RunTurnBench(context.Background(), set, cfg)
	if err == nil || !strings.Contains(err.Error(), "unknown execution profile") {
		t.Fatalf("err = %v, want an unknown-profile rejection", err)
	}
	if ran {
		t.Fatal("nothing may run under an unknown profile")
	}
}

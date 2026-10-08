package trace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/agenteval"
)

func TestRecorderSpanAccumulates(t *testing.T) {
	r := NewRecorder("s1", "r1", "")
	r.Start()
	s := r.Span(SpanGeneration)
	time.Sleep(2 * time.Millisecond)
	s.End()
	s2 := r.Span(SpanGeneration)
	time.Sleep(1 * time.Millisecond)
	s2.End()

	tr := r.Finish()
	got := tr.Span(SpanGeneration)
	if got < 3*time.Millisecond {
		t.Fatalf("generation span did not accumulate across stamps; got %v", got)
	}
	// Spans are stored as occurrences (one entry per stamp), not merged by name,
	// so the recorder can derive parent/child nesting by interval containment.
	if len(tr.Spans) != 2 {
		t.Fatalf("expected two generation span occurrences, got %d", len(tr.Spans))
	}
}

func TestRecorderCounters(t *testing.T) {
	r := NewRecorder("s1", "r1", "")
	r.Counter(CounterToolCalls, 1)
	r.Counter(CounterToolCalls, 2)
	r.Counter(CounterModelRequests, 3)

	tr := r.Finish()
	if got := tr.Counter(CounterToolCalls); got != 3 {
		t.Fatalf("tool_calls = %d, want 3", got)
	}
	if got := tr.Counter(CounterModelRequests); got != 3 {
		t.Fatalf("model_requests = %d, want 3", got)
	}
}

func TestRecorderFirstTokenOnce(t *testing.T) {
	r := NewRecorder("s", "r", "")
	r.Start()
	r.StampFirstToken()
	first := r.Finish().FirstTokenAt
	r.StampFirstToken() // no-op after Finish; should not panic
	if r.Finish().FirstTokenAt != first {
		t.Fatal("StampFirstToken should not move the timestamp after the first stamp")
	}
}

func TestFinishSnapshotIsCopy(t *testing.T) {
	r := NewRecorder("s", "r", "")
	r.Counter(CounterToolCalls, 5)
	tr := r.Finish()
	tr.Counters[0].Value = 999
	if got := r.Finish().Counter(CounterToolCalls); got != 5 {
		t.Fatalf("Finish snapshot must be a copy; mutating it changed recorder state to %d", got)
	}
}

func TestFinishFreezesState(t *testing.T) {
	// Once Finish returns a snapshot, the recorder is frozen: later stamps must
	// not mutate its state, so a second Finish yields the same trace.
	r := NewRecorder("s", "r", "")
	r.Start()
	r.Counter(CounterToolCalls, 2)
	r.RecordSpan(SpanGeneration, 5*time.Millisecond)
	r.StampFirstToken()
	first := r.Finish()

	// Post-finish stamps of every kind must be dropped.
	r.Counter(CounterToolCalls, 100)
	r.RecordSpan(SpanGeneration, 100*time.Millisecond)
	r.StampFirstToken()
	r.StampFirstVisibleEvent()
	r.StampFirstUsefulAction()
	s := r.Span(SpanToolExecution)
	time.Sleep(time.Millisecond)
	s.End()

	second := r.Finish()
	if got := second.Counter(CounterToolCalls); got != 2 {
		t.Fatalf("post-finish Counter leaked into snapshot: got %d, want 2", got)
	}
	if got := second.Span(SpanGeneration); got != 5*time.Millisecond {
		t.Fatalf("post-finish RecordSpan leaked into snapshot: got %v, want 5ms", got)
	}
	if got := second.Span(SpanToolExecution); got != 0 {
		t.Fatalf("post-finish Span leaked into snapshot: got %v, want 0", got)
	}
	if second.FirstTokenAt != first.FirstTokenAt {
		t.Fatalf("post-finish StampFirstToken moved timestamp")
	}
	if !second.FirstVisibleEventAt.IsZero() {
		t.Fatalf("post-finish StampFirstVisibleEvent leaked into snapshot")
	}
	if !second.FirstUsefulActionAt.IsZero() {
		t.Fatalf("post-finish StampFirstUsefulAction leaked into snapshot")
	}
}

func TestNilRecorderIsNoOp(t *testing.T) {
	var r *Recorder
	r.Start()
	r.Counter(CounterToolCalls, 1)
	r.StampFirstToken()
	r.StampFirstVisibleEvent()
	r.StampFirstUsefulAction()
	r.RecordSpan(SpanGeneration, time.Millisecond)
	s := r.Span(SpanGeneration)
	s.End()
	if tr := r.Finish(); tr != nil {
		t.Fatalf("nil recorder Finish should return nil, got %+v", tr)
	}
}

func TestRecorderConcurrent(t *testing.T) {
	r := NewRecorder("s", "r", "")
	r.Start()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := r.Span(SpanToolExecution)
			time.Sleep(time.Millisecond)
			s.End()
			r.Counter(CounterToolCalls, 1)
			r.StampFirstToken()
		}()
	}
	wg.Wait()
	tr := r.Finish()
	if got := tr.Counter(CounterToolCalls); got != 50 {
		t.Fatalf("tool_calls = %d, want 50", got)
	}
	if got := tr.Span(SpanToolExecution); got <= 0 {
		t.Fatalf("tool_execution span empty after concurrent stamps; got %v", got)
	}
}

func TestContextRoundTrip(t *testing.T) {
	r := NewRecorder("s", "r", "")
	ctx := WithContext(context.Background(), r)
	if got := FromContext(ctx); got != r {
		t.Fatal("FromContext did not return the injected recorder")
	}
	if got := FromContext(context.Background()); got != nil {
		t.Fatalf("FromContext on a bare context should return nil, got %v", got)
	}
}

func TestContextNilRecorder(t *testing.T) {
	ctx := WithContext(context.Background(), nil)
	if got := FromContext(ctx); got != nil {
		t.Fatalf("FromContext should return nil for an injected nil recorder, got %v", got)
	}
}

func TestWriteNDJSONMatchesAgentevalContract(t *testing.T) {
	r := NewRecorder("s1", "r1", "cold")
	r.Start()
	r.RecordSpan(SpanToolPartition, 10*time.Millisecond)
	r.RecordSpan(SpanGeneration, 50*time.Millisecond)
	r.RecordSpan(SpanToolExecution, 5*time.Millisecond)
	r.RecordSpan(SpanPermissionWait, 1*time.Millisecond)
	r.RecordSpan(SpanCompaction, 2*time.Millisecond)
	r.RecordSpan(SpanProviderConnect, 8*time.Millisecond)
	r.Counter(CounterModelRequests, 2)
	r.Counter(CounterToolCalls, 3)
	r.Counter(CounterInputTokens, 100)
	r.Counter(CounterOutputTokens, 40)
	tr := r.Finish()

	var buf bytes.Buffer
	if err := WriteNDJSON(&buf, tr); err != nil {
		t.Fatalf("WriteNDJSON: %v", err)
	}
	stdout := buf.String()

	missing := agenteval.MissingTraceEvents(RequiredEventKeys(), stdout)
	if len(missing) > 0 {
		t.Fatalf("NDJSON missing required event keys: %v\noutput:\n%s", missing, stdout)
	}

	keys := agenteval.ParseTraceEventKeys(stdout)
	want := map[string]bool{
		"trace:run":                       true,
		"span:" + SpanToolPartition:       true,
		"span:" + SpanGeneration:          true,
		"span:" + SpanToolExecution:       true,
		"span:" + SpanProviderConnect:     true,
		"counter:" + CounterModelRequests: true,
		"counter:" + CounterToolCalls:     true,
		"counter:" + CounterInputTokens:   true,
		"counter:" + CounterOutputTokens:  true,
	}
	for k := range want {
		if !contains(keys, k) {
			t.Fatalf("expected key %q in parsed keys %v", k, keys)
		}
	}

	// Each line must be valid JSON.
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("non-JSON NDJSON line: %q (%v)", line, err)
		}
	}
}

func TestWriteTextIsReadable(t *testing.T) {
	r := NewRecorder("s", "r", "")
	r.Start()
	r.RecordSpan(SpanGeneration, 42*time.Millisecond)
	r.Counter(CounterToolCalls, 7)
	tr := r.Finish()
	var buf bytes.Buffer
	if err := WriteText(&buf, tr); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"trace run=", "spans:", "generation", "counters:", "tool_calls"} {
		if !strings.Contains(out, want) {
			t.Fatalf("text trace missing %q:\n%s", want, out)
		}
	}
}

func TestWriteTextPropagatesWriteError(t *testing.T) {
	r := NewRecorder("s", "r", "")
	r.Start()
	r.RecordSpan(SpanGeneration, 42*time.Millisecond)
	r.Counter(CounterToolCalls, 7)
	tr := r.Finish()
	if err := WriteText(errWriter{}, tr); err == nil {
		t.Fatal("WriteText to a failing sink returned nil; want the write error")
	}
}

type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) { return 0, errors.New("write failed") }

func TestAttributionRatio(t *testing.T) {
	r := NewRecorder("s", "r", "")
	r.Start()
	// Two sequential, non-overlapping spans (RecordSpan synthesizes them
	// back-to-back from the cursor) so neither contains the other: both are
	// top-level and AttributedDuration is their sum with no double-counting.
	r.RecordSpan(SpanGeneration, 10*time.Millisecond)
	r.RecordSpan(SpanToolExecution, 10*time.Millisecond)
	tr := r.Finish()

	// Attributed time is the deterministic sum of top-level span durations; it
	// does not depend on wall-clock timing.
	if want := 20 * time.Millisecond; tr.AttributedDuration() != want {
		t.Fatalf("attributed = %v, want %v", tr.AttributedDuration(), want)
	}

	// AttributionRatio is Coverage — the fraction of wall covered by the union
	// of span intervals, capped at 1.0. It never exceeds 1 (no double-counting
	// of overlapping/nested spans), and equals Coverage by definition.
	if got := tr.AttributionRatio(); got > 1 {
		t.Fatalf("attribution ratio = %v, must never exceed 1", got)
	}
	if got := tr.AttributionRatio(); got != tr.Coverage() {
		t.Fatalf("attribution ratio = %v, want Coverage() = %v", got, tr.Coverage())
	}
}

func TestCoverageExcludesDoubleCountAndCaps(t *testing.T) {
	// A nested span (provider_connect inside generation) must NOT push coverage
	// above 1, and the parent's exclusive time must subtract the child's interval.
	r := NewRecorder("s", "r", "")
	r.Start()
	// Synthesize a containing generation interval of 100ms, then a provider_connect
	// recorded after it so its interval sits fully inside generation's.
	gen := r.Span(SpanGeneration)
	time.Sleep(2 * time.Millisecond)
	// Record a provider_connect that starts after generation started and ends
	// before generation ends: nested, so it is the child of generation.
	pc := r.Span(SpanProviderConnect)
	time.Sleep(1 * time.Millisecond)
	pc.End()
	gen.End()
	tr := r.Finish()

	var genExclusive, pcExclusive time.Duration
	var genParent, pcParent string
	for _, s := range tr.Spans {
		switch s.Name {
		case SpanGeneration:
			genExclusive = s.Exclusive
			genParent = s.Parent
		case SpanProviderConnect:
			pcExclusive = s.Exclusive
			pcParent = s.Parent
		}
	}
	// provider_connect has no children, so its exclusive time equals its own
	// duration and is positive.
	if pcExclusive <= 0 {
		t.Fatalf("provider_connect exclusive = %v, want > 0", pcExclusive)
	}
	if pcParent != SpanGeneration {
		t.Fatalf("provider_connect parent = %q, want %q (nested by interval containment)", pcParent, SpanGeneration)
	}
	if genParent != "" {
		t.Fatalf("generation should be top-level, got parent %q", genParent)
	}
	// generation's exclusive time must be its duration minus provider_connect's
	// interval, not its full duration.
	if genExclusive >= tr.Span(SpanGeneration) {
		t.Fatalf("generation exclusive = %v should be less than its inclusive %v (child subtracted)",
			genExclusive, tr.Span(SpanGeneration))
	}
	if tr.Coverage() > 1 {
		t.Fatalf("coverage = %v, must never exceed 1 even with nested spans", tr.Coverage())
	}
}

func TestAttributionRatioZeroWall(t *testing.T) {
	// A trace with no completed run has zero wall and a defined-zero ratio
	// (covers the divide-by-zero guard).
	tr := &TurnTrace{}
	if got := tr.AttributionRatio(); got != 0 {
		t.Fatalf("zero-wall ratio = %v, want 0", got)
	}
	if got := tr.AttributedDuration(); got != 0 {
		t.Fatalf("empty attributed = %v, want 0", got)
	}
	if got := tr.WallDuration(); got != 0 {
		t.Fatalf("empty wall = %v, want 0", got)
	}
}

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}

func TestReadNDJSONRoundTrip(t *testing.T) {
	r := NewRecorder("s1", "r1", "cold")
	r.Start()
	r.RecordSpan(SpanToolPartition, 10*time.Millisecond)
	r.RecordSpan(SpanGeneration, 50*time.Millisecond)
	r.RecordSpan(SpanGeneration, 5*time.Millisecond) // accumulates to 55ms
	r.Counter(CounterModelRequests, 3)
	r.Counter(CounterToolCalls, 7)
	r.StampFirstToken()
	// Two prefix_hash events so the round-trip test covers the third
	// event type. Insertion order is preserved by the parser (no sort
	// is applied on read or write).
	r.EmitPrefixHash(PrefixHash{
		SystemPromptHash:       "system1",
		BaseInstructionsHash:   "b1",
		ConfirmationPolicyHash: "c1",
		ProjectContextHash:     "p1",
		SkillsHash:             "s1",
		ToolsHash:              "t1",
		SchemaHash:             "x1",
		CompletePrefixHash:     "complete1",
		InvalidationReason:     "initial",
	})
	r.EmitPrefixHash(PrefixHash{
		SystemPromptHash:       "system2",
		BaseInstructionsHash:   "b2",
		ConfirmationPolicyHash: "c2",
		ProjectContextHash:     "p2",
		SkillsHash:             "s2",
		ToolsHash:              "t2",
		SchemaHash:             "x2",
		CompletePrefixHash:     "complete2",
		InvalidationReason:     "tools",
	})
	original := r.Finish()

	var buf bytes.Buffer
	if err := WriteNDJSON(&buf, original); err != nil {
		t.Fatalf("WriteNDJSON: %v", err)
	}
	parsed, err := ReadNDJSON(&buf)
	if err != nil {
		t.Fatalf("ReadNDJSON: %v", err)
	}
	if parsed == nil {
		t.Fatal("ReadNDJSON returned nil")
	}
	if parsed.RunID != original.RunID || parsed.SessionID != original.SessionID || parsed.Profile != original.Profile {
		t.Fatalf("identity mismatch: got %+v want %+v", parsed, original)
	}
	if got := parsed.Span(SpanGeneration); got != 55*time.Millisecond {
		t.Fatalf("generation span after round-trip = %v, want 55ms", got)
	}
	if got := parsed.Counter(CounterModelRequests); got != 3 {
		t.Fatalf("model_requests after round-trip = %d, want 3", got)
	}
	if got := parsed.Counter(CounterToolCalls); got != 7 {
		t.Fatalf("tool_calls after round-trip = %d, want 7", got)
	}
	if parsed.FirstTokenAt.IsZero() {
		t.Fatal("first_token_at lost in round-trip")
	}
	// prefix_hash round-trip: two events, in insertion order, with all hash
	// fields and the invalidation reason preserved exactly.
	if len(parsed.PrefixHashes) != 2 {
		t.Fatalf("expected 2 prefix_hash events after round-trip, got %d", len(parsed.PrefixHashes))
	}
	if parsed.PrefixHashes[0].CompletePrefixHash != "complete1" || parsed.PrefixHashes[1].CompletePrefixHash != "complete2" {
		t.Fatalf("prefix_hash insertion order lost: got %+v want [complete1, complete2]", parsed.PrefixHashes)
	}
	if parsed.PrefixHashes[0].SystemPromptHash != "system1" || parsed.PrefixHashes[0].BaseInstructionsHash != "b1" || parsed.PrefixHashes[0].SchemaHash != "x1" {
		t.Fatalf("prefix_hash sub-hashes lost on first event: got %+v", parsed.PrefixHashes[0])
	}
	if parsed.PrefixHashes[1].SystemPromptHash != "system2" || parsed.PrefixHashes[1].BaseInstructionsHash != "b2" || parsed.PrefixHashes[1].SchemaHash != "x2" {
		t.Fatalf("prefix_hash sub-hashes lost on second event: got %+v", parsed.PrefixHashes[1])
	}
	if parsed.PrefixHashes[0].InvalidationReason != "initial" || parsed.PrefixHashes[1].InvalidationReason != "tools" {
		t.Fatalf("prefix invalidation reasons lost: %+v", parsed.PrefixHashes)
	}
}

func TestWriteNDJSONOmitsEmptyPrefixInvalidationReason(t *testing.T) {
	recorder := NewRecorder("session", "run", "test")
	recorder.Start()
	recorder.EmitPrefixHash(PrefixHash{CompletePrefixHash: "stable"})
	tr := recorder.Finish()

	var output bytes.Buffer
	if err := WriteNDJSON(&output, tr); err != nil {
		t.Fatalf("WriteNDJSON: %v", err)
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if event["type"] != "prefix_hash" {
			continue
		}
		found = true
		if _, exists := event["invalidation_reason"]; exists {
			t.Fatalf("empty invalidation reason should be omitted: %s", line)
		}
	}
	if !found {
		t.Fatal("prefix_hash event not written")
	}
}

func TestReadNDJSONRejectsNonTrace(t *testing.T) {
	// A file with content but no type:trace header is never a valid empty trace.
	if _, err := ReadNDJSON(strings.NewReader("not json at all\n")); err == nil {
		t.Fatal("expected error for non-JSON input with no trace header")
	}
	if _, err := ReadNDJSON(strings.NewReader(`{"type":"span","name":"generation","duration_ms":5}` + "\n")); err == nil {
		t.Fatal("expected error for span lines with no preceding trace header")
	}
}

func TestReadNDJSONRejectsHeaderOnly(t *testing.T) {
	// A header with no recoverable spans, counters, or prefix hashes is
	// corrupt/truncated, not empty. prefix_hash is now a third valid
	// event type, so a header with only prefix_hash events is accepted.
	header := `{"type":"trace","name":"run","session_id":"s","run_id":"r"}` + "\n"
	if _, err := ReadNDJSON(strings.NewReader(header)); err == nil {
		t.Fatal("expected error for a trace header with no spans, counters, or prefix hashes")
	}
}

func TestReadNDJSONRejectsEmpty(t *testing.T) {
	// Empty or blank-only input means emission never produced a trace line
	// (e.g. the agent crashed before writing the header, or --trace was not
	// honored). That must surface as an error so the harness records a
	// TraceIssue rather than treating a crashed run as clean zero-attribution.
	if _, err := ReadNDJSON(strings.NewReader("")); err == nil {
		t.Fatal("expected error for empty input")
	}
	if _, err := ReadNDJSON(strings.NewReader("\n  \n\n")); err == nil {
		t.Fatal("expected error for blank-only input")
	}
}

func TestExclusiveZeroRoundTrips(t *testing.T) {
	// A parent whose children exactly tile its interval has exclusive time 0.
	// That 0 must survive a write-then-read round-trip — overwriting it with the
	// inclusive Duration on re-parse re-introduces the double-counting the
	// exclusive-time model exists to prevent.
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	spans := []Span{
		{Name: SpanGeneration, Start: base, End: base.Add(100 * time.Millisecond), Duration: 100 * time.Millisecond},
		{Name: SpanProviderConnect, Start: base, End: base.Add(60 * time.Millisecond), Duration: 60 * time.Millisecond},
		{Name: SpanToolExecution, Start: base.Add(60 * time.Millisecond), End: base.Add(100 * time.Millisecond), Duration: 40 * time.Millisecond},
	}
	deriveNesting(spans)
	if spans[0].Exclusive != 0 {
		t.Fatalf("setup invariant: generation exclusive = %v, want 0 (children tile it)", spans[0].Exclusive)
	}
	original := &TurnTrace{
		SessionID:   "s",
		RunID:       "r",
		StartedAt:   base,
		CompletedAt: base.Add(100 * time.Millisecond),
		Spans:       spans,
	}

	var buf bytes.Buffer
	if err := WriteNDJSON(&buf, original); err != nil {
		t.Fatalf("WriteNDJSON: %v", err)
	}
	parsed, err := ReadNDJSON(&buf)
	if err != nil {
		t.Fatalf("ReadNDJSON: %v", err)
	}
	var parsedGenExclusive time.Duration
	for _, s := range parsed.Spans {
		if s.Name == SpanGeneration {
			parsedGenExclusive = s.Exclusive
		}
	}
	if parsedGenExclusive != 0 {
		t.Fatalf("generation exclusive after round-trip = %v, want 0 (a written exclusive_ms:0 must be preserved, not overwritten with Duration)", parsedGenExclusive)
	}
	// The harness ranks by exclusive; the sum of exclusive across the run must
	// equal the wall (children carry the time, the parent contributes 0), with
	// no double-count.
	var exclusiveSum time.Duration
	for _, s := range parsed.Spans {
		exclusiveSum += s.Exclusive
	}
	if exclusiveSum != original.WallDuration() {
		t.Fatalf("exclusive sum = %v, want wall %v (double-count or gap on re-parse)", exclusiveSum, original.WallDuration())
	}
}

func TestIdenticalIntervalsNoCycle(t *testing.T) {
	// Two spans stamped at the exact same [start, end] would, under a symmetric
	// containment check, pick each other as parent and form a 2-cycle, dropping
	// both from top-level and zeroing AttributedDuration. The tie-break must
	// keep one top-level so AttributedDuration stays correct (no cycle, no
	// double-count).
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	spans := []Span{
		{Name: SpanGeneration, Start: base, End: base.Add(100 * time.Millisecond), Duration: 100 * time.Millisecond},
		{Name: SpanProviderConnect, Start: base, End: base.Add(100 * time.Millisecond), Duration: 100 * time.Millisecond},
	}
	deriveNesting(spans)

	var topLevel int
	var parents []string
	for _, s := range spans {
		if s.Parent == "" {
			topLevel++
		} else {
			parents = append(parents, s.Name+"->"+s.Parent)
		}
	}
	if topLevel == 0 {
		t.Fatalf("identical intervals formed a parent cycle; no top-level spans (parents=%v)", parents)
	}
	if topLevel != 1 {
		t.Fatalf("expected exactly one top-level span for co-extensive pair, got %d (parents=%v)", topLevel, parents)
	}
	// AttributedDuration is the single top-level span's duration — not 0 (cycle)
	// and not 2x (siblings double-count).
	tr := &TurnTrace{StartedAt: base, CompletedAt: base.Add(100 * time.Millisecond), Spans: spans}
	if got := tr.AttributedDuration(); got != 100*time.Millisecond {
		t.Fatalf("AttributedDuration = %v, want 100ms (one top-level span, no cycle)", got)
	}
}

func TestCounterPrecisionRoundTrip(t *testing.T) {
	// Counter values decode through json.Number so an int64 above 2^53
	// round-trips exactly instead of losing precision via float64.
	r := NewRecorder("s", "r", "")
	r.Start()
	const big = int64(1<<53 + 1) // 9007199254740993 — not representable exactly as float64
	r.Counter(CounterInputTokens, big)
	tr := r.Finish()

	var buf bytes.Buffer
	if err := WriteNDJSON(&buf, tr); err != nil {
		t.Fatalf("WriteNDJSON: %v", err)
	}
	parsed, err := ReadNDJSON(&buf)
	if err != nil {
		t.Fatalf("ReadNDJSON: %v", err)
	}
	if got := parsed.Counter(CounterInputTokens); got != big {
		t.Fatalf("counter round-trip = %d, want %d (float64 precision loss)", got, big)
	}
}

func TestOptionalEventKeysIncludeRuntimeCounters(t *testing.T) {
	keys := make(map[string]bool)
	for _, key := range OptionalEventKeys() {
		keys[key] = true
	}
	for _, counter := range []string{
		CounterCacheWriteTokens,
		CounterResponseChainReused,
		CounterResponseChainReset,
		CounterResponsesHTTPFallback,
		CounterPostureEscalations,
	} {
		key := "counter:" + counter
		if !keys[key] {
			t.Errorf("%s missing from OptionalEventKeys: %v", key, OptionalEventKeys())
		}
	}
}

package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/trace"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// keepAliveClient returns a client whose transport retains idle connections on
// EVERY platform. The shared default transport disables keep-alives on macOS,
// where the session correctly skips the probe — tests that assert the probe
// fires must therefore pin a reusable transport to stay platform-deterministic.
func keepAliveClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = false
	return &http.Client{Transport: transport}
}

// newPrewarmTestProvider builds a provider whose transport always allows the
// prewarm probe, regardless of host platform.
func newPrewarmTestProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	return newTestProviderWithOptions(t, Options{APIKey: "sk-test", HTTPClient: keepAliveClient()}, handler)
}

func waitPrewarmDone(t *testing.T, session *turnSession) {
	t.Helper()
	if session.prewarmDone == nil {
		t.Fatal("prewarm was never launched")
	}
	select {
	case <-session.prewarmDone:
	case <-time.After(10 * time.Second):
		t.Fatal("prewarm probe did not settle in time")
	}
}

func openOptimizedSession(t *testing.T, provider *Provider) *turnSession {
	t.Helper()
	session, err := NewTurnSessionProvider(provider, zeroruntime.ProviderCapabilities{}).OpenTurnSession(context.Background())
	if err != nil {
		t.Fatalf("OpenTurnSession: %v", err)
	}
	return session.(*turnSession)
}

func TestTurnSessionPrewarmSendsSingleHEAD(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	var lastMethod atomic.Value
	provider := newPrewarmTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		lastMethod.Store(r.Method)
		w.WriteHeader(http.StatusMethodNotAllowed)
	})

	session := openOptimizedSession(t, provider)
	if err := session.Prewarm(context.Background()); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}
	waitPrewarmDone(t, session)

	if got := requests.Load(); got != 1 {
		t.Fatalf("prewarm sent %d requests, want exactly 1 (no retries even on 405)", got)
	}
	if method, _ := lastMethod.Load().(string); method != http.MethodHead {
		t.Fatalf("prewarm method = %q, want HEAD", method)
	}

	// A second Prewarm (e.g. after a mid-run swap re-open) must not re-probe.
	if err := session.Prewarm(context.Background()); err != nil {
		t.Fatalf("second Prewarm: %v", err)
	}
	waitPrewarmDone(t, session)
	if got := requests.Load(); got != 1 {
		t.Fatalf("second Prewarm re-probed: %d requests, want 1", got)
	}
}

func TestTurnSessionPrewarmNonFatalOnConnectError(t *testing.T) {
	t.Parallel()
	// A server that is immediately closed leaves a port that refuses connections.
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := server.URL
	server.Close()

	provider, err := New(Options{APIKey: "sk-test", BaseURL: deadURL, Model: "gpt-test", HTTPClient: keepAliveClient()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	session := openOptimizedSession(t, provider)
	if err := session.Prewarm(context.Background()); err != nil {
		t.Fatalf("Prewarm must be advisory even when the probe cannot connect, got %v", err)
	}
	waitPrewarmDone(t, session)
}

func TestTurnSessionPrewarmTraceStamps(t *testing.T) {
	t.Parallel()
	provider := newPrewarmTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	recorder := trace.NewRecorder("session-test", "run-1", "test")
	recorder.Start()
	ctx := trace.WithContext(context.Background(), recorder)

	session := openOptimizedSession(t, provider)
	if err := session.Prewarm(ctx); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}
	waitPrewarmDone(t, session)

	tr := recorder.Finish()
	if got := tr.Counter(trace.CounterPrewarmAttempts); got != 1 {
		t.Fatalf("prewarm_attempts = %d, want 1", got)
	}
	var sawSpan bool
	for _, span := range tr.Spans {
		if span.Name == trace.SpanProviderPrewarm {
			sawSpan = true
		}
	}
	if !sawSpan {
		t.Fatalf("missing %s span in %+v", trace.SpanProviderPrewarm, tr.Spans)
	}
}

func TestTurnSessionStreamDelegatesToProvider(t *testing.T) {
	t.Parallel()
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}

	direct := newTestProvider(t, handler)
	viaSession := newTestProvider(t, handler)
	session := openOptimizedSession(t, viaSession)

	request := zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	}
	collect := func(stream <-chan zeroruntime.StreamEvent) []zeroruntime.StreamEvent {
		var events []zeroruntime.StreamEvent
		for event := range stream {
			events = append(events, event)
		}
		return events
	}

	directStream, err := direct.StreamCompletion(context.Background(), request)
	if err != nil {
		t.Fatalf("direct StreamCompletion: %v", err)
	}
	sessionStream, err := session.Stream(context.Background(), request)
	if err != nil {
		t.Fatalf("session Stream: %v", err)
	}

	directEvents := collect(directStream)
	sessionEvents := collect(sessionStream)
	if len(directEvents) != len(sessionEvents) {
		t.Fatalf("event counts diverged: direct=%d session=%d", len(directEvents), len(sessionEvents))
	}
	for i := range directEvents {
		if directEvents[i].Type != sessionEvents[i].Type || directEvents[i].Content != sessionEvents[i].Content {
			t.Fatalf("event %d diverged: direct=%+v session=%+v", i, directEvents[i], sessionEvents[i])
		}
	}
}

func TestTurnSessionFingerprintIgnoresMessages(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(http.ResponseWriter, *http.Request) {})
	session := openOptimizedSession(t, provider)

	tools := []zeroruntime.ToolDefinition{{Name: "read_file", Parameters: map[string]any{"type": "object"}}}
	base := session.computeFingerprint(zeroruntime.CompletionRequest{
		Messages:       []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "turn one"}},
		Tools:          tools,
		PromptCacheKey: "session-a",
	})
	grown := session.computeFingerprint(zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{
			{Role: zeroruntime.MessageRoleUser, Content: "turn one"},
			{Role: zeroruntime.MessageRoleAssistant, Content: "reply"},
			{Role: zeroruntime.MessageRoleUser, Content: "turn two"},
		},
		Tools:          tools,
		PromptCacheKey: "session-a",
	})
	if base != grown {
		t.Fatal("fingerprint changed when only messages grew — conversation content must not be an input")
	}
}

func TestTurnSessionFingerprintDriftOnCacheKey(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(http.ResponseWriter, *http.Request) {})
	session := openOptimizedSession(t, provider)

	tools := []zeroruntime.ToolDefinition{{Name: "read_file", Parameters: map[string]any{"type": "object"}}}
	base := session.computeFingerprint(zeroruntime.CompletionRequest{Tools: tools, PromptCacheKey: "session-a"})
	changed := session.computeFingerprint(zeroruntime.CompletionRequest{Tools: tools, PromptCacheKey: "session-b"})
	if base == changed {
		t.Fatal("fingerprint did not drift on a changed prompt-cache key — it is a serialized request field")
	}
}

func TestTurnSessionFingerprintDriftOnToolsAndEffort(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(http.ResponseWriter, *http.Request) {})
	session := openOptimizedSession(t, provider)

	base := session.computeFingerprint(zeroruntime.CompletionRequest{
		Tools: []zeroruntime.ToolDefinition{{Name: "read_file", Parameters: map[string]any{"type": "object"}}},
	})
	changedTools := session.computeFingerprint(zeroruntime.CompletionRequest{
		Tools: []zeroruntime.ToolDefinition{{Name: "write_file", Parameters: map[string]any{"type": "object"}}},
	})
	if base == changedTools {
		t.Fatal("fingerprint did not drift on a changed tool set")
	}
	changedEffort := session.computeFingerprint(zeroruntime.CompletionRequest{
		Tools:           []zeroruntime.ToolDefinition{{Name: "read_file", Parameters: map[string]any{"type": "object"}}},
		ReasoningEffort: "high",
	})
	if base == changedEffort {
		t.Fatal("fingerprint did not drift on a changed reasoning effort")
	}
	// Unrecognized efforts normalize to omitted — same as empty.
	droppedEffort := session.computeFingerprint(zeroruntime.CompletionRequest{
		Tools:           []zeroruntime.ToolDefinition{{Name: "read_file", Parameters: map[string]any{"type": "object"}}},
		ReasoningEffort: "bogus",
	})
	if base != droppedEffort {
		t.Fatal("an effort the wire would omit must fingerprint like no effort")
	}
	changedTier := session.computeFingerprint(zeroruntime.CompletionRequest{
		Tools:       []zeroruntime.ToolDefinition{{Name: "read_file", Parameters: map[string]any{"type": "object"}}},
		ServiceTier: "priority",
	})
	if base == changedTier {
		t.Fatal("fingerprint did not drift on a changed service tier")
	}
}

func TestTurnSessionFingerprintDriftOnToolDescription(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(http.ResponseWriter, *http.Request) {})
	session := openOptimizedSession(t, provider)

	base := session.computeFingerprint(zeroruntime.CompletionRequest{
		Tools: []zeroruntime.ToolDefinition{{Name: "read_file", Description: "Reads a file.", Parameters: map[string]any{"type": "object"}}},
	})
	changedDescription := session.computeFingerprint(zeroruntime.CompletionRequest{
		Tools: []zeroruntime.ToolDefinition{{Name: "read_file", Description: "Reads a file, now with ranges.", Parameters: map[string]any{"type": "object"}}},
	})
	if base == changedDescription {
		t.Fatal("fingerprint did not drift on a description-only tool change — descriptions are model-visible request bytes")
	}
}

func TestTurnSessionFingerprintDriftOnToolReorder(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(http.ResponseWriter, *http.Request) {})
	session := openOptimizedSession(t, provider)

	forward := session.computeFingerprint(zeroruntime.CompletionRequest{Tools: []zeroruntime.ToolDefinition{
		{Name: "alpha", Parameters: map[string]any{"type": "object"}},
		{Name: "beta", Parameters: map[string]any{"type": "object"}},
	}})
	reversed := session.computeFingerprint(zeroruntime.CompletionRequest{Tools: []zeroruntime.ToolDefinition{
		{Name: "beta", Parameters: map[string]any{"type": "object"}},
		{Name: "alpha", Parameters: map[string]any{"type": "object"}},
	}})
	if forward == reversed {
		t.Fatal("fingerprint did not drift on a tool reorder — request serialization preserves tool order, so a reorder changes the wire bytes")
	}
}

// TestTurnSessionPrewarmDoesNotStampProviderConnect guards the A/B metric: the
// prewarm probe must appear ONLY under provider_prewarm — a provider_connect
// stamp from the probe would contaminate the exact span the benchmark compares.
func TestTurnSessionPrewarmDoesNotStampProviderConnect(t *testing.T) {
	t.Parallel()
	provider := newPrewarmTestProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	recorder := trace.NewRecorder("session-test", "run-1", "test")
	recorder.Start()
	ctx := trace.WithContext(context.Background(), recorder)

	session := openOptimizedSession(t, provider)
	if err := session.Prewarm(ctx); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}
	waitPrewarmDone(t, session)

	tr := recorder.Finish()
	for _, span := range tr.Spans {
		if span.Name == trace.SpanProviderConnect {
			t.Fatalf("prewarm stamped a %s span — it must stay solely under %s", trace.SpanProviderConnect, trace.SpanProviderPrewarm)
		}
	}
}

// TestTurnSessionPrewarmSkippedWhenKeepAlivesDisabled verifies the probe is not
// spent when the transport cannot retain idle connections (the macOS shared
// transport): no request, no trace stamps, and the done channel still closes.
func TestTurnSessionPrewarmSkippedWhenKeepAlivesDisabled(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer server.Close()

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	provider, err := New(Options{
		APIKey:     "sk-test",
		BaseURL:    server.URL,
		Model:      "gpt-test",
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	recorder := trace.NewRecorder("session-test", "run-1", "test")
	recorder.Start()
	ctx := trace.WithContext(context.Background(), recorder)

	session := openOptimizedSession(t, provider)
	if err := session.Prewarm(ctx); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}
	waitPrewarmDone(t, session)

	if got := requests.Load(); got != 0 {
		t.Fatalf("keep-alive-disabled transport still received %d probe requests, want 0", got)
	}
	tr := recorder.Finish()
	if got := tr.Counter(trace.CounterPrewarmAttempts); got != 0 {
		t.Fatalf("prewarm_attempts = %d, want 0 for a skipped probe", got)
	}
}

func TestTurnSessionPrefixCounters(t *testing.T) {
	t.Parallel()
	handler := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
	provider := newTestProvider(t, handler)
	session := openOptimizedSession(t, provider)

	recorder := trace.NewRecorder("session-test", "run-1", "test")
	recorder.Start()
	ctx := trace.WithContext(context.Background(), recorder)

	stable := zeroruntime.CompletionRequest{Tools: []zeroruntime.ToolDefinition{{Name: "read_file", Parameters: map[string]any{"type": "object"}}}}
	drain := func(request zeroruntime.CompletionRequest) {
		stream, err := session.Stream(ctx, request)
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		for range stream {
		}
	}

	drain(stable)                                                                                                                                 // seeds — neither stable nor drift
	drain(stable)                                                                                                                                 // stable
	drain(zeroruntime.CompletionRequest{Tools: []zeroruntime.ToolDefinition{{Name: "write_file", Parameters: map[string]any{"type": "object"}}}}) // drift

	tr := recorder.Finish()
	if got := tr.Counter(trace.CounterPrefixStable); got != 1 {
		t.Fatalf("prefix_stable = %d, want 1", got)
	}
	if got := tr.Counter(trace.CounterPrefixDrift); got != 1 {
		t.Fatalf("prefix_drift = %d, want 1", got)
	}
}

func TestTurnSessionCloseIdempotentAndCompactUnsupported(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(http.ResponseWriter, *http.Request) {})
	session := openOptimizedSession(t, provider)
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := session.Compact(context.Background(), zeroruntime.CompletionRequest{}); !errors.Is(err, zeroruntime.ErrCompactionUnsupported) {
		t.Fatalf("Compact error = %v, want ErrCompactionUnsupported", err)
	}
}

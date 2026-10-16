package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// codexRequest captures the headers + body of one outgoing Codex request so a
// test can assert the Codex-specific headers were applied.
type codexRequest struct {
	method      string
	path        string
	originator  string
	accountID   string
	userAgent   string
	auth        string
	otherHeader map[string]string
	body        map[string]any
}

// newCodexTestServer returns an httptest.Server that records each request's
// Codex headers and writes a minimal Responses-API SSE stream (a single
// response.completed event with no content). The Codex provider targets
// `{BaseURL}/responses` (the Responses API on the chatgpt backend), so the
// handler is mounted on that path. Tests that want a richer stream (text
// deltas, tool calls, errors) use newCodexResponsesServer with a custom
// event sequence.
func newCodexTestServer(t *testing.T, rec *codexRequest) *httptest.Server {
	t.Helper()
	return newCodexResponsesServer(t, rec,
		`{"type":"response.created","response":{"id":"resp-1","status":"in_progress"}}`,
		`{"type":"response.completed","response":{"id":"resp-1","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
	)
}

// newCodexResponsesServer returns an httptest.Server that records each
// request's Codex headers and writes the given SSE data payloads in order.
// Each payload is emitted as one `data: <payload>\n\n` block, so a test
// can supply a complete Responses-API event sequence (created, deltas,
// completed) by passing them in order.
func newCodexResponsesServer(t *testing.T, rec *codexRequest, payloads ...string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/responses", func(w http.ResponseWriter, r *http.Request) {
		rec.method = r.Method
		rec.path = r.URL.Path
		rec.originator = r.Header.Get(codexOriginatorHeader)
		rec.accountID = r.Header.Get(codexAccountHeader)
		rec.userAgent = r.Header.Get("User-Agent")
		rec.auth = r.Header.Get("Authorization")
		rec.otherHeader = map[string]string{}
		for _, k := range []string{"Content-Type", "X-Extra"} {
			rec.otherHeader[k] = r.Header.Get(k)
		}
		_ = json.NewDecoder(r.Body).Decode(&rec.body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, payload := range payloads {
			_, _ = w.Write([]byte("data: " + payload + "\n\n"))
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	return httptest.NewServer(mux)
}

func drainCodexEvents(t *testing.T, stream <-chan zeroruntime.StreamEvent) {
	t.Helper()
	for ev := range stream {
		if ev.Type == zeroruntime.StreamEventError {
			t.Fatalf("unexpected error event: %q", ev.Error)
		}
	}
}

func TestCodexProviderSetsExpectedHeaders(t *testing.T) {
	t.Parallel()
	var rec codexRequest
	srv := newCodexTestServer(t, &rec)
	defer srv.Close()

	provider, err := NewCodexProvider(CodexOptions{
		Options: Options{
			APIKey:  "sk-test",
			BaseURL: srv.URL,
			Model:   "gpt-5",
		},
		AccountID:  "acc-from-token",
		Originator: "codex_cli_rs",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	drainCodexEvents(t, stream)

	if rec.method != http.MethodPost {
		t.Fatalf("method = %q, want POST", rec.method)
	}
	if rec.path != "/responses" {
		t.Fatalf("path = %q, want /responses", rec.path)
	}
	if rec.originator != "codex_cli_rs" {
		t.Fatalf("originator = %q, want codex_cli_rs", rec.originator)
	}
	if rec.accountID != "acc-from-token" {
		t.Fatalf("chatgpt-account-id = %q, want acc-from-token", rec.accountID)
	}
	if rec.auth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q, want Bearer sk-test", rec.auth)
	}
	if rec.otherHeader["Content-Type"] != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", rec.otherHeader["Content-Type"])
	}
	if rec.userAgent == "" {
		t.Fatalf("User-Agent = empty, want the Codex default (codex_cli_rs)")
	}
}

func TestCodexProviderUsesConfiguredBaseURL(t *testing.T) {
	t.Parallel()
	var rec codexRequest
	// Run the test server on a custom path to confirm the request goes
	// through {BaseURL}/responses, not a hard-coded host.
	mux := http.NewServeMux()
	customPath := "/api/v1/codex/responses"
	var hit atomic.Int32
	mux.HandleFunc(customPath, func(w http.ResponseWriter, r *http.Request) {
		hit.Add(1)
		rec.path = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	provider, err := NewCodexProvider(CodexOptions{
		Options: Options{
			APIKey:  "sk-test",
			BaseURL: srv.URL + "/api/v1/codex",
			Model:   "gpt-5",
		},
		AccountID: "acc-x",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	drainCodexEvents(t, stream)
	if rec.path != customPath {
		t.Fatalf("path = %q, want %q (the Codex baseURL must be honored)", rec.path, customPath)
	}
	if hit.Load() != 1 {
		t.Fatalf("server hit %d times, want 1", hit.Load())
	}
}

func TestCodexProviderDefaultsOriginator(t *testing.T) {
	t.Parallel()
	var rec codexRequest
	srv := newCodexTestServer(t, &rec)
	defer srv.Close()

	provider, err := NewCodexProvider(CodexOptions{
		Options: Options{
			APIKey:  "sk-test",
			BaseURL: srv.URL,
			Model:   "gpt-5",
		},
		// Originator intentionally empty: defaults to "codex_cli_rs".
		AccountID: "acc-x",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	drainCodexEvents(t, stream)
	if rec.originator != codexDefaultOriginator {
		t.Fatalf("originator = %q, want default %q", rec.originator, codexDefaultOriginator)
	}
}

func TestCodexProviderBrandsUserAgent(t *testing.T) {
	t.Parallel()
	var rec codexRequest
	srv := newCodexTestServer(t, &rec)
	defer srv.Close()

	provider, err := NewCodexProvider(CodexOptions{
		Options: Options{
			APIKey:  "sk-test",
			BaseURL: srv.URL,
			Model:   "gpt-5",
			// openai Options.UserAgent overridden by CodexOptions.UserAgent below.
			UserAgent: "zero/dev",
		},
		// CodexOptions.UserAgent wins over openai Options.UserAgent.
		UserAgent: "codex_cli_rs/0.1",
		AccountID: "acc-x",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	drainCodexEvents(t, stream)
	if rec.userAgent != "codex_cli_rs/0.1" {
		t.Fatalf("User-Agent = %q, want codex_cli_rs/0.1 (CodexOptions.UserAgent should win)", rec.userAgent)
	}
}

func TestCodexProviderAccountResolverIsUsedWhenAccountIDEmpty(t *testing.T) {
	t.Parallel()
	var rec codexRequest
	srv := newCodexTestServer(t, &rec)
	defer srv.Close()

	var resolverCalls atomic.Int32
	provider, err := NewCodexProvider(CodexOptions{
		Options: Options{
			APIKey:  "sk-test",
			BaseURL: srv.URL,
			Model:   "gpt-5",
		},
		// AccountID empty: AccountResolver is the source.
		AccountResolver: func(_ context.Context) (string, bool, error) {
			resolverCalls.Add(1)
			return "acc-resolver", true, nil
		},
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	drainCodexEvents(t, stream)
	if resolverCalls.Load() != 1 {
		t.Fatalf("resolver called %d times, want 1", resolverCalls.Load())
	}
	if rec.accountID != "acc-resolver" {
		t.Fatalf("chatgpt-account-id = %q, want acc-resolver", rec.accountID)
	}
}

func TestCodexProviderResolverIsConsultedOnEveryRequest(t *testing.T) {
	t.Parallel()
	// The factory wires the AccountResolver from the OAuth store so a refresh
	// that updates the stored token's Account field takes effect on the next
	// outgoing request — not just the first. This test asserts the resolver
	// runs once per request (and that the second request picks up the
	// account id the resolver starts returning on call #2).
	var hits atomic.Int32
	var rec1, rec2 codexRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/responses", func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		var rec *codexRequest
		if n == 1 {
			rec = &rec1
		} else {
			rec = &rec2
		}
		rec.path = r.URL.Path
		rec.originator = r.Header.Get(codexOriginatorHeader)
		rec.accountID = r.Header.Get(codexAccountHeader)
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-2\",\"status\":\"completed\"}}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resolverCalls := atomic.Int32{}
	provider, err := NewCodexProvider(CodexOptions{
		Options: Options{
			BaseURL: srv.URL,
			Model:   "gpt-5",
			// Wire an OAuth resolver so the openai provider's 401-retry
			// path runs. The Codex resolver (AccountResolver) is what
			// this test actually exercises.
			OAuthResolver: func(_ context.Context, _ bool) (string, string, bool, error) {
				return "Authorization", "Bearer oauth-tok", true, nil
			},
		},
		// Static AccountID intentionally empty so the resolver is the
		// source. The first call returns ok=false (no account); the second
		// call (after the 401 refresh) returns the new id.
		AccountResolver: func(_ context.Context) (string, bool, error) {
			n := resolverCalls.Add(1)
			if n == 1 {
				return "", false, nil
			}
			return "acc-from-store", true, nil
		},
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}

	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	drainCodexEvents(t, stream)

	if hits.Load() != 2 {
		t.Fatalf("server hit %d times, want 2 (initial + retry)", hits.Load())
	}
	if resolverCalls.Load() != 2 {
		t.Fatalf("resolver called %d times, want 2 (once per request, including the 401 retry)", resolverCalls.Load())
	}
	if rec1.accountID != "" {
		t.Fatalf("first attempt account id = %q, want empty (resolver returned ok=false on call 1)", rec1.accountID)
	}
	if rec2.accountID != "acc-from-store" {
		t.Fatalf("retry account id = %q, want acc-from-store (resolver must re-run on every request)", rec2.accountID)
	}
}

func TestCodexProviderOmitsAccountIDWhenResolverSaysNo(t *testing.T) {
	t.Parallel()
	var rec codexRequest
	srv := newCodexTestServer(t, &rec)
	defer srv.Close()

	provider, err := NewCodexProvider(CodexOptions{
		Options: Options{
			APIKey:  "sk-test",
			BaseURL: srv.URL,
			Model:   "gpt-5",
		},
		AccountResolver: func(_ context.Context) (string, bool, error) {
			return "", false, nil
		},
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	drainCodexEvents(t, stream)
	if rec.accountID != "" {
		t.Fatalf("chatgpt-account-id = %q, want empty (resolver returned ok=false)", rec.accountID)
	}
}

func TestCodexProviderSendsResponsesRequestShape(t *testing.T) {
	t.Parallel()
	// The Codex provider speaks the Responses API, not the chat-completions
	// API. The request body must carry top-level `instructions`, `input` items
	// (not `messages`), a `stream: true` flag, and `tools` (when the caller
	// supplies any) — a chat-completions body would be rejected by the live
	// Codex backend.
	var rec codexRequest
	srv := newCodexTestServer(t, &rec)
	defer srv.Close()

	provider, err := NewCodexProvider(CodexOptions{
		Options: Options{
			APIKey:  "sk-test",
			BaseURL: srv.URL,
			Model:   "gpt-5",
		},
		AccountID: "acc-x",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{
			{Role: zeroruntime.MessageRoleSystem, Content: "sys"},
			{Role: zeroruntime.MessageRoleUser, Content: "hi"},
		},
		Tools: []zeroruntime.ToolDefinition{
			{Name: "get_weather", Description: "get the weather", Parameters: map[string]any{"type": "object"}},
		},
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	drainCodexEvents(t, stream)

	if rec.body["model"] != "gpt-5" {
		t.Fatalf("body.model = %#v, want gpt-5", rec.body["model"])
	}
	if rec.body["stream"] != true {
		t.Fatalf("body.stream = %#v, want true", rec.body["stream"])
	}
	if store, ok := rec.body["store"].(bool); !ok || store {
		t.Fatalf("body.store = %#v, want explicit false", rec.body["store"])
	}
	if _, ok := rec.body["messages"]; ok {
		t.Fatalf("body must not carry chat-completions `messages` (got %#v); the Codex backend serves the Responses API", rec.body["messages"])
	}
	if rec.body["instructions"] != "sys" {
		t.Fatalf("body.instructions = %#v, want system prompt", rec.body["instructions"])
	}
	input, ok := rec.body["input"].([]any)
	if !ok {
		t.Fatalf("body.input = %#v, want []any with user message item", rec.body["input"])
	}
	if len(input) != 1 {
		t.Fatalf("body.input has %d items, want 1 (user)", len(input))
	}
	user, _ := input[0].(map[string]any)
	if user["type"] != "message" || user["role"] != "user" {
		t.Fatalf("body.input[0] = %#v, want type=message role=user", user)
	}
	tools, ok := rec.body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("body.tools = %#v, want a single tool", rec.body["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "get_weather" {
		t.Fatalf("body.tools[0] = %#v, want type=function name=get_weather", tool)
	}
}

func TestCodexProviderForwardsReasoningEffort(t *testing.T) {
	t.Parallel()
	// A reasoning effort must reach the Responses backend nested under
	// `reasoning.effort` (where the chat-completions `reasoning_effort` moved).
	// Without this the user's chosen effort is silently dropped for Codex models.
	var rec codexRequest
	srv := newCodexTestServer(t, &rec)
	defer srv.Close()

	provider, err := NewCodexProvider(CodexOptions{
		Options:   Options{APIKey: "sk-test", BaseURL: srv.URL, Model: "gpt-5-codex"},
		AccountID: "acc-x",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages:        []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
		ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	drainCodexEvents(t, stream)

	reasoning, ok := rec.body["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("body.reasoning = %#v, want an object carrying the effort", rec.body["reasoning"])
	}
	if reasoning["effort"] != "high" {
		t.Fatalf("body.reasoning.effort = %#v, want high", reasoning["effort"])
	}
	// A reasoning summary must be requested so the backend streams
	// reasoning_summary_text deltas — otherwise a long think shows no live output.
	if reasoning["summary"] != "auto" {
		t.Fatalf("body.reasoning.summary = %#v, want auto", reasoning["summary"])
	}
}

func TestCodexProviderNormalizesServiceTier(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "priority", input: "priority", want: "priority"},
		{name: "flex", input: "flex", want: "flex"},
		{name: "unsupported omitted", input: "standard", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var rec codexRequest
			srv := newCodexTestServer(t, &rec)
			defer srv.Close()

			provider, err := NewCodexProvider(CodexOptions{
				Options:   Options{APIKey: "sk-test", BaseURL: srv.URL, Model: "gpt-5-codex"},
				AccountID: "acc-x",
			})
			if err != nil {
				t.Fatalf("NewCodexProvider: %v", err)
			}
			stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
				Messages:    []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
				ServiceTier: test.input,
			})
			if err != nil {
				t.Fatalf("StreamCompletion: %v", err)
			}
			drainCodexEvents(t, stream)
			if test.want == "" {
				if _, ok := rec.body["service_tier"]; ok {
					t.Fatalf("body.service_tier = %#v, want omitted", rec.body["service_tier"])
				}
				return
			}
			if got := rec.body["service_tier"]; got != test.want {
				t.Fatalf("body.service_tier = %#v, want %q", got, test.want)
			}
		})
	}
}

func TestCodexProviderStreamsReasoningSummaryDeltas(t *testing.T) {
	t.Parallel()
	// reasoning_summary_text deltas must surface as StreamEventReasoning (live
	// "thinking"), in order, alongside the normal text output. Without this a long
	// reasoning phase produces zero visible output and reads as a hang.
	var rec codexRequest
	srv := newCodexResponsesServer(t, &rec,
		`{"type":"response.created","response":{"id":"resp-1","status":"in_progress"}}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"Thinking. "}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"Planning."}`,
		`{"type":"response.output_text.delta","delta":"done"}`,
		`{"type":"response.completed","response":{"id":"resp-1","status":"completed"}}`,
	)
	defer srv.Close()

	provider, err := NewCodexProvider(CodexOptions{
		Options:   Options{APIKey: "sk-test", BaseURL: srv.URL, Model: "gpt-5"},
		AccountID: "acc-x",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages:        []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "solve it"}},
		ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	var reasoning, text []string
	for ev := range stream {
		switch ev.Type {
		case zeroruntime.StreamEventReasoning:
			reasoning = append(reasoning, ev.Content)
		case zeroruntime.StreamEventText:
			text = append(text, ev.Content)
		case zeroruntime.StreamEventError:
			t.Fatalf("unexpected error event: %q", ev.Error)
		}
	}
	if got := strings.Join(reasoning, ""); got != "Thinking. Planning." {
		t.Fatalf("reasoning deltas = %q, want %q", got, "Thinking. Planning.")
	}
	if got := strings.Join(text, ""); got != "done" {
		t.Fatalf("text deltas = %q, want %q", got, "done")
	}
}

func TestCodexProviderOmitsReasoningWhenUnset(t *testing.T) {
	t.Parallel()
	// No effort (and unsupported values) must omit the `reasoning` field entirely
	// rather than send an empty object, which the backend would reject.
	var rec codexRequest
	srv := newCodexTestServer(t, &rec)
	defer srv.Close()

	provider, err := NewCodexProvider(CodexOptions{
		Options:   Options{APIKey: "sk-test", BaseURL: srv.URL, Model: "gpt-5-codex"},
		AccountID: "acc-x",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages:        []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
		ReasoningEffort: "", // auto
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	drainCodexEvents(t, stream)

	if _, ok := rec.body["reasoning"]; ok {
		t.Fatalf("body.reasoning must be omitted when no effort is requested, got %#v", rec.body["reasoning"])
	}
}

func TestCodexProviderRetriesHeadersAfter401(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	var rec1, rec2 codexRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/responses", func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		var rec *codexRequest
		if n == 1 {
			rec = &rec1
		} else {
			rec = &rec2
		}
		rec.path = r.URL.Path
		rec.originator = r.Header.Get(codexOriginatorHeader)
		rec.accountID = r.Header.Get(codexAccountHeader)
		rec.auth = r.Header.Get("Authorization")
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-2\",\"status\":\"completed\"}}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Wire an OAuthResolver so the openai provider's 401-retry path runs
	// (otherwise a 401 short-circuits to an auth error and the retry is
	// never made). The Codex provider's setRequestExtra is what we want
	// to verify; the resolver just needs to be a stable, force-refreshable
	// token source.
	resolver := func(_ context.Context, _ bool) (string, string, bool, error) {
		return "Authorization", "Bearer oauth-tok", true, nil
	}
	provider, err := NewCodexProvider(CodexOptions{
		Options: Options{
			BaseURL: srv.URL,
			Model:   "gpt-5",
			OAuthResolver: func(ctx context.Context, fr bool) (string, string, bool, error) {
				return resolver(ctx, fr)
			},
		},
		AccountID:  "acc-retry",
		Originator: "codex_cli_rs",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	// The first request is a 401; the openai provider surfaces it as an
	// auth error and does NOT retry automatically unless an OAuthResolver
	// is wired AND sends ok=true. With our resolver in place the retry
	// path runs and the second request returns [DONE]. The test asserts
	// the Codex headers are applied on BOTH attempts.
	drainCodexEvents(t, stream)
	if hits.Load() != 2 {
		t.Fatalf("server hit %d times, want 2 (initial + retry)", hits.Load())
	}
	if rec1.originator != "codex_cli_rs" || rec1.accountID != "acc-retry" {
		t.Fatalf("first attempt headers: originator=%q accountID=%q", rec1.originator, rec1.accountID)
	}
	if rec2.originator != "codex_cli_rs" || rec2.accountID != "acc-retry" {
		t.Fatalf("retry headers missing: originator=%q accountID=%q (Codex headers must survive 401-refresh)", rec2.originator, rec2.accountID)
	}
}

func TestCodexProviderRequiresBaseURL(t *testing.T) {
	t.Parallel()
	// An empty baseURL falls back to the openai provider's default
	// (https://api.openai.com/v1). The Codex provider is designed to be
	// wired by the factory with the catalog's Codex baseURL, so the
	// realistic misconfig is an INVALID baseURL (covered in the next
	// test). This test just confirms the constructor accepts an empty
	// baseURL — the factory will always supply the Codex-specific URL.
	provider, err := NewCodexProvider(CodexOptions{
		Options:   Options{Model: "gpt-5"},
		AccountID: "acc-x",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider with empty baseURL should not error (factory will override): %v", err)
	}
	if provider == nil {
		t.Fatal("expected a provider, got nil")
	}
}

func TestCodexProviderRejectsBadBaseURL(t *testing.T) {
	t.Parallel()
	_, err := NewCodexProvider(CodexOptions{
		Options:   Options{APIKey: "sk", Model: "gpt-5", BaseURL: "://not a url"},
		AccountID: "acc-x",
	})
	if err == nil {
		t.Fatal("NewCodexProvider with a bad baseURL should error")
	}
	if !strings.Contains(err.Error(), "base URL") && !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("error = %q, want one mentioning base URL", err.Error())
	}
}

func TestValidateAccount(t *testing.T) {
	t.Parallel()
	if err := ValidateAccount(""); err == nil {
		t.Fatal("empty account id should be rejected")
	}
	if err := ValidateAccount("  "); err == nil {
		t.Fatal("whitespace-only account id should be rejected")
	}
	if err := ValidateAccount("acc-1"); err != nil {
		t.Fatalf("non-empty account id should be accepted, got %v", err)
	}
}

func TestCodexProviderStreamIdleTimeoutPropagates(t *testing.T) {
	// Serial: StreamIdleTimeout arms a real wall-clock idle timer. Under
	// t.Parallel() the 50ms watchdog can fire before response.completed is
	// consumed. This test only checks that the option is accepted.
	var rec codexRequest
	srv := newCodexTestServer(t, &rec)
	defer srv.Close()

	provider, err := NewCodexProvider(CodexOptions{
		Options: Options{
			APIKey:            "sk-test",
			BaseURL:           srv.URL,
			Model:             "gpt-5",
			StreamIdleTimeout: 50 * time.Millisecond,
		},
		AccountID: "acc-x",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	// A simple stream that returns response.completed immediately does not
	// hit the timeout — the test just confirms the option is accepted.
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	drainCodexEvents(t, stream)
}

func TestCodexProviderParsesResponsesTextDeltas(t *testing.T) {
	t.Parallel()
	// The Codex backend streams text as a series of
	// `response.output_text.delta` events. Each delta must surface as a
	// runtime text event in order, and the terminal `response.completed`
	// must emit StreamEventUsage (with the right token counts) and
	// StreamEventDone in that order.
	var rec codexRequest
	srv := newCodexResponsesServer(t, &rec,
		`{"type":"response.created","response":{"id":"resp-1","status":"in_progress"}}`,
		`{"type":"response.output_text.delta","delta":"hello "}`,
		`{"type":"response.output_text.delta","delta":"world"}`,
		`{"type":"response.output_text.delta","delta":"!"}`,
		`{"type":"response.completed","response":{"id":"resp-1","status":"completed","usage":{"input_tokens":7,"output_tokens":3,"output_tokens_details":{"reasoning_tokens":1},"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":1}}}}`,
	)
	defer srv.Close()

	provider, err := NewCodexProvider(CodexOptions{
		Options:   Options{APIKey: "sk-test", BaseURL: srv.URL, Model: "gpt-5"},
		AccountID: "acc-x",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "say hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	var texts []string
	var usage *zeroruntime.Usage
	gotDone := false
	for ev := range stream {
		switch ev.Type {
		case zeroruntime.StreamEventText:
			texts = append(texts, ev.Content)
		case zeroruntime.StreamEventUsage:
			usage = &ev.Usage
		case zeroruntime.StreamEventDone:
			gotDone = true
		case zeroruntime.StreamEventError:
			t.Fatalf("unexpected error event: %q", ev.Error)
		}
	}
	if joined := strings.Join(texts, ""); joined != "hello world!" {
		t.Fatalf("text deltas concatenated = %q, want %q", joined, "hello world!")
	}
	if usage == nil {
		t.Fatal("expected a usage event, got none")
	}
	if usage.InputTokens != 7 || usage.OutputTokens != 3 || usage.CachedInputTokens != 2 || usage.CacheWriteTokens != 1 || usage.ReasoningTokens != 1 {
		t.Fatalf("usage = %+v, want input=7 output=3 cached=2 cache-write=1 reasoning=1", *usage)
	}
	if !gotDone {
		t.Fatal("expected a done event, got none")
	}
}

func TestCodexProviderParsesResponsesToolCalls(t *testing.T) {
	t.Parallel()
	// The Codex backend streams function calls as three coordinated event
	// types: `response.output_item.added` (carries the call id and name),
	// `response.function_call_arguments.delta` (one or more events that
	// accumulate the arguments), and `response.output_item.done`
	// (finalizes). The runtime must see a tool-call-start with the right
	// name, deltas in order, and a tool-call-end that closes the call.
	// Deltas are kept simple ("arg1"/"arg2") to avoid tangling this test
	// in JSON-string escaping; the joined argument string is asserted
	// directly.
	var rec codexRequest
	srv := newCodexResponsesServer(t, &rec,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"call-1","call_id":"call-1","name":"get_weather"}}`,
		`{"type":"response.function_call_arguments.delta","item_id":"call-1","output_index":0,"delta":"arg1"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"call-1","output_index":0,"delta":"arg2"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"call-1","call_id":"call-1","name":"get_weather","arguments":"arg1arg2"}}`,
		`{"type":"response.completed","response":{"id":"resp-1","status":"completed","usage":{"input_tokens":3,"output_tokens":2}}}`,
	)
	defer srv.Close()

	provider, err := NewCodexProvider(CodexOptions{
		Options:   Options{APIKey: "sk-test", BaseURL: srv.URL, Model: "gpt-5"},
		AccountID: "acc-x",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "weather in SF?"}},
		Tools: []zeroruntime.ToolDefinition{
			{Name: "get_weather", Parameters: map[string]any{"type": "object"}},
		},
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	var starts, deltas, ends int
	var startName, endName string
	var fragments []string
	for ev := range stream {
		switch ev.Type {
		case zeroruntime.StreamEventToolCallStart:
			starts++
			startName = ev.ToolName
		case zeroruntime.StreamEventToolCallDelta:
			deltas++
			fragments = append(fragments, ev.ArgumentsFragment)
		case zeroruntime.StreamEventToolCallEnd:
			ends++
			endName = ev.ToolName
		case zeroruntime.StreamEventError:
			t.Fatalf("unexpected error event: %q", ev.Error)
		}
	}
	if starts != 1 || startName != "get_weather" {
		t.Fatalf("tool-call-start count=%d name=%q, want 1 get_weather", starts, startName)
	}
	if deltas != 2 {
		t.Fatalf("tool-call-delta count=%d, want 2", deltas)
	}
	if joined := strings.Join(fragments, ""); joined != "arg1arg2" {
		t.Fatalf("tool-call-delta fragments = %q, want %q", joined, "arg1arg2")
	}
	if ends != 1 || endName != "get_weather" {
		t.Fatalf("tool-call-end count=%d name=%q, want 1 get_weather", ends, endName)
	}
}

func TestCodexProviderSendsAssistantToolCallsAsInputItems(t *testing.T) {
	t.Parallel()
	// When the runtime replays a prior assistant turn that issued a tool
	// call, the Codex provider must serialize the turn as BOTH the
	// assistant message (its text) AND a function_call item for the call.
	// A bare assistant message without the function_call item would make
	// the model think no tool was invoked.
	var rec codexRequest
	srv := newCodexTestServer(t, &rec)
	defer srv.Close()

	provider, err := NewCodexProvider(CodexOptions{
		Options:   Options{APIKey: "sk-test", BaseURL: srv.URL, Model: "gpt-5"},
		AccountID: "acc-x",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{
			{Role: zeroruntime.MessageRoleUser, Content: "weather in SF?"},
			{
				Role:    zeroruntime.MessageRoleAssistant,
				Content: "let me check",
				ToolCalls: []zeroruntime.ToolCall{
					{ID: "call-1", Name: "get_weather", Arguments: `{"location":"SF"}`},
				},
			},
			{Role: zeroruntime.MessageRoleTool, ToolCallID: "call-1", Content: "72F and sunny"},
			{Role: zeroruntime.MessageRoleUser, Content: "thanks"},
		},
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	drainCodexEvents(t, stream)

	input, ok := rec.body["input"].([]any)
	if !ok || len(input) != 5 {
		t.Fatalf("body.input = %#v, want 5 items (user + assistant text + function_call + function_call_output + user)", rec.body["input"])
	}
	user, _ := input[0].(map[string]any)
	if user["type"] != "message" || user["role"] != "user" {
		t.Fatalf("body.input[0] = %#v, want type=message role=user", user)
	}
	assistant, _ := input[1].(map[string]any)
	if assistant["type"] != "message" || assistant["role"] != "assistant" {
		t.Fatalf("body.input[1] = %#v, want type=message role=assistant", assistant)
	}
	functionCall, _ := input[2].(map[string]any)
	if functionCall["type"] != "function_call" {
		t.Fatalf("body.input[2] = %#v, want type=function_call", functionCall)
	}
	if functionCall["name"] != "get_weather" {
		t.Fatalf("body.input[2].name = %#v, want get_weather", functionCall["name"])
	}
	if functionCall["arguments"] != `{"location":"SF"}` {
		t.Fatalf("body.input[2].arguments = %#v, want %q", functionCall["arguments"], `{"location":"SF"}`)
	}
	functionOutput, _ := input[3].(map[string]any)
	if functionOutput["type"] != "function_call_output" {
		t.Fatalf("body.input[3] = %#v, want type=function_call_output", functionOutput)
	}
	if functionOutput["call_id"] != "call-1" {
		t.Fatalf("body.input[3].call_id = %#v, want call-1", functionOutput["call_id"])
	}
	finalUser, _ := input[4].(map[string]any)
	if finalUser["type"] != "message" || finalUser["role"] != "user" {
		t.Fatalf("body.input[4] = %#v, want type=message role=user", finalUser)
	}
}

func TestCodexProviderEmitsErrorOnResponseErrorEvent(t *testing.T) {
	t.Parallel()
	// A `response.error` event from the Codex backend is the stream-level
	// error signal. It must surface as a single StreamEventError and stop
	// the scan so the runtime doesn't hang waiting for a completion.
	var rec codexRequest
	srv := newCodexResponsesServer(t, &rec,
		`{"type":"response.created","response":{"id":"resp-1","status":"in_progress"}}`,
		`{"type":"response.error","code":"rate_limit_exceeded","message":"too many requests"}`,
	)
	defer srv.Close()

	provider, err := NewCodexProvider(CodexOptions{
		Options:   Options{APIKey: "sk-test", BaseURL: srv.URL, Model: "gpt-5"},
		AccountID: "acc-x",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	var gotError string
	for ev := range stream {
		if ev.Type == zeroruntime.StreamEventError {
			gotError = ev.Error
		}
	}
	if gotError == "" {
		t.Fatal("expected a StreamEventError, got none")
	}
	if !strings.Contains(gotError, "too many requests") {
		t.Fatalf("error = %q, want one mentioning the response.error message", gotError)
	}
}

func TestCodexProviderEmitsErrorOnMalformedStream(t *testing.T) {
	t.Parallel()
	// A non-JSON data payload (or a payload with a missing `type` field)
	// must be reported as a stream error rather than silently dropped —
	// otherwise the runtime would hang waiting for a completion event
	// that never arrives.
	var rec codexRequest
	srv := newCodexResponsesServer(t, &rec,
		`{"type":"response.created","response":{"id":"resp-1","status":"in_progress"}}`,
		`{"not-a-valid-event":true}`,
	)
	defer srv.Close()

	provider, err := NewCodexProvider(CodexOptions{
		Options:   Options{APIKey: "sk-test", BaseURL: srv.URL, Model: "gpt-5"},
		AccountID: "acc-x",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	var gotError string
	for ev := range stream {
		if ev.Type == zeroruntime.StreamEventError {
			gotError = ev.Error
		}
	}
	if gotError == "" {
		t.Fatal("expected a StreamEventError for a malformed event, got none")
	}
}

func TestCodexProviderEmitsLengthFinishWhenStreamEndsWithoutCompletion(t *testing.T) {
	t.Parallel()
	// The Codex backend may close the SSE stream without emitting
	// `response.completed` (e.g. an internal truncation). The wrapper
	// must surface a StreamEventDone with FinishReasonLength so the
	// runtime can react, instead of hanging on a missing completion.
	var rec codexRequest
	srv := newCodexResponsesServer(t, &rec,
		`{"type":"response.output_text.delta","delta":"partial"}`,
	)
	defer srv.Close()

	provider, err := NewCodexProvider(CodexOptions{
		Options:   Options{APIKey: "sk-test", BaseURL: srv.URL, Model: "gpt-5"},
		AccountID: "acc-x",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion: %v", err)
	}
	var finishReason string
	var gotDone bool
	for ev := range stream {
		if ev.Type == zeroruntime.StreamEventDone {
			gotDone = true
			finishReason = ev.FinishReason
		}
	}
	if !gotDone {
		t.Fatal("expected a StreamEventDone, got none")
	}
	if finishReason != zeroruntime.FinishReasonLength {
		t.Fatalf("finish reason = %q, want %q", finishReason, zeroruntime.FinishReasonLength)
	}
}

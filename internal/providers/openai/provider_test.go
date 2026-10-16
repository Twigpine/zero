package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/providers/providerio"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func init() {
	providerio.ShrinkBackoffForTest()
}

func TestStreamCompletionPostsChatCompletionRequest(t *testing.T) {
	t.Parallel()
	var gotPath string
	var gotAuth string
	var gotUserAgent string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotUserAgent = r.Header.Get("User-Agent")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		writeSSE(w, `{"choices":[]}`)
		writeSSE(w, `[DONE]`)
	}))
	defer server.Close()

	provider, err := New(Options{
		APIKey:    "sk-secret",
		BaseURL:   server.URL + "/",
		Model:     "gpt-test",
		UserAgent: "zero-test",
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{
			{Role: zeroruntime.MessageRoleSystem, Content: "system"},
			{Role: zeroruntime.MessageRoleUser, Content: "user"},
			{
				Role:    zeroruntime.MessageRoleAssistant,
				Content: "using a tool",
				ToolCalls: []zeroruntime.ToolCall{{
					ID:        "call_1",
					Name:      "read_file",
					Arguments: `{"path":"README.md"}`,
				}},
			},
			{Role: zeroruntime.MessageRoleTool, Content: "contents", ToolCallID: "call_1"},
		},
		Tools: []zeroruntime.ToolDefinition{{
			Name:        "read_file",
			Description: "Read a file",
			Parameters:  map[string]any{"type": "object"},
		}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}
	drain(stream)

	if gotPath != "/chat/completions" {
		t.Fatalf("path = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-secret" {
		t.Fatalf("auth = %q, want bearer token", gotAuth)
	}
	if gotUserAgent != "zero-test" {
		t.Fatalf("user agent = %q, want zero-test", gotUserAgent)
	}
	if gotBody["model"] != "gpt-test" || gotBody["stream"] != true {
		t.Fatalf("unexpected model/stream: %#v", gotBody)
	}
	streamOpts, ok := gotBody["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options missing or wrong type: %#v", gotBody["stream_options"])
	}
	if streamOpts["include_usage"] != true {
		t.Fatalf("stream_options.include_usage = %#v, want true", streamOpts["include_usage"])
	}
	messages := gotBody["messages"].([]any)
	assistant := messages[2].(map[string]any)
	toolCalls := assistant["tool_calls"].([]any)
	toolCall := toolCalls[0].(map[string]any)
	if toolCall["id"] != "call_1" || toolCall["type"] != "function" {
		t.Fatalf("unexpected assistant tool call: %#v", toolCall)
	}
	function := toolCall["function"].(map[string]any)
	if function["name"] != "read_file" || function["arguments"] != `{"path":"README.md"}` {
		t.Fatalf("unexpected tool call function: %#v", function)
	}
	toolResult := messages[3].(map[string]any)
	if toolResult["role"] != "tool" || toolResult["tool_call_id"] != "call_1" {
		t.Fatalf("unexpected tool result message: %#v", toolResult)
	}
	tools := gotBody["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Fatalf("unexpected tool wrapper: %#v", tool)
	}
	functionDefinition := tool["function"].(map[string]any)
	parameters := functionDefinition["parameters"].(map[string]any)
	properties, ok := parameters["properties"].(map[string]any)
	if !ok || len(properties) != 0 {
		t.Fatalf("parameters.properties = %#v, want empty object", parameters["properties"])
	}
}

// TestStreamCompletionSerializesTypedNilPropertiesAsEmptyObject locks in the
// normalizeToolParameters fix: a tool whose parameters carry a typed-nil
// properties map must serialize "properties":{} on the wire, never
// "properties":null, because strict OpenAI-compatible servers (LM Studio)
// reject the null form.
func TestStreamCompletionSerializesTypedNilPropertiesAsEmptyObject(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	var gotRaw []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		gotRaw = raw
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		writeSSE(w, `{"choices":[]}`)
		writeSSE(w, `[DONE]`)
	}))
	defer server.Close()

	provider, err := New(Options{
		APIKey:  "sk-secret",
		BaseURL: server.URL + "/",
		Model:   "gpt-test",
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
		Tools: []zeroruntime.ToolDefinition{{
			Name:        "no_args",
			Description: "Takes no arguments",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any(nil)},
		}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}
	drain(stream)

	if !strings.Contains(string(gotRaw), `"properties":{}`) {
		t.Fatalf("request body must serialize properties as an empty object, got: %s", gotRaw)
	}
	if strings.Contains(string(gotRaw), `"properties":null`) {
		t.Fatalf("request body must not serialize properties as null, got: %s", gotRaw)
	}
	tools := gotBody["tools"].([]any)
	tool := tools[0].(map[string]any)
	functionDefinition := tool["function"].(map[string]any)
	parameters := functionDefinition["parameters"].(map[string]any)
	properties, ok := parameters["properties"].(map[string]any)
	if !ok || len(properties) != 0 {
		t.Fatalf("parameters.properties = %#v, want empty object", parameters["properties"])
	}
}

func TestNewRequiresModelButNotAPIKey(t *testing.T) {
	t.Parallel()
	if _, err := New(Options{}); err == nil {
		t.Fatal("New without model returned nil error")
	}
	if _, err := New(Options{Model: "gpt-test"}); err != nil {
		t.Fatalf("New without API key returned error: %v", err)
	}
}

func TestStreamCompletionOmitsAuthAndToolsWhenEmpty(t *testing.T) {
	t.Parallel()
	var gotAuth string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		writeSSE(w, `[DONE]`)
	}))
	defer server.Close()

	provider, err := New(Options{BaseURL: server.URL, Model: "local-model"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}
	drain(stream)

	if gotAuth != "" {
		t.Fatalf("auth = %q, want empty", gotAuth)
	}
	if _, ok := gotBody["tools"]; ok {
		t.Fatalf("tools present for empty tools: %#v", gotBody["tools"])
	}
}

func TestStreamCompletionAppliesCustomAuthAndHeaders(t *testing.T) {
	t.Parallel()
	var gotAuth string
	var gotAltAuth string
	var gotReferer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAltAuth = r.Header.Get("X-API-Key")
		gotReferer = r.Header.Get("HTTP-Referer")
		writeSSE(w, `[DONE]`)
	}))
	defer server.Close()

	provider, err := New(Options{
		APIKey:        "sk-custom",
		BaseURL:       server.URL,
		Model:         "custom-model",
		AuthHeader:    "X-API-Key",
		AuthScheme:    "Token",
		CustomHeaders: map[string]string{"HTTP-Referer": "https://zero.dev"},
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}
	drain(stream)

	if gotAuth != "" {
		t.Fatalf("Authorization = %q, want empty when custom auth header is used", gotAuth)
	}
	if gotAltAuth != "Token sk-custom" {
		t.Fatalf("X-API-Key = %q, want custom scheme token", gotAltAuth)
	}
	if gotReferer != "https://zero.dev" {
		t.Fatalf("HTTP-Referer = %q, want custom header", gotReferer)
	}
}

func TestStreamCompletionEmitsTextUsageAndDone(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"content":"hello "}}]}`)
		writeSSE(w, `{"choices":[{"delta":{"content":"zero"}}],"usage":{"prompt_tokens":12,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":3,"cache_write_tokens":2}}}`)
		writeSSE(w, `[DONE]`)
	})

	events := collectProviderEvents(t, provider)
	assertEvent(t, events[0], zeroruntime.StreamEventText, "hello ")
	assertEvent(t, events[1], zeroruntime.StreamEventText, "zero")
	if events[2].Type != zeroruntime.StreamEventUsage || events[2].Usage.PromptTokens != 12 || events[2].Usage.CompletionTokens != 5 || events[2].Usage.CachedInputTokens != 3 || events[2].Usage.CacheWriteTokens != 2 {
		t.Fatalf("unexpected usage event: %#v", events[2])
	}
	if events[3].Type != zeroruntime.StreamEventDone {
		t.Fatalf("last event = %#v, want done", events[3])
	}
}

func TestStreamCompletionEmitsReasoningContentDeltas(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"reasoning_content":"Thinking. "}}]}`)
		writeSSE(w, `{"choices":[{"delta":{"reasoning_content":"Answering now."}}]}`)
		writeSSE(w, `[DONE]`)
	})

	events := collectProviderEvents(t, provider)
	reasoning := eventsOfType(events, zeroruntime.StreamEventReasoning)
	if len(reasoning) != 2 {
		t.Fatalf("reasoning events = %#v, want two reasoning deltas", reasoning)
	}
	if reasoning[0].Content != "Thinking. " || reasoning[1].Content != "Answering now." {
		t.Fatalf("unexpected reasoning events: %#v", reasoning)
	}
	if text := eventsOfType(events, zeroruntime.StreamEventText); len(text) != 0 {
		t.Fatalf("reasoning_content must not emit text events, got %#v", text)
	}
}

func TestStreamCompletionEmitsReasoningAliasDeltas(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"reasoning":"Thinking. "}}]}`)
		writeSSE(w, `{"choices":[{"delta":{"reasoning":"Answering now."}}]}`)
		writeSSE(w, `[DONE]`)
	})

	events := collectProviderEvents(t, provider)
	reasoning := eventsOfType(events, zeroruntime.StreamEventReasoning)
	if len(reasoning) != 2 {
		t.Fatalf("reasoning events = %#v, want two reasoning deltas", reasoning)
	}
	if reasoning[0].Content != "Thinking. " || reasoning[1].Content != "Answering now." {
		t.Fatalf("unexpected reasoning events: %#v", reasoning)
	}
	if text := eventsOfType(events, zeroruntime.StreamEventText); len(text) != 0 {
		t.Fatalf("reasoning must not emit text events, got %#v", text)
	}
}

func TestStreamCompletionPrefersReasoningContentOverAlias(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"reasoning_content":"standard","reasoning":"alias"}}]}`)
		writeSSE(w, `[DONE]`)
	})

	events := collectProviderEvents(t, provider)
	reasoning := eventsOfType(events, zeroruntime.StreamEventReasoning)
	if len(reasoning) != 1 || reasoning[0].Content != "standard" {
		t.Fatalf("reasoning events = %#v, want standard reasoning_content", reasoning)
	}
}

func TestStreamCompletionEmitsReasoningBeforeRegularContent(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"reasoning_content":"Thinking. ","content":"Answer."}}]}`)
		writeSSE(w, `[DONE]`)
	})

	events := collectProviderEvents(t, provider)
	if len(events) < 3 {
		t.Fatalf("events = %#v, want reasoning, text, done", events)
	}
	assertEvent(t, events[0], zeroruntime.StreamEventReasoning, "Thinking. ")
	assertEvent(t, events[1], zeroruntime.StreamEventText, "Answer.")
	if events[2].Type != zeroruntime.StreamEventDone {
		t.Fatalf("third event = %#v, want done", events[2])
	}
}

func TestStreamCompletionPreservesLiteralThinkTagsByDefault(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"content":"show <think>literal</think> markup"}}]}`)
		writeSSE(w, `[DONE]`)
	})

	events := collectProviderEvents(t, provider)
	assertEvent(t, events[0], zeroruntime.StreamEventText, "show <think>literal</think> markup")
	if reasoning := eventsOfType(events, zeroruntime.StreamEventReasoning); len(reasoning) != 0 {
		t.Fatalf("literal think tags must not emit reasoning by default, got %#v", reasoning)
	}
}

func TestStreamCompletionSplitsInlineThinkTagsFromContent(t *testing.T) {
	t.Parallel()
	provider := newTestProviderWithThinkTags(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"content":"<think>private reasoning</think>public answer"}}]}`)
		writeSSE(w, `[DONE]`)
	})

	events := collectProviderEvents(t, provider)
	assertEvent(t, events[0], zeroruntime.StreamEventReasoning, "private reasoning")
	assertEvent(t, events[1], zeroruntime.StreamEventText, "public answer")
	if events[2].Type != zeroruntime.StreamEventDone {
		t.Fatalf("third event = %#v, want done", events[2])
	}
}

func TestStreamCompletionSplitsInlineThinkTagsAcrossChunks(t *testing.T) {
	t.Parallel()
	provider := newTestProviderWithThinkTags(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"content":"<thi"}}]}`)
		writeSSE(w, `{"choices":[{"delta":{"content":"nk>reason"}}]}`)
		writeSSE(w, `{"choices":[{"delta":{"content":"ing</"}}]}`)
		writeSSE(w, `{"choices":[{"delta":{"content":"think> answer"}}]}`)
		writeSSE(w, `[DONE]`)
	})

	events := collectProviderEvents(t, provider)
	reasoning := eventsOfType(events, zeroruntime.StreamEventReasoning)
	if len(reasoning) != 2 || reasoning[0].Content != "reason" || reasoning[1].Content != "ing" {
		t.Fatalf("reasoning events = %#v, want split reasoning content", reasoning)
	}
	text := eventsOfType(events, zeroruntime.StreamEventText)
	if len(text) != 1 || text[0].Content != " answer" {
		t.Fatalf("text events = %#v, want answer-only content", text)
	}
}

func TestStreamCompletionBuffersToolArgsUntilIDAndNameArrive(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`)
		writeSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"\"README.md\"}"}}]},"finish_reason":"tool_calls"}]}`)
		writeSSE(w, `[DONE]`)
	})

	events := collectProviderEvents(t, provider)
	if len(events) != 4 {
		t.Fatalf("events = %#v, want start, delta, end, done", events)
	}
	if events[0].Type != zeroruntime.StreamEventToolCallStart || events[0].ToolCallID != "call_1" || events[0].ToolName != "read_file" {
		t.Fatalf("unexpected start event: %#v", events[0])
	}
	if events[1].Type != zeroruntime.StreamEventToolCallDelta || events[1].ToolCallID != "call_1" || events[1].ArgumentsFragment != `{"path":"README.md"}` {
		t.Fatalf("unexpected delta event: %#v", events[1])
	}
	if events[2].Type != zeroruntime.StreamEventToolCallEnd || events[2].ToolCallID != "call_1" {
		t.Fatalf("unexpected end event: %#v", events[2])
	}
	if events[3].Type != zeroruntime.StreamEventDone {
		t.Fatalf("unexpected done event: %#v", events[3])
	}
}

func TestStreamCompletionTracksMultipleToolCallsByIndex(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"query\":"}},{"index":0,"id":"call_a","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]}}]}`)
		writeSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"grep","arguments":"\"zero\"}"}}]},"finish_reason":"tool_calls"}]}`)
		writeSSE(w, `[DONE]`)
	})

	events := collectProviderEvents(t, provider)
	starts := eventsOfType(events, zeroruntime.StreamEventToolCallStart)
	deltas := eventsOfType(events, zeroruntime.StreamEventToolCallDelta)
	ends := eventsOfType(events, zeroruntime.StreamEventToolCallEnd)
	if len(starts) != 2 || len(deltas) != 2 || len(ends) != 2 {
		t.Fatalf("events = %#v, want two starts/deltas/ends", events)
	}
	if deltas[0].ToolCallID != "call_a" || deltas[0].ArgumentsFragment != `{"path":"a"}` {
		t.Fatalf("unexpected first delta: %#v", deltas[0])
	}
	if deltas[1].ToolCallID != "call_b" || deltas[1].ArgumentsFragment != `{"query":"zero"}` {
		t.Fatalf("unexpected second delta: %#v", deltas[1])
	}
}

func TestStreamCompletionClassifiesHTTPErrorsAndRedactsToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		status     int
		body       string
		wantPrefix string
	}{
		{"auth", http.StatusUnauthorized, `{"error":{"message":"bad key sk-secret","type":"invalid_request_error"}}`, "auth error:"},
		{"rate limit", http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`, "rate limit error:"},
		{"service unavailable", http.StatusServiceUnavailable, `{"error":{"message":"overloaded"}}`, "rate limit error:"},
		{"overloaded 529", 529, `{"error":{"message":"overloaded"}}`, "rate limit error:"},
		{"bad request", http.StatusBadRequest, `{"error":{"message":"bad request"}}`, "provider request error:"},
		{"server", http.StatusInternalServerError, `server saw Bearer sk-secret`, "provider error:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := newTestProviderWithKey(t, "sk-secret", func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, tc.body, tc.status)
			})
			stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{})
			if err != nil {
				t.Fatalf("StreamCompletion returned setup error: %v", err)
			}
			events := readAll(stream)
			if len(events) != 1 || events[0].Type != zeroruntime.StreamEventError {
				t.Fatalf("events = %#v, want one error", events)
			}
			if !strings.HasPrefix(events[0].Error, tc.wantPrefix) {
				t.Fatalf("error = %q, want prefix %q", events[0].Error, tc.wantPrefix)
			}
			// The real security property: the secret token must never appear. Redact now
			// preserves the word "Bearer" and scrubs only the token-shaped value after it,
			// so we assert the token is gone rather than that "Bearer" was rewritten. (AUDIT-H7)
			if strings.Contains(events[0].Error, "sk-secret") {
				t.Fatalf("error leaked token: %q", events[0].Error)
			}
		})
	}
}

func TestStreamCompletionHumanizesUpstreamUnreachableGatewayError(t *testing.T) {
	t.Parallel()
	// A local Ollama daemon serving a "-cloud" model answers on localhost but
	// returns HTTP 502 with an opaque proxied transport error when it cannot reach
	// its cloud backend. The adapter must surface a clear connectivity message
	// naming the host, not the raw proxied JSON body.
	provider := newTestProviderWithKey(t, "sk-secret", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"Post \"https://ollama.com:443/v1/chat/completions?ts=1\": net/http: TLS handshake timeout"}`, http.StatusBadGateway)
	})
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{})
	if err != nil {
		t.Fatalf("StreamCompletion returned setup error: %v", err)
	}
	events := readAll(stream)
	if len(events) != 1 || events[0].Type != zeroruntime.StreamEventError {
		t.Fatalf("events = %#v, want one error", events)
	}
	got := events[0].Error
	if !strings.HasPrefix(got, "upstream unreachable: ") {
		t.Fatalf("error = %q, want upstream-unreachable prefix", got)
	}
	for _, want := range []string{"ollama.com:443", "TLS handshake timeout"} {
		if !strings.Contains(got, want) {
			t.Fatalf("error = %q, want it to contain %q", got, want)
		}
	}
}

func TestStreamCompletionEmitsStreamErrorObject(t *testing.T) {
	t.Parallel()
	provider := newTestProviderWithKey(t, "sk-secret", func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"error":{"message":"stream failed sk-secret","type":"server_error"}}`)
	})

	events := collectProviderEvents(t, provider)
	if len(events) != 1 || events[0].Type != zeroruntime.StreamEventError {
		t.Fatalf("events = %#v, want one error", events)
	}
	if !strings.HasPrefix(events[0].Error, "provider error:") {
		t.Fatalf("error = %q, want provider error prefix", events[0].Error)
	}
	if strings.Contains(events[0].Error, "sk-secret") {
		t.Fatalf("error leaked token: %q", events[0].Error)
	}
}

func TestStreamCompletionClassifiesStreamErrorCode(t *testing.T) {
	t.Parallel()
	// The error arrives inside a 200 OK SSE payload's "code" field, not the
	// HTTP status, so this exercises openAIStreamErrorStatusByCode directly:
	// both the numeric-string codes some providers send and the semantic
	// string codes OpenAI-compatible providers commonly send instead must
	// classify identically.
	cases := []struct {
		name       string
		code       string
		wantPrefix string
	}{
		{"numeric 429", `"429"`, "rate limit error:"},
		{"numeric 401", `"401"`, "auth error:"},
		{"numeric 403", `"403"`, "auth error:"},
		{"json number 429", `429`, "rate limit error:"},
		{"semantic rate_limit_exceeded", `"rate_limit_exceeded"`, "rate limit error:"},
		{"semantic insufficient_quota", `"insufficient_quota"`, "rate limit error:"},
		{"semantic invalid_api_key", `"invalid_api_key"`, "auth error:"},
		{"unknown code", `"server_error"`, "provider error:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				writeSSE(w, `{"error":{"message":"failed","code":`+tc.code+`}}`)
			})

			events := collectProviderEvents(t, provider)
			if len(events) != 1 || events[0].Type != zeroruntime.StreamEventError {
				t.Fatalf("events = %#v, want one error", events)
			}
			if !strings.HasPrefix(events[0].Error, tc.wantPrefix) {
				t.Fatalf("error = %q, want prefix %q", events[0].Error, tc.wantPrefix)
			}
		})
	}
}

func TestStreamCompletionEmitsErrorForMalformedJSON(t *testing.T) {
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":`)
	})

	events := collectProviderEvents(t, provider)
	if len(events) != 1 || events[0].Type != zeroruntime.StreamEventError {
		t.Fatalf("events = %#v, want one error", events)
	}
	if !strings.HasPrefix(events[0].Error, "provider stream error: malformed JSON") {
		t.Fatalf("error = %q, want malformed JSON provider stream error", events[0].Error)
	}
}

func TestStreamCompletionEmitsErrorWhenContextCancels(t *testing.T) {
	t.Parallel()
	requestStarted := make(chan struct{})
	release := make(chan struct{})
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-release
	})
	ctx, cancel := context.WithCancel(context.Background())

	stream, err := provider.StreamCompletion(ctx, zeroruntime.CompletionRequest{})
	if err != nil {
		t.Fatalf("StreamCompletion returned setup error: %v", err)
	}
	<-requestStarted
	cancel()
	close(release)

	events := readAll(stream)
	if len(events) != 1 || events[0].Type != zeroruntime.StreamEventError {
		t.Fatalf("events = %#v, want context error", events)
	}
	if !strings.Contains(events[0].Error, "context canceled") {
		t.Fatalf("error = %q, want context canceled", events[0].Error)
	}
}

// A parent-context deadline surfaces as a transport error whose string contains
// "context deadline exceeded" plus the request host. That must be reported as a
// caller timeout, NOT humanized into an "upstream unreachable" outage (the host
// is reachable; the caller's clock ran out).
func TestStreamCompletionContextDeadlineNotHumanizedAsUpstream(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	defer close(release)
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the request open until the parent deadline fires
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	stream, err := provider.StreamCompletion(ctx, zeroruntime.CompletionRequest{})
	if err != nil {
		t.Fatalf("StreamCompletion returned setup error: %v", err)
	}
	events := readAll(stream)
	if len(events) != 1 || events[0].Type != zeroruntime.StreamEventError {
		t.Fatalf("events = %#v, want one error", events)
	}
	got := events[0].Error
	if strings.Contains(got, "upstream unreachable") {
		t.Fatalf("parent-context deadline mislabeled as an upstream outage: %q", got)
	}
	if !strings.Contains(got, "context deadline exceeded") {
		t.Fatalf("error = %q, want the context deadline surfaced verbatim", got)
	}
}

func TestStreamCompletionFlushesBufferedContentWhenContextCancels(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	provider := newTestProviderWithThinkTags(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"content":"visible <thi"}}]}`)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := provider.StreamCompletion(ctx, zeroruntime.CompletionRequest{})
	if err != nil {
		t.Fatalf("StreamCompletion returned setup error: %v", err)
	}

	events := []zeroruntime.StreamEvent{}
	select {
	case event, ok := <-stream:
		if !ok {
			t.Fatal("stream closed before first text event")
		}
		events = append(events, event)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first text event")
	}
	cancel()
	events = append(events, readAll(stream)...)

	if len(events) != 3 ||
		events[0].Type != zeroruntime.StreamEventText || events[0].Content != "visible " ||
		events[1].Type != zeroruntime.StreamEventText || events[1].Content != "<thi" ||
		events[2].Type != zeroruntime.StreamEventError {
		t.Fatalf("events = %#v, want text, buffered text, then context error", events)
	}
	if done := eventsOfType(events, zeroruntime.StreamEventDone); len(done) != 0 {
		t.Fatalf("events = %#v, want no done after context cancel", events)
	}
}

func newTestProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	return newTestProviderWithKey(t, "", handler)
}

func newTestProviderWithKey(t *testing.T, apiKey string, handler http.HandlerFunc) *Provider {
	t.Helper()
	return newTestProviderWithOptions(t, Options{APIKey: apiKey}, handler)
}

func newTestProviderWithThinkTags(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	return newTestProviderWithOptions(t, Options{ParseThinkTags: true}, handler)
}

func newTestProviderWithOptions(t *testing.T, options Options, handler http.HandlerFunc) *Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	options.BaseURL = server.URL
	if strings.TrimSpace(options.Model) == "" {
		options.Model = "gpt-test"
	}
	provider, err := New(Options{
		APIKey:            options.APIKey,
		BaseURL:           options.BaseURL,
		Model:             options.Model,
		AuthHeader:        options.AuthHeader,
		AuthScheme:        options.AuthScheme,
		AuthHeaderValue:   options.AuthHeaderValue,
		CustomHeaders:     options.CustomHeaders,
		HTTPClient:        options.HTTPClient,
		UserAgent:         options.UserAgent,
		MaxTokens:         options.MaxTokens,
		StreamIdleTimeout: options.StreamIdleTimeout,
		ParseThinkTags:    options.ParseThinkTags,
		SetRequestExtra:   options.SetRequestExtra,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	return provider
}

func collectProviderEvents(t *testing.T, provider *Provider) []zeroruntime.StreamEvent {
	t.Helper()
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{})
	if err != nil {
		t.Fatalf("StreamCompletion returned setup error: %v", err)
	}
	return readAll(stream)
}

func readAll(stream <-chan zeroruntime.StreamEvent) []zeroruntime.StreamEvent {
	events := []zeroruntime.StreamEvent{}
	for event := range stream {
		events = append(events, event)
	}
	return events
}

func drain(stream <-chan zeroruntime.StreamEvent) {
	for range stream {
	}
}

func writeSSE(w http.ResponseWriter, payload string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte("data: " + payload + "\n\n"))
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func assertEvent(t *testing.T, event zeroruntime.StreamEvent, eventType zeroruntime.StreamEventType, content string) {
	t.Helper()
	if event.Type != eventType || event.Content != content {
		t.Fatalf("event = %#v, want %s %q", event, eventType, content)
	}
}

func eventsOfType(events []zeroruntime.StreamEvent, eventType zeroruntime.StreamEventType) []zeroruntime.StreamEvent {
	matching := []zeroruntime.StreamEvent{}
	for _, event := range events {
		if event.Type == eventType {
			matching = append(matching, event)
		}
	}
	return matching
}

func TestStreamCompletionDoesNotHangOnEOFWithOpenToolCall(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"}}]}}]}`)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	stream, err := provider.StreamCompletion(ctx, zeroruntime.CompletionRequest{})
	if err != nil {
		t.Fatalf("StreamCompletion returned setup error: %v", err)
	}
	events := []zeroruntime.StreamEvent{}
	for {
		select {
		case event, ok := <-stream:
			if !ok {
				if len(eventsOfType(events, zeroruntime.StreamEventToolCallEnd)) != 1 {
					t.Fatalf("events = %#v, want one tool-call-end on EOF", events)
				}
				if len(eventsOfType(events, zeroruntime.StreamEventDone)) != 1 {
					t.Fatalf("events = %#v, want done on EOF", events)
				}
				return
			}
			events = append(events, event)
		case <-ctx.Done():
			t.Fatal("stream did not close")
		}
	}
}

func TestStreamCompletionSkipsNamelessToolCallOnEOF(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"arguments":"{\"path\":\"README.md\"}"}}]}}]}`)
	})

	events := collectProviderEvents(t, provider)
	if len(eventsOfType(events, zeroruntime.StreamEventToolCallStart)) != 0 {
		t.Fatalf("events = %#v, want no start for nameless tool call", events)
	}
	if len(eventsOfType(events, zeroruntime.StreamEventToolCallDelta)) != 0 {
		t.Fatalf("events = %#v, want no delta for nameless tool call", events)
	}
	if len(eventsOfType(events, zeroruntime.StreamEventToolCallEnd)) != 0 {
		t.Fatalf("events = %#v, want no end for nameless tool call", events)
	}
	if len(eventsOfType(events, zeroruntime.StreamEventDone)) != 1 {
		t.Fatalf("events = %#v, want done event", events)
	}
}

func TestStreamCompletionIdleTimeoutAbortsStalledStream(t *testing.T) {
	t.Parallel()
	released := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Send one token, then hang without sending [DONE] or closing —
		// simulating a stalled upstream (the freeze in the screenshot).
		writeSSE(w, `{"choices":[{"delta":{"content":"hi"}}]}`)
		select {
		case <-r.Context().Done():
		case <-released:
		}
	}))
	defer server.Close()
	defer close(released)

	provider, err := New(Options{
		BaseURL:           server.URL + "/",
		Model:             "gpt-test",
		StreamIdleTimeout: 80 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{})
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}

	// Must terminate (channel closes) rather than hang forever.
	done := make(chan []zeroruntime.StreamEvent, 1)
	go func() { done <- readAll(stream) }()
	var events []zeroruntime.StreamEvent
	select {
	case events = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not terminate on idle — it hung")
	}

	var gotText, gotIdleError bool
	for _, e := range events {
		if e.Type == zeroruntime.StreamEventText && e.Content == "hi" {
			gotText = true
		}
		if e.Type == zeroruntime.StreamEventError && strings.Contains(strings.ToLower(e.Error), "idle") {
			gotIdleError = true
		}
	}
	if !gotText {
		t.Error("expected the first token before the stall")
	}
	if !gotIdleError {
		t.Errorf("expected a surfaced idle-timeout error, got events: %+v", events)
	}
}

func TestStreamCompletionSendsMaxCompletionTokens(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		writeSSE(w, `[DONE]`)
	}))
	defer server.Close()

	provider, err := New(Options{BaseURL: server.URL + "/", Model: "gpt-test", MaxTokens: 1234})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}
	drain(stream)

	got, ok := gotBody["max_completion_tokens"]
	if !ok {
		t.Fatalf("max_completion_tokens missing from request: %#v", gotBody)
	}
	if n, _ := got.(float64); int(n) != 1234 {
		t.Fatalf("max_completion_tokens = %#v, want 1234", got)
	}
}

func TestStreamCompletionSendsReasoningEffort(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		writeSSE(w, `[DONE]`)
	}))
	defer server.Close()

	provider, err := New(Options{BaseURL: server.URL + "/", Model: "o3"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages:        []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
		ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}
	drain(stream)

	if got := gotBody["reasoning_effort"]; got != "high" {
		t.Fatalf("reasoning_effort = %#v, want \"high\"", got)
	}
}

func TestStreamCompletionNormalizesServiceTier(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "priority", input: "priority", want: "priority"},
		{name: "flex with surrounding whitespace", input: " FlEx ", want: "flex"},
		{name: "unsupported omitted", input: "standard", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var gotBody map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
					t.Fatalf("decode request body: %v", err)
				}
				writeSSE(w, `[DONE]`)
			}))
			defer server.Close()

			provider, err := New(Options{BaseURL: server.URL + "/", Model: "gpt-test"})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
				Messages:    []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
				ServiceTier: test.input,
			})
			if err != nil {
				t.Fatalf("StreamCompletion: %v", err)
			}
			drain(stream)
			if test.want == "" {
				if _, ok := gotBody["service_tier"]; ok {
					t.Fatalf("service_tier = %#v, want omitted", gotBody["service_tier"])
				}
				return
			}
			if got := gotBody["service_tier"]; got != test.want {
				t.Fatalf("service_tier = %#v, want %q", got, test.want)
			}
		})
	}
}

func TestStreamCompletionOmitsReasoningEffortWhenUnsetOrInvalid(t *testing.T) {
	t.Parallel()
	for _, effort := range []string{"", "none", "bogus"} {
		var gotBody map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			writeSSE(w, `[DONE]`)
		}))

		provider, err := New(Options{BaseURL: server.URL + "/", Model: "gpt-test"})
		if err != nil {
			server.Close()
			t.Fatalf("New returned error: %v", err)
		}
		stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
			Messages:        []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
			ReasoningEffort: effort,
		})
		if err != nil {
			server.Close()
			t.Fatalf("StreamCompletion returned error: %v", err)
		}
		drain(stream)
		server.Close()

		if _, ok := gotBody["reasoning_effort"]; ok {
			t.Fatalf("reasoning_effort should be omitted for %q: %#v", effort, gotBody["reasoning_effort"])
		}
	}
}

func TestStreamCompletionOmitsMaxCompletionTokensWhenUnset(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		writeSSE(w, `[DONE]`)
	}))
	defer server.Close()

	provider, err := New(Options{BaseURL: server.URL + "/", Model: "gpt-test"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}
	drain(stream)

	if _, ok := gotBody["max_completion_tokens"]; ok {
		t.Fatalf("max_completion_tokens should be omitted when unset: %#v", gotBody["max_completion_tokens"])
	}
}

// A finish_reason of "length" means the response was truncated at the output cap.
// The provider must surface it on the done event so a clipped answer is not
// mistaken for a complete one.
func TestStreamCompletionSurfacesLengthFinishReason(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"content":"truncated"}}]}`)
		writeSSE(w, `{"choices":[{"delta":{},"finish_reason":"length"}]}`)
		writeSSE(w, `[DONE]`)
	})

	events := collectProviderEvents(t, provider)
	var doneReason string
	var sawDone bool
	for _, e := range events {
		if e.Type == zeroruntime.StreamEventDone {
			sawDone = true
			doneReason = e.FinishReason
		}
	}
	if !sawDone {
		t.Fatalf("no done event; events: %+v", events)
	}
	if doneReason != zeroruntime.FinishReasonLength {
		t.Fatalf("done FinishReason = %q, want %q", doneReason, zeroruntime.FinishReasonLength)
	}

	// And it round-trips through the runtime collector's FinishReason.
	collected := zeroruntime.CollectStream(context.Background(), replay(events))
	if collected.FinishReason != zeroruntime.FinishReasonLength {
		t.Fatalf("collected = %+v, want truncated length", collected)
	}
}

// A content_filter finish_reason maps to the runtime's content-filter reason.
func TestStreamCompletionSurfacesContentFilterFinishReason(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{},"finish_reason":"content_filter"}]}`)
		writeSSE(w, `[DONE]`)
	})

	events := collectProviderEvents(t, provider)
	var sawDone bool
	for _, e := range events {
		if e.Type == zeroruntime.StreamEventDone {
			sawDone = true
			if e.FinishReason != zeroruntime.FinishReasonContentFilter {
				t.Fatalf("done FinishReason = %q, want %q", e.FinishReason, zeroruntime.FinishReasonContentFilter)
			}
		}
	}
	if !sawDone {
		t.Fatalf("no done event; events: %+v", events)
	}
}

// A normal "stop" finish must leave FinishReason empty on the done event.
func TestStreamCompletionNormalFinishHasNoReason(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`)
		writeSSE(w, `[DONE]`)
	})

	events := collectProviderEvents(t, provider)
	var sawDone bool
	for _, e := range events {
		if e.Type == zeroruntime.StreamEventDone {
			sawDone = true
			if e.FinishReason != "" {
				t.Fatalf("normal finish leaked FinishReason %q", e.FinishReason)
			}
		}
	}
	if !sawDone {
		t.Fatalf("no done event; events: %+v", events)
	}
}

// The shared SSE reader must join multi-line "data:" continuation fields into a
// single payload (the OpenAI provider previously parsed one line at a time and
// would drop the continuation, producing malformed JSON).
func TestStreamCompletionJoinsMultiLineDataFields(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// One SSE event whose JSON payload is split across two data: lines.
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":\ndata: {\"content\":\"joined\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer server.Close()

	provider, err := New(Options{BaseURL: server.URL + "/", Model: "gpt-test"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	events := collectProviderEvents(t, provider)
	var text string
	for _, e := range events {
		if e.Type == zeroruntime.StreamEventText {
			text += e.Content
		}
		if e.Type == zeroruntime.StreamEventError {
			t.Fatalf("multi-line data field produced an error: %q", e.Error)
		}
	}
	if text != "joined" {
		t.Fatalf("text = %q, want %q (continuation data: line dropped?)", text, "joined")
	}
}

func replay(events []zeroruntime.StreamEvent) <-chan zeroruntime.StreamEvent {
	ch := make(chan zeroruntime.StreamEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch
}

func TestStreamCompletionEmitsDroppedOnNamelessToolCall(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A tool call with arguments + finish_reason but no function name.
		writeSSE(w, `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"arguments":"{}"}}]}}]}`)
		writeSSE(w, `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
		writeSSE(w, `[DONE]`)
	}))
	defer server.Close()

	provider, err := New(Options{BaseURL: server.URL + "/", Model: "gpt-test"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	events := collectProviderEvents(t, provider)

	var dropped, started bool
	for _, e := range events {
		if e.Type == zeroruntime.StreamEventToolCallDropped {
			dropped = true
		}
		if e.Type == zeroruntime.StreamEventToolCallStart {
			started = true
		}
	}
	if started {
		t.Error("a nameless tool call must not start")
	}
	if !dropped {
		t.Errorf("expected a dropped-tool-call signal, got events: %+v", events)
	}
}

func TestContentPartImageURLMarshalsDataURI(t *testing.T) {
	t.Parallel()
	parts := []contentPart{
		{Type: "text", Text: "look"},
		{Type: "image_url", ImageURL: &imageURLPart{URL: "data:image/png;base64,QUJD"}},
	}
	got, err := json.Marshal(parts)
	if err != nil {
		t.Fatalf("marshal content parts: %v", err)
	}
	want := `[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]`
	if string(got) != want {
		t.Fatalf("content parts JSON =\n  %s\nwant\n  %s", got, want)
	}

	// The text field is omitted on an image-only part; image_url is omitted on a text part.
	imgOnly, _ := json.Marshal(contentPart{Type: "image_url", ImageURL: &imageURLPart{URL: "data:image/png;base64,QQ=="}})
	if strings.Contains(string(imgOnly), `"text"`) {
		t.Fatalf("image-only part must omit empty text, got: %s", imgOnly)
	}
	textOnly, _ := json.Marshal(contentPart{Type: "text", Text: "hi"})
	if strings.Contains(string(textOnly), `"image_url"`) {
		t.Fatalf("text part must omit nil image_url, got: %s", textOnly)
	}
}

func TestMapMessageBuildsImageURLContentParts(t *testing.T) {
	t.Parallel()
	msg := mapMessage(zeroruntime.Message{
		Role:    zeroruntime.MessageRoleUser,
		Content: "describe these",
		Images: []zeroruntime.ImageBlock{
			{MediaType: "image/png", Data: []byte("ABC")},
			{MediaType: "image/jpeg", Data: []byte{0xff, 0xd8, 0xff}},
		},
	})

	parts, ok := msg.Content.([]contentPart)
	if !ok {
		t.Fatalf("Content type = %T, want []contentPart", msg.Content)
	}
	if len(parts) != 3 {
		t.Fatalf("len(parts) = %d, want 3 (1 text + 2 images)", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "describe these" {
		t.Fatalf("part[0] = %#v, want text part", parts[0])
	}
	// Data "ABC" base64-encodes to "QUJD".
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil ||
		parts[1].ImageURL.URL != "data:image/png;base64,QUJD" {
		t.Fatalf("part[1] = %#v, want png data URI", parts[1])
	}
	// {0xff,0xd8,0xff} base64-encodes to "/9j/".
	if parts[2].ImageURL == nil || parts[2].ImageURL.URL != "data:image/jpeg;base64,/9j/" {
		t.Fatalf("part[2] = %#v, want jpeg data URI", parts[2])
	}
}

func TestMapMessageImageOnlyOmitsTextPart(t *testing.T) {
	t.Parallel()
	msg := mapMessage(zeroruntime.Message{
		Role:    zeroruntime.MessageRoleUser,
		Content: "",
		Images:  []zeroruntime.ImageBlock{{MediaType: "image/png", Data: []byte("A")}},
	})
	parts, ok := msg.Content.([]contentPart)
	if !ok {
		t.Fatalf("Content type = %T, want []contentPart", msg.Content)
	}
	if len(parts) != 1 {
		t.Fatalf("len(parts) = %d, want 1 (image only, no text part)", len(parts))
	}
	// Data "A" base64-encodes to "QQ==".
	if parts[0].Type != "image_url" || parts[0].ImageURL == nil ||
		parts[0].ImageURL.URL != "data:image/png;base64,QQ==" {
		t.Fatalf("part[0] = %#v, want image-only png part", parts[0])
	}
}

func TestMapMessageTextOnlyKeepsStringContent(t *testing.T) {
	t.Parallel()
	msg := mapMessage(zeroruntime.Message{Role: zeroruntime.MessageRoleUser, Content: "hi"})
	if got, ok := msg.Content.(string); !ok || got != "hi" {
		t.Fatalf("Content = %#v, want string \"hi\"", msg.Content)
	}
	empty := mapMessage(zeroruntime.Message{Role: zeroruntime.MessageRoleAssistant, Content: ""})
	if got, ok := empty.Content.(string); !ok || got != "" {
		t.Fatalf("empty text content = %#v, want \"\" so it serializes as content:\"\" (strict servers reject a missing/null content)", empty.Content)
	}
}

// TestMapMessageNonUserRolesNeverCarryImages locks the invariant that only the
// user role emits image content-parts. Anthropic/Gemini only attach images on
// their user branches; OpenAI funnels every role through mapMessage, so an
// assistant/tool/system message that happens to carry Images must still
// serialize plain string content (never a content-parts array).
func TestMapMessageNonUserRolesNeverCarryImages(t *testing.T) {
	t.Parallel()
	images := []zeroruntime.ImageBlock{{MediaType: "image/png", Data: []byte("ABC")}}
	for _, role := range []zeroruntime.MessageRole{
		zeroruntime.MessageRoleAssistant,
		zeroruntime.MessageRoleTool,
		zeroruntime.MessageRoleSystem,
	} {
		msg := mapMessage(zeroruntime.Message{Role: role, Content: "plain", Images: images})
		if got, ok := msg.Content.(string); !ok || got != "plain" {
			t.Fatalf("role %q content = %#v, want plain string (no content-parts)", role, msg.Content)
		}
		if _, isParts := msg.Content.([]contentPart); isParts {
			t.Fatalf("role %q must not emit image content-parts", role)
		}
	}
}

func TestStreamCompletionSerializesImageContentParts(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		writeSSE(w, `{"choices":[]}`)
		writeSSE(w, `[DONE]`)
	}))
	defer server.Close()

	provider, err := New(Options{
		APIKey:  "sk-secret",
		BaseURL: server.URL + "/",
		Model:   "gpt-test",
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{
			Role:    zeroruntime.MessageRoleUser,
			Content: "what is this",
			Images:  []zeroruntime.ImageBlock{{MediaType: "image/png", Data: []byte("ABC")}},
		}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}
	drain(stream)

	messages := gotBody["messages"].([]any)
	user := messages[0].(map[string]any)
	content, ok := user["content"].([]any)
	if !ok {
		t.Fatalf("user content not an array: %#v", user["content"])
	}
	if len(content) != 2 {
		t.Fatalf("content parts = %d, want 2", len(content))
	}
	textPart := content[0].(map[string]any)
	if textPart["type"] != "text" || textPart["text"] != "what is this" {
		t.Fatalf("text part = %#v", textPart)
	}
	imagePart := content[1].(map[string]any)
	if imagePart["type"] != "image_url" {
		t.Fatalf("image part type = %#v, want image_url", imagePart["type"])
	}
	imageURL := imagePart["image_url"].(map[string]any)
	if imageURL["url"] != "data:image/png;base64,QUJD" {
		t.Fatalf("image url = %#v, want data:image/png;base64,QUJD", imageURL["url"])
	}
}

// TestOpenAIRequestEmptyContentHandling locks in the fix for strict OpenAI-
// compatible servers (e.g. glm-* on Ollama-cloud) that reject a message whose
// `content` is absent/null with "invalid message content type: <nil>": a
// contentless assistant turn with no tool calls is dropped, and every other
// message serializes an explicit `"content"` field (empty string when there is
// no text) instead of omitting it.
func TestOpenAIRequestEmptyContentHandling(t *testing.T) {
	t.Parallel()
	provider, err := New(Options{Model: "gpt-test"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	req := provider.openAIRequest(zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{
			{Role: zeroruntime.MessageRoleUser, Content: "hi"},
			// Degenerate empty assistant turn (e.g. a sub-agent that failed with no
			// output): must be dropped, not sent.
			{Role: zeroruntime.MessageRoleAssistant, Content: "   "},
			// Assistant with tool calls but no text: kept, content present as "".
			{Role: zeroruntime.MessageRoleAssistant, Content: "", ToolCalls: []zeroruntime.ToolCall{{
				ID: "call_1", Name: "read_file", Arguments: "{}",
			}}},
			// Tool result with empty content: kept, content present as "".
			{Role: zeroruntime.MessageRoleTool, Content: "", ToolCallID: "call_1"},
		},
	})

	if len(req.Messages) != 3 {
		t.Fatalf("empty assistant turn should be dropped; got %d messages: %#v", len(req.Messages), req.Messages)
	}

	// No message may serialize with an absent/null content field.
	for i, message := range req.Messages {
		if message.Content == nil {
			t.Fatalf("message %d has nil content (would serialize as null/omitted): %#v", i, message)
		}
		data, err := json.Marshal(message)
		if err != nil {
			t.Fatalf("marshal message %d: %v", i, err)
		}
		if !strings.Contains(string(data), `"content":`) {
			t.Fatalf("message %d omits the content field: %s", i, data)
		}
	}

	if req.Messages[0].Content != "hi" {
		t.Fatalf("user content = %#v, want \"hi\"", req.Messages[0].Content)
	}
	// The kept-but-textless messages must send content as an explicit empty string.
	for _, idx := range []int{1, 2} {
		data, err := json.Marshal(req.Messages[idx])
		if err != nil {
			t.Fatalf("marshal message %d: %v", idx, err)
		}
		if !strings.Contains(string(data), `"content":""`) {
			t.Fatalf("message %d should send content:\"\", got %s", idx, data)
		}
	}
	if len(req.Messages[1].ToolCalls) != 1 {
		t.Fatalf("assistant tool calls dropped: %#v", req.Messages[1])
	}
}

// TestOpenAIRequestPromptCacheKey locks in prompt_cache_key forwarding: a
// session-carrying request serializes the key so the backend can route to a
// replica holding the cached prefix, a keyless request omits the field
// entirely (strict servers see byte-identical requests to before),
// DisablePromptCacheKey (used for openai-compatible gateways) suppresses it,
// and ZERO_DISABLE_PROMPT_CACHE_KEY is the env kill switch for any endpoint.
func TestOpenAIRequestPromptCacheKey(t *testing.T) {
	provider, err := New(Options{Model: "gpt-test"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	messages := []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}}

	req := provider.openAIRequest(zeroruntime.CompletionRequest{
		Messages:       messages,
		PromptCacheKey: "sess_123",
	})
	if req.PromptCacheKey != "sess_123" {
		t.Fatalf("PromptCacheKey = %q, want sess_123", req.PromptCacheKey)
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"prompt_cache_key":"sess_123"`) {
		t.Fatalf("prompt_cache_key not serialized: %s", data)
	}

	req = provider.openAIRequest(zeroruntime.CompletionRequest{Messages: messages})
	if data, err = json.Marshal(req); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "prompt_cache_key") {
		t.Fatalf("keyless request must omit prompt_cache_key: %s", data)
	}

	// openai-compatible providers are constructed with DisablePromptCacheKey so
	// strict gateways (NVIDIA NIM, …) never see the OpenAI-only field.
	compat, err := New(Options{Model: "gpt-test", DisablePromptCacheKey: true})
	if err != nil {
		t.Fatalf("New(DisablePromptCacheKey) returned error: %v", err)
	}
	req = compat.openAIRequest(zeroruntime.CompletionRequest{
		Messages:       messages,
		PromptCacheKey: "sess_123",
	})
	if req.PromptCacheKey != "" {
		t.Fatalf("DisablePromptCacheKey ignored; PromptCacheKey = %q", req.PromptCacheKey)
	}
	if data, err = json.Marshal(req); err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "prompt_cache_key") {
		t.Fatalf("compatible provider must omit prompt_cache_key: %s", data)
	}

	t.Setenv("ZERO_DISABLE_PROMPT_CACHE_KEY", "1")
	req = provider.openAIRequest(zeroruntime.CompletionRequest{
		Messages:       messages,
		PromptCacheKey: "sess_123",
	})
	if req.PromptCacheKey != "" {
		t.Fatalf("kill switch ignored; PromptCacheKey = %q", req.PromptCacheKey)
	}

	// Explicitly-falsy kill switch values must NOT disable forwarding — only
	// truthy values flip the toggle (same parsing as ZERO_FORMAT_ON_WRITE).
	for _, value := range []string{"0", "false", "FALSE"} {
		t.Setenv("ZERO_DISABLE_PROMPT_CACHE_KEY", value)
		req = provider.openAIRequest(zeroruntime.CompletionRequest{
			Messages:       messages,
			PromptCacheKey: "sess_123",
		})
		if req.PromptCacheKey != "sess_123" {
			t.Fatalf("ZERO_DISABLE_PROMPT_CACHE_KEY=%q must be a no-op; PromptCacheKey = %q", value, req.PromptCacheKey)
		}
	}
}

func TestOpenAIRequestPreservesCacheablePrefixAcrossTurns(t *testing.T) {
	t.Parallel()
	provider, err := New(Options{Model: "gpt-test"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	tools := []zeroruntime.ToolDefinition{{
		Name:        "read_file",
		Description: "Read a file.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
		},
	}}
	firstMessages := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleSystem, Content: "stable system prompt"},
		{Role: zeroruntime.MessageRoleUser, Content: "first turn"},
	}
	secondMessages := append([]zeroruntime.Message(nil), firstMessages...)
	secondMessages = append(secondMessages,
		zeroruntime.Message{Role: zeroruntime.MessageRoleAssistant, Content: "first response"},
		zeroruntime.Message{Role: zeroruntime.MessageRoleUser, Content: "second turn"},
	)

	marshalBody := func(messages []zeroruntime.Message) map[string]any {
		t.Helper()
		mapped := provider.openAIRequest(zeroruntime.CompletionRequest{
			Messages:       messages,
			Tools:          tools,
			PromptCacheKey: "session-stable-prefix",
		})
		data, marshalErr := json.Marshal(mapped)
		if marshalErr != nil {
			t.Fatalf("marshal request: %v", marshalErr)
		}
		var body map[string]any
		if unmarshalErr := json.Unmarshal(data, &body); unmarshalErr != nil {
			t.Fatalf("unmarshal request: %v", unmarshalErr)
		}
		return body
	}

	first := marshalBody(firstMessages)
	second := marshalBody(secondMessages)
	firstWireMessages := first["messages"].([]any)
	secondWireMessages := second["messages"].([]any)
	if !reflect.DeepEqual(secondWireMessages[:len(firstWireMessages)], firstWireMessages) {
		t.Fatalf("first wire messages must be an exact prefix of the second:\nfirst=%#v\nsecond=%#v", firstWireMessages, secondWireMessages)
	}
	if !reflect.DeepEqual(first["tools"], second["tools"]) {
		t.Fatalf("wire tool definitions drifted:\nfirst=%#v\nsecond=%#v", first["tools"], second["tools"])
	}
	if first["prompt_cache_key"] != second["prompt_cache_key"] || first["prompt_cache_key"] != "session-stable-prefix" {
		t.Fatalf("wire prompt cache key must remain stable: first=%#v second=%#v", first["prompt_cache_key"], second["prompt_cache_key"])
	}
}

// TestStreamCompletionSerializesNonMapPropertiesAsEmptyObject locks in the
// normalizeToolParameters type-assert branch: when a tool's parameters carry a
// `properties` field whose value is a non-map (e.g., an empty slice), it must
// be normalized to "properties":{} on the wire, never "properties":[] or
// "properties":null.
func TestStreamCompletionSerializesNonMapPropertiesAsEmptyObject(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	var gotRaw []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		gotRaw = raw
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		writeSSE(w, `{"choices":[]}`)
		writeSSE(w, `[DONE]`)
	}))
	defer server.Close()

	provider, err := New(Options{
		APIKey:  "sk-secret",
		BaseURL: server.URL + "/",
		Model:   "gpt-test",
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
		Tools: []zeroruntime.ToolDefinition{{
			Name:        "no_args",
			Description: "Takes no arguments",
			Parameters:  map[string]any{"type": "object", "properties": []any{}},
		}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}
	drain(stream)

	if !strings.Contains(string(gotRaw), `"properties":{}`) {
		t.Fatalf("request body must serialize properties as an empty object, got: %s", gotRaw)
	}
	if strings.Contains(string(gotRaw), `"properties":null`) {
		t.Fatalf("request body must not serialize properties as null, got: %s", gotRaw)
	}
	if strings.Contains(string(gotRaw), `"properties":[]`) {
		t.Fatalf("request body must not serialize properties as an empty array, got: %s", gotRaw)
	}
	tools := gotBody["tools"].([]any)
	tool := tools[0].(map[string]any)
	functionDefinition := tool["function"].(map[string]any)
	parameters := functionDefinition["parameters"].(map[string]any)
	properties, ok := parameters["properties"].(map[string]any)
	if !ok || len(properties) != 0 {
		t.Fatalf("parameters.properties = %#v, want empty object", parameters["properties"])
	}
}

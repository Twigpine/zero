package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestStreamCompletionPostsGenerateContentRequest(t *testing.T) {
	t.Parallel()
	var gotPath string
	var gotQuery string
	var gotAPIKey string
	var gotUserAgent string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotAPIKey = r.Header.Get("x-goog-api-key")
		gotUserAgent = r.Header.Get("User-Agent")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		writeSSE(w, `{"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1}}`)
	}))
	defer server.Close()

	provider, err := New(Options{
		APIKey:    "sk-google",
		BaseURL:   server.URL + "/",
		Model:     "models/gemini-2.5-flash",
		MaxTokens: 65_536,
		UserAgent: "zero-test",
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{
			{Role: zeroruntime.MessageRoleSystem, Content: "You are Zero."},
			{Role: zeroruntime.MessageRoleUser, Content: "Read the file."},
			{
				Role:    zeroruntime.MessageRoleAssistant,
				Content: "I will inspect it.",
				ToolCalls: []zeroruntime.ToolCall{{
					ID:        "call_1",
					Name:      "read_file",
					Arguments: `{"path":"src/index.ts"}`,
				}},
			},
			{Role: zeroruntime.MessageRoleTool, Content: "file contents", ToolCallID: "call_1"},
			{Role: zeroruntime.MessageRoleUser, Content: "Now grep for Zero."},
		},
		Tools: []zeroruntime.ToolDefinition{{
			Name:        "read_file",
			Description: "Read a file",
			Parameters:  map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
		}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}
	drain(stream)

	if gotPath != "/v1beta/models/gemini-2.5-flash:streamGenerateContent" {
		t.Fatalf("path = %q, want Gemini stream path", gotPath)
	}
	if gotQuery != "alt=sse" {
		t.Fatalf("query = %q, want alt=sse", gotQuery)
	}
	if gotAPIKey != "sk-google" {
		t.Fatalf("x-goog-api-key = %q, want key", gotAPIKey)
	}
	if gotUserAgent != "zero-test" {
		t.Fatalf("User-Agent = %q, want zero-test", gotUserAgent)
	}
	systemInstruction := gotBody["systemInstruction"].(map[string]any)
	if _, ok := systemInstruction["role"]; ok {
		t.Fatalf("systemInstruction = %#v, want omitted role", systemInstruction)
	}
	generationConfig := gotBody["generationConfig"].(map[string]any)
	if generationConfig["maxOutputTokens"] != float64(65_536) {
		t.Fatalf("maxOutputTokens = %#v, want 65536", generationConfig["maxOutputTokens"])
	}
	contents := gotBody["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents = %#v, want user, model, merged user", contents)
	}
	modelTurn := contents[1].(map[string]any)
	modelParts := modelTurn["parts"].([]any)
	functionCall := modelParts[1].(map[string]any)["functionCall"].(map[string]any)
	if functionCall["id"] != "call_1" || functionCall["name"] != "read_file" {
		t.Fatalf("unexpected functionCall: %#v", functionCall)
	}
	args := functionCall["args"].(map[string]any)
	if args["path"] != "src/index.ts" {
		t.Fatalf("functionCall args = %#v, want path", args)
	}
	mergedUserParts := contents[2].(map[string]any)["parts"].([]any)
	if mergedUserParts[0].(map[string]any)["functionResponse"].(map[string]any)["name"] != "read_file" {
		t.Fatalf("unexpected functionResponse: %#v", mergedUserParts[0])
	}
	if mergedUserParts[1].(map[string]any)["text"] != "Now grep for Zero." {
		t.Fatalf("user text after tool result was not merged: %#v", mergedUserParts)
	}
	tools := gotBody["tools"].([]any)
	declarations := tools[0].(map[string]any)["functionDeclarations"].([]any)
	if declarations[0].(map[string]any)["name"] != "read_file" {
		t.Fatalf("unexpected tool declarations: %#v", declarations)
	}
}

func TestStreamCompletionEnablesThinkingWhenEffortRequested(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		writeSSE(w, `{"usageMetadata":{"promptTokenCount":1}}`)
	}))
	defer server.Close()

	provider, err := New(Options{APIKey: "k", BaseURL: server.URL + "/", Model: "models/gemini-2.5-flash", MaxTokens: 65_536})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages:        []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
		ReasoningEffort: "medium",
	})
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}
	drain(stream)

	cfg := gotBody["generationConfig"].(map[string]any)
	thinking, ok := cfg["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("thinkingConfig missing: %#v", cfg)
	}
	if budget, _ := thinking["thinkingBudget"].(float64); int(budget) != 8192 {
		t.Fatalf("thinkingBudget = %#v, want 8192", thinking["thinkingBudget"])
	}
}

func TestStreamCompletionOmitsThinkingWithoutEffort(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		writeSSE(w, `{"usageMetadata":{"promptTokenCount":1}}`)
	}))
	defer server.Close()

	provider, err := New(Options{APIKey: "k", BaseURL: server.URL + "/", Model: "models/gemini-2.5-flash", MaxTokens: 65_536})
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

	cfg := gotBody["generationConfig"].(map[string]any)
	if _, ok := cfg["thinkingConfig"]; ok {
		t.Fatalf("thinkingConfig should be omitted without effort: %#v", cfg["thinkingConfig"])
	}
}

func TestStreamCompletionCapturesThoughtSignatureAndSkipsThoughtText(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		// A thought-summary part (must not surface as answer text) followed by a
		// functionCall part carrying its thoughtSignature.
		writeSSE(w, `{"candidates":[{"content":{"parts":[{"thought":true,"text":"internal reasoning"},{"functionCall":{"name":"grep","args":{"pattern":"x"}},"thoughtSignature":"sig-xyz"}]}}]}`)
	})

	events := collectProviderEvents(t, provider)
	for _, event := range events {
		if event.Type == zeroruntime.StreamEventText && strings.Contains(event.Content, "internal reasoning") {
			t.Fatalf("thought text leaked into answer: %#v", event)
		}
	}
	starts := eventsOfType(events, zeroruntime.StreamEventToolCallStart)
	if len(starts) != 1 {
		t.Fatalf("want one tool-call start, got %#v", events)
	}
	if starts[0].ToolCallSignature != "sig-xyz" {
		t.Fatalf("tool call signature = %q, want sig-xyz", starts[0].ToolCallSignature)
	}
}

func TestGeminiRequestReplaysThoughtSignature(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		writeSSE(w, `{"usageMetadata":{"promptTokenCount":1}}`)
	}))
	defer server.Close()

	provider, err := New(Options{APIKey: "k", BaseURL: server.URL + "/", Model: "models/gemini-2.5-flash"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{
			{Role: zeroruntime.MessageRoleUser, Content: "go"},
			{
				Role:      zeroruntime.MessageRoleAssistant,
				ToolCalls: []zeroruntime.ToolCall{{ID: "call_1", Name: "grep", Arguments: `{"pattern":"x"}`, Signature: "sig-xyz"}},
			},
			{Role: zeroruntime.MessageRoleTool, Content: "result", ToolCallID: "call_1"},
		},
	})
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}
	drain(stream)

	contents := gotBody["contents"].([]any)
	modelParts := contents[1].(map[string]any)["parts"].([]any)
	part := modelParts[0].(map[string]any)
	if part["thoughtSignature"] != "sig-xyz" {
		t.Fatalf("functionCall part missing replayed thoughtSignature: %#v", part)
	}
}

func TestStreamCompletionAppliesCustomAuthAndHeaders(t *testing.T) {
	t.Parallel()
	var gotDefaultAuth string
	var gotCustomAuth string
	var gotTenant string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDefaultAuth = r.Header.Get("x-goog-api-key")
		gotCustomAuth = r.Header.Get("Authorization")
		gotTenant = r.Header.Get("X-Tenant")
		writeSSE(w, `{}`)
	}))
	defer server.Close()

	provider, err := New(Options{
		APIKey:        "sk-google",
		BaseURL:       server.URL,
		Model:         "gemini-test",
		AuthHeader:    "Authorization",
		AuthScheme:    "Bearer",
		CustomHeaders: map[string]string{"X-Tenant": "zero"},
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

	if gotDefaultAuth != "" {
		t.Fatalf("x-goog-api-key = %q, want empty when custom auth header is used", gotDefaultAuth)
	}
	if gotCustomAuth != "Bearer sk-google" {
		t.Fatalf("Authorization = %q, want bearer token", gotCustomAuth)
	}
	if gotTenant != "zero" {
		t.Fatalf("X-Tenant = %q, want custom header", gotTenant)
	}
}

func TestStreamCompletionEmitsTextUsageAndReasoningTokens(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"candidates":[{"content":{"parts":[{"text":"Hello"}]}}]}`)
		writeSSE(w, `{"candidates":[{"content":{"parts":[{"text":" Zero"}]}}],"usageMetadata":{"promptTokenCount":25,"candidatesTokenCount":15,"thoughtsTokenCount":3,"cachedContentTokenCount":7}}`)
	})

	events := collectProviderEvents(t, provider)
	want := []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "Hello"},
		{Type: zeroruntime.StreamEventText, Content: " Zero"},
		{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{InputTokens: 25, OutputTokens: 18, PromptTokens: 25, CompletionTokens: 18, ReasoningTokens: 3, CachedInputTokens: 7}},
		{Type: zeroruntime.StreamEventDone},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestStreamCompletionEmitsCandidateFunctionCalls(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_1","name":"read_file","args":{"path":"src/index.ts"}}},{"functionCall":{"id":"call_2","name":"grep","args":{"pattern":"Zero"}}}]}}]}`)
	})

	events := collectProviderEvents(t, provider)
	want := []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call_1", ToolName: "read_file"},
		{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call_1", ArgumentsFragment: `{"path":"src/index.ts"}`},
		{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call_1"},
		{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call_2", ToolName: "grep"},
		{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call_2", ArgumentsFragment: `{"pattern":"Zero"}`},
		{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call_2"},
		{Type: zeroruntime.StreamEventDone},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestStreamCompletionEmitsTopLevelFunctionCalls(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"functionCalls":[{"id":"call_1","name":"read_file","args":{"path":"README.md"}}]}`)
	})

	events := collectProviderEvents(t, provider)
	want := []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "call_1", ToolName: "read_file"},
		{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "call_1", ArgumentsFragment: `{"path":"README.md"}`},
		{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "call_1"},
		{Type: zeroruntime.StreamEventDone},
	}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %#v, want %#v", events, want)
	}
}

func TestStreamCompletionUsesSyntheticToolIDsWhenMissing(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"functionCalls":[{"name":"grep","args":{"pattern":"Zero"}}]}`)
	})

	events := collectProviderEvents(t, provider)
	if events[0].ToolCallID != "gemini_tool_1" || events[0].ToolName != "grep" {
		t.Fatalf("events = %#v, want synthetic tool id", events)
	}
}

func TestStreamCompletionClassifiesHTTPAndPromptBlockErrors(t *testing.T) {
	t.Parallel()
	authProvider := newTestProviderWithKey(t, "sk-google", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"API key not valid"}}`, http.StatusUnauthorized)
	})
	stream, err := authProvider.StreamCompletion(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("StreamCompletion returned setup error: %v", err)
	}
	events := readAll(stream)
	if len(events) != 1 || events[0].Type != zeroruntime.StreamEventError || !strings.HasPrefix(events[0].Error, "auth error:") {
		t.Fatalf("events = %#v, want auth error", events)
	}
	if strings.Contains(events[0].Error, "sk-google") {
		t.Fatalf("error leaked token: %q", events[0].Error)
	}

	blockProvider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"promptFeedback":{"blockReason":"SAFETY","blockReasonMessage":"blocked by policy"}}`)
	})
	events = collectProviderEvents(t, blockProvider)
	if len(events) != 1 || events[0].Type != zeroruntime.StreamEventError || !strings.Contains(events[0].Error, "Content blocked: blocked by policy") {
		t.Fatalf("events = %#v, want content block error", events)
	}
}

// A 401 with an OAuth resolver is retried once with a force-refreshed token; the
// replayed request carries the refreshed bearer and succeeds.
func TestStreamCompletionRetries401WithRefreshedToken(t *testing.T) {
	t.Parallel()
	var attempts int
	var secondAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			http.Error(w, `{"error":{"message":"token expired"}}`, http.StatusUnauthorized)
			return
		}
		secondAuth = r.Header.Get("Authorization")
		writeSSE(w, `{}`)
	}))
	defer server.Close()

	var forceRefreshOnRetry bool
	resolver := func(ctx context.Context, forceRefresh bool) (string, string, bool, error) {
		if forceRefresh {
			forceRefreshOnRetry = true
			return "Authorization", "Bearer refreshed", true, nil
		}
		return "Authorization", "Bearer stale", true, nil
	}

	provider, err := New(Options{
		APIKey:        "sk-google",
		BaseURL:       server.URL,
		Model:         "gemini-test",
		OAuthResolver: resolver,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}
	drain(stream)

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (initial 401 + one refreshed retry)", attempts)
	}
	if !forceRefreshOnRetry {
		t.Fatalf("resolver was not called with forceRefresh on the retry")
	}
	if secondAuth != "Bearer refreshed" {
		t.Fatalf("retry Authorization = %q, want refreshed bearer", secondAuth)
	}
}

func TestStreamCompletionEmitsStreamErrorObject(t *testing.T) {
	t.Parallel()
	provider := newTestProviderWithKey(t, "sk-google", func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"error":{"code":429,"message":"stream failed sk-google","status":"RESOURCE_EXHAUSTED"}}`)
	})

	events := collectProviderEvents(t, provider)
	if len(events) != 1 || events[0].Type != zeroruntime.StreamEventError {
		t.Fatalf("events = %#v, want one error", events)
	}
	if !strings.HasPrefix(events[0].Error, "rate limit error:") {
		t.Fatalf("error = %q, want rate limit error prefix", events[0].Error)
	}
	if strings.Contains(events[0].Error, "sk-google") {
		t.Fatalf("error leaked token: %q", events[0].Error)
	}
}

func TestStreamCompletionStopsOnMalformedStreamToolArgs(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"functionCalls":[{"id":"call_1","name":"grep","args":"raw"}]}`)
		writeSSE(w, `{"candidates":[{"content":{"parts":[{"text":"should not emit"}]}}]}`)
	})

	events := collectProviderEvents(t, provider)
	if len(events) != 1 || events[0].Type != zeroruntime.StreamEventError {
		t.Fatalf("events = %#v, want one error", events)
	}
	if !strings.Contains(events[0].Error, "streamed tool arguments for grep") {
		t.Fatalf("error = %q, want streamed tool arguments error", events[0].Error)
	}
	if len(eventsOfType(events, zeroruntime.StreamEventDone)) != 0 {
		t.Fatalf("events = %#v, want no done after stream tool arg error", events)
	}
}

func TestStreamCompletionRejectsMalformedHistoryBeforeDispatch(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("provider should not dispatch malformed history")
	})

	_, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleTool, Content: "missing id"}},
	})
	if err == nil || !strings.Contains(err.Error(), "requires toolCallId") {
		t.Fatalf("error = %v, want missing toolCallId", err)
	}

	_, err = provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{
			{Role: zeroruntime.MessageRoleUser, Content: "call tool"},
			{
				Role:      zeroruntime.MessageRoleAssistant,
				ToolCalls: []zeroruntime.ToolCall{{ID: "call_1", Name: "read_file", Arguments: `"src/index.ts"`}},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "requires tool arguments for read_file to be a JSON object") {
		t.Fatalf("error = %v, want non-object tool argument error", err)
	}
}

func TestNewRequiresModelAndPositiveMaxTokens(t *testing.T) {
	t.Parallel()
	if _, err := New(Options{}); err == nil {
		t.Fatal("New without model returned nil error")
	}
	if _, err := New(Options{Model: "gemini-test", MaxTokens: -1}); err == nil {
		t.Fatal("New with negative max tokens returned nil error")
	}
}

func newTestProvider(t *testing.T, handler http.HandlerFunc) *Provider {
	t.Helper()
	return newTestProviderWithKey(t, "", handler)
}

func newTestProviderWithKey(t *testing.T, apiKey string, handler http.HandlerFunc) *Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := New(Options{
		APIKey:  apiKey,
		BaseURL: server.URL,
		Model:   "gemini-test",
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	return provider
}

func collectProviderEvents(t *testing.T, provider *Provider) []zeroruntime.StreamEvent {
	t.Helper()
	stream, err := provider.StreamCompletion(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("StreamCompletion returned setup error: %v", err)
	}
	return readAll(stream)
}

func validRequest() zeroruntime.CompletionRequest {
	return zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hello"}},
	}
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

func eventsOfType(events []zeroruntime.StreamEvent, eventType zeroruntime.StreamEventType) []zeroruntime.StreamEvent {
	matching := []zeroruntime.StreamEvent{}
	for _, event := range events {
		if event.Type == eventType {
			matching = append(matching, event)
		}
	}
	return matching
}

// TestSanitizeGeminiSchemaStripsUnsupportedFields: the sanitizer keeps only
// Gemini-supported keywords and recurses into properties/items/anyOf, so no
// OpenAI-ism (additionalProperties, $schema, patternProperties) survives at any
// depth while legitimate fields (type/description/enum/required/default) stay.
func TestSanitizeGeminiSchemaStripsUnsupportedFields(t *testing.T) {
	t.Parallel()
	in := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"required":             []string{"path"},
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "a path",
				"default":     ".",
			},
			"nested": map[string]any{
				"type":                 "object",
				"additionalProperties": false, // must be stripped at depth too
				"properties": map[string]any{
					"tags": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "string", "additionalProperties": true},
					},
				},
			},
		},
	}
	out := sanitizeGeminiSchema(in)

	assertNoAdditionalProps(t, out, "root")
	if out["additionalProperties"] != nil {
		t.Fatal("top-level additionalProperties must be stripped")
	}
	if out["$schema"] != nil {
		t.Fatal("$schema must be stripped")
	}
	// Legitimate fields survive.
	if out["type"] != "object" {
		t.Fatalf("type should survive, got %v", out["type"])
	}
	props := out["properties"].(map[string]any)
	pathProp := props["path"].(map[string]any)
	if pathProp["description"] != "a path" || pathProp["default"] != "." {
		t.Fatalf("legitimate property fields must survive: %#v", pathProp)
	}
	// nil input stays nil (parameterless tool).
	if sanitizeGeminiSchema(nil) != nil {
		t.Fatal("nil schema should map to nil")
	}
}

// assertNoAdditionalProps fails if additionalProperties appears anywhere in the
// (recursively walked) schema.
func assertNoAdditionalProps(t *testing.T, node any, path string) {
	t.Helper()
	switch v := node.(type) {
	case map[string]any:
		if _, ok := v["additionalProperties"]; ok {
			t.Fatalf("additionalProperties leaked at %s", path)
		}
		for k, sub := range v {
			assertNoAdditionalProps(t, sub, path+"."+k)
		}
	case []any:
		for i, sub := range v {
			assertNoAdditionalProps(t, sub, fmt.Sprintf("%s[%d]", path, i))
		}
	}
}

// TestGeminiRequestOmitsAdditionalPropertiesInToolSchema: end-to-end, the tool
// parameters Zero emits (schemaToRuntimeMap always writes additionalProperties)
// must not reach Gemini — otherwise every functionDeclaration is 400-rejected
// ("Unknown name additionalProperties"), which broke all tool-using exec calls
// against Google (issue #373).
func TestGeminiRequestOmitsAdditionalPropertiesInToolSchema(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		writeSSE(w, `{"usageMetadata":{"promptTokenCount":1}}`)
	}))
	defer server.Close()

	provider, err := New(Options{APIKey: "k", BaseURL: server.URL + "/", Model: "models/gemini-2.5-flash"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
		Tools: []zeroruntime.ToolDefinition{{
			Name:        "grep",
			Description: "search",
			Parameters: map[string]any{ // exactly what schemaToRuntimeMap produces
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"pattern"},
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string", "description": "regex"},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}
	drain(stream)

	tools := gotBody["tools"].([]any)
	decls := tools[0].(map[string]any)["functionDeclarations"].([]any)
	params := decls[0].(map[string]any)["parameters"].(map[string]any)
	if _, ok := params["additionalProperties"]; ok {
		t.Fatalf("additionalProperties must not be sent to Gemini: %#v", params)
	}
	// The tool is still usable: its real schema survived.
	if params["type"] != "object" {
		t.Fatalf("tool parameters type should survive, got %v", params["type"])
	}
	props := params["properties"].(map[string]any)
	if _, ok := props["pattern"]; !ok {
		t.Fatalf("tool property must survive sanitization: %#v", props)
	}
}

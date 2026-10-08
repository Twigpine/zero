package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Gitlawb/zero/internal/trace"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestCodexTurnSessionChainsOnlyAppendOnlyRequests(t *testing.T) {
	var mu sync.Mutex
	requests := []map[string]any{}
	betaHeader := ""
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
			http.Error(writer, "websocket required", http.StatusUpgradeRequired)
			return
		}
		mu.Lock()
		betaHeader = request.Header.Get(responsesWebSocketBetaHeader)
		mu.Unlock()
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		for index := 0; index < 3; index++ {
			_, body, err := connection.Read(request.Context())
			if err != nil {
				return
			}
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				return
			}
			mu.Lock()
			requests = append(requests, payload)
			mu.Unlock()
			switch index {
			case 0:
				writeWebSocketEvents(request.Context(), connection,
					`{"type":"response.created","response":{"id":"resp-1","status":"in_progress"}}`,
					`{"type":"response.output_item.added","item_id":"call-1","item":{"type":"function_call","id":"call-1","call_id":"call-1","name":"read_file"}}`,
					`{"type":"response.function_call_arguments.delta","item_id":"call-1","delta":"{}"}`,
					`{"type":"response.output_item.done","item_id":"call-1","item":{"type":"function_call","id":"call-1","call_id":"call-1","name":"read_file","arguments":"{}"}}`,
					`{"type":"response.completed","response":{"id":"resp-1","status":"completed"}}`,
				)
			case 1:
				writeWebSocketEvents(request.Context(), connection,
					`{"type":"response.output_text.delta","delta":"done"}`,
					`{"type":"response.completed","response":{"id":"resp-2","status":"completed"}}`,
				)
			case 2:
				writeWebSocketEvents(request.Context(), connection,
					`{"type":"response.completed","response":{"id":"resp-3","status":"completed"}}`,
				)
			}
		}
	}))
	defer server.Close()

	provider := newCodexSessionTestProvider(t, server)
	// A disabled stream watchdog must use the parent context rather than creating
	// an immediately-expired read deadline.
	provider.inner.streamIdleTimeout = 0
	session := openCodexSession(t, provider)
	defer session.Close()
	recorder := trace.NewRecorder("session-1", "run-1", "test")
	recorder.Start()
	streamContext := trace.WithContext(t.Context(), recorder)

	base := zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{
			{Role: zeroruntime.MessageRoleSystem, Content: "Use tools carefully."},
			{Role: zeroruntime.MessageRoleUser, Content: "Read the file."},
		},
		Tools:          []zeroruntime.ToolDefinition{{Name: "read_file", Parameters: map[string]any{"type": "object"}}},
		PromptCacheKey: "session-1",
	}
	first := collectCodexSessionEvents(t, session, base)
	if !hasToolCallEnd(first, "call-1") {
		t.Fatalf("first stream did not produce the tool call: %#v", first)
	}

	second := base
	second.Messages = append(append([]zeroruntime.Message(nil), base.Messages...),
		zeroruntime.Message{Role: zeroruntime.MessageRoleAssistant, ToolCalls: []zeroruntime.ToolCall{{ID: "call-1", Name: "read_file", Arguments: "{}"}}},
		zeroruntime.Message{Role: zeroruntime.MessageRoleTool, ToolCallID: "call-1", Content: "contents"},
	)
	secondEvents := collectCodexSessionEventsContext(t, streamContext, session, second)
	if got := joinedText(secondEvents); got != "done" {
		t.Fatalf("second stream text = %q, want done", got)
	}

	third := second
	third.ReasoningEffort = "high"
	third.Messages = append(append([]zeroruntime.Message(nil), second.Messages...),
		zeroruntime.Message{Role: zeroruntime.MessageRoleAssistant, Content: "done"},
		zeroruntime.Message{Role: zeroruntime.MessageRoleUser, Content: "Continue."},
	)
	collectCodexSessionEventsContext(t, streamContext, session, third)
	finishedTrace := recorder.Finish()
	if got := finishedTrace.Counter(trace.CounterResponseChainReused); got != 1 {
		t.Fatalf("response-chain reuse count = %d, want 1", got)
	}
	if got := finishedTrace.Counter(trace.CounterResponseChainReset); got != 1 {
		t.Fatalf("response-chain reset count = %d, want 1", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 3 {
		t.Fatalf("websocket requests = %d, want 3", len(requests))
	}
	if betaHeader != responsesWebSocketBetaValue {
		t.Fatalf("OpenAI-Beta = %q, want %q", betaHeader, responsesWebSocketBetaValue)
	}
	if requests[0]["type"] != responsesWebSocketRequest || requests[0]["previous_response_id"] != nil {
		t.Fatalf("first request chaining fields = %#v", requests[0])
	}
	if requests[0]["parallel_tool_calls"] != true || requests[0]["prompt_cache_key"] != "session-1" {
		t.Fatalf("first request stable fields = %#v", requests[0])
	}
	if requests[1]["previous_response_id"] != "resp-1" {
		t.Fatalf("second previous_response_id = %#v", requests[1]["previous_response_id"])
	}
	secondInput, _ := requests[1]["input"].([]any)
	if len(secondInput) != 1 || secondInput[0].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("second incremental input = %#v", requests[1]["input"])
	}
	if requests[2]["previous_response_id"] != nil {
		t.Fatalf("incompatible request reused response chain: %#v", requests[2])
	}
	thirdInput, _ := requests[2]["input"].([]any)
	if len(thirdInput) <= 1 {
		t.Fatalf("incompatible request did not send full input: %#v", thirdInput)
	}
}

func TestCodexTurnSessionFallsBackWhenPreviousResponseIsMissing(t *testing.T) {
	var mu sync.Mutex
	webSocketRequests := 0
	httpRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
			connection, err := websocket.Accept(writer, request, nil)
			if err != nil {
				return
			}
			defer connection.CloseNow()
			for index := 0; index < 2; index++ {
				if _, _, err := connection.Read(request.Context()); err != nil {
					return
				}
				mu.Lock()
				webSocketRequests++
				mu.Unlock()
				if index == 0 {
					writeWebSocketEvents(request.Context(), connection,
						`{"type":"response.output_item.added","item_id":"call-1","item":{"type":"function_call","id":"call-1","call_id":"call-1","name":"read_file"}}`,
						`{"type":"response.output_item.done","item_id":"call-1","item":{"type":"function_call","id":"call-1","call_id":"call-1","name":"read_file","arguments":"{}"}}`,
						`{"type":"response.completed","response":{"id":"resp-1","status":"completed"}}`,
					)
					continue
				}
				writeWebSocketEvents(request.Context(), connection,
					`{"type":"response.error","code":"previous_response_not_found","message":"missing"}`,
				)
			}
			return
		}

		mu.Lock()
		httpRequests++
		mu.Unlock()
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fallback\"}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-http\",\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()

	provider := newCodexSessionTestProvider(t, server)
	session := openCodexSession(t, provider)
	defer session.Close()
	base := zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "Read it."}},
		Tools:    []zeroruntime.ToolDefinition{{Name: "read_file", Parameters: map[string]any{"type": "object"}}},
	}
	collectCodexSessionEvents(t, session, base)
	second := base
	second.Messages = append(append([]zeroruntime.Message(nil), base.Messages...),
		zeroruntime.Message{Role: zeroruntime.MessageRoleAssistant, ToolCalls: []zeroruntime.ToolCall{{ID: "call-1", Name: "read_file", Arguments: "{}"}}},
		zeroruntime.Message{Role: zeroruntime.MessageRoleTool, ToolCallID: "call-1", Content: "contents"},
	)
	events := collectCodexSessionEvents(t, session, second)
	if got := joinedText(events); got != "fallback" {
		t.Fatalf("fallback stream text = %q, want fallback; events=%#v", got, events)
	}
	for _, event := range events {
		if event.Type == zeroruntime.StreamEventError {
			t.Fatalf("response-chain recovery leaked an error: %#v", event)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if webSocketRequests != 2 || httpRequests != 1 {
		t.Fatalf("requests = websocket:%d http:%d, want 2/1", webSocketRequests, httpRequests)
	}
}

func TestCodexTurnSessionFallsBackWhenSocketClosesBeforeOutput(t *testing.T) {
	var mu sync.Mutex
	httpRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
			connection, err := websocket.Accept(writer, request, nil)
			if err != nil {
				return
			}
			if _, _, err := connection.Read(request.Context()); err == nil {
				_ = connection.CloseNow()
			}
			return
		}
		mu.Lock()
		httpRequests++
		mu.Unlock()
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"recovered\"}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-http\",\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()

	provider := newCodexSessionTestProvider(t, server)
	session := openCodexSession(t, provider)
	defer session.Close()
	recorder := trace.NewRecorder("session-fallback", "run-fallback", "test")
	recorder.Start()
	events := collectCodexSessionEventsContext(t, trace.WithContext(t.Context(), recorder), session, zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "Hello"}},
	})
	if got := joinedText(events); got != "recovered" {
		t.Fatalf("fallback text = %q, want recovered", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if httpRequests != 1 {
		t.Fatalf("HTTP fallback requests = %d, want 1", httpRequests)
	}
	if got := recorder.Finish().Counter(trace.CounterResponsesHTTPFallback); got != 1 {
		t.Fatalf("responses HTTP fallback counter = %d, want 1", got)
	}
}

func TestCodexTurnSessionKeepsWebSocketAfterIncompleteResponse(t *testing.T) {
	var mu sync.Mutex
	webSocketRequests := 0
	httpRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
			connection, err := websocket.Accept(writer, request, nil)
			if err != nil {
				return
			}
			defer connection.CloseNow()
			for index := 0; index < 2; index++ {
				if _, _, err := connection.Read(request.Context()); err != nil {
					return
				}
				mu.Lock()
				webSocketRequests++
				mu.Unlock()
				if index == 0 {
					writeWebSocketEvents(request.Context(), connection,
						`{"type":"response.incomplete","response":{"id":"resp-1","status":"incomplete"}}`,
					)
					continue
				}
				writeWebSocketEvents(request.Context(), connection,
					`{"type":"response.output_text.delta","delta":"websocket"}`,
					`{"type":"response.completed","response":{"id":"resp-2","status":"completed"}}`,
				)
			}
			return
		}

		mu.Lock()
		httpRequests++
		mu.Unlock()
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"fallback\"}\n\n")
		_, _ = fmt.Fprint(writer, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-http\",\"status\":\"completed\"}}\n\n")
	}))
	defer server.Close()

	provider := newCodexSessionTestProvider(t, server)
	session := openCodexSession(t, provider)
	defer session.Close()
	request := zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "Continue."}},
	}
	first := collectCodexSessionEvents(t, session, request)
	if len(first) != 1 || first[0].Type != zeroruntime.StreamEventDone || first[0].FinishReason != zeroruntime.FinishReasonLength {
		t.Fatalf("incomplete response events = %#v, want length completion", first)
	}
	second := collectCodexSessionEvents(t, session, request)
	if got := joinedText(second); got != "websocket" {
		t.Fatalf("second turn text = %q, want websocket; events=%#v", got, second)
	}
	mu.Lock()
	defer mu.Unlock()
	if webSocketRequests != 2 || httpRequests != 0 {
		t.Fatalf("requests = websocket:%d http:%d, want 2/0", webSocketRequests, httpRequests)
	}
}

func TestCodexTurnSessionDoesNotReplayAfterVisibleOutput(t *testing.T) {
	var mu sync.Mutex
	httpRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
			connection, err := websocket.Accept(writer, request, nil)
			if err != nil {
				return
			}
			if _, _, err := connection.Read(request.Context()); err == nil {
				writeWebSocketEvents(request.Context(), connection,
					`{"type":"response.output_text.delta","delta":"visible"}`,
				)
			}
			_ = connection.Close(websocket.StatusInternalError, "interrupted")
			return
		}
		mu.Lock()
		httpRequests++
		mu.Unlock()
	}))
	defer server.Close()

	provider := newCodexSessionTestProvider(t, server)
	session := openCodexSession(t, provider)
	defer session.Close()
	stream, err := session.Stream(t.Context(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "Hello"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	sawText, sawError := false, false
	for event := range stream {
		sawText = sawText || event.Type == zeroruntime.StreamEventText
		sawError = sawError || event.Type == zeroruntime.StreamEventError
	}
	if !sawText || !sawError {
		t.Fatalf("visible interrupted stream = text:%v error:%v", sawText, sawError)
	}
	mu.Lock()
	defer mu.Unlock()
	if httpRequests != 0 {
		t.Fatalf("visible interrupted stream was replayed over HTTP %d time(s)", httpRequests)
	}
}

func TestCodexTurnSessionReportsWebSocketIdleTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		if _, _, err := connection.Read(request.Context()); err != nil {
			return
		}
		writeWebSocketEvents(request.Context(), connection,
			`{"type":"response.output_text.delta","delta":"visible"}`,
		)
		<-request.Context().Done()
	}))
	defer server.Close()

	provider := newCodexSessionTestProvider(t, server)
	provider.inner.streamIdleTimeout = 200 * time.Millisecond
	session := openCodexSession(t, provider)
	defer session.Close()
	stream, err := session.Stream(t.Context(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "Hello"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var streamError string
	for event := range stream {
		if event.Type == zeroruntime.StreamEventError {
			streamError = event.Error
			break
		}
	}
	if !strings.Contains(streamError, "idle timeout after 200ms") {
		t.Fatalf("stream error = %q, want idle-timeout detail", streamError)
	}
}

func TestCodexTurnSessionRejectsOverlappingStreams(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseStream := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseStream)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.CloseNow()
		if _, _, err := connection.Read(request.Context()); err != nil {
			return
		}
		close(started)
		<-release
		writeWebSocketEvents(request.Context(), connection,
			`{"type":"response.completed","response":{"id":"resp-1","status":"completed"}}`,
		)
	}))
	defer server.Close()

	provider := newCodexSessionTestProvider(t, server)
	session := openCodexSession(t, provider)
	defer session.Close()
	request := zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "Hello"}},
	}
	first, err := session.Stream(t.Context(), request)
	if err != nil {
		t.Fatalf("first Stream: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first WebSocket stream did not start")
	}
	if _, err := session.Stream(t.Context(), request); !errors.Is(err, errCodexTurnSessionBusy) {
		t.Fatalf("overlapping Stream error = %v, want %v", err, errCodexTurnSessionBusy)
	}
	releaseStream()
	for range first {
	}
}

func TestCodexTurnSessionHTTPFallbackRedactsSetupError(t *testing.T) {
	const secret = "sk-secret-fallback"
	provider, err := NewCodexProvider(CodexOptions{Options: Options{
		APIKey:  secret,
		BaseURL: "https://chatgpt.example/backend-api/codex",
		Model:   "gpt-test",
	}})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	session := &codexTurnSession{provider: provider}
	events := make(chan zeroruntime.StreamEvent, 1)
	session.forwardHTTP(t.Context(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRole(secret), Content: "invalid"}},
	}, events)

	select {
	case event := <-events:
		if event.Type != zeroruntime.StreamEventError {
			t.Fatalf("fallback event type = %s, want error", event.Type)
		}
		if strings.Contains(event.Error, secret) {
			t.Fatalf("fallback error leaked credential: %q", event.Error)
		}
	case <-time.After(time.Second):
		t.Fatal("fallback did not emit setup error")
	}
}

func TestCodexTurnSessionCloseDoesNotStartPrewarm(t *testing.T) {
	provider, err := NewCodexProvider(CodexOptions{Options: Options{
		APIKey:  "test-token",
		BaseURL: "https://chatgpt.example/backend-api/codex",
		Model:   "gpt-test",
	}})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	session, err := NewCodexTurnSessionProvider(provider, zeroruntime.ProviderCapabilities{}).OpenTurnSession(t.Context())
	if err != nil {
		t.Fatalf("OpenTurnSession: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- session.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Close blocked without a prewarm attempt")
	}
}

func newCodexSessionTestProvider(t *testing.T, server *httptest.Server) *CodexProvider {
	t.Helper()
	provider, err := NewCodexProvider(CodexOptions{
		Options: Options{
			APIKey:     "test-token",
			BaseURL:    server.URL,
			Model:      "gpt-test",
			HTTPClient: server.Client(),
		},
		AccountID: "account-test",
	})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	return provider
}

func openCodexSession(t *testing.T, provider *CodexProvider) zeroruntime.TurnSession {
	t.Helper()
	session, err := NewCodexTurnSessionProvider(provider, zeroruntime.ProviderCapabilities{}).OpenTurnSession(t.Context())
	if err != nil {
		t.Fatalf("OpenTurnSession: %v", err)
	}
	if err := session.Prewarm(t.Context()); err != nil {
		t.Fatalf("Prewarm: %v", err)
	}
	return session
}

func collectCodexSessionEvents(t *testing.T, session zeroruntime.TurnSession, request zeroruntime.CompletionRequest) []zeroruntime.StreamEvent {
	t.Helper()
	return collectCodexSessionEventsContext(t, t.Context(), session, request)
}

func collectCodexSessionEventsContext(t *testing.T, ctx context.Context, session zeroruntime.TurnSession, request zeroruntime.CompletionRequest) []zeroruntime.StreamEvent {
	t.Helper()
	stream, err := session.Stream(ctx, request)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := []zeroruntime.StreamEvent{}
	for event := range stream {
		events = append(events, event)
		if event.Type == zeroruntime.StreamEventError {
			t.Fatalf("stream error: %s", event.Error)
		}
	}
	return events
}

func writeWebSocketEvents(ctx context.Context, connection *websocket.Conn, events ...string) {
	for _, event := range events {
		if err := connection.Write(ctx, websocket.MessageText, []byte(event)); err != nil {
			return
		}
	}
}

func hasToolCallEnd(events []zeroruntime.StreamEvent, id string) bool {
	for _, event := range events {
		if event.Type == zeroruntime.StreamEventToolCallEnd && event.ToolCallID == id {
			return true
		}
	}
	return false
}

func joinedText(events []zeroruntime.StreamEvent) string {
	var builder strings.Builder
	for _, event := range events {
		if event.Type == zeroruntime.StreamEventText {
			builder.WriteString(event.Content)
		}
	}
	return builder.String()
}

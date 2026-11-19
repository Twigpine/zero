package openai

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

// A gateway can put any JSON type in the optional `model` key. The key is
// attribution only, so a value that is not a string must read as absent and
// must not stop the frame's content, usage or completion from being handled.
var nonStringModelValues = []string{"123", "17.5", "true", "null", "{}", `{"id":"x"}`, "[]", `["a"]`}

func TestStreamCompletionNonStringModelDoesNotAbortStream(t *testing.T) {
	t.Parallel()
	for _, value := range nonStringModelValues {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				writeSSE(w, fmt.Sprintf(`{"model":%s,"choices":[{"delta":{"content":"ok"}}]}`, value))
				writeSSE(w, fmt.Sprintf(`{"model":%s,"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}`, value))
				writeSSE(w, `[DONE]`)
			})
			events := collectProviderEvents(t, provider)
			var usageEvent *zeroruntime.StreamEvent
			var content string
			for i := range events {
				switch events[i].Type {
				case zeroruntime.StreamEventError:
					t.Fatalf("model=%s aborted the stream: %q", value, events[i].Error)
				case zeroruntime.StreamEventUsage:
					usageEvent = &events[i]
				case zeroruntime.StreamEventText:
					content += events[i].Content
				}
			}
			if content != "ok" {
				t.Fatalf("model=%s: content = %q, want ok", value, content)
			}
			if usageEvent == nil {
				t.Fatalf("model=%s: missing usage event: %#v", value, events)
			}
			if usageEvent.Usage.PromptTokens != 3 || usageEvent.Usage.CompletionTokens != 1 {
				t.Fatalf("model=%s: usage = %#v, want 3/1", value, usageEvent.Usage)
			}
			if usageEvent.Usage.ResolvedModel != "" {
				t.Fatalf("model=%s: ResolvedModel = %q, want empty", value, usageEvent.Usage.ResolvedModel)
			}
		})
	}
}

func TestCodexTerminalNonStringModelDoesNotAbortTurn(t *testing.T) {
	t.Parallel()
	for _, value := range nonStringModelValues {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			var rec codexRequest
			srv := newCodexResponsesServer(t, &rec,
				`{"type":"response.created","response":{"id":"resp-1","status":"in_progress"}}`,
				fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp-1","status":"completed","model":%s,"usage":{"input_tokens":3,"output_tokens":2}}}`, value),
			)
			defer srv.Close()
			provider, err := NewCodexProvider(CodexOptions{Options: Options{APIKey: "sk-test", BaseURL: srv.URL, Model: "gpt-test"}})
			if err != nil {
				t.Fatalf("NewCodexProvider: %v", err)
			}
			stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
				Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("StreamCompletion: %v", err)
			}
			var usageEv *zeroruntime.StreamEvent
			done := false
			for ev := range stream {
				switch ev.Type {
				case zeroruntime.StreamEventError:
					t.Fatalf("model=%s aborted the turn: %q", value, ev.Error)
				case zeroruntime.StreamEventUsage:
					usageEv = &ev
				case zeroruntime.StreamEventDone:
					done = true
				}
			}
			if usageEv == nil || !done {
				t.Fatalf("model=%s: usage=%v done=%v", value, usageEv != nil, done)
			}
			if usageEv.Usage.ResolvedModel != "" {
				t.Fatalf("model=%s: ResolvedModel = %q, want empty", value, usageEv.Usage.ResolvedModel)
			}
		})
	}
}

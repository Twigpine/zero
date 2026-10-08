package anthropic

import (
	"net/http"
	"testing"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// A message_delta with stop_reason=="max_tokens" means the response was
// truncated at the output cap. The provider must surface it on the done event so
// the agent does not treat a clipped answer as complete.
func TestStreamCompletionSurfacesMaxTokensStopReason(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSEEvent(w, "message_start", `{"type":"message_start","message":{"usage":{"input_tokens":5,"output_tokens":0}}}`)
		writeSSEEvent(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cut"}}`)
		writeSSEEvent(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":7}}`)
		writeSSEEvent(w, "message_stop", `{"type":"message_stop"}`)
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
}

// A normal end_turn stop_reason must leave the done event's FinishReason empty.
func TestStreamCompletionNormalStopReasonHasNoFinishReason(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSEEvent(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`)
		writeSSEEvent(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`)
		writeSSEEvent(w, "message_stop", `{"type":"message_stop"}`)
	})

	events := collectProviderEvents(t, provider)
	var sawDone bool
	for _, e := range events {
		if e.Type == zeroruntime.StreamEventDone {
			sawDone = true
			if e.FinishReason != "" {
				t.Fatalf("normal stop leaked FinishReason %q", e.FinishReason)
			}
		}
	}
	if !sawDone {
		t.Fatalf("no done event; events: %+v", events)
	}
}

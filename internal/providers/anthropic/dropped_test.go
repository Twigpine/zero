package anthropic

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// A tool_use content_block_start that arrives without a usable name/id can't be
// dispatched. The provider must signal a dropped tool call (once) so the agent
// can ask the model to retry, mirroring the OpenAI provider's behavior, instead
// of silently dropping it.
func TestStreamCompletionEmitsDroppedOnNamelessToolUseBlock(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSEEvent(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"","name":""}}`)
		writeSSEEvent(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		writeSSEEvent(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer server.Close()

	provider, err := New(Options{BaseURL: server.URL + "/", Model: "claude-test"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	events := collectProviderEvents(t, provider)

	var dropped, started int
	for _, e := range events {
		switch e.Type {
		case zeroruntime.StreamEventToolCallDropped:
			dropped++
		case zeroruntime.StreamEventToolCallStart:
			started++
		}
	}
	if started != 0 {
		t.Errorf("a nameless tool_use block must not start a tool call, got %d starts", started)
	}
	if dropped != 1 {
		t.Errorf("expected exactly one dropped-tool-call signal, got %d; events: %+v", dropped, events)
	}
}

// A well-formed tool_use block must NOT emit a dropped signal.
func TestStreamCompletionDoesNotDropValidToolUseBlock(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSEEvent(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file"}}`)
		writeSSEEvent(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		writeSSEEvent(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer server.Close()

	provider, err := New(Options{BaseURL: server.URL + "/", Model: "claude-test"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	events := collectProviderEvents(t, provider)

	for _, e := range events {
		if e.Type == zeroruntime.StreamEventToolCallDropped {
			t.Errorf("valid tool_use block must not be dropped; events: %+v", events)
		}
	}
}

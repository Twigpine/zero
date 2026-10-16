package gemini

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// A stalled-but-open Gemini upstream (sends one chunk, then hangs without
// closing) must abort on the idle timeout instead of blocking the agent forever.
func TestStreamCompletionIdleTimeoutAbortsStalledStream(t *testing.T) {
	t.Parallel()
	released := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`)
		select {
		case <-r.Context().Done():
		case <-released:
		}
	}))
	defer server.Close()
	defer close(released)

	provider, err := New(Options{
		BaseURL:           server.URL + "/",
		Model:             "gemini-test",
		StreamIdleTimeout: 80 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}

	stream, err := provider.StreamCompletion(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}

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

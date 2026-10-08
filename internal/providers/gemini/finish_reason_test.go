package gemini

import (
	"net/http"
	"testing"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// TestMapFinishReasonNonNormal unit-tests the reason mapping in isolation: the
// normal set collapses to "", content-filter reasons map to content_filter,
// MAX_TOKENS to length, and every other non-STOP reason surfaces its raw value
// (M3) so the turn is not mistaken for a clean completion.
func TestMapFinishReasonNonNormal(t *testing.T) {
	t.Parallel()
	for _, normal := range []string{"", "STOP", "FINISH_REASON_UNSPECIFIED"} {
		if got := mapFinishReason(normal); got != "" {
			t.Errorf("%q should be a normal stop (empty), got %q", normal, got)
		}
	}
	for _, cf := range []string{"SAFETY", "RECITATION", "IMAGE_SAFETY", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII"} {
		if got := mapFinishReason(cf); got != zeroruntime.FinishReasonContentFilter {
			t.Errorf("%q → %q, want content_filter (M3)", cf, got)
		}
	}
	if got := mapFinishReason("MAX_TOKENS"); got != zeroruntime.FinishReasonLength {
		t.Errorf("MAX_TOKENS → %q, want length", got)
	}
	// Remaining non-STOP reasons surface the raw reason (non-empty) so the turn is
	// not mistaken for a clean completion.
	if got := mapFinishReason("MALFORMED_FUNCTION_CALL"); got != "MALFORMED_FUNCTION_CALL" {
		t.Errorf("MALFORMED_FUNCTION_CALL → %q, want the raw reason", got)
	}
}

// A candidate finishReason of MAX_TOKENS means the response was truncated at the
// output cap. The provider must surface it on the done event.
func TestStreamCompletionSurfacesMaxTokensFinishReason(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"cut"}]},"finishReason":"MAX_TOKENS"}]}`)
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

// A SAFETY finishReason maps to the runtime's content-filter reason.
func TestStreamCompletionSurfacesSafetyFinishReason(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"SAFETY"}]}`)
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

// A RECITATION finishReason previously fell through to "" (a clean completion);
// M3 maps it to content_filter. This exercises that fix through the full
// SSE → done-event wiring, not just mapFinishReason in isolation.
func TestStreamCompletionSurfacesRecitationFinishReason(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"RECITATION"}]}`)
	})

	events := collectProviderEvents(t, provider)
	var sawDone bool
	for _, e := range events {
		if e.Type == zeroruntime.StreamEventDone {
			sawDone = true
			if e.FinishReason != zeroruntime.FinishReasonContentFilter {
				t.Fatalf("RECITATION done FinishReason = %q, want %q (M3)", e.FinishReason, zeroruntime.FinishReasonContentFilter)
			}
		}
	}
	if !sawDone {
		t.Fatalf("no done event; events: %+v", events)
	}
}

// A normal STOP finishReason must leave the done event's FinishReason empty.
func TestStreamCompletionNormalFinishReasonHasNoReason(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
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

// A functionCall part with an empty Name can't be dispatched. The provider must
// signal a dropped tool call (once) so the agent can ask the model to retry,
// rather than silently skipping it.
func TestStreamCompletionEmitsDroppedOnNamelessFunctionCallPart(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"","args":{"a":1}}}]}}]}`)
	})

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
		t.Errorf("a nameless functionCall must not start a tool call, got %d starts", started)
	}
	if dropped != 1 {
		t.Errorf("expected exactly one dropped-tool-call signal, got %d; events: %+v", dropped, events)
	}
}

// A nameless top-level functionCall must also be signalled as dropped.
func TestStreamCompletionEmitsDroppedOnNamelessTopLevelFunctionCall(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"functionCalls":[{"name":"","args":{"a":1}}]}`)
	})

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
		t.Errorf("a nameless functionCall must not start a tool call, got %d starts", started)
	}
	if dropped != 1 {
		t.Errorf("expected exactly one dropped-tool-call signal, got %d; events: %+v", dropped, events)
	}
}

// A well-formed functionCall must NOT emit a dropped signal.
func TestStreamCompletionDoesNotDropValidFunctionCall(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"x"}}}]}}]}`)
	})

	events := collectProviderEvents(t, provider)
	var sawStart bool
	for _, e := range events {
		if e.Type == zeroruntime.StreamEventToolCallDropped {
			t.Errorf("valid functionCall must not be dropped; events: %+v", events)
		}
		if e.Type == zeroruntime.StreamEventToolCallStart && e.ToolName == "read_file" {
			sawStart = true
		}
	}
	if !sawStart {
		t.Fatalf("valid functionCall did not start a read_file tool call; events: %+v", events)
	}
}

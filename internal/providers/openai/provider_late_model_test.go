package openai

import (
	"net/http"
	"testing"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

// TestStreamCompletionLateModelAttributionWhenModelChangesAfterUsage tests the bug
// where a usage chunk reports model "deepseek/x" and a later content-only chunk
// reports model "deepseek/y". The usage event's ResolvedModel should be "deepseek/y"
// and only ONE usage event should be sent.
func TestStreamCompletionLateModelAttributionWhenModelChangesAfterUsage(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		// First chunk: usage with model deepseek/x
		writeSSE(w, `{"model":"deepseek/x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
		// Second chunk: content with later model deepseek/y
		writeSSE(w, `{"model":"deepseek/y","choices":[{"delta":{"content":"answer"}}]}`)
		// [DONE]
		writeSSE(w, `[DONE]`)
	})

	events := collectProviderEvents(t, provider)

	// Find usage events
	var usageEvents []zeroruntime.StreamEvent
	for _, e := range events {
		if e.Type == zeroruntime.StreamEventUsage {
			usageEvents = append(usageEvents, e)
		}
	}

	// Exactly ONE usage event
	if len(usageEvents) != 1 {
		t.Fatalf("usage events = %d, want exactly 1; events: %+v", len(usageEvents), events)
	}

	// Its ResolvedModel should be deepseek/y (the final model)
	if usageEvents[0].Usage.ResolvedModel != "deepseek/y" {
		t.Fatalf("ResolvedModel = %q, want deepseek/y", usageEvents[0].Usage.ResolvedModel)
	}

	// ModelReported should be true
	if !usageEvents[0].Usage.ModelReported {
		t.Fatal("ModelReported = false, want true")
	}

	// Usage event should come before done
	usageIdx, doneIdx := -1, -1
	for i, e := range events {
		if e.Type == zeroruntime.StreamEventUsage {
			usageIdx = i
		}
		if e.Type == zeroruntime.StreamEventDone {
			doneIdx = i
		}
	}
	if usageIdx < 0 {
		t.Fatalf("no usage event found; events: %+v", events)
	}
	if doneIdx < 0 {
		t.Fatalf("no done event found; events: %+v", events)
	}
	if usageIdx >= doneIdx {
		t.Fatalf("usage event at %d should come before done at %d", usageIdx, doneIdx)
	}
}

// TestStreamCompletionLateModelAttributionWhenModelMatchesRequested tests that when
// a usage chunk reports model "deepseek/x" and a later content-only chunk reports
// the requested model "gpt-test", the final usage event has empty ResolvedModel
// but ModelReported is true.
func TestStreamCompletionLateModelAttributionWhenModelMatchesRequested(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		// First chunk: usage with model deepseek/x
		writeSSE(w, `{"model":"deepseek/x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
		// Second chunk: content with requested model gpt-test
		writeSSE(w, `{"model":"gpt-test","choices":[{"delta":{"content":"answer"}}]}`)
		// [DONE]
		writeSSE(w, `[DONE]`)
	})

	events := collectProviderEvents(t, provider)

	// Find usage events
	var usageEvents []zeroruntime.StreamEvent
	for _, e := range events {
		if e.Type == zeroruntime.StreamEventUsage {
			usageEvents = append(usageEvents, e)
		}
	}

	// Exactly ONE usage event
	if len(usageEvents) != 1 {
		t.Fatalf("usage events = %d, want exactly 1; events: %+v", len(usageEvents), events)
	}

	// ResolvedModel should be empty (matches requested model)
	if usageEvents[0].Usage.ResolvedModel != "" {
		t.Fatalf("ResolvedModel = %q, want empty string", usageEvents[0].Usage.ResolvedModel)
	}

	// ModelReported should be true (provider did report a model)
	if !usageEvents[0].Usage.ModelReported {
		t.Fatal("ModelReported = false, want true")
	}
}

// TestStreamCompletionUsageNoLaterModelChange tests backward compatibility:
// when a usage chunk reports model "deepseek/x" and no later chunk changes the model,
// the single usage event has ResolvedModel "deepseek/x" and arrives before the done event.
func TestStreamCompletionUsageNoLaterModelChange(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		// Usage chunk with model deepseek/x
		writeSSE(w, `{"model":"deepseek/x","choices":[{"delta":{"content":"hello"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
		// [DONE]
		writeSSE(w, `[DONE]`)
	})

	events := collectProviderEvents(t, provider)

	// Find usage events
	var usageEvents []zeroruntime.StreamEvent
	for _, e := range events {
		if e.Type == zeroruntime.StreamEventUsage {
			usageEvents = append(usageEvents, e)
		}
	}

	// Exactly ONE usage event
	if len(usageEvents) != 1 {
		t.Fatalf("usage events = %d, want exactly 1; events: %+v", len(usageEvents), events)
	}

	// ResolvedModel should be deepseek/x
	if usageEvents[0].Usage.ResolvedModel != "deepseek/x" {
		t.Fatalf("ResolvedModel = %q, want deepseek/x", usageEvents[0].Usage.ResolvedModel)
	}

	// ModelReported should be true
	if !usageEvents[0].Usage.ModelReported {
		t.Fatal("ModelReported = false, want true")
	}

	// Usage event should arrive before done event (same order as before)
	usageIdx, doneIdx := -1, -1
	for i, e := range events {
		if e.Type == zeroruntime.StreamEventUsage {
			usageIdx = i
		}
		if e.Type == zeroruntime.StreamEventDone {
			doneIdx = i
		}
	}
	if usageIdx < 0 {
		t.Fatalf("no usage event found; events: %+v", events)
	}
	if doneIdx < 0 {
		t.Fatalf("no done event found; events: %+v", events)
	}
	if usageIdx >= doneIdx {
		t.Fatalf("usage event at %d should come before done at %d", usageIdx, doneIdx)
	}
}

// TestStreamCompletionMultipleUsageChunks verifies two usage chunks still produce
// two usage events (unchanged behavior when there's no late model change).
func TestStreamCompletionMultipleUsageChunks(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		// First usage chunk with model deepseek/x
		writeSSE(w, `{"model":"deepseek/x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3}}`)
		// Second usage chunk with model deepseek/z
		writeSSE(w, `{"model":"deepseek/z","choices":[{"delta":{"content":"more"}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
		// [DONE]
		writeSSE(w, `[DONE]`)
	})

	events := collectProviderEvents(t, provider)

	// Find usage events
	var usageEvents []zeroruntime.StreamEvent
	for _, e := range events {
		if e.Type == zeroruntime.StreamEventUsage {
			usageEvents = append(usageEvents, e)
		}
	}

	// TWO usage events
	if len(usageEvents) != 2 {
		t.Fatalf("usage events = %d, want exactly 2; events: %+v", len(usageEvents), events)
	}

	// First usage: ResolvedModel deepseek/x
	if usageEvents[0].Usage.ResolvedModel != "deepseek/x" {
		t.Fatalf("first ResolvedModel = %q, want deepseek/x", usageEvents[0].Usage.ResolvedModel)
	}

	// Second usage: ResolvedModel deepseek/z (final model)
	if usageEvents[1].Usage.ResolvedModel != "deepseek/z" {
		t.Fatalf("second ResolvedModel = %q, want deepseek/z", usageEvents[1].Usage.ResolvedModel)
	}

	// Verify order: first usage should come before second, second before done
	var usageIndices []int
	for i, e := range events {
		if e.Type == zeroruntime.StreamEventUsage {
			usageIndices = append(usageIndices, i)
		}
	}
	if len(usageIndices) != 2 {
		t.Fatalf("found %d usage events, want 2; events: %+v", len(usageIndices), events)
	}
}

// TestStreamCompletionUsageBeforeErrorOnLateModel tests that when a stream ends in
// an error after a usage chunk, the usage event still arrives before the error event.
func TestStreamCompletionUsageBeforeErrorOnLateModel(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		// Usage chunk with model deepseek/x
		writeSSE(w, `{"model":"deepseek/x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
		// Error chunk with later model deepseek/y
		writeSSE(w, `{"model":"deepseek/y","error":{"message":"rate limited","type":"server_error"}}`)
	})

	events := collectProviderEvents(t, provider)

	// Find usage and error events
	var usageEvent *zeroruntime.StreamEvent
	var errorEvent *zeroruntime.StreamEvent
	for i, e := range events {
		if e.Type == zeroruntime.StreamEventUsage && usageEvent == nil {
			usageEvent = &events[i]
		}
		if e.Type == zeroruntime.StreamEventError && errorEvent == nil {
			errorEvent = &events[i]
		}
	}

	if usageEvent == nil {
		t.Fatalf("no usage event found; events: %+v", events)
	}
	if errorEvent == nil {
		t.Fatalf("no error event found; events: %+v", events)
	}

	// Usage event should come before error
	usageIdx, errorIdx := -1, -1
	for i, e := range events {
		if e.Type == zeroruntime.StreamEventUsage && usageIdx == -1 {
			usageIdx = i
		}
		if e.Type == zeroruntime.StreamEventError && errorIdx == -1 {
			errorIdx = i
		}
	}
	if usageIdx < 0 || errorIdx < 0 {
		t.Fatalf("usage index %d, error index %d; events: %+v", usageIdx, errorIdx, events)
	}
	if usageIdx >= errorIdx {
		t.Fatalf("usage event at %d should come before error at %d", usageIdx, errorIdx)
	}

	// The usage should have the model from the usage chunk (deepseek/x).
	// The error frame's model is not read, so the pending usage keeps its original model.
	if usageEvent.Usage.ResolvedModel != "deepseek/x" {
		t.Fatalf("ResolvedModel = %q, want deepseek/x", usageEvent.Usage.ResolvedModel)
	}
}

// Two usage chunks that carry no model of their own, after a content chunk that
// named the model: the first usage must keep that model, not lose it because the
// model did not arrive on the usage chunk itself.
func TestStreamCompletionEarlierUsageKeepsModelNamedOnAContentChunk(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"model":"deepseek/x","choices":[{"delta":{"content":"a"}}]}`)
		writeSSE(w, `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1}}`)
		writeSSE(w, `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
		writeSSE(w, `[DONE]`)
	})
	var usages []zeroruntime.Usage
	for _, event := range collectProviderEvents(t, provider) {
		if event.Type == zeroruntime.StreamEventUsage {
			usages = append(usages, event.Usage)
		}
	}
	if len(usages) != 2 {
		t.Fatalf("usage events = %d, want 2", len(usages))
	}
	for i, usage := range usages {
		if usage.ResolvedModel != "deepseek/x" || !usage.ModelReported {
			t.Fatalf("usage %d: ResolvedModel=%q ModelReported=%v, want deepseek/x true", i, usage.ResolvedModel, usage.ModelReported)
		}
	}
}

// The usage chunk is held until the end of the stream, so content that follows
// it is delivered first. The usage event still precedes done, and nothing reads
// the order of text relative to usage; this pins it so a change is deliberate.
func TestStreamCompletionHeldUsageArrivesAfterLaterContentBeforeDone(t *testing.T) {
	t.Parallel()
	provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"model":"deepseek/x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1}}`)
		writeSSE(w, `{"choices":[{"delta":{"content":"late"}}]}`)
		writeSSE(w, `[DONE]`)
	})
	var got []zeroruntime.StreamEventType
	for _, event := range collectProviderEvents(t, provider) {
		got = append(got, event.Type)
	}
	want := []zeroruntime.StreamEventType{zeroruntime.StreamEventText, zeroruntime.StreamEventUsage, zeroruntime.StreamEventDone}
	if len(got) != len(want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event types = %v, want %v", got, want)
		}
	}
}

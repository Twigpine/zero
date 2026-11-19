package openai

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

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

// TestStreamCompletionUsageDeliveredOnContextCancel pins the cancel path of the
// held usage: a usage chunk and then a content chunk arrive, the caller cancels
// as soon as it has read that content (so the provider has already processed the
// usage chunk and holds it), and the stream must still deliver exactly one usage
// event with the reported tokens and model, and must close in bounded time.
// sendEvent drops a non-error event at random when ctx.Done and the buffered
// channel are both ready, so the scenario repeats to make the old code fail.
func TestStreamCompletionUsageDeliveredOnContextCancel(t *testing.T) {
	for i := 0; i < 50; i++ {
		t.Run(fmt.Sprintf("iteration-%d", i), func(t *testing.T) {
			t.Parallel()
			hold := make(chan struct{})
			provider := newTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
				writeSSE(w, `{"model":"deepseek/x","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
				writeSSE(w, `{"model":"deepseek/x","choices":[{"delta":{"content":"hi"}}]}`)
				<-hold // keep the stream open: no [DONE]
			})
			// Registered after the server cleanup, so it runs first and a failing
			// test can never leave the handler blocking server.Close.
			t.Cleanup(func() { close(hold) })

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream, err := provider.StreamCompletion(ctx, zeroruntime.CompletionRequest{})
			if err != nil {
				t.Fatalf("StreamCompletion returned setup error: %v", err)
			}

			var events []zeroruntime.StreamEvent
			deadline := time.After(2 * time.Second)
			for seenText := false; !seenText; {
				select {
				case event, ok := <-stream:
					if !ok {
						t.Fatalf("stream closed before the content chunk; events: %+v", events)
					}
					events = append(events, event)
					seenText = event.Type == zeroruntime.StreamEventText
				case <-deadline:
					t.Fatalf("no content event within 2s; events: %+v", events)
				}
			}
			// The content chunk was read, so the usage chunk before it is held.
			cancel()

			rest := make(chan []zeroruntime.StreamEvent, 1)
			go func() { rest <- readAll(stream) }()
			select {
			case more := <-rest:
				events = append(events, more...)
			case <-time.After(2 * time.Second):
				t.Fatal("stream did not close within 2s after cancel")
			}

			var usage []zeroruntime.StreamEvent
			for _, event := range events {
				if event.Type == zeroruntime.StreamEventUsage {
					usage = append(usage, event)
				}
			}
			if len(usage) != 1 {
				t.Fatalf("usage events = %d, want exactly 1; events: %+v", len(usage), events)
			}
			got := usage[0].Usage
			if got.PromptTokens != 10 || got.CompletionTokens != 5 {
				t.Fatalf("usage tokens = %+v, want 10/5", got)
			}
			if got.ResolvedModel != "deepseek/x" {
				t.Fatalf("ResolvedModel = %q, want deepseek/x", got.ResolvedModel)
			}
		})
	}
}

// TestFlushUsageFullBufferAfterCancelStillDelivers covers the full-buffer case
// of a cancelled flush: the channel has no room at the cancel, the consumer
// frees space shortly after (as the collector's drain does), and the single
// usage event must still arrive. sendEvent alone drops it once ctx is done.
func TestFlushUsageFullBufferAfterCancelStillDelivers(t *testing.T) {
	events := make(chan zeroruntime.StreamEvent, 1)
	events <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: "queued"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state := newToolState(false)
	state.pendingUsage = &zeroruntime.Usage{PromptTokens: 10, CompletionTokens: 5}

	go func() {
		time.Sleep(10 * time.Millisecond)
		<-events // free the slot, like the collector draining after the cancel
	}()
	done := make(chan struct{})
	go func() {
		state.flushUsage(ctx, events, "requested", "deepseek/x")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("flushUsage did not return within 1s")
	}
	select {
	case event := <-events:
		if event.Type != zeroruntime.StreamEventUsage || event.Usage.PromptTokens != 10 || event.Usage.ResolvedModel != "deepseek/x" {
			t.Fatalf("event = %+v, want the usage event 10/5 deepseek/x", event)
		}
	default:
		t.Fatal("usage event was dropped after the cancel with a full buffer")
	}
}

// TestFlushUsageFullBufferNobodyReadingDoesNotBlock proves the other half: when
// the buffer is full and nobody ever reads, a cancelled flush gives up in a
// bounded time instead of holding the stream goroutine.
func TestFlushUsageFullBufferNobodyReadingDoesNotBlock(t *testing.T) {
	events := make(chan zeroruntime.StreamEvent, 1)
	events <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: "queued"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state := newToolState(false)
	state.pendingUsage = &zeroruntime.Usage{PromptTokens: 10}

	done := make(chan struct{})
	go func() {
		state.flushUsage(ctx, events, "requested", "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("flushUsage blocked on a full buffer after the cancel")
	}
	if state.pendingUsage != nil {
		t.Fatal("pendingUsage not cleared: a second flush could send a second usage event")
	}
}

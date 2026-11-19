package zeroruntime

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestCollectStreamWithOptionsCancelDeliversUsage covers the cancel path of a
// provider that holds its last usage event until the stream ends: the caller
// cancels first, the producer then hands over exactly one usage event and
// sends its cancellation error and closes. The tokens must be collected and
// reported once, and the result must still say the call was cancelled (the
// collector may read the producer's error event or its own ctx.Done first).
func TestCollectStreamWithOptionsCancelDeliversUsage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan StreamEvent, 16)
	go func() {
		<-ctx.Done()
		events <- StreamEvent{Type: StreamEventUsage, Usage: Usage{PromptTokens: 10, CompletionTokens: 5, ResolvedModel: "deepseek/x"}}
		events <- StreamEvent{Type: StreamEventError, Error: "provider stream error: " + ctx.Err().Error()}
		close(events)
	}()

	var onUsage []Usage
	done := make(chan CollectedStream, 1)
	go func() {
		done <- CollectStreamWithOptions(ctx, events, CollectOptions{OnUsage: func(usage Usage) { onUsage = append(onUsage, usage) }})
	}()
	cancel()

	select {
	case collected := <-done:
		if !strings.Contains(collected.Error, context.Canceled.Error()) {
			t.Fatalf("collected.Error = %q, want it to mention %q", collected.Error, context.Canceled.Error())
		}
		if collected.Usage.PromptTokens != 10 || collected.Usage.CompletionTokens != 5 || collected.Usage.ResolvedModel != "deepseek/x" {
			t.Fatalf("collected.Usage = %+v, want 10/5 deepseek/x", collected.Usage)
		}
		if len(onUsage) != 1 {
			t.Fatalf("OnUsage calls = %d, want exactly 1", len(onUsage))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CollectStreamWithOptions did not return within 2s")
	}
}

// TestCollectStreamWithOptionsCancelNoBlockOnSilentProducer proves the drain
// is bounded: a producer that never sends and never closes cannot hold a
// cancelled caller.
func TestCollectStreamWithOptionsCancelNoBlockOnSilentProducer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan StreamEvent, 16)
	cancel()

	done := make(chan CollectedStream, 1)
	go func() { done <- CollectStreamWithOptions(ctx, events, CollectOptions{}) }()

	select {
	case collected := <-done:
		if collected.Error != context.Canceled.Error() {
			t.Fatalf("collected.Error = %q, want %q", collected.Error, context.Canceled.Error())
		}
	case <-time.After(time.Second):
		t.Fatal("CollectStreamWithOptions did not return within 1s on a silent producer after cancel")
	}
}

// TestCollectStreamWithOptionsCancelDrainIsUsageOnlyAndStopsAtTerminal keeps
// the drain narrow: after the cancel only usage is merged, text is ignored, and
// a terminal event ends the drain so anything behind it is not read.
func TestCollectStreamWithOptionsCancelDrainIsUsageOnlyAndStopsAtTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan StreamEvent, 16)
	cancel() // already cancelled, nothing queued: the collector goes straight to the drain
	go func() {
		time.Sleep(5 * time.Millisecond)
		events <- StreamEvent{Type: StreamEventText, Content: "late"}
		events <- StreamEvent{Type: StreamEventUsage, Usage: Usage{PromptTokens: 7}}
		events <- StreamEvent{Type: StreamEventDone}
		events <- StreamEvent{Type: StreamEventUsage, Usage: Usage{PromptTokens: 99}}
	}()

	collected := CollectStreamWithOptions(ctx, events, CollectOptions{})
	if collected.Text != "" {
		t.Fatalf("collected.Text = %q, want empty", collected.Text)
	}
	if collected.Usage.PromptTokens != 7 {
		t.Fatalf("collected.Usage.PromptTokens = %d, want 7 (usage before the terminal event only)", collected.Usage.PromptTokens)
	}
}

// TestCollectStreamWithOptionsCancelDrainEndsEvenWhenTheChannelStaysReady keeps
// the 50 ms bound honest: a producer that always has another non-terminal event
// ready must not stretch the drain past the grace period.
func TestCollectStreamWithOptionsCancelDrainEndsEvenWhenTheChannelStaysReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan StreamEvent, 16)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case events <- StreamEvent{Type: StreamEventText, Content: "x"}:
			case <-stop:
				return
			}
		}
	}()
	cancel()

	done := make(chan struct{})
	start := time.Now()
	go func() {
		CollectStreamWithOptions(ctx, events, CollectOptions{})
		close(done)
	}()
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("drain took %v, want about the 50ms grace", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drain never ended while the channel stayed ready")
	}
}

package tui

import (
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// recorder is a thread-safe sink that captures forwarded messages in order.
type recorder struct {
	mu   sync.Mutex
	msgs []tea.Msg
}

func (r *recorder) forward(msg tea.Msg) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, msg)
}

func (r *recorder) snapshot() []tea.Msg {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]tea.Msg, len(r.msgs))
	copy(out, r.msgs)
	return out
}

// manualTimer is a coalesceTimer that never fires on its own, so a test can prove
// buffering deterministically without racing the real 16ms frame timer: the flush
// happens only when the test calls c.flush() explicitly.
type manualTimer struct{ stopped bool }

func (m *manualTimer) Stop() bool {
	prev := m.stopped
	m.stopped = true
	return !prev
}

// Rapid deltas within one frame collapse into a single agentTextMsg carrying the
// concatenated text.
func TestCoalescerBatchesDeltas(t *testing.T) {
	rec := &recorder{}
	c := newTextCoalescer(rec.forward)
	// Swap in a timer that never auto-fires so the "still buffered" assertion below
	// cannot flake if the scheduler pauses this goroutine past the frame interval.
	c.afterFunc = func(func()) coalesceTimer { return &manualTimer{} }

	c.send(agentTextMsg{runID: 1, delta: "Hel"})
	c.send(agentTextMsg{runID: 1, delta: "lo, "})
	c.send(agentTextMsg{runID: 1, delta: "world"})

	// Nothing forwarded yet — still buffered within the frame (the timer we injected
	// never fires; only the explicit flush below delivers the text).
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("deltas should buffer, forwarded %d early: %#v", len(got), got)
	}

	c.flush()

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 coalesced message, got %d: %#v", len(got), got)
	}
	text, ok := got[0].(agentTextMsg)
	if !ok || text.delta != "Hello, world" || text.runID != 1 {
		t.Fatalf("coalesced message = %#v, want agentTextMsg{1, \"Hello, world\"}", got[0])
	}
}

// A non-text message flushes buffered text first, so ordering (text before the
// tool call it precedes) is preserved.
func TestCoalescerFlushesTextBeforeOtherMessages(t *testing.T) {
	rec := &recorder{}
	c := newTextCoalescer(rec.forward)

	c.send(agentTextMsg{runID: 1, delta: "about to run"})
	c.send(toolCallStreamStartMsg{runID: 1, id: "t1", name: "bash"})

	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected text then tool-call, got %d: %#v", len(got), got)
	}
	if text, ok := got[0].(agentTextMsg); !ok || text.delta != "about to run" {
		t.Fatalf("first forwarded message must be the flushed text, got %#v", got[0])
	}
	if _, ok := got[1].(toolCallStreamStartMsg); !ok {
		t.Fatalf("second forwarded message must be the tool-call start, got %#v", got[1])
	}
}

// A delta for a new run flushes the previous run's buffered text before buffering
// the new run's, so text is never mis-attributed across runs.
func TestCoalescerFlushesOnRunSwitch(t *testing.T) {
	rec := &recorder{}
	c := newTextCoalescer(rec.forward)

	c.send(agentTextMsg{runID: 1, delta: "run one text"})
	c.send(agentTextMsg{runID: 2, delta: "run two text"})
	c.flush()

	got := rec.snapshot()
	if len(got) != 2 {
		t.Fatalf("expected one message per run, got %d: %#v", len(got), got)
	}
	if first := got[0].(agentTextMsg); first.runID != 1 || first.delta != "run one text" {
		t.Fatalf("first message = %#v, want run 1 text flushed on switch", got[0])
	}
	if second := got[1].(agentTextMsg); second.runID != 2 || second.delta != "run two text" {
		t.Fatalf("second message = %#v, want run 2 text", got[1])
	}
}

// Under concurrency (the frame timer firing while a non-text message is sent),
// forwarding must stay serialized and ordered: buffered text is always delivered
// before a following non-text message, never after it. Run with -race to catch
// the interleaving CodeRabbit flagged.
func TestCoalescerOrdersTextBeforeNonTextUnderConcurrency(t *testing.T) {
	for iter := 0; iter < 200; iter++ {
		rec := &recorder{}
		c := newTextCoalescer(rec.forward)

		// Buffer text, then let the timer race against an inline non-text send.
		c.send(agentTextMsg{runID: 1, delta: "before-tool"})
		done := make(chan struct{})
		go func() {
			c.send(toolCallStreamStartMsg{runID: 1, id: "t1", name: "bash"})
			close(done)
		}()
		<-done

		// Give the timer a chance to also fire, then settle.
		time.Sleep(2 * streamCoalesceInterval)
		c.flush()

		got := rec.snapshot()
		textIdx, toolIdx := -1, -1
		for i, m := range got {
			switch m.(type) {
			case agentTextMsg:
				if textIdx == -1 {
					textIdx = i
				}
			case toolCallStreamStartMsg:
				toolIdx = i
			}
		}
		if textIdx == -1 || toolIdx == -1 {
			t.Fatalf("iter %d: missing messages: %#v", iter, got)
		}
		if textIdx > toolIdx {
			t.Fatalf("iter %d: text (%d) forwarded after tool-call (%d): %#v", iter, textIdx, toolIdx, got)
		}
	}
}

// The frame timer flushes buffered text on its own without an explicit flush or a
// following message.
func TestCoalescerTimerFlushes(t *testing.T) {
	rec := &recorder{}
	c := newTextCoalescer(rec.forward)

	c.send(agentTextMsg{runID: 1, delta: "timer-driven"})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(rec.snapshot()) > 0 {
			break
		}
		time.Sleep(streamCoalesceInterval)
	}

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("timer should have flushed exactly one message, got %d: %#v", len(got), got)
	}
	if text := got[0].(agentTextMsg); text.delta != "timer-driven" {
		t.Fatalf("timer-flushed message = %#v", got[0])
	}
}

// Rapid reasoning deltas within one frame collapse into a single
// agentReasoningMsg, exactly like text deltas. Without this, every reasoning
// token forces a full Update→View cycle that re-renders the whole growing
// thinking block, so the UI falls behind the stream and keeps "streaming"
// thoughts long after the provider finished.
func TestCoalescerBatchesReasoningDeltas(t *testing.T) {
	rec := &recorder{}
	c := newTextCoalescer(rec.forward)
	c.afterFunc = func(func()) coalesceTimer { return &manualTimer{} }

	c.send(agentReasoningMsg{runID: 1, delta: "think"})
	c.send(agentReasoningMsg{runID: 1, delta: "ing…"})

	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("reasoning deltas should buffer, forwarded %d early: %#v", len(got), got)
	}

	c.flush()

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 coalesced message, got %d: %#v", len(got), got)
	}
	reasoning, ok := got[0].(agentReasoningMsg)
	if !ok || reasoning.delta != "thinking…" || reasoning.runID != 1 {
		t.Fatalf("coalesced message = %#v, want agentReasoningMsg{1, \"thinking…\"}", got[0])
	}
}

// A text delta following buffered reasoning flushes the reasoning first, and
// vice versa, so interleaved thinking/prose never reorders.
func TestCoalescerPreservesReasoningTextOrder(t *testing.T) {
	rec := &recorder{}
	c := newTextCoalescer(rec.forward)
	c.afterFunc = func(func()) coalesceTimer { return &manualTimer{} }

	c.send(agentReasoningMsg{runID: 1, delta: "thought"})
	c.send(agentTextMsg{runID: 1, delta: "answer"})
	c.send(agentReasoningMsg{runID: 1, delta: "more thought"})
	c.flush()

	got := rec.snapshot()
	if len(got) != 3 {
		t.Fatalf("expected reasoning, text, reasoning in order, got %d: %#v", len(got), got)
	}
	if r, ok := got[0].(agentReasoningMsg); !ok || r.delta != "thought" {
		t.Fatalf("first message must be the flushed reasoning, got %#v", got[0])
	}
	if tx, ok := got[1].(agentTextMsg); !ok || tx.delta != "answer" {
		t.Fatalf("second message must be the flushed text, got %#v", got[1])
	}
	if r, ok := got[2].(agentReasoningMsg); !ok || r.delta != "more thought" {
		t.Fatalf("third message must be the final reasoning, got %#v", got[2])
	}
}

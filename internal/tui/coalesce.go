package tui

import (
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

// streamCoalesceInterval is roughly one 60fps frame. Assistant-text and
// reasoning deltas that arrive within this window are merged into a single
// message, so the render rate decouples from the token rate: a fast local
// model (100+ tok/s) no longer forces 100+ full Update→View cycles (each
// re-rendering the growing markdown or thinking block) per second. Reasoning
// matters as much as text here: interimBlock re-renders the ENTIRE accumulated
// thinking string every frame, so uncoalesced reasoning deltas are O(n) work
// per token (O(n²) per segment) and the message queue backs up faster than it
// drains — the UI keeps replaying "thoughts" long after the provider stopped
// thinking. Rendering stays smooth regardless of provider speed.
const streamCoalesceInterval = 16 * time.Millisecond

// textCoalescer batches agentTextMsg and agentReasoningMsg deltas before
// forwarding them to the Bubble Tea program. Any OTHER message flushes the
// pending stream first, so ordering between streamed prose/reasoning and
// tool-call / row / usage messages is preserved. A switch between text and
// reasoning (or between runs) also flushes the buffered kind first, so
// interleaved thinking and prose never reorder. The turn's final
// agentResponseMsg does not pass through here (it is a tea.Cmd return, not a
// sink message), but the model drops deltas whose runID is no longer active,
// so a flush that races just past end-of-turn is harmless.
//
// Sink messages originate from the single agent goroutine and so arrive
// serially; the only concurrent caller is the flush timer. The mutex guards the
// buffer/timer AND is held across the downstream forward, so a timer-fired text
// flush can never overtake a concurrent non-text message: whoever holds the lock
// drains and forwards atomically, and the other caller blocks until it is done.
type textCoalescer struct {
	forward func(tea.Msg) // downstream sink (external sink + program.Send)
	// afterFunc schedules fn to run after one frame interval and returns a
	// stoppable timer. Defaults to a real time.AfterFunc(streamCoalesceInterval, …);
	// tests swap in a controllable timer so flush timing is deterministic instead of
	// racing the 16ms wall clock.
	afterFunc func(fn func()) coalesceTimer

	mu    sync.Mutex
	buf   []byte
	runID int
	// reasoning records whether the buffered deltas are reasoning (true) or
	// prose (false). Only one kind is ever buffered at a time: a kind switch
	// flushes first, which preserves interleaved order.
	reasoning bool
	timer     coalesceTimer
}

// coalesceTimer is the subset of *time.Timer the coalescer needs. Abstracted
// behind afterFunc so a test can substitute a timer it controls.
type coalesceTimer interface {
	Stop() bool
}

func newTextCoalescer(forward func(tea.Msg)) *textCoalescer {
	return &textCoalescer{
		forward: forward,
		afterFunc: func(fn func()) coalesceTimer {
			return time.AfterFunc(streamCoalesceInterval, fn)
		},
	}
}

// send is the coalescing entry point installed as the RuntimeMessageSink.
func (c *textCoalescer) send(msg tea.Msg) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var (
		delta     string
		runID     int
		reasoning bool
		streaming bool
	)
	switch m := msg.(type) {
	case agentTextMsg:
		delta, runID, streaming = m.delta, m.runID, true
	case agentReasoningMsg:
		delta, runID, reasoning, streaming = m.delta, m.runID, true, true
	}
	if !streaming {
		// Non-stream message: flush buffered stream first (preserving order), then
		// forward it — both under the lock so nothing can interleave between them.
		c.drainAndForwardLocked()
		c.forward(msg)
		return
	}

	// A kind or run switch: flush the buffered kind before buffering the new
	// one, so interleaved reasoning/prose is delivered in arrival order. In
	// practice runs are sequential (the prior run's end already flushed via a
	// non-stream message), so the run switch is belt-and-braces.
	if len(c.buf) > 0 && (runID != c.runID || reasoning != c.reasoning) {
		c.drainAndForwardLocked()
	}
	c.runID = runID
	c.reasoning = reasoning
	c.buf = append(c.buf, delta...)
	if c.timer == nil {
		c.timer = c.afterFunc(c.flush)
	}
}

// flush forwards any buffered stream deltas as one coalesced message. Runs on
// the timer goroutine; the lock it takes serializes it against send so its
// output can't be reordered around a concurrent non-stream message.
func (c *textCoalescer) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.drainAndForwardLocked()
}

// drainAndForwardLocked forwards any buffered deltas as one coalesced
// agentTextMsg or agentReasoningMsg (whichever kind was buffered) and stops the
// timer, all while the caller holds c.mu — so a flush and any concurrent
// forward are strictly ordered and never interleave. A no-op when nothing is
// buffered. string(c.buf) copies, so reusing the backing array via [:0] is safe.
func (c *textCoalescer) drainAndForwardLocked() {
	if len(c.buf) == 0 {
		return
	}
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	text := string(c.buf)
	c.buf = c.buf[:0]
	if c.reasoning {
		c.forward(agentReasoningMsg{runID: c.runID, delta: text})
		return
	}
	c.forward(agentTextMsg{runID: c.runID, delta: text})
}

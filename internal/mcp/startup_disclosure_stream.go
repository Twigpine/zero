package mcp

import "sync"

// StartupDisclosureStream carries launch disclosures out of the runtime as
// TYPED EVENTS, for whichever component owns the output to drain on its own
// goroutine.
//
// It replaces handing the runtime a presentation callback. That callback closed
// over the CLI's writer, and the runtime invoked it synchronously from whichever
// goroutine happened to resolve the launch. For a server still inside cmd.Start
// when registration gave up, that goroutine is the abandoned connect attempt,
// which runs on no schedule the caller controls: the write landed off the output
// owner's goroutine, raced any other writer, and could arrive after the owner had
// returned or after Bubble Tea had taken the alt screen. The runtime owns the
// FACT that a process started; it does not own anyone's writer.
//
// So the runtime only ever appends a value here. Delivery, ordering against other
// startup output, and the decision to stop listening all belong to the owner.
//
// LIFETIME IS EXPLICIT. Close is the owner saying "I will not write again".
// Offers after Close are dropped rather than queued for a consumer that no longer
// exists, and Wait returns false so a pump exits. Both are deliberate: a
// disclosure is worth printing while someone can print it, and worth dropping
// rather than corrupting a screen that now belongs to something else. Close is
// idempotent and safe from any goroutine, so the runtime and the owner may both
// call it.
type StartupDisclosureStream struct {
	mu     sync.Mutex
	queue  []StartupDisclosure
	closed bool
	wake   chan struct{}
}

func newStartupDisclosureStream() *StartupDisclosureStream {
	return &StartupDisclosureStream{wake: make(chan struct{}, 1)}
}

// offer queues one disclosure. Called from the registration goroutine for a
// launch already known at commit, and from an abandoned connect goroutine for one
// that resolves later. It takes a lock and appends; it never touches a writer,
// which is the whole point of the type.
func (stream *StartupDisclosureStream) offer(disclosure StartupDisclosure) {
	if stream == nil || len(disclosure.Notices) == 0 {
		return
	}
	stream.mu.Lock()
	if stream.closed {
		stream.mu.Unlock()
		return
	}
	stream.queue = append(stream.queue, disclosure)
	stream.mu.Unlock()
	select {
	case stream.wake <- struct{}{}:
	default:
	}
}

// Drain removes and returns everything queued right now, without blocking. The
// owner calls this on the goroutine that owns the writer, so every disclosure is
// printed by exactly one goroutine at a time.
func (stream *StartupDisclosureStream) Drain() []StartupDisclosure {
	if stream == nil {
		return nil
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if len(stream.queue) == 0 {
		return nil
	}
	queued := stream.queue
	stream.queue = nil
	return queued
}

// Wait blocks until at least one disclosure is queued or the stream is closed. It
// reports whether draining is still worthwhile: false means closed and empty, so
// a pump loop should return.
func (stream *StartupDisclosureStream) Wait() bool {
	if stream == nil {
		return false
	}
	for {
		stream.mu.Lock()
		queued := len(stream.queue) > 0
		closed := stream.closed
		stream.mu.Unlock()
		if queued {
			return true
		}
		if closed {
			return false
		}
		<-stream.wake
	}
}

// Close ends delivery. Idempotent, safe from any goroutine, and safe to call
// from both the runtime and the output owner.
func (stream *StartupDisclosureStream) Close() {
	if stream == nil {
		return
	}
	stream.mu.Lock()
	if stream.closed {
		stream.mu.Unlock()
		return
	}
	stream.closed = true
	stream.mu.Unlock()
	select {
	case stream.wake <- struct{}{}:
	default:
	}
}

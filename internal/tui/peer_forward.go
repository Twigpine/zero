package tui

import (
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/peermsg"
)

// PEER CALLBACKS CAN FIRE ON THE EVENT LOOP'S OWN GOROUTINE.
//
// Service.UpdateIdentity re-evaluates the held inbox under the parity policy and
// invokes the held-release handler synchronously for every message that just
// became deliverable. The TUI calls UpdateIdentity from inside Update, on the
// shift+tab and ctrl+g paths that change the permission class, so that handler
// runs on the goroutine Bubble Tea is using to run Update. program.Send waits
// on the program's unbuffered message channel, and the only goroutine that
// reads it is the one waiting for Update to return. Confirming full-auto while
// a bypass-mode peer's message was held therefore froze the shell.
//
// So peer callbacks do not call program.Send themselves. They append to a FIFO
// and return; one goroutine drains the FIFO into program.Send in order. Order
// among peer events is preserved because there is exactly one drainer, the
// caller never waits on the loop, and after the program has exited Send returns
// immediately on its cancelled context, so a late callback cannot wedge either.
// Reported by @jatmn.
type peerForwarder struct {
	deliver func(tea.Msg)
	mu      sync.Mutex
	queue   []tea.Msg
	wake    chan struct{}
	done    chan struct{}
	drained chan struct{}
}

func newPeerForwarder(deliver func(tea.Msg)) *peerForwarder {
	forwarder := &peerForwarder{
		deliver: deliver,
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		drained: make(chan struct{}),
	}
	go forwarder.run()
	return forwarder
}

// send queues msg and returns without waiting for the loop.
func (forwarder *peerForwarder) send(msg tea.Msg) {
	forwarder.mu.Lock()
	forwarder.queue = append(forwarder.queue, msg)
	forwarder.mu.Unlock()
	select {
	case forwarder.wake <- struct{}{}:
	default:
	}
}

func (forwarder *peerForwarder) run() {
	defer close(forwarder.drained)
	for {
		select {
		case <-forwarder.done:
			return
		case <-forwarder.wake:
		}
		for {
			forwarder.mu.Lock()
			if len(forwarder.queue) == 0 {
				forwarder.mu.Unlock()
				break
			}
			msg := forwarder.queue[0]
			forwarder.queue = forwarder.queue[1:]
			forwarder.mu.Unlock()
			forwarder.deliver(msg)
		}
	}
}

// stop ends the drainer once the program is gone; anything still queued has
// no loop left to reach. Safe to call more than once.
func (forwarder *peerForwarder) stop() {
	select {
	case <-forwarder.done:
	default:
		close(forwarder.done)
	}
	<-forwarder.drained
}

// peerAdmitTimeout bounds how long an inbound peer message waits for the shell
// to admit it before the service is told no.
const peerAdmitTimeout = 4 * time.Second

// peerAdmitGate is the single admission decision for one inbound message.
// The service timeout and the shell's Update share it, so a refusal and a
// later delivery of the same queued message cannot both succeed.
type peerAdmitGate struct {
	mu   sync.Mutex
	done bool
	ch   chan bool
}

func newPeerAdmitGate() *peerAdmitGate {
	return &peerAdmitGate{ch: make(chan bool, 1)}
}

// claim records the shell's decision. It returns false when the timeout
// already refused the message; the caller must not deliver it.
func (gate *peerAdmitGate) claim(accepted bool) bool {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.done {
		return false
	}
	gate.done = true
	gate.ch <- accepted
	return true
}

// refuse records the timeout. It returns false when the shell already
// decided, in which case the caller must use the value on ch.
func (gate *peerAdmitGate) refuse() bool {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.done {
		return false
	}
	gate.done = true
	return true
}

// peerServiceWiring is what the shell needs from the peer service to hook it up.
// An interface rather than *peermsg.Service so the event-loop regression can
// stand in a fake that hands back the handlers it was given.
type peerServiceWiring interface {
	SetStatusHandler(peermsg.StatusHandler)
	SetHeldEvictionHandler(peermsg.HeldEvictionHandler)
	SetHeldReleaseHandler(peermsg.HeldReleaseHandler)
	Start(peermsg.Handler) error
}

// wirePeerService installs the shell's peer handlers, every one delivering
// through forward, and starts the service. It reports whether the service
// started, so the caller knows whether it owns a Close.
func wirePeerService(service peerServiceWiring, forward func(tea.Msg)) bool {
	service.SetStatusHandler(func(event peermsg.StatusEvent) {
		forward(peerStatusMsg{event: event})
	})
	service.SetHeldEvictionHandler(func(messageID string) {
		forward(peerApprovalExpiredMsg{messageID: messageID})
	})
	service.SetHeldReleaseHandler(func(message peermsg.InboundMessage) {
		forward(peerHeldReleasedMsg{message: message})
	})
	if err := service.Start(func(message peermsg.InboundMessage) bool {
		// One gate for the timeout and the shell. Returning false on the
		// timer used to leave the queued message free to admit itself later,
		// so a refused delivery still ran when the loop caught up.
		admit := newPeerAdmitGate()
		forward(peerMessageMsg{message: message, admit: admit})
		timer := time.NewTimer(peerAdmitTimeout)
		defer timer.Stop()
		select {
		case accepted := <-admit.ch:
			return accepted
		case <-timer.C:
			if admit.refuse() {
				return false
			}
			return <-admit.ch
		}
	}); err != nil {
		forward(peerRuntimeErrorMsg{err: err})
		return false
	}
	return true
}

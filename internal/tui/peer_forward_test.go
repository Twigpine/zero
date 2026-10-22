package tui

import (
	"context"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/peermsg"
)

// fakePeerService hands back the handlers the shell installs on it.
type fakePeerService struct {
	status   peermsg.StatusHandler
	eviction peermsg.HeldEvictionHandler
	release  peermsg.HeldReleaseHandler
	inbound  peermsg.Handler
	startErr error
}

func (s *fakePeerService) SetStatusHandler(handler peermsg.StatusHandler) { s.status = handler }
func (s *fakePeerService) SetHeldEvictionHandler(handler peermsg.HeldEvictionHandler) {
	s.eviction = handler
}
func (s *fakePeerService) SetHeldReleaseHandler(handler peermsg.HeldReleaseHandler) {
	s.release = handler
}
func (s *fakePeerService) Start(handler peermsg.Handler) error {
	s.inbound = handler
	return s.startErr
}

// releaseLoopModel is the production shape of the hazard reduced to its
// mechanism: an Update that, like the ctrl+g confirm through
// syncPeerIdentity and Service.UpdateIdentity, invokes the held-release
// handler SYNCHRONOUSLY on the event loop's goroutine.
type releaseLoopModel struct {
	release  peermsg.HeldReleaseHandler
	released chan peermsg.InboundMessage
	probed   chan struct{}
}

type triggerReleaseMsg struct{ message peermsg.InboundMessage }
type probeMsg struct{}

func (m releaseLoopModel) Init() tea.Cmd { return nil }

func (m releaseLoopModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case triggerReleaseMsg:
		m.release(msg.message)
	case peerHeldReleasedMsg:
		m.released <- msg.message
	case probeMsg:
		close(m.probed)
	}
	return m, nil
}

func (m releaseLoopModel) View() tea.View { return tea.NewView("") }

// CONFIRMING FULL-AUTO MUST NOT FREEZE THE SHELL ON THE MESSAGE IT RELEASES.
//
// A real Bubble Tea program, the real wiring, and a release invoked from inside
// Update the way UpdateIdentity invokes it. With the release handler calling
// program.Send directly this never returns: Send waits on the message channel
// that only the loop reads, and the loop is inside this Update. Through the
// forwarder the callback returns, the loop goes on to process the next message,
// and the released message reaches the peerHeldReleasedMsg path exactly once.
func TestFullAutoConfirmationReleasesAHeldPeerMessageWithoutBlockingTheLoop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	service := &fakePeerService{}
	released := make(chan peermsg.InboundMessage, 4)
	probed := make(chan struct{})
	var program *tea.Program
	forward := func(msg tea.Msg) {
		if program != nil {
			program.Send(msg)
		}
	}
	deliver := newPeerForwarder(forward)
	defer deliver.stop()
	if !wirePeerService(service, deliver.send) {
		t.Fatal("wiring reported the service did not start")
	}
	if service.release == nil {
		t.Fatal("SETUP INVALID: no held-release handler was installed")
	}

	program = tea.NewProgram(
		releaseLoopModel{release: service.release, released: released, probed: probed},
		tea.WithContext(ctx),
		tea.WithInput(nil),
		tea.WithoutRenderer(),
	)
	finished := make(chan error, 1)
	go func() {
		_, err := program.Run()
		finished <- err
	}()

	held := peermsg.InboundMessage{ID: "held-1", Body: "released once the classes match"}
	program.Send(triggerReleaseMsg{message: held})
	program.Send(probeMsg{})

	select {
	case <-probed:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop never processed the message after the release: Update blocked delivering the released message to itself")
	}
	select {
	case got := <-released:
		if got.ID != held.ID {
			t.Fatalf("released message %q, want %q", got.ID, held.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the released message never reached the shell")
	}
	select {
	case extra := <-released:
		t.Fatalf("the released message was delivered again: %q", extra.ID)
	case <-time.After(200 * time.Millisecond):
	}

	program.Quit()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("program exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("program did not exit")
	}
	// After exit a late callback must not wedge the caller either.
	done := make(chan struct{})
	go func() {
		service.release(peermsg.InboundMessage{ID: "late"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a release after the program exited blocked its caller")
	}
}

// The forwarder keeps peer events in order and never makes the caller wait.
func TestPeerForwarderDeliversInOrderWithoutBlockingTheCaller(t *testing.T) {
	var mu sync.Mutex
	var got []int
	gate := make(chan struct{})
	first := true
	deliver := func(msg tea.Msg) {
		if first {
			first = false
			<-gate // the loop is busy for the first message
		}
		mu.Lock()
		got = append(got, msg.(int))
		mu.Unlock()
	}
	forwarder := newPeerForwarder(deliver)
	defer forwarder.stop()

	returned := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			forwarder.send(i)
		}
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("send blocked while delivery was stalled")
	}
	close(gate)
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 50 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("delivered %d of 50", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, v := range got {
		if v != i {
			t.Fatalf("delivery order broken at %d: %v", i, got)
		}
	}
}

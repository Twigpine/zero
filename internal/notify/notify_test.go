package notify

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestShouldEmit(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		event   Event
		focused bool
		want    bool
	}{
		{"off never", Config{Mode: ModeOff, FocusMode: FocusAlways}, Completion, false, false},
		{"empty mode never", Config{}, Completion, false, false},
		{"always when focused", Config{Mode: ModeBell, FocusMode: FocusAlways}, Completion, true, true},
		{"always when unfocused", Config{Mode: ModeBell, FocusMode: FocusAlways}, Completion, false, true},
		{"unfocused emits when unfocused", Config{Mode: ModeBell, FocusMode: FocusUnfocused}, Completion, false, true},
		{"unfocused silent when focused", Config{Mode: ModeBell, FocusMode: FocusUnfocused}, Completion, true, false},
		{"empty focusmode == unfocused", Config{Mode: ModeBell}, Completion, true, false},
		{"focused emits when focused", Config{Mode: ModeBell, FocusMode: FocusFocused}, AwaitingInput, true, true},
		{"focused silent when unfocused", Config{Mode: ModeBell, FocusMode: FocusFocused}, AwaitingInput, false, false},
		{"awaiting-input also eligible", Config{Mode: ModeBoth, FocusMode: FocusAlways}, AwaitingInput, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldEmit(tc.cfg, tc.event, tc.focused); got != tc.want {
				t.Fatalf("shouldEmit=%v want %v", got, tc.want)
			}
		})
	}
}

func TestSequence(t *testing.T) {
	if got := sequence(ModeBell, "hi"); got != "\x07" {
		t.Fatalf("bell seq=%q", got)
	}
	if got := sequence(ModeNotify, "hi"); got != "\x1b]9;hi\x07" {
		t.Fatalf("notify seq=%q", got)
	}
	if got := sequence(ModeBoth, "hi"); got != "\x07\x1b]9;hi\x07" {
		t.Fatalf("both seq=%q", got)
	}
	if got := sequence(ModeOff, "hi"); got != "" {
		t.Fatalf("off seq=%q", got)
	}
}

// Configure swaps the policy at runtime: after turning a silent notifier on,
// the next Notify emits; after turning a noisy one off, it stays silent. Sinks
// survive the swap (the TUI relies on this when /notify reconfigures mid-run).
func TestConfigureAppliesImmediatelyAndKeepsSinks(t *testing.T) {
	var buf bytes.Buffer
	n := New(&buf, Config{Mode: ModeOff})
	n.SetFocused(true)

	n.Notify(Completion, "x")
	if buf.Len() != 0 {
		t.Fatalf("off should be silent, got %q", buf.String())
	}

	sink := &recordingSink{}
	n.AddSink(sink)
	n.Configure(Config{Mode: ModeBell, FocusMode: FocusAlways})
	n.Notify(Completion, "x")
	if buf.String() != "\x07" {
		t.Fatalf("after Configure(bell) should bell, got %q", buf.String())
	}
	sink.mu.Lock()
	got := len(sink.events)
	sink.mu.Unlock()
	if got != 1 {
		t.Fatalf("sink should still receive events after Configure, got %d", got)
	}

	n.Configure(Config{Mode: ModeOff})
	buf.Reset()
	n.Notify(Completion, "x")
	sink.mu.Lock()
	got = len(sink.events)
	sink.mu.Unlock()
	if buf.Len() != 0 || got != 1 {
		t.Fatalf("after Configure(off) should be fully silent, buf=%q events=%d", buf.String(), got)
	}
}

func TestSanitizeMessage(t *testing.T) {
	if got := sanitizeMessage("ok\x1b]0;evil\x07more\nx"); got != "ok]0;evilmorex" {
		t.Fatalf("sanitize=%q", got)
	}
	long := strings.Repeat("a", 500)
	if got := sanitizeMessage(long); len([]rune(got)) != maxMessageLen {
		t.Fatalf("clamp len=%d want %d", len([]rune(got)), maxMessageLen)
	}
}

func TestNotifyWritesAndRespectsPolicy(t *testing.T) {
	var buf bytes.Buffer
	n := New(&buf, Config{Mode: ModeBoth, FocusMode: FocusAlways})
	n.Notify(Completion, "Zero: ready")
	if buf.String() != "\x07\x1b]9;Zero: ready\x07" {
		t.Fatalf("emitted=%q", buf.String())
	}

	buf.Reset()
	off := New(&buf, Config{Mode: ModeOff})
	off.Notify(Completion, "x")
	if buf.Len() != 0 {
		t.Fatalf("off should emit nothing, got %q", buf.String())
	}

	buf.Reset()
	uf := New(&buf, Config{Mode: ModeBell, FocusMode: FocusUnfocused})
	uf.SetFocused(true)
	uf.Notify(Completion, "x")
	if buf.Len() != 0 {
		t.Fatalf("focused+unfocused-mode should be silent, got %q", buf.String())
	}
	uf.SetFocused(false)
	uf.Notify(Completion, "x")
	if buf.String() != "\x07" {
		t.Fatalf("unfocused should bell, got %q", buf.String())
	}
}

func TestNotifierDefaultFocusFalse(t *testing.T) {
	var buf bytes.Buffer
	n := New(&buf, Config{Mode: ModeBell, FocusMode: FocusUnfocused})
	n.Notify(Completion, "x")
	if buf.String() != "\x07" {
		t.Fatalf("default-focus headless should bell, got %q", buf.String())
	}
}

func TestNotifyRaceSafe(t *testing.T) {
	n := New(&bytes.Buffer{}, Config{Mode: ModeBell, FocusMode: FocusAlways})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); n.SetFocused(true) }()
		go func() { defer wg.Done(); n.Notify(Completion, "x") }()
	}
	wg.Wait()
}

// Configure mutates cfg under the lock while Notify reads it; this pair must
// be race-clean (run under -race). Regression for the unsynchronized cfg read
// Notify used to perform before acquiring the lock.
func TestConfigureConcurrentWithNotify(t *testing.T) {
	n := New(&bytes.Buffer{}, Config{Mode: ModeBell, FocusMode: FocusAlways})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); n.Configure(Config{Mode: ModeBoth, FocusMode: FocusAlways}) }()
		go func() { defer wg.Done(); n.Configure(Config{Mode: ModeOff}) }()
		go func() { defer wg.Done(); n.Notify(AwaitingInput, "x") }()
	}
	wg.Wait()
}

func TestDefaultMessage(t *testing.T) {
	if DefaultMessage(Completion) != "Zero: ready" {
		t.Fatal("completion message")
	}
	if DefaultMessage(AwaitingInput) != "Zero: needs input" {
		t.Fatal("awaiting message")
	}
}

func TestEnabled(t *testing.T) {
	if Enabled(ModeOff) || Enabled("") {
		t.Fatal("off/empty should be disabled")
	}
	if !Enabled(ModeBell) || !Enabled(ModeNotify) || !Enabled(ModeBoth) {
		t.Fatal("bell/notify/both should be enabled")
	}
}

// recordingSink captures every event/message it receives. It is the test double
// used to assert fan-out from the Notifier to its attached sinks.
type recordingSink struct {
	mu     sync.Mutex
	events []Event
	last   string
}

func (s *recordingSink) Emit(event Event, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	s.last = message
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

func TestNotifierFanOutHitsTerminalAndSink(t *testing.T) {
	var buf bytes.Buffer
	sink := &recordingSink{}
	n := New(&buf, Config{Mode: ModeBell, FocusMode: FocusAlways})
	n.AddSink(sink)

	n.Notify(Completion, "Zero: ready")

	if buf.String() != "\x07" {
		t.Fatalf("terminal bell not emitted, got %q", buf.String())
	}
	if sink.count() != 1 {
		t.Fatalf("sink received %d events, want 1", sink.count())
	}
	if sink.last != "Zero: ready" {
		t.Fatalf("sink message = %q", sink.last)
	}
}

func TestNotifierSinkFiresEvenWithoutTerminalWriter(t *testing.T) {
	// A headless caller may have no terminal writer but still want a webhook to
	// fire. A nil writer must not suppress sink delivery.
	sink := &recordingSink{}
	n := New(nil, Config{Mode: ModeNotify, FocusMode: FocusAlways})
	n.AddSink(sink)

	n.Notify(Completion, "hi")

	if sink.count() != 1 {
		t.Fatalf("sink received %d events, want 1", sink.count())
	}
}

func TestNotifierOffSuppressesSinks(t *testing.T) {
	sink := &recordingSink{}
	n := New(&bytes.Buffer{}, Config{Mode: ModeOff})
	n.AddSink(sink)
	n.Notify(Completion, "hi")
	if sink.count() != 0 {
		t.Fatalf("off mode must not fan out to sinks, got %d", sink.count())
	}
}

func TestNotifierSinkRespectsFocusPolicy(t *testing.T) {
	sink := &recordingSink{}
	n := New(&bytes.Buffer{}, Config{Mode: ModeBell, FocusMode: FocusUnfocused})
	n.AddSink(sink)
	n.SetFocused(true)
	n.Notify(Completion, "hi")
	if sink.count() != 0 {
		t.Fatalf("focused + unfocused-policy must skip sink, got %d", sink.count())
	}
	n.SetFocused(false)
	n.Notify(Completion, "hi")
	if sink.count() != 1 {
		t.Fatalf("unfocused must deliver to sink, got %d", sink.count())
	}
}

// panickingSink models a misbehaving sink. A sink fault must never crash the run.
type panickingSink struct{}

func (panickingSink) Emit(Event, string) { panic("sink boom") }

func TestNotifierSinkFailSoft(t *testing.T) {
	var buf bytes.Buffer
	n := New(&buf, Config{Mode: ModeBell, FocusMode: FocusAlways})
	n.AddSink(panickingSink{})
	good := &recordingSink{}
	n.AddSink(good)

	// Must not propagate the panic, and a sibling sink must still receive the event.
	n.Notify(Completion, "hi")

	if buf.String() != "\x07" {
		t.Fatalf("terminal output suppressed by sink panic, got %q", buf.String())
	}
	if good.count() != 1 {
		t.Fatalf("sibling sink starved by panicking sink, got %d", good.count())
	}
}

func TestNotifierFanOutRaceSafe(t *testing.T) {
	n := New(&bytes.Buffer{}, Config{Mode: ModeBell, FocusMode: FocusAlways})
	n.AddSink(&recordingSink{})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); n.SetFocused(true) }()
		go func() { defer wg.Done(); n.AddSink(&recordingSink{}) }()
		go func() { defer wg.Done(); n.Notify(Completion, "x") }()
	}
	wg.Wait()
}

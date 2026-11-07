// Package notify emits dep-free terminal notifications (BEL and/or OSC-9
// desktop notifications) when Zero finishes a turn or needs user input.
package notify

import (
	"io"
	"strings"
	"sync"
)

// Mode selects the notification mechanism.
type Mode string

const (
	ModeOff    Mode = "off"
	ModeBell   Mode = "bell"
	ModeNotify Mode = "notify" // OSC-9 desktop notification
	ModeBoth   Mode = "both"
)

// FocusMode gates emission on terminal focus (a TUI concept).
type FocusMode string

const (
	FocusUnfocused FocusMode = "unfocused" // default: only when terminal is NOT focused
	FocusAlways    FocusMode = "always"
	FocusFocused   FocusMode = "focused"
)

// Event is the moment that triggered a notification.
type Event int

const (
	Completion Event = iota
	AwaitingInput
)

// Config is the resolved notifier policy. Zero value (Mode=="") is silent.
type Config struct {
	Mode      Mode
	FocusMode FocusMode
}

const maxMessageLen = 120

// Sink is an additional notification destination beyond the terminal (bell /
// OSC-9). A webhook/Slack sink is the canonical implementation: it lets an
// unattended run report "finished / needs input / verify failed" to a chat
// channel. Emit is fire-and-forget — it must never return an error, panic out,
// or block the run for long, so an unreachable endpoint cannot disrupt the
// agent. Implementations are responsible for their own redaction and timeouts.
type Sink interface {
	Emit(event Event, message string)
}

// Notifier emits notifications to w according to cfg, and fans them out to any
// attached Sinks. Safe for concurrent use.
type Notifier struct {
	w   io.Writer
	cfg Config // swap at runtime via Configure; reads outside the lock are safe

	mu      sync.Mutex
	focused bool
	sinks   []Sink
}

// New returns a Notifier. focused defaults to false so a headless caller (no
// focus signal) still emits under the default "unfocused" focus mode; an
// interactive caller should call SetFocused(true) at launch.
func New(w io.Writer, cfg Config) *Notifier {
	return &Notifier{w: w, cfg: cfg}
}

// Configure swaps the mode/focus policy at runtime. Sinks, focus state, and the
// writer are preserved, so an in-session preference change (e.g. the TUI's
// /notify command) applies from the next Notify call. Safe to call concurrently
// with Notify.
func (n *Notifier) Configure(cfg Config) {
	n.mu.Lock()
	n.cfg = cfg
	n.mu.Unlock()
}

// AddSink registers an additional destination that receives every eligible
// event (subject to the same mode/focus policy as the terminal). Sinks fire
// even when the Notifier has no terminal writer, so a headless CI run can still
// reach Slack. Safe to call concurrently with Notify.
func (n *Notifier) AddSink(sink Sink) {
	if sink == nil {
		return
	}
	n.mu.Lock()
	n.sinks = append(n.sinks, sink)
	n.mu.Unlock()
}

// SetFocused records the terminal focus state (TUI FocusMsg/BlurMsg).
func (n *Notifier) SetFocused(focused bool) {
	n.mu.Lock()
	n.focused = focused
	n.mu.Unlock()
}

// Notify emits a notification for event if policy allows. message is the OSC-9
// body (ignored for bell) and is also forwarded verbatim to every sink. Write
// errors are intentionally ignored — a failed notification must never disrupt
// the run.
//
// The terminal sequence is gated by Mode (bell vs OSC-9) and by the focus
// policy; sinks are gated only by "notifications enabled" plus the focus policy,
// because a sink is a separate channel and not bound to the terminal mechanism.
// Sinks are invoked outside the lock so a slow/blocking sink cannot stall a
// concurrent Notify or SetFocused.
func (n *Notifier) Notify(event Event, message string) {
	n.mu.Lock()
	// cfg is mutable at runtime (Configure), so every read happens under the
	// lock; shouldEmit/sequence work on the local copy.
	cfg := n.cfg
	if cfg.Mode == ModeOff || cfg.Mode == "" {
		n.mu.Unlock()
		return
	}
	eligible := shouldEmit(cfg, event, n.focused)
	var sinks []Sink
	if eligible && len(n.sinks) > 0 {
		sinks = append(sinks, n.sinks...)
	}
	if eligible && n.w != nil {
		if seq := sequence(cfg.Mode, message); seq != "" {
			_, _ = io.WriteString(n.w, seq)
		}
	}
	n.mu.Unlock()

	for _, sink := range sinks {
		emitToSink(sink, event, message)
	}
}

// emitToSink invokes one sink, isolating a panic so a misbehaving sink cannot
// crash the run or starve its siblings. A well-behaved Sink already fails soft;
// this is defense in depth.
func emitToSink(sink Sink, event Event, message string) {
	defer func() { _ = recover() }()
	sink.Emit(event, message)
}

// DefaultMessage is the generic OSC-9 body for an event (no prompt content).
func DefaultMessage(event Event) string {
	if event == AwaitingInput {
		return "Zero: needs input"
	}
	return "Zero: ready"
}

func shouldEmit(cfg Config, _ Event, focused bool) bool {
	if cfg.Mode == ModeOff || cfg.Mode == "" {
		return false
	}
	switch cfg.FocusMode {
	case FocusAlways:
		return true
	case FocusFocused:
		return focused
	default: // FocusUnfocused, "", or unknown
		return !focused
	}
}

func sequence(mode Mode, message string) string {
	switch mode {
	case ModeBell:
		return "\x07"
	case ModeNotify:
		return "\x1b]9;" + sanitizeMessage(message) + "\x07"
	case ModeBoth:
		return "\x07\x1b]9;" + sanitizeMessage(message) + "\x07"
	default:
		return ""
	}
}

// sanitizeMessage drops control bytes (so the message can't break the escape or
// inject terminal control) and clamps to maxMessageLen runes.
func sanitizeMessage(s string) string {
	var b strings.Builder
	count := 0
	for _, r := range s {
		// C0 controls, DEL, and the C1 range (U+0080–U+009F) are all dropped:
		// C1 includes the single-byte CSI/OSC/ST introducers, which would break
		// or escape the OSC-9 sequence exactly like their ESC-prefixed forms.
		if r == 0x1b || r == 0x07 || r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			continue
		}
		b.WriteRune(r)
		count++
		if count >= maxMessageLen {
			break
		}
	}
	return b.String()
}

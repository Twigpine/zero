package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestStartNewSessionResetsState(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.activeSession = sessions.Metadata{SessionID: "sess-old"}
	m.sessionEvents = []sessions.Event{{Type: sessions.EventMessage}}
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendUser, text: "hello"})
	// Stage attachments + a queued message that /new must not leak into the new session.
	m.pendingImages = make([]zeroruntime.ImageBlock, 1)
	m.pendingImageLabels = []string{"pic.png"}
	m.pendingDocuments = []pendingDocument{{label: "doc.pdf"}}
	m.queuedMessage = "queued"
	// The /retry attachment snapshot is prior-session state too and must not survive.
	m.lastImages = make([]zeroruntime.ImageBlock, 1)
	m.lastImageLabels = []string{"pic.png"}
	m.lastDocuments = []pendingDocument{{label: "doc.pdf"}}

	next := m.startNewSession()

	if next.activeSession.SessionID != "" {
		t.Fatalf("expected active session id cleared, got %q", next.activeSession.SessionID)
	}
	if len(next.sessionEvents) != 0 {
		t.Fatalf("expected session events cleared, got %d", len(next.sessionEvents))
	}
	if len(next.transcript) != 2 || next.transcript[0].kind != rowWelcome {
		t.Fatalf("expected transcript reset to welcome + note, got %#v", next.transcript)
	}
	// The note must name the prior session id so the user can /resume it.
	if !transcriptContains(next.transcript, "sess-old") {
		t.Fatalf("expected note to reference previous session id, got %#v", next.transcript)
	}
	// Staged attachments and the queued message must not leak into the new session.
	if len(next.pendingImages) != 0 || len(next.pendingImageLabels) != 0 || len(next.pendingDocuments) != 0 || next.queuedMessage != "" {
		t.Fatalf("startNewSession must clear staged input, got images=%d labels=%d docs=%d queued=%q",
			len(next.pendingImages), len(next.pendingImageLabels), len(next.pendingDocuments), next.queuedMessage)
	}
	// The /retry snapshot must not leak the previous session's attachments.
	if len(next.lastImages) != 0 || len(next.lastImageLabels) != 0 || len(next.lastDocuments) != 0 {
		t.Fatalf("startNewSession must clear the retry snapshot, got images=%d labels=%d docs=%d",
			len(next.lastImages), len(next.lastImageLabels), len(next.lastDocuments))
	}
}

func TestNewCommandStartsFreshSession(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.activeSession = sessions.Metadata{SessionID: "sess-old"}
	m.input.SetValue("/new")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	if next.activeSession.SessionID != "" {
		t.Fatalf("expected /new to clear the active session, got %q", next.activeSession.SessionID)
	}
}

func TestNewCommandDoesNotResetDuringRun(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.activeSession = sessions.Metadata{SessionID: "sess-old"}
	m.pending = true
	m.input.SetValue("/new")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)

	// The safety invariant: /new must never strand an in-flight session.
	if next.activeSession.SessionID != "sess-old" {
		t.Fatalf("/new must not reset an in-flight session, got %q", next.activeSession.SessionID)
	}
}

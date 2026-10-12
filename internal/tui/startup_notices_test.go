package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/sessions"
)

// The caller decides what a session should know before its first prompt; the
// model shows each notice as a system row, in order, and skips blank entries so
// a caller can pass one unconditionally.
func TestStartupNoticesOpenTheSession(t *testing.T) {
	m := newModel(context.Background(), Options{StartupNotices: []string{"first notice", "  ", "", "second notice"}})

	var shown []string
	for _, row := range m.transcript {
		if row.kind == rowSystem {
			shown = append(shown, row.text)
		}
	}
	if len(shown) != 2 || shown[0] != "first notice" || shown[1] != "second notice" {
		t.Fatalf("system rows = %q, want the two non-blank notices in order", shown)
	}
}

// A RESUMED SESSION IS TOLD TOO. /resume rebuilds the transcript from nothing,
// so the startup notices went with the old one, and the session it switched to
// ran under a degraded sandbox without a word. They come back first, before the
// resume summary, blank entries still skipped. Reported by CodeRabbit.
func TestResumeRepeatsTheStartupNotices(t *testing.T) {
	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{Title: "Earlier", ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.AppendEvent(session.SessionID, sessions.AppendEventInput{Type: sessions.EventMessage, Payload: map[string]string{"content": "hello"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	m := newModel(context.Background(), Options{SessionStore: store, StartupNotices: []string{"sandbox notice", " "}})

	next, _ := m.handleResumeCommand(session.SessionID)
	var shown []string
	for _, row := range next.transcript {
		if row.kind == rowSystem {
			shown = append(shown, row.text)
		}
	}
	if len(shown) < 2 || shown[0] != "sandbox notice" {
		t.Fatalf("resumed system rows = %q, want the startup notice first", shown)
	}
	for _, text := range shown[1:] {
		if text == "sandbox notice" || strings.TrimSpace(text) == "" {
			t.Fatalf("resumed system rows = %q, want the notice once and no blank row", shown)
		}
	}
}

// /new is a session opening too. It cleared the transcript and added only its
// own note, so the new session started under a degraded sandbox without a
// word. Reported by CodeRabbit.
func TestNewSessionRepeatsTheStartupNotices(t *testing.T) {
	m := newModel(context.Background(), Options{StartupNotices: []string{"sandbox notice", ""}})
	next := m.startNewSession()

	var shown []string
	for _, row := range next.transcript {
		if row.kind == rowSystem {
			shown = append(shown, row.text)
		}
	}
	if len(shown) != 2 || shown[0] != "sandbox notice" || shown[1] != "Started a new session." {
		t.Fatalf("system rows after /new = %q, want the startup notice and then the new-session note", shown)
	}
}

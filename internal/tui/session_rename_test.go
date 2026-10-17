package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/sessions"
)

func renameTestModel(t *testing.T) (model, *sessions.Store, sessions.Metadata) {
	t.Helper()
	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{Title: "Current session name"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	m := newModel(context.Background(), Options{SessionStore: store})
	m.activeSession = session
	return m, store, session
}

func TestRenameCommandRenamesCurrentSessionWithoutAgentRun(t *testing.T) {
	m, store, session := renameTestModel(t)
	m.input.SetValue("/rename   Better session name  ")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("/rename must not start an agent or background command")
	}
	if next.activeSession.Title != "Better session name" {
		t.Fatalf("active title = %q", next.activeSession.Title)
	}
	stored, err := store.Get(session.SessionID)
	if err != nil || stored == nil {
		t.Fatalf("get renamed session: %v", err)
	}
	if stored.Title != "Better session name" {
		t.Fatalf("stored title = %q", stored.Title)
	}
	if !next.titledSessions[session.SessionID] {
		t.Fatal("manual rename should suppress later automatic naming")
	}
	if !transcriptContains(next.transcript, "Session renamed to Better session name.") {
		t.Fatalf("missing rename confirmation: %#v", next.transcript)
	}
}

func TestRenameCommandCapsSessionTitle(t *testing.T) {
	m, store, session := renameTestModel(t)
	longTitle := strings.Repeat("界", tuiSessionTitleLimit+20)
	m.input.SetValue("/rename " + longTitle)

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("/rename must not start an agent or background command")
	}
	want := cutRunes(longTitle, tuiSessionTitleLimit)
	if next.activeSession.Title != want {
		t.Fatalf("active title has %d runes, want %d", len([]rune(next.activeSession.Title)), tuiSessionTitleLimit)
	}
	stored, err := store.Get(session.SessionID)
	if err != nil || stored == nil || stored.Title != want {
		t.Fatalf("stored capped title = %#v, err=%v", stored, err)
	}
}

func TestRenameEditorBordersWrappedTitleLines(t *testing.T) {
	m, _, _ := renameTestModel(t)
	m.input.SetValue(strings.Repeat("x", tuiSessionTitleLimit))
	m.input.CursorEnd()
	m.renamePrompt = &sessionRenamePrompt{}

	view := plainRender(t, m.sessionRenamePromptView(80))
	lines := strings.Split(view, "\n")
	if len(lines) < 6 {
		t.Fatalf("expected wrapped rename editor, got:\n%s", view)
	}
	for _, line := range lines[1 : len(lines)-1] {
		if !strings.HasPrefix(line, "│ ") || !strings.HasSuffix(line, " │") {
			t.Fatalf("wrapped editor row lost its border: %q\n%s", line, view)
		}
	}
}

func TestBareRenameOpensPrefilledEditorAndSaves(t *testing.T) {
	m, store, session := renameTestModel(t)
	m.input.SetValue("/rename")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd != nil || next.renamePrompt == nil {
		t.Fatalf("bare /rename should open editor without a command: prompt=%#v cmd=%v", next.renamePrompt, cmd)
	}
	if next.input.Value() != "Current session name" {
		t.Fatalf("editor value = %q", next.input.Value())
	}
	view := plainRender(t, next.View())
	for _, want := range []string{"Rename session", "Current session name", "Enter save", "Esc cancel"} {
		if !strings.Contains(view, want) {
			t.Fatalf("rename editor missing %q:\n%s", want, view)
		}
	}

	next.input.SetValue("Edited in prompt")
	updated, cmd = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	if cmd != nil || next.renamePrompt != nil {
		t.Fatalf("saving should close editor without a command: prompt=%#v cmd=%v", next.renamePrompt, cmd)
	}
	stored, err := store.Get(session.SessionID)
	if err != nil || stored == nil || stored.Title != "Edited in prompt" {
		t.Fatalf("stored session after editor save = %#v, err=%v", stored, err)
	}
}

func TestRenameNamesFreshSessionBeforeFirstPrompt(t *testing.T) {
	store := testSessionStore(t)
	m := newModel(context.Background(), Options{SessionStore: store})
	m.input.SetValue("/rename Fresh session name")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("fresh /rename must stay local")
	}
	if next.activeSession.SessionID != "" || next.pendingSessionTitle != "Fresh session name" {
		t.Fatalf("fresh rename state = active:%q pending:%q", next.activeSession.SessionID, next.pendingSessionTitle)
	}

	next, err := next.ensureActiveSession("first real prompt")
	if err != nil {
		t.Fatalf("ensure active session: %v", err)
	}
	if next.activeSession.Title != "Fresh session name" || next.pendingSessionTitle != "" {
		t.Fatalf("created session title = %q, pending=%q", next.activeSession.Title, next.pendingSessionTitle)
	}
	if !next.titledSessions[next.activeSession.SessionID] {
		t.Fatal("a manually named fresh session must skip automatic naming")
	}
	stored, err := store.Get(next.activeSession.SessionID)
	if err != nil || stored == nil || stored.Title != "Fresh session name" {
		t.Fatalf("stored fresh session = %#v, err=%v", stored, err)
	}
}

func TestRenameEditorRejectsBlankAndEscCancels(t *testing.T) {
	m, store, session := renameTestModel(t)
	m = m.openSessionRenamePrompt()
	m.input.SetValue("   ")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if next.renamePrompt != nil {
		t.Fatal("blank submit should close the editor")
	}
	if !transcriptContains(next.transcript, "session name cannot be empty") {
		t.Fatalf("missing blank-name error: %#v", next.transcript)
	}
	stored, _ := store.Get(session.SessionID)
	if stored == nil || stored.Title != "Current session name" {
		t.Fatalf("blank rename changed title: %#v", stored)
	}

	next = next.openSessionRenamePrompt()
	next.input.SetValue("Do not save")
	updated, _ = next.Update(testKey(tea.KeyEsc))
	next = updated.(model)
	if next.renamePrompt != nil || next.input.Value() != "" {
		t.Fatalf("Esc should close and clear editor: prompt=%#v input=%q", next.renamePrompt, next.input.Value())
	}
	stored, _ = store.Get(session.SessionID)
	if stored == nil || stored.Title != "Current session name" {
		t.Fatalf("cancelled rename changed title: %#v", stored)
	}
}

func TestRetitleCommandIsRemoved(t *testing.T) {
	if command, ok := resolveCommand("/retitle"); ok {
		t.Fatalf("/retitle should not resolve, got %#v", command)
	}
	command, ok := resolveCommand("/rename")
	if !ok || command.kind != commandRename {
		t.Fatalf("/rename should resolve, got ok=%v command=%#v", ok, command)
	}
}

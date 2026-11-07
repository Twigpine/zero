package sessions

import (
	"testing"
	"time"
)

func newTitleTestStore(t *testing.T) *Store {
	t.Helper()
	now := time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC)
	return NewStore(StoreOptions{
		RootDir: t.TempDir(),
		Now: func() time.Time {
			now = now.Add(time.Second)
			return now
		},
	})
}

func TestUpdateTitle(t *testing.T) {
	store := newTitleTestStore(t)
	session, err := store.Create(CreateInput{Title: "first message title"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// An append bumps UpdatedAt and EventCount; UpdateTitle must preserve both.
	if _, err := store.AppendEvent(session.SessionID, AppendEventInput{
		Type:    EventMessage,
		Payload: map[string]any{"role": "user", "content": "hi"},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	before, err := store.Get(session.SessionID)
	if err != nil || before == nil {
		t.Fatalf("get before: %v", err)
	}

	updated, err := store.UpdateTitle(session.SessionID, "  Clean Generated Title  ")
	if err != nil {
		t.Fatalf("update title: %v", err)
	}
	if updated.Title != "Clean Generated Title" {
		t.Fatalf("title not trimmed/stored: %q", updated.Title)
	}
	if updated.UpdatedAt != before.UpdatedAt {
		t.Fatalf("UpdatedAt must not change on rename: before=%q after=%q", before.UpdatedAt, updated.UpdatedAt)
	}
	if updated.EventCount != before.EventCount {
		t.Fatalf("EventCount changed: before=%d after=%d", before.EventCount, updated.EventCount)
	}

	persisted, err := store.Get(session.SessionID)
	if err != nil || persisted == nil {
		t.Fatalf("get after: %v", err)
	}
	if persisted.Title != "Clean Generated Title" {
		t.Fatalf("persisted title = %q, want %q", persisted.Title, "Clean Generated Title")
	}

	// A blank title is rejected and must not erase the existing one.
	if _, err := store.UpdateTitle(session.SessionID, "   "); err == nil {
		t.Fatal("expected a blank title to be rejected")
	}
	if got, _ := store.Get(session.SessionID); got == nil || got.Title != "Clean Generated Title" {
		t.Fatalf("title must survive a rejected blank update, got %#v", got)
	}

	// An unchanged title is a no-op (still succeeds).
	if _, err := store.UpdateTitle(session.SessionID, "Clean Generated Title"); err != nil {
		t.Fatalf("no-op rename should succeed: %v", err)
	}

	// An invalid session id is rejected.
	if _, err := store.UpdateTitle("../escape", "whatever"); err == nil {
		t.Fatal("expected an invalid session id to be rejected")
	}
}

func TestUpdateTitleIfCurrent(t *testing.T) {
	store := newTitleTestStore(t)
	session, err := store.Create(CreateInput{Title: "Automatic title"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	updated, applied, err := store.UpdateTitleIfCurrent(session.SessionID, "Automatic title", "Generated title")
	if err != nil || !applied || updated.Title != "Generated title" {
		t.Fatalf("matching update = (%#v, %v, %v)", updated, applied, err)
	}

	if _, err := store.UpdateTitle(session.SessionID, "Manual title"); err != nil {
		t.Fatalf("manual rename: %v", err)
	}
	updated, applied, err = store.UpdateTitleIfCurrent(session.SessionID, "Generated title", "Late generated title")
	if err != nil {
		t.Fatalf("stale update: %v", err)
	}
	if applied || updated.Title != "Manual title" {
		t.Fatalf("stale update should preserve manual title: (%#v, %v)", updated, applied)
	}
}

func TestUpdateModel(t *testing.T) {
	store := newTitleTestStore(t)
	session, err := store.Create(CreateInput{ModelID: "model-a"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.AppendEvent(session.SessionID, AppendEventInput{Type: EventMessage, Payload: map[string]any{"role": "user"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	before, err := store.Get(session.SessionID)
	if err != nil || before == nil {
		t.Fatalf("get before: %v", err)
	}

	updated, err := store.UpdateModel(session.SessionID, "  model-b  ")
	if err != nil {
		t.Fatalf("update model: %v", err)
	}
	if updated.ModelID != "model-b" {
		t.Fatalf("model = %q, want model-b", updated.ModelID)
	}
	if updated.UpdatedAt != before.UpdatedAt || updated.EventCount != before.EventCount {
		t.Fatalf("model update changed activity metadata: before=%+v after=%+v", before, updated)
	}
	persisted, err := store.Get(session.SessionID)
	if err != nil || persisted == nil || persisted.ModelID != "model-b" {
		t.Fatalf("persisted model: metadata=%+v err=%v", persisted, err)
	}
}

func TestUpdateMode(t *testing.T) {
	store := newTitleTestStore(t)
	session, err := store.Create(CreateInput{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.AppendEvent(session.SessionID, AppendEventInput{Type: EventMessage, Payload: map[string]any{"role": "user"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	before, err := store.Get(session.SessionID)
	if err != nil || before == nil {
		t.Fatalf("get before: %v", err)
	}

	updated, err := store.UpdateMode(session.SessionID, "  plan  ")
	if err != nil {
		t.Fatalf("update mode: %v", err)
	}
	if updated.PermissionMode != "plan" {
		t.Fatalf("mode = %q, want plan", updated.PermissionMode)
	}
	if updated.UpdatedAt != before.UpdatedAt || updated.EventCount != before.EventCount {
		t.Fatalf("mode update changed activity metadata: before=%+v after=%+v", before, updated)
	}
	persisted, err := store.Get(session.SessionID)
	if err != nil || persisted == nil || persisted.PermissionMode != "plan" {
		t.Fatalf("persisted mode: metadata=%+v err=%v", persisted, err)
	}

	cleared, err := store.UpdateMode(session.SessionID, "")
	if err != nil || cleared.PermissionMode != "" {
		t.Fatalf("clearing mode: metadata=%+v err=%v", cleared, err)
	}
}

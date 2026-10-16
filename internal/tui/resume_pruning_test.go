package tui

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Gitlawb/zero/internal/sessions"
)

// A resume that finds zero sessions prune holding the session is refused, not
// read from the raw log without the lease.
func TestResumeEventsIsRefusedWhilePruneHoldsTheSession(t *testing.T) {
	root := t.TempDir()
	creator := sessions.NewStore(sessions.StoreOptions{RootDir: root})
	meta, err := creator.Create(sessions.CreateInput{Title: "resume me"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := creator.AppendEvent(meta.SessionID, sessions.AppendEventInput{Type: sessions.EventMessage, Payload: map[string]string{"content": "hello"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	creator.Release(meta.SessionID)

	release, locked, err := sessions.NewStore(sessions.StoreOptions{RootDir: root}).HoldExclusive(meta.SessionID)
	if err != nil || !locked {
		t.Fatalf("take the lease the way prune does: locked=%v, %v", locked, err)
	}
	defer release()

	m := model{sessionStore: sessions.NewStore(sessions.StoreOptions{RootDir: root})}
	events, err := m.resumeEvents(meta.SessionID)
	if !errors.Is(err, sessions.ErrPruning) || events != nil {
		t.Fatalf("resume while prune holds the session: %d events, err = %v, want it refused", len(events), err)
	}
}

// A session picked for /resume that prune removes before it is read is refused
// too, not resumed as an empty conversation: part way through the removal, with
// the directory still there and its metadata gone, and once it is gone.
func TestResumeEventsIsRefusedOnceThePickedSessionIsRemoved(t *testing.T) {
	root := t.TempDir()
	creator := sessions.NewStore(sessions.StoreOptions{RootDir: root})
	meta, err := creator.Create(sessions.CreateInput{Title: "resume me"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := creator.AppendEvent(meta.SessionID, sessions.AppendEventInput{Type: sessions.EventMessage, Payload: map[string]string{"content": "hello"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	creator.Release(meta.SessionID)
	dir := filepath.Join(root, meta.SessionID)

	if err := os.Remove(filepath.Join(dir, sessions.MetadataFile)); err != nil {
		t.Fatal(err)
	}
	m := model{sessionStore: sessions.NewStore(sessions.StoreOptions{RootDir: root})}
	if events, err := m.resumeEvents(meta.SessionID); !errors.Is(err, sessions.ErrPruning) || events != nil {
		t.Errorf("resume while prune is removing the session: %d events, err = %v, want it refused", len(events), err)
	}

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	m = model{sessionStore: sessions.NewStore(sessions.StoreOptions{RootDir: root})}
	if events, err := m.resumeEvents(meta.SessionID); !errors.Is(err, sessions.ErrPruning) || events != nil {
		t.Errorf("resume after prune removed the session: %d events, err = %v, want it refused", len(events), err)
	}
}

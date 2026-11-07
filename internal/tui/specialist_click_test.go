package tui

import (
	"context"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Twigpine/zero/internal/sessions"
)

// specialistClickTestModel builds a model with a single specialist card in the
// transcript and the child session registered in the store so subchat.enter
// can load it. Returns the model and the specialist's childSessionID.
func specialistClickTestModel(t *testing.T, childSessionID string) model {
	t.Helper()
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	if _, err := store.Create(sessions.CreateInput{SessionID: childSessionID}); err != nil {
		t.Fatalf("create child session: %v", err)
	}
	m := newModel(context.Background(), Options{ModelName: "gpt-4", SessionStore: store})
	m.width = 80
	m.height = 40
	m.altScreen = true
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	m.transcript = append(m.transcript, transcriptRow{
		kind: rowSpecialist,
		specialistInfo: &specialistInfo{
			name:           "worker",
			description:    "fix tests",
			childSessionID: childSessionID,
			status:         specialistRunning,
			startedAt:      now,
		},
	})
	return m
}

// specialistCardClickPoint resolves the screen coordinates (x, y) of the first
// selectable specialist card line in the model's transcript layout.
func specialistCardClickPoint(m model, x int) (int, int, bool) {
	width := m.chatColumnWidth()
	layout := m.transcriptBodyLayout(width, "")
	frame := m.scrollableTranscriptFrame(m.pinnedTitleBar(width), m.footerView(width))
	for _, line := range layout.selectable {
		if line.specialistCard {
			return x, frame.bodyRect.y + line.bodyY, true
		}
	}
	return 0, 0, false
}

func TestSpecialistCardClickDrillsIntoSubchat(t *testing.T) {
	const childID = "child-sess-1"
	m := specialistClickTestModel(t, childID)

	x, y, ok := specialistCardClickPoint(m, 2)
	if !ok {
		t.Fatal("expected a specialist card selectable line in the layout")
	}

	click := testMouseClick(tea.MouseLeft, x, y)
	updated, _, handled := m.handleTranscriptSelectionMouse(click)
	if !handled {
		t.Fatal("click on specialist card should be handled")
	}
	m2 := updated
	if !m2.subchat.active {
		t.Fatal("subchat should be active after clicking specialist card")
	}
	if m2.subchat.childSessionID != childID {
		t.Errorf("subchat childSessionID = %q, want %q", m2.subchat.childSessionID, childID)
	}
}

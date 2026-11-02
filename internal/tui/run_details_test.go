package tui

import (
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
)

func TestRunDetailsOverlaySurfacesCurrentState(t *testing.T) {
	m := sidebarTestModel()
	m.runDetailsOpen = true
	plain := plainRender(t, m.runDetailsOverlay(m.width))
	for _, want := range []string{"Run details", "PLAN", "wire it up", "Esc or Ctrl+B closes"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("run details missing %q:\n%s", want, plain)
		}
	}
}

func TestRunDetailsTogglePreservesFullWidthTranscript(t *testing.T) {
	m := sidebarTestModel()
	if got, want := m.chatColumnWidth(), chatWidth(m.width); got != want {
		t.Fatalf("chat width before details = %d, want %d", got, want)
	}
	updated, _ := m.Update(testKeyCtrl('b'))
	opened := updated.(model)
	if !opened.runDetailsOpen {
		t.Fatal("Ctrl+B should open run details when the composer is empty")
	}
	if got, want := opened.chatColumnWidth(), chatWidth(opened.width); got != want {
		t.Fatalf("chat width with details = %d, want %d", got, want)
	}
	updated, _ = opened.Update(testKeyCtrl('b'))
	closed := updated.(model)
	if closed.runDetailsOpen {
		t.Fatal("Ctrl+B should close run details")
	}
}

func TestRunDetailsSwallowsComposerInputUntilClosed(t *testing.T) {
	m := sidebarTestModel()
	m.runDetailsOpen = true
	updated, _ := m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if got := updated.(model).composerValue(); got != "" {
		t.Fatalf("input behind run details = %q, want empty", got)
	}
}

func TestRunDetailsClosesWhenTerminalBecomesTooNarrow(t *testing.T) {
	m := sidebarTestModel()
	m.runDetailsOpen = true
	updated, _ := m.Update(tea.WindowSizeMsg{Width: runDetailsMinWidth - 1, Height: m.height})
	if updated.(model).runDetailsOpen {
		t.Fatal("narrow resize should close an unavailable run-details overlay")
	}
}

func TestRunDetailsOverlayHidesBehindPicker(t *testing.T) {
	m := sidebarTestModel()
	m.runDetailsOpen = true
	m.picker = &commandPicker{}
	if got := m.runDetailsOverlay(m.width); got != "" {
		t.Fatalf("run details must yield to a picker that owns the keyboard, got:\n%s", got)
	}
}

func TestRunDetailsHitsAlignWithSidebar(t *testing.T) {
	m := filesPanelTestModel()
	inner := 40
	fileLines, hits := m.sidebarFileLines(inner)
	if len(hits) == 0 || len(fileLines) == 0 {
		t.Fatal("sidebar hits")
	}
	layout := m.runDetailsLayout(inner)
	if layout.fileStart < 0 {
		t.Fatal("run details dropped FILES block identity")
	}
	if len(layout.fileHits) != len(hits) && len(fileLines) <= runDetailsMaxItems {
		t.Fatalf("hit count %d vs sidebar %d", len(layout.fileHits), len(hits))
	}
	for _, h := range layout.fileHits {
		idx := layout.fileStart + h.lineOffset
		if idx < 0 || idx >= len(layout.lines) {
			t.Fatalf("hit %q offset %d outside details", h.path, h.lineOffset)
		}
		if h.lineOffset < 0 || h.lineOffset >= len(fileLines) {
			t.Fatalf("hit %q offset %d outside sidebar lines", h.path, h.lineOffset)
		}
		if layout.lines[idx] != fileLines[h.lineOffset] {
			t.Fatalf("hit map != rendered row for %q", h.path)
		}
	}
}

package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// The subchat drill-in (viewing a subagent/swarm child session) swaps the
// on-screen transcript to m.subchat.childRows. These tests guard that mouse
// selection resolves against those child rows, plans are never pinned above the
// composer, and selection extends across wheel scrolling.

func TestFooterDoesNotPinPlanDuringSubchat(t *testing.T) {
	m := runningPlanModel(t, 3)
	m.altScreen = true
	m.height = 30
	width := m.chatColumnWidth()

	withoutSubchat := m.footerView(width)
	if strings.Contains(withoutSubchat, "Step number 1 here") {
		t.Fatalf("plan must render in the transcript rather than footer, got:\n%s", withoutSubchat)
	}

	m.subchat.active = true
	m.subchat.childSessionID = "child-1"
	withSubchat := m.footerView(width)
	if strings.Contains(withSubchat, "Step number 1 here") {
		t.Fatalf("plan must not appear in a child-session footer, got:\n%s", withSubchat)
	}
}

func TestTranscriptSelectionInSubchatUsesChildSessionRows(t *testing.T) {
	m := mouseTestModel()
	m.mouseCapture = true
	// Parent transcript has DIFFERENT text than the child session: if selection
	// hit-tests against the wrong (parent) rows, this proves it by either matching
	// nothing or matching the wrong text.
	m.transcript = appendRow(m.transcript, rowUser, "parent transcript text")
	m.subchat.active = true
	m.subchat.childSessionID = "child-1"
	m.subchat.childRows = appendRow(nil, rowUser, "hello world")

	textY := topmostVisibleTranscriptMouseY(t, m)
	updated, _ := m.Update(testMouseClick(tea.MouseLeft, 3, textY))
	m = updated.(model)
	updated, _ = m.Update(testMouseMotion(tea.MouseLeft, 8, textY))
	m = updated.(model)

	if got := m.selectedTranscriptText(); got != "hello" {
		t.Fatalf("selectedTranscriptText() in subchat = %q, want hello (from the CHILD session's rows)", got)
	}
}

func TestTranscriptSelectionExtendsAcrossWheelScroll(t *testing.T) {
	m := mouseTestModel()
	m.mouseCapture = true
	// Enough rows that the transcript overflows the viewport, so a wheel-scroll is
	// a real (non-clamped) scroll. chatScrollOffset=0 anchors to the BOTTOM (newest
	// content, see transcriptViewport.window: start = totalLines-height-offset), so
	// with overflow content the topmost VISIBLE line is not the transcript's first
	// line — topmostVisibleTranscriptMouseY (window-aware) finds it correctly.
	for i := 0; i < 80; i++ {
		m.transcript = appendRow(m.transcript, rowUser, "line content")
	}
	textY := topmostVisibleTranscriptMouseY(t, m)

	updated, _ := m.Update(testMouseClick(tea.MouseLeft, 0, textY))
	m = updated.(model)
	if !m.transcriptSelection.active {
		t.Fatal("selection should be active after a left click on transcript text")
	}
	cursorBefore := m.transcriptSelection.cursor.bodyY
	scrollBefore := m.chatScrollOffset

	// Wheel UP reveals OLDER content above (offset increases -> window.start
	// decreases): re-evaluating the SAME on-screen Y afterward must land on an
	// EARLIER (smaller bodyY) line now that the viewport has shifted, extending the
	// selection upward instead of leaving the cursor pinned to the pre-scroll line.
	updated, _ = m.Update(testMouseWheel(tea.MouseWheelUp, 0, textY))
	m = updated.(model)

	if m.chatScrollOffset == scrollBefore {
		t.Fatal("sanity check failed: wheel-up should have scrolled (80 rows overflow a 30-row terminal)")
	}
	if !m.transcriptSelection.active {
		t.Fatal("selection must survive a wheel-scroll, not be cleared")
	}
	if m.transcriptSelection.cursor.bodyY >= cursorBefore {
		t.Fatalf("selection cursor bodyY = %d, want < %d (it must extend to follow the scroll, not freeze)", m.transcriptSelection.cursor.bodyY, cursorBefore)
	}
}

// topmostVisibleTranscriptMouseY returns the on-screen Y of the topmost currently
// VISIBLE selectable text line — window-aware (unlike firstTranscriptTextMouseY,
// which walks the full unwindowed layout and only lands on-screen when everything
// fits in one viewport). It resolves against transcriptHitTestSource, the same
// subchat-aware source transcriptLineAtMouse uses, so it works for both the parent
// transcript and a subchat child session.
func topmostVisibleTranscriptMouseY(t *testing.T, m model) int {
	t.Helper()
	header, items, width := m.transcriptHitTestSource()
	frame := m.scrollableTranscriptFrame(header, m.footerView(width))
	metrics := measureTranscriptBodyItems(items, m.transcriptBodyHeights)
	window := transcriptViewportForLayout(metrics, frame, m.chatScrollOffset).window()
	layout := layoutVisibleTranscriptBodyItems(items, metrics, window)
	for _, line := range layout.selectable {
		if line.text != "" && !line.toggle {
			return frame.bodyRect.y + (line.bodyY - window.start)
		}
	}
	t.Fatalf("no selectable visible transcript text line found: %#v", layout.selectable)
	return 0
}

package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
)

// A ROW'S FREE SPACE IS MEASURED IN COLUMNS, NOT RUNES.
//
// The status glyph is styled, so it is one column of "✓" wrapped in around
// twenty runes of escape sequence. Subtracting len([]rune(head)) charged the
// row for all of them, so the space left for the summary came out roughly
// twenty columns short and the `remaining > 8` guard dropped summaries that
// fit with room to spare — on wide terminals most of all, where there was
// never any real pressure on the line.
func TestATaskSummaryIsKeptWhenItFitsInColumns(t *testing.T) {
	task := orchestrateTask{
		id:      "trace",
		status:  orchestrateDone,
		summary: "found the seam",
	}
	const width = 40

	line := model{}.renderOrchestrateTaskLine(task, time.Time{}, width)

	if !strings.Contains(line, "found the seam") {
		t.Fatalf("the summary was dropped from a %d-column row that had room for it:\n  visible: %q (%d columns)",
			width, ansiStripped(line), lipgloss.Width(line))
	}
}

// The other half: measuring in columns must not let a row overflow its width.
func TestATaskRowStaysInsideItsWidth(t *testing.T) {
	task := orchestrateTask{
		id:      "trace",
		status:  orchestrateDone,
		summary: strings.Repeat("long summary text ", 20),
	}
	for _, width := range []int{28, 40, 80, 120} {
		line := model{}.renderOrchestrateTaskLine(task, time.Time{}, width)
		if got := lipgloss.Width(line); got > width {
			t.Errorf("a %d-column row rendered %d columns wide: %q", width, got, ansiStripped(line))
		}
	}
}

func ansiStripped(text string) string {
	var b strings.Builder
	inEscape := false
	for _, r := range text {
		switch {
		case r == 0x1b:
			inEscape = true
		case inEscape && (r == 'm' || r == 'K'):
			inEscape = false
		case !inEscape:
			b.WriteRune(r)
		}
	}
	return b.String()
}

package tui

import (
	"strings"
	"testing"
)

// THE CHIP IS ON THE STATUS LINE, so only the status line may be hit-tested.
//
// The hit test scanned every row footerView renders and took the FIRST one
// containing the word. But footerView is not only chips: it renders the plan
// panel, the idle hints, a queued-message preview and THE COMPOSER. So typing
// "zeromaxing" into the composer put a matching row above the chip's, and the
// hover target became the composer — a click meant for the chip landed on text
// the user was still writing, and the chip itself stopped responding.
//
// The composer is the reachable case, and a plan task prompt containing the word
// is the same defect arriving through the panel.
func TestTypingTheChipLabelInTheComposerDoesNotStealTheChipsRow(t *testing.T) {
	m := newZeromaxingChipModel(t)

	before, ok := m.zeromaxingChipRow()
	if !ok {
		t.Fatal("setup: the chip is not in the footer, so this test measures nothing")
	}

	typed := m
	typed.input.SetValue("why is zeromaxing slow")
	// The premise: the typed word really does reach the footer. Without this the
	// test passes for the wrong reason on any build where the composer is hidden.
	if !strings.Contains(ansiStripLine(typed.composerBox(typed.chatColumnWidth())), zeromaxingChipLabel) {
		t.Skip("the composer does not render its own text in this configuration")
	}

	after, ok := typed.zeromaxingChipRow()
	if !ok {
		t.Fatal("the chip's row vanished once the label was typed")
	}
	if after != before {
		t.Fatalf("typing the label moved the chip's hit row from %d to %d: the composer became the click target", before, after)
	}
}

// The row the hit test reports must be the row the chip is actually drawn on —
// asserted against the rendered screen, not against the search that found it.
func TestTheReportedChipRowIsTheRowTheChipIsDrawnOn(t *testing.T) {
	m := newZeromaxingChipModel(t)
	m.input.SetValue("why is zeromaxing slow")

	row, ok := m.zeromaxingChipRow()
	if !ok {
		t.Fatal("the chip's row was not found")
	}
	footer := viewLines(m.footerView(m.chatColumnWidth()))
	index := len(footer) - (m.height - row)
	if index < 0 || index >= len(footer) {
		t.Fatalf("reported row %d is outside the footer (%d rows, height %d)", row, len(footer), m.height)
	}
	line := ansiStripLine(footer[index])
	if !strings.Contains(line, zeromaxingChipLabel) {
		t.Fatalf("row %d does not carry the chip: %q", row, line)
	}
	// And it is the STATUS line, not merely some row bearing the word: the
	// composer draws the typed text inside a box, the status line does not.
	if strings.Contains(line, "why is") {
		t.Fatalf("the composer's own row was reported as the chip's: %q", line)
	}
}

// A click at the chip's reported position must still land on it. The narrowing
// must not have moved the span off the chip while making the row correct.
func TestTheChipStillAnswersAClickAfterNarrowingToTheStatusLine(t *testing.T) {
	m := newZeromaxingChipModel(t)
	m.input.SetValue("zeromaxing")

	row, ok := m.zeromaxingChipRow()
	if !ok {
		t.Fatal("the chip's row was not found")
	}
	start, end, ok := m.zeromaxingChipSpan()
	if !ok {
		t.Fatal("the chip's span was not found")
	}
	if start >= end {
		t.Fatalf("the chip's span is empty: [%d,%d)", start, end)
	}
	footer := viewLines(m.footerView(m.chatColumnWidth()))
	line := ansiStripLine(footer[len(footer)-(m.height-row)])
	if !strings.Contains(line, zeromaxingChipLabel) {
		t.Fatalf("the span was taken from a row without the chip: %q", line)
	}
}

package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/Twigpine/zero/internal/agent"
)

// THE CHIP'S SPAN IS A SCREEN COORDINATE, so it must be measured in columns.
//
// strings.Index returns a BYTE offset. Every multi-byte rune earlier on the
// footer row — the "●" in the permission chip, an em dash, a non-ASCII branch
// name — makes that offset larger than the column the chip actually starts at,
// so the clickable span slides right of the chip the user can see. The bug is
// invisible on an all-ASCII footer, which is why it survived: this repo's own
// footer draws "●" before the chip in every ordinary session.
func TestTheChipSpanIsMeasuredInColumnsNotBytes(t *testing.T) {
	m := newZeromaxingChipModel(t)

	start, end, ok := m.zeromaxingChipSpan()
	if !ok {
		t.Fatal("the chip is not in the footer, so this test measures nothing")
	}

	row, ok := m.zeromaxingChipRow()
	if !ok {
		t.Fatal("the chip's row was not found")
	}
	footer := viewLines(m.footerView(m.chatColumnWidth()))
	line := ansiStripLine(footer[len(footer)-(m.height-row)])

	// The label must actually sit inside the reported span, measured the way a
	// terminal measures: by column.
	labelAt := strings.Index(line, zeromaxingChipLabel)
	if labelAt < 0 {
		t.Fatalf("the label is not on the row the span was taken from: %q", line)
	}
	labelColumn := lipgloss.Width(line[:labelAt])

	if labelColumn < start || labelColumn >= end {
		t.Fatalf("the label starts at column %d but the clickable span is [%d,%d): a click on the chip lands outside it.\n  row: %q\n  bytes before the label: %d, columns: %d",
			labelColumn, start, end, line, labelAt, labelColumn)
	}
	// And the multi-byte content really is present, or the case above is not
	// being exercised at all.
	if labelAt == labelColumn {
		t.Fatalf("nothing before the chip is multi-byte, so this test cannot detect the byte/column confusion: %q", line)
	}
}

func newZeromaxingChipModel(t *testing.T) model {
	t.Helper()
	m := sidebarDetailModel(t)
	m.execProfileName = "zeromaxing"
	m.zeromaxing = agent.ZeromaxingActive
	if !m.zeromaxingActive() {
		t.Fatal("setup: the posture must be active or the chip is never drawn")
	}
	return m
}

package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Twigpine/zero/internal/tools"
)

func TestTranscriptCommandTogglesDetailedView(t *testing.T) {
	m := transcriptViewTestModel()
	m.input.SetValue("/transcript")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	assertContains(t, plainRender(t, next.View()), "Transcript")

	next.input.SetValue("/transcript")
	updated, _ = next.Update(testKey(tea.KeyEnter))
	next = updated.(model)
	assertNotContains(t, plainRender(t, next.View()), "Transcript")
}

func TestCtrlOTogglesDetailedTranscriptView(t *testing.T) {
	m := transcriptViewTestModel()

	updated, _ := m.Update(testKeyCtrl('o'))
	next := updated.(model)
	assertContains(t, plainRender(t, next.View()), "Transcript")

	updated, _ = next.Update(testKeyCtrl('o'))
	next = updated.(model)
	assertNotContains(t, plainRender(t, next.View()), "Transcript")
}

func TestEscExitsDetailedTranscriptView(t *testing.T) {
	m := transcriptViewTestModel()
	updated, _ := m.Update(testKeyCtrl('o'))
	next := updated.(model)
	assertContains(t, plainRender(t, next.View()), "Transcript")

	updated, _ = next.Update(testKey(tea.KeyEsc))
	next = updated.(model)
	assertNotContains(t, plainRender(t, next.View()), "Transcript")
}

func TestDetailedTranscriptIncludesToolOutputBeyondLiveCap(t *testing.T) {
	m := transcriptViewTestModel()
	output := numberedLines(flushCardBodyMaxLines + 4)
	row := transcriptRow{kind: rowToolResult, id: "tool-1", tool: "custom_tool", status: tools.StatusOK, detail: output}
	m.transcript = append(m.transcript, row)
	m.flushed = len(m.transcript)

	// Long generic tool output collapses in the live view (click to expand);
	// the body is hidden behind a one-line summary.
	compact := plainRender(t, m.renderRow(row, m.width, buildRowContext(m.transcript)))
	assertNotContains(t, compact, "line-020")
	assertContains(t, compact, "click to expand")

	// The detailed transcript view (Ctrl+O) still shows the full, uncapped output.
	updated, _ := m.Update(testKeyCtrl('o'))
	next := updated.(model)
	view := plainRender(t, next.View())
	assertContains(t, view, "line-404")
	assertNotContains(t, view, "more lines")
	assertNotContains(t, view, "click to expand")
}

func TestDetailedTranscriptViewNeverExceedsTerminalWidth(t *testing.T) {
	for _, width := range []int{24, 40, 58, 80, 120} {
		m := transcriptViewTestModel()
		m.width = width
		m.transcript = append(m.transcript,
			transcriptRow{kind: rowUser, text: "please inspect this very long request and show the transcript without overflowing"},
			transcriptRow{kind: rowToolResult, id: "wide", tool: "custom_tool", status: tools.StatusOK, detail: strings.Repeat("wide-output-", 30)},
			transcriptRow{kind: rowAssistant, text: "done with a final answer that also needs wrapping", final: true, turnTools: 1},
		)
		m.flushed = len(m.transcript)

		updated, _ := m.Update(testKeyCtrl('o'))
		next := updated.(model)
		view := viewString(next.View())
		assertContains(t, plainRender(t, view), "Transcript")
		for index, line := range strings.Split(view, "\n") {
			if got := lipgloss.Width(line); got > chatWidth(width) {
				t.Fatalf("width %d: detailed transcript line %d is %d cells wide: %q", width, index, got, line)
			}
		}
	}
}

func TestDetailedTranscriptSwallowsNormalChatSubmit(t *testing.T) {
	m := transcriptViewTestModel()
	updated, _ := m.Update(testKeyCtrl('o'))
	next := updated.(model)
	next.input.SetValue("this should not launch")

	updated, cmd := next.Update(testKey(tea.KeyEnter))
	next = updated.(model)

	if cmd != nil {
		t.Fatal("detailed transcript should not return a run command for Enter")
	}
	if transcriptContains(next.transcript, "this should not launch") {
		t.Fatalf("detailed transcript should not submit composer text, got %#v", next.transcript)
	}
	assertContains(t, plainRender(t, next.View()), "Transcript")
}

func transcriptViewTestModel() model {
	m := newModel(context.Background(), Options{
		Cwd:          "/work/zero",
		ProviderName: "openai",
		ModelName:    "gpt-test",
	})
	m.width = 96
	m.height = 30
	m.headerPrinted = true
	return m
}

func TestPgUpDownScrollsDetailedTranscript(t *testing.T) {
	m := transcriptViewTestModel()
	m.altScreen = true
	// Fill the transcript enough to overflow the 30-row viewport.
	for i := 0; i < 60; i++ {
		m.transcript = append(m.transcript, transcriptRow{kind: rowUser, text: "line content", final: true})
	}
	m.flushed = len(m.transcript)

	// Enter detailed mode.
	updated, _ := m.Update(testKeyCtrl('o'))
	m = updated.(model)

	if !m.transcriptDetailed {
		t.Fatal("sanity check failed: Ctrl+O should enter detailed mode")
	}
	if m.chatScrollOffset != 0 {
		t.Fatal("sanity check failed: detailed transcript should start at bottom")
	}

	// PgUp reveals older content = positive scroll offset.
	updated, _ = m.Update(testKey(tea.KeyPgUp))
	m = updated.(model)
	if m.chatScrollOffset <= 0 {
		t.Fatalf("PgUp in detailed mode should scroll toward older content, got chatScrollOffset=%d", m.chatScrollOffset)
	}

	// PgDown scrolls back toward bottom.
	updated, _ = m.Update(testKey(tea.KeyPgDown))
	m = updated.(model)
	if m.chatScrollOffset != 0 {
		t.Fatalf("PgDown after PgUp should return to bottom in detailed mode, got chatScrollOffset=%d", m.chatScrollOffset)
	}
}

func TestUpDownArrowsScrollDetailedTranscript(t *testing.T) {
	m := transcriptViewTestModel()
	m.altScreen = true
	for i := 0; i < 60; i++ {
		m.transcript = append(m.transcript, transcriptRow{kind: rowUser, text: "line content", final: true})
	}
	m.flushed = len(m.transcript)

	updated, _ := m.Update(testKeyCtrl('o'))
	m = updated.(model)

	if !m.transcriptDetailed {
		t.Fatal("sanity check failed: Ctrl+O should enter detailed mode")
	}

	// Arrow down scrolls one line toward newer content; at bottom it stays at 0.
	updated, _ = m.Update(testKey(tea.KeyDown))
	m = updated.(model)
	if m.chatScrollOffset != 0 {
		t.Fatalf("KeyDown at bottom should stay at 0, got %d", m.chatScrollOffset)
	}

	// Arrow up scrolls one line toward older content.
	updated, _ = m.Update(testKey(tea.KeyUp))
	m = updated.(model)
	if m.chatScrollOffset != 1 {
		t.Fatalf("KeyUp should scroll one line up, got chatScrollOffset=%d", m.chatScrollOffset)
	}

	// Arrow down scrolls back toward bottom.
	updated, _ = m.Update(testKey(tea.KeyDown))
	m = updated.(model)
	if m.chatScrollOffset != 0 {
		t.Fatalf("KeyDown after KeyUp should return to bottom, got chatScrollOffset=%d", m.chatScrollOffset)
	}
}

func numberedLines(count int) string {
	lines := make([]string, 0, count)
	for i := 1; i <= count; i++ {
		lines = append(lines, fmt.Sprintf("line-%03d", i))
	}
	return strings.Join(lines, "\n")
}

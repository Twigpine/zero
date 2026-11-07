package tui

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestMouseWheelScrollsChatWithoutRecallingInputHistory(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	m.width = 90
	m.height = 14
	m.mouseCapture = true
	m.inputHistory = []string{"old prompt"}
	m.historyIdx = len(m.inputHistory)
	for index := 0; index < 12; index++ {
		m.transcript = appendRow(m.transcript, rowAssistant, "message "+string(rune('A'+index)))
	}

	updated, cmd := m.Update(testMouseWheel(tea.MouseWheelUp, 0, 0))
	m = updated.(model)
	if cmd != nil {
		t.Fatal("mouse wheel should not return a command")
	}
	if got := m.input.Value(); got != "" {
		t.Fatalf("mouse wheel should not recall input history, got %q", got)
	}
	if m.chatScrollOffset != chatWheelScrollLines {
		t.Fatalf("chatScrollOffset = %d, want %d", m.chatScrollOffset, chatWheelScrollLines)
	}
}

func TestScrollChatClampsOffsetAtTranscriptTop(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	m.width = 90
	m.height = 14
	for index := 0; index < 40; index++ {
		m.transcript = appendRow(m.transcript, rowAssistant, "message "+string(rune('A'+index%26)))
	}
	maxOffset := m.chatMaxScrollOffset()
	if maxOffset <= chatWheelScrollLines {
		t.Fatalf("test transcript should be scrollable, maxOffset=%d", maxOffset)
	}

	m = m.scrollChat(maxOffset + 100)
	if m.chatScrollOffset != maxOffset {
		t.Fatalf("scroll beyond top offset = %d, want %d", m.chatScrollOffset, maxOffset)
	}

	m.chatScrollOffset = maxOffset + 100 // Simulate an offset saved before clamping existed.
	m = m.scrollChat(-chatWheelScrollLines)
	if want := maxOffset - chatWheelScrollLines; m.chatScrollOffset != want {
		t.Fatalf("scroll down from inflated offset = %d, want %d", m.chatScrollOffset, want)
	}
}

func TestScrollChatDoesNotAccumulateWhenTranscriptFits(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	m.width = 90
	m.height = 20
	m.transcript = appendRow(m.transcript, rowAssistant, "short")

	m = m.scrollChat(100)
	if m.chatScrollOffset != 0 {
		t.Fatalf("non-scrollable transcript offset = %d, want 0", m.chatScrollOffset)
	}
}

func TestMouseWheelOverWrappedComposerMovesComposerCursor(t *testing.T) {
	text := "Create a book library dashboard page with cards, filters, charts, and responsive behavior."
	m := newModel(context.Background(), Options{AltScreen: true})
	m.width = 44
	m.height = 20
	m.mouseCapture = true
	m.input.SetValue(text)
	m.input.CursorEnd()
	startCursor := len([]rune(text))

	updated, cmd := m.Update(testMouseWheel(tea.MouseWheelUp, 0, 14))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("mouse wheel over composer should not return a command")
	}
	if next.chatScrollOffset != 0 {
		t.Fatalf("chatScrollOffset = %d, want unchanged", next.chatScrollOffset)
	}
	if got := next.currentComposerState().cursor; got >= startCursor {
		t.Fatalf("composer cursor = %d, want moved before end cursor %d", got, startCursor)
	}
}

func TestMouseWheelOnClippedFooterStatusDoesNotMoveComposerCursor(t *testing.T) {
	text := "Create a book library dashboard page with cards, filters, charts, and responsive behavior."
	m := newModel(context.Background(), Options{AltScreen: true})
	m.width = 44
	m.height = 3
	m.mouseCapture = true
	m.input.SetValue(text)
	m.input.CursorEnd()
	startCursor := len([]rune(text))

	updated, cmd := m.Update(testMouseWheel(tea.MouseWheelUp, 0, m.height-1))
	next := updated.(model)
	if cmd != nil {
		t.Fatal("mouse wheel on clipped footer should not return a command")
	}
	if got := next.currentComposerState().cursor; got != startCursor {
		t.Fatalf("composer cursor = %d, want unchanged end cursor %d", got, startCursor)
	}
}

func TestAltScreenTranscriptScrollKeepsFooterFixed(t *testing.T) {
	// Cwd is pinned because an empty Options.Cwd makes newModel take the real
	// working directory, which at test time is this package inside whoever's
	// checkout, and the branch assertion below is not a monotonic function of
	// that path's length. The title bar picks the widest candidate that fits:
	// workspace carries branch AND path, cwdOnly carries only the path, and
	// compactLeft carries only the branch. At width 90 with this branch, a cwd
	// of up to 52 bytes keeps branch and path, 53 to 74 falls to path-only and
	// loses the branch, and 75 or more falls again to branch-only and gets it
	// back. So this failed from a checkout 74 bytes deep and passed from one 80
	// bytes deep, while CI sits at 40 and never sees it.
	m := newModel(context.Background(), Options{
		AltScreen:    true,
		ProviderName: "openai",
		ModelName:    "gpt-4.1",
		Cwd:          filepath.Join(string(filepath.Separator), "workspace", "zero"),
	})
	m.width = 90
	m.height = 10
	m.gitBranch = "feat/pinned-header"
	for index := 0; index < 14; index++ {
		m.transcript = appendRow(m.transcript, rowAssistant, "message "+string(rune('A'+index)))
	}

	bottom := plainRender(t, m.View())
	if strings.Contains(bottom, "message A") {
		t.Fatalf("bottom view should start near recent history, got:\n%s", bottom)
	}
	if !strings.Contains(bottom, "describe a task for zero") || !strings.Contains(bottom, "openai") {
		t.Fatalf("bottom view should keep composer/status fixed, got:\n%s", bottom)
	}
	if !strings.Contains(bottom, "feat/pinned-header") || !strings.Contains(bottom, "gpt-4.1") {
		t.Fatalf("bottom view should keep title bar fixed, got:\n%s", bottom)
	}

	m = m.scrollChat(80)
	scrolled := plainRender(t, m.View())
	if !strings.Contains(scrolled, "message A") {
		t.Fatalf("scrolled view should reveal older history, got:\n%s", scrolled)
	}
	if !strings.Contains(scrolled, "describe a task for zero") || !strings.Contains(scrolled, "openai") {
		t.Fatalf("scrolled view should keep composer/status fixed, got:\n%s", scrolled)
	}
	if !strings.Contains(scrolled, "feat/pinned-header") || !strings.Contains(scrolled, "gpt-4.1") {
		t.Fatalf("scrolled view should keep title bar fixed, got:\n%s", scrolled)
	}
}

func TestAltScreenTranscriptClampsFooterToTerminalHeight(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true, ProviderName: "openai", ModelName: "gpt-4.1"})
	m.width = 80
	m.height = 3
	m.copyStatus = "Copied!"
	m.transcript = appendRow(m.transcript, rowAssistant, "hello")

	view := plainRender(t, m.View())
	if got := len(viewLines(view)); got > m.height {
		t.Fatalf("view rendered %d lines, want at most terminal height %d:\n%s", got, m.height, view)
	}
}

func TestEmptySubmitKeepsChatScrollOffset(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	m.width = 90
	m.height = 14
	for index := 0; index < 12; index++ {
		m.transcript = appendRow(m.transcript, rowAssistant, "message "+string(rune('A'+index)))
	}

	// Scroll up, then press Enter on an empty composer: the no-op submit must not
	// yank the viewport back to the bottom.
	m.chatScrollOffset = 7
	m.input.SetValue("")
	updated, _ := m.handleSubmit()
	m = updated.(model)
	if m.chatScrollOffset != 7 {
		t.Fatalf("empty submit changed chatScrollOffset to %d, want it left at 7", m.chatScrollOffset)
	}

	// A real submission (here a slash command) still snaps back to the bottom.
	m.chatScrollOffset = 7
	m.input.SetValue("/help")
	updated, _ = m.handleSubmit()
	m = updated.(model)
	if m.chatScrollOffset != 0 {
		t.Fatalf("real submit chatScrollOffset = %d, want 0", m.chatScrollOffset)
	}
}

func TestPageKeysScrollAltScreenTranscript(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	m.width = 90
	m.height = 20
	for index := 0; index < 30; index++ {
		m.transcript = appendRow(m.transcript, rowAssistant, "message "+string(rune('A'+index%26)))
	}

	updated, _ := m.Update(testKey(tea.KeyPgUp))
	m = updated.(model)
	if m.chatScrollOffset != m.chatPageScrollLines() {
		t.Fatalf("page up offset = %d, want %d", m.chatScrollOffset, m.chatPageScrollLines())
	}

	updated, _ = m.Update(testKey(tea.KeyPgDown))
	m = updated.(model)
	if m.chatScrollOffset != 0 {
		t.Fatalf("page down should return to bottom, got offset %d", m.chatScrollOffset)
	}
}

package tui

import (
	"strings"
	"testing"
	"time"
)

func TestTransientNoticeReplacesAndExpiresSafely(t *testing.T) {
	m := model{}
	m, _ = m.showTransientNotice("First", transientNoticeInfo)
	first := m.transientNoticeSeq
	m, _ = m.showTransientNotice("Second", transientNoticeSuccess)
	second := m.transientNoticeSeq
	if second <= first || m.transientNotice.text != "Second" {
		t.Fatalf("replacement = seq %d text %q, want newer Second", second, m.transientNotice.text)
	}

	next, _ := m.updateModel(transientNoticeExpiredMsg{seq: first})
	m = next.(model)
	if m.transientNotice.text != "Second" {
		t.Fatalf("stale expiry cleared latest notice: %q", m.transientNotice.text)
	}
	next, _ = m.updateModel(transientNoticeExpiredMsg{seq: second})
	m = next.(model)
	if m.transientNotice.text != "" {
		t.Fatalf("latest expiry left notice %q", m.transientNotice.text)
	}
}

func TestInlineTransientNoticeSchedulesIndependentExpiry(t *testing.T) {
	m := model{}
	m = m.showTransientNoticeInline("Voice mode on", transientNoticeSuccess)
	if m.transientNotice.text != "Voice mode on" {
		t.Fatalf("notice = %q", m.transientNotice.text)
	}
	next, cmd := m.Update(struct{}{})
	m = next.(model)
	if cmd == nil || m.transientNoticeTimerSeq != m.transientNoticeSeq {
		t.Fatal("inline notice did not schedule its independent expiry")
	}
	next, _ = m.updateModel(transientNoticeExpiredMsg{seq: m.transientNoticeSeq})
	if got := next.(model).transientNotice.text; got != "" {
		t.Fatalf("expired inline notice = %q", got)
	}
}

func TestTransientNoticeRendersAboveComposerWithoutTranscriptRow(t *testing.T) {
	m := newModel(t.Context(), Options{})
	m.width = 80
	m.altScreen = true
	before := len(m.transcript)
	m, _ = m.showTransientNotice("No background terminals running.", transientNoticeInfo)
	if len(m.transcript) != before {
		t.Fatalf("transient notice mutated transcript: %#v", m.transcript)
	}
	footer := plainRender(t, m.footerView(80))
	if !strings.Contains(footer, "No background terminals running.") {
		t.Fatalf("footer missing transient notice:\n%s", footer)
	}
	if first, _, _ := strings.Cut(footer, "\n"); strings.Contains(first, "●") {
		t.Fatalf("transient notice should be text-only, got:\n%s", footer)
	}
}

func TestBackgroundTerminalsEmptyUsesTransientNotice(t *testing.T) {
	m := modelWithFakeExecSessions(&fakeExecSessionTool{}, time.Unix(100, 0))
	next, _ := m.dispatchCommand(parseCommand("/ps"))
	got := next.(model)
	if got.transientNotice.text != "No background terminals running." {
		t.Fatalf("notice = %q", got.transientNotice.text)
	}
	if len(got.transcript) != 0 {
		t.Fatalf("empty /ps should not append transcript rows: %#v", got.transcript)
	}
}

func TestThemeSwitchUsesTransientNotice(t *testing.T) {
	defer applyTheme(themeDark, true)
	m := model{themeMode: themeSystem, hasDarkBg: true}
	next, _ := m.dispatchCommand(parseCommand("/theme dracula"))
	got := next.(model)
	if got.transientNotice.text != "Theme: Dracula" {
		t.Fatalf("notice = %q", got.transientNotice.text)
	}
	if len(got.transcript) != 0 {
		t.Fatalf("theme switch should not append transcript rows: %#v", got.transcript)
	}
}

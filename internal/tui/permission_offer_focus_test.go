package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/Twigpine/zero/internal/agent"
)

// The full-auto offer is valid only for the keypress immediately after it. A
// focus change is not that keypress, and the keypress-wide reset never runs for
// it, so the offer used to survive switching away and back: shift+tab, away,
// back, an ordinary ctrl+g, and permission prompts were off.
//
// Paste and mouse already cancel the offer for the same reason; blur was the
// omission rather than a missing rule.
func TestFocusLossCancelsTheFullAutoOffer(t *testing.T) {
	m := newModel(context.Background(), Options{PermissionMode: agent.PermissionModeAuto})
	m.width = 96

	// Auto -> Ask
	updated, _ := m.Update(testKeyShift(tea.KeyTab))
	m = updated.(model)
	// Ask -> offers full-auto
	updated, _ = m.Update(testKeyShift(tea.KeyTab))
	m = updated.(model)
	t.Logf("after two shift+tab: mode=%v armed=%v", m.permissionMode, m.unsafeArmed)
	if !m.unsafeArmed {
		t.Fatalf("precondition: the offer was not armed")
	}

	// The user switches away and back. Not a keypress, so the keypress-wide
	// reset never runs.
	updated, _ = m.Update(tea.BlurMsg{})
	m = updated.(model)
	t.Logf("after blur:            armed=%v", m.unsafeArmed)
	updated, _ = m.Update(tea.FocusMsg{})
	m = updated.(model)

	// An ordinary ctrl+g, with no fresh offer in front of it.
	updated, _ = m.Update(testKeyCtrl('g'))
	m = updated.(model)
	t.Logf("after blur/focus/ctrl+g: mode=%v", m.permissionMode)
	if m.permissionMode == agent.PermissionModeFullAuto {
		t.Errorf("full-auto entered after a focus change, with no fresh offer")
	}
}

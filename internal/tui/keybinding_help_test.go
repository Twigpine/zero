package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestQuestionMarkOpensHelpOnEmptyComposer(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	updated, _ := m.Update(testKeyText("?"))
	next := updated.(model)
	if !next.helpOverlay {
		t.Fatal("? on an empty composer should open the help overlay")
	}
	// The ? must NOT have been typed into the composer.
	if next.composerValue() != "" {
		t.Fatalf("composer should stay empty when ? opens help, got %q", next.composerValue())
	}
}

func TestQuestionMarkTypesIntoNonEmptyComposer(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	m = typeRunes(t, m, "what")
	updated, _ := m.Update(testKeyText("?"))
	next := updated.(model)
	if next.helpOverlay {
		t.Fatal("? after text should type a literal '?', not open help")
	}
	if got := next.composerValue(); got != "what?" {
		t.Fatalf("composer = %q, want %q", got, "what?")
	}
}

func TestHelpOverlayClosesOnQuestionMarkAndEsc(t *testing.T) {
	for _, closer := range []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{"question-mark", testKeyText("?")},
		{"esc", testKey(tea.KeyEsc)},
		{"q", testKeyText("q")},
		{"enter", testKey(tea.KeyEnter)},
	} {
		m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
		m.helpOverlay = true
		updated, _ := m.Update(closer.key)
		next := updated.(model)
		if next.helpOverlay {
			t.Fatalf("%s should close the help overlay", closer.name)
		}
	}
}

func TestHelpOverlaySwallowsOtherKeys(t *testing.T) {
	// While the overlay is open, an ordinary key must not type into the composer.
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	m.helpOverlay = true
	updated, _ := m.Update(testKeyText("x"))
	next := updated.(model)
	if !next.helpOverlay {
		t.Fatal("an ordinary key should not close the overlay")
	}
	if next.composerValue() != "" {
		t.Fatalf("keys must be swallowed while the overlay is open, composer = %q", next.composerValue())
	}
}

func TestHelpOverlayViewRendersGroupsAndKeys(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	m.width = 90
	m.height = 40
	m.helpOverlay = true
	view := plainRender(t, m.View())
	for _, want := range []string{
		"Keyboard Shortcuts",
		"Ctrl+T", "cycle reasoning effort",
		"Shift+Tab", "Ctrl+P", "Ctrl+O",
		"drill into its sub-session",
		"Ctrl+X then letter", "/model",
		keybindingHelpFooter,
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("help overlay view missing %q, got:\n%s", want, view)
		}
	}
}

func TestBuildKeybindingGroupsAreWellFormed(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	groups := m.buildKeybindingGroups()
	if len(groups) == 0 {
		t.Fatal("buildKeybindingGroups must not return empty")
	}
	for _, group := range groups {
		if strings.TrimSpace(group.title) == "" {
			t.Fatal("every keybinding group needs a title")
		}
		if len(group.bindings) == 0 {
			t.Fatalf("group %q has no bindings", group.title)
		}
		for _, binding := range group.bindings {
			if strings.TrimSpace(binding.keys) == "" || strings.TrimSpace(binding.desc) == "" {
				t.Fatalf("group %q has a binding with an empty key or description: %+v", group.title, binding)
			}
		}
	}
}

// #419: the `?` help overlay must render ON TOP of the chat (like the model
// picker), not REPLACE the whole screen. The old full-screen replace produced
// only the centered shortcut block on a blank canvas — no title bar, no
// composer. With the overlay open, surrounding chat chrome that is NOT covered
// by the centered box (the model title bar at top, the composer at bottom) must
// still be present alongside "Keyboard Shortcuts".
func TestHelpOverlayCompositesOverChatNotReplacingIt(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	m.width = 100
	m.height = 40
	m.altScreen = true

	base := plainRender(t, m.View()) // no overlay: baseline chrome
	m.helpOverlay = true
	over := plainRender(t, m.View())

	if !strings.Contains(over, "Keyboard Shortcuts") {
		t.Fatalf("help overlay not rendered:\n%s", over)
	}
	// Chrome that renders in the baseline (and sits outside the centered overlay
	// box) must survive behind the overlay. The full-screen replace showed none.
	for _, marker := range []string{"gpt-4o", "describe a task"} {
		if !strings.Contains(base, marker) {
			t.Fatalf("precondition: baseline chat should contain %q:\n%s", marker, base)
		}
		if !strings.Contains(over, marker) {
			t.Fatalf("#419: help replaced the chat instead of overlaying it; %q is gone:\n%s", marker, over)
		}
	}
}

// A populated transcript row also survives behind the overlay (peeking out to
// the left of the centered box), proving the chat body — not just the chrome —
// is composited under the overlay rather than discarded.
func TestHelpOverlayKeepsTranscriptBodyBehindIt(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	m.width = 120
	m.height = 40
	m.altScreen = true
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: "hello there this is a chat line"})
	m.helpOverlay = true

	view := plainRender(t, m.View())
	if !strings.Contains(view, "Keyboard Shortcuts") {
		t.Fatalf("help overlay not rendered:\n%s", view)
	}
	// The start of the transcript line peeks to the left of the centered box.
	if !strings.Contains(view, "hello") {
		t.Fatalf("#419: transcript body was replaced by the help overlay:\n%s", view)
	}
}

func TestCtrlBCtrlECursorNavigationBypass(t *testing.T) {
	// 1. Empty composer: Ctrl+B should toggle run details, Ctrl+E should toggle mouseReleased.
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	m.altScreen = true
	m.width = 120
	m.height = 40
	m.transcript = append(m.transcript, transcriptRow{kind: rowToolCall, tool: "read_file", detail: "main.go"})
	if !m.runDetailsAllowed() {
		t.Fatal("run details should be allowed")
	}

	initialDetails := m.runDetailsOpen
	updated, _ := m.Update(testKeyCtrl('b'))
	next := updated.(model)
	if next.runDetailsOpen == initialDetails {
		t.Fatal("Ctrl+B on empty composer should toggle run details")
	}
	updated, _ = next.Update(testKeyCtrl('b'))
	next = updated.(model)

	initialMouse := next.mouseReleased
	updated, _ = next.Update(testKeyCtrl('e'))
	next = updated.(model)
	if next.mouseReleased == initialMouse {
		t.Fatal("Ctrl+E on empty composer should toggle mouseReleased")
	}

	// 2. Non-empty composer: Ctrl+B and Ctrl+E should not toggle state, but act as cursor movement
	m2 := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	m2.altScreen = true
	m2.width = 120
	m2.height = 40
	m2.transcript = append(m2.transcript, transcriptRow{kind: rowToolCall, tool: "read_file", detail: "main.go"})

	m2 = typeRunes(t, m2, "hello")
	if m2.composerValue() != "hello" {
		t.Fatalf("composerValue = %q, want 'hello'", m2.composerValue())
	}

	initialDetails2 := m2.runDetailsOpen
	updated, _ = m2.Update(testKeyCtrl('b'))
	next2 := updated.(model)
	if next2.runDetailsOpen != initialDetails2 {
		t.Fatal("Ctrl+B on non-empty composer should NOT toggle run details")
	}

	initialMouse2 := next2.mouseReleased
	updated, _ = next2.Update(testKeyCtrl('e'))
	next2 = updated.(model)
	if next2.mouseReleased != initialMouse2 {
		t.Fatal("Ctrl+E on non-empty composer should NOT toggle mouseReleased")
	}
}

// TestRemappedToggleBindingsIgnoreComposerGuard guards against the
// composer-empty bypass over-applying to a user-remapped, non-conflicting
// binding: it exists so the default Ctrl+E/Ctrl+B chords (which readline
// navigation also claims while typing) don't hijack keystrokes mid-sentence,
// not to block every toggleMouse/toggleSidebar binding while composing.
func TestRemappedToggleBindingsIgnoreComposerGuard(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	m.altScreen = true
	m.width = 120
	m.height = 40
	m.transcript = append(m.transcript, transcriptRow{kind: rowToolCall, tool: "read_file", detail: "main.go"})
	m.keyBindings.toggleMouse = parseBinding("ctrl+m")
	// Avoid Ctrl+N: idle Ctrl+N is reserved as an emacs menu no-op and never
	// reaches configurable global bindings.
	m.keyBindings.toggleSidebar = parseBinding("ctrl+y")
	if !m.runDetailsAllowed() {
		t.Fatal("run details should be allowed")
	}

	m = typeRunes(t, m, "hello")
	if m.composerValue() != "hello" {
		t.Fatalf("composerValue = %q, want 'hello'", m.composerValue())
	}

	initialMouse := m.mouseReleased
	updated, _ := m.Update(testKeyCtrl('m'))
	next := updated.(model)
	if next.mouseReleased == initialMouse {
		t.Fatal("remapped Ctrl+M toggleMouse binding should still fire with a non-empty composer")
	}
	if next.composerValue() != "hello" {
		t.Fatalf("remapped toggleMouse binding should not fall through to composer input, got %q", next.composerValue())
	}

	initialDetails := next.runDetailsOpen
	updated, _ = next.Update(testKeyCtrl('y'))
	next = updated.(model)
	if next.runDetailsOpen == initialDetails {
		t.Fatal("remapped Ctrl+Y toggleSidebar binding should still open run details with a non-empty composer")
	}
	if next.composerValue() != "hello" {
		t.Fatalf("remapped toggleSidebar binding should not fall through to composer input, got %q", next.composerValue())
	}
}

// TestExplicitDefaultChordConfigStillRequiresEmptyComposer guards against
// treating "explicitly configured" the same as "genuinely remapped": a user
// who writes toggleMouse: "ctrl+e" / toggleSidebar: "ctrl+b" in config (the
// same chord as the built-in default) gets a non-zero parsedBinding, not the
// isZero() sentinel, so a check that only asked "is this unset" would wrongly
// let Ctrl+E/Ctrl+B bypass the composer guard again instead of reaching the
// readline cursor-navigation handlers.
func TestExplicitDefaultChordConfigStillRequiresEmptyComposer(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	m.altScreen = true
	m.width = 120
	m.height = 40
	m.transcript = append(m.transcript, transcriptRow{kind: rowToolCall, tool: "read_file", detail: "main.go"})
	m.keyBindings.toggleMouse = parseBinding("ctrl+e")
	m.keyBindings.toggleSidebar = parseBinding("ctrl+b")
	if !m.runDetailsAllowed() {
		t.Fatal("run details should be allowed")
	}

	m = typeRunes(t, m, "hello")
	if m.composerValue() != "hello" {
		t.Fatalf("composerValue = %q, want 'hello'", m.composerValue())
	}

	initialMouse := m.mouseReleased
	updated, _ := m.Update(testKeyCtrl('e'))
	next := updated.(model)
	if next.mouseReleased != initialMouse {
		t.Fatal("explicit ctrl+e config should NOT toggle mouseReleased with a non-empty composer")
	}

	initialDetails := next.runDetailsOpen
	updated, _ = next.Update(testKeyCtrl('b'))
	next = updated.(model)
	if next.runDetailsOpen != initialDetails {
		t.Fatal("explicit ctrl+b config should NOT toggle run details with a non-empty composer")
	}
}

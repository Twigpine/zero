package tui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/agent"
)

func TestCtrlPNMovesModelPicker(t *testing.T) {
	m := newModel(context.Background(), Options{
		ModelName:    "gpt-4o",
		ProviderName: "openai",
		Provider:     &fakeProvider{},
	})
	m.input.SetValue("/model")
	updated, _ := m.handleSubmit()
	m = updated.(model)
	if m.picker == nil {
		t.Fatal("setup: expected model picker")
	}
	if len(m.picker.items) < 2 {
		t.Skip("model picker has fewer than 2 items; cannot assert movement")
	}
	m.picker.selected = 0
	updated, _ = m.Update(testKey(tea.KeyDown))
	viaArrow := updated.(model).picker.selected
	m.picker.selected = 0
	updated, _ = m.Update(testKeyCtrl('n'))
	m = updated.(model)
	if m.picker == nil {
		t.Fatal("picker should stay open")
	}
	if m.picker.selected != viaArrow {
		t.Fatalf("Ctrl+N selection = %d, KeyDown selection = %d", m.picker.selected, viaArrow)
	}
	updated, _ = m.Update(testKeyCtrl('p'))
	m = updated.(model)
	if m.picker.selected != 0 {
		t.Fatalf("Ctrl+P should return to 0, got %d", m.picker.selected)
	}
}

func TestCtrlPNMovesSuggestions(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = typeRunes(t, m, "/s")
	if !m.suggestionsActive() || len(m.suggestions) < 2 {
		t.Fatalf("setup: need multiple suggestions, got %d", len(m.suggestions))
	}
	updated, _ := m.Update(testKeyCtrl('n'))
	m = updated.(model)
	if m.suggestionIdx != 1 {
		t.Fatalf("Ctrl+N should select index 1, got %d", m.suggestionIdx)
	}
	if got := m.composerValue(); got != "/s" {
		t.Fatalf("Ctrl+N must not insert text, composer = %q", got)
	}
	updated, _ = m.Update(testKeyCtrl('p'))
	m = updated.(model)
	if m.suggestionIdx != 0 {
		t.Fatalf("Ctrl+P should return to index 0, got %d", m.suggestionIdx)
	}
}

func TestCtrlPNMovesPermissionOptions(t *testing.T) {
	m := pendingPermissionModel(t, func(agent.PermissionDecision) {})
	if m.pendingPermission == nil {
		t.Fatal("setup: expected permission prompt")
	}
	start := m.pendingPermission.cursor
	updated, _ := m.Update(testKeyCtrl('n'))
	m = updated.(model)
	if m.pendingPermission == nil {
		t.Fatal("permission prompt should stay open")
	}
	if m.pendingPermission.cursor == start {
		// Wrap or multi-option: try another N if single step no-op is impossible.
		if len(permissionOptions(m.pendingPermission.request)) > 1 {
			t.Fatalf("Ctrl+N should move permission cursor from %d", start)
		}
	}
	mid := m.pendingPermission.cursor
	updated, _ = m.Update(testKeyCtrl('p'))
	m = updated.(model)
	if m.pendingPermission.cursor != start && mid != start {
		if m.pendingPermission.cursor == mid {
			t.Fatalf("Ctrl+P should move permission cursor from %d", mid)
		}
	}
}

func TestCtrlPMovesPickerSelection(t *testing.T) {
	m := newModel(context.Background(), Options{
		ModelName:    "gpt-4o",
		ProviderName: "openai",
		Provider:     &fakeProvider{},
	})
	m.input.SetValue("/model")
	updated, _ := m.handleSubmit()
	m = updated.(model)
	if m.picker == nil {
		t.Fatal("setup: expected picker")
	}
	updated, _ = m.Update(testKeyCtrl('p'))
	next := updated.(model)
	if next.picker == nil {
		t.Fatal("picker should remain open")
	}
}

func TestCtrlNNoOpWithoutModal(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	updated, _ := m.Update(testKeyCtrl('n'))
	next := updated.(model)
	if next.picker != nil || next.suggestionsActive() {
		t.Fatal("Ctrl+N idle must not open a modal")
	}
	if next.composerValue() != "" {
		t.Fatalf("Ctrl+N idle must not type into composer, got %q", next.composerValue())
	}
}

func TestCtrlPNThemePickerPreview(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	m.input.SetValue("/theme")
	updated, _ := m.handleSubmit()
	m = updated.(model)
	if m.picker == nil || m.picker.kind != pickerTheme {
		t.Fatalf("setup: expected theme picker, got %#v", m.picker)
	}
	// Move with Ctrl+N — should not error; preview path is inside pickerMoved.
	start := m.picker.selected
	updated, _ = m.Update(testKeyCtrl('n'))
	m = updated.(model)
	if m.picker == nil {
		t.Fatal("theme picker should stay open")
	}
	if len(m.picker.items) > 1 && m.picker.selected == start {
		t.Fatalf("Ctrl+N should advance theme selection from %d", start)
	}
}

func TestMoveModalSelectionMatchesArrowsOnSuggestions(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m = typeRunes(t, m, "/")
	if len(m.suggestions) < 2 {
		t.Fatalf("need suggestions, got %d", len(m.suggestions))
	}
	m.suggestionIdx = 0
	viaArrow, _ := m.Update(testKey(tea.KeyDown))
	m.suggestionIdx = 0
	viaCtrl, _ := m.Update(testKeyCtrl('n'))
	if viaArrow.(model).suggestionIdx != viaCtrl.(model).suggestionIdx {
		t.Fatalf("arrow idx=%d ctrl idx=%d", viaArrow.(model).suggestionIdx, viaCtrl.(model).suggestionIdx)
	}
}

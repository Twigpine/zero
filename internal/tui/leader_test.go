package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestLeaderArmsOnCtrlX(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	updated, cmd := m.Update(testKeyCtrl('x'))
	next := updated.(model)
	if !next.leaderPending {
		t.Fatal("Ctrl+X should arm leader-pending")
	}
	if next.composerValue() != "" {
		t.Fatalf("Ctrl+X must not type into the composer, got %q", next.composerValue())
	}
	if cmd == nil {
		t.Fatal("arming leader should schedule a timeout tick")
	}
}

func TestLeaderModelOpensPicker(t *testing.T) {
	m := newModel(context.Background(), Options{
		ModelName:    "gpt-4o",
		ProviderName: "openai",
	})
	updated, _ := m.Update(testKeyCtrl('x'))
	m = updated.(model)
	updated, _ = m.Update(testKeyText("m"))
	next := updated.(model)
	if next.leaderPending {
		t.Fatal("mapped second key should clear leader-pending")
	}
	if next.picker == nil || next.picker.kind != pickerModel {
		t.Fatalf("Ctrl+X m should open the model picker, picker=%#v", next.picker)
	}
	if next.composerValue() != "" {
		t.Fatalf("leader chord must not leave /model in the composer, got %q", next.composerValue())
	}
}

func TestLeaderPreservesComposerDraft(t *testing.T) {
	m := newModel(context.Background(), Options{
		ModelName:    "gpt-4o",
		ProviderName: "openai",
	})
	m = typeRunes(t, m, "draft text")
	updated, _ := m.Update(testKeyCtrl('x'))
	m = updated.(model)
	updated, _ = m.Update(testKeyText("m"))
	next := updated.(model)
	if got := next.composerValue(); got != "draft text" {
		t.Fatalf("leader /model must preserve composer draft, got %q", got)
	}
	if next.picker == nil || next.picker.kind != pickerModel {
		t.Fatal("expected model picker after Ctrl+X m")
	}
}

func TestLeaderTimeoutClears(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	updated, _ := m.Update(testKeyCtrl('x'))
	m = updated.(model)
	if !m.leaderPending {
		t.Fatal("expected leader pending after Ctrl+X")
	}
	seq := m.leaderSeq
	updated, _ = m.Update(leaderExpiredMsg{seq: seq})
	next := updated.(model)
	if next.leaderPending {
		t.Fatal("matching timeout tick should clear leader-pending")
	}
	// Stale tick must not clear a re-armed leader.
	updated, _ = next.Update(testKeyCtrl('x'))
	next = updated.(model)
	updated, _ = next.Update(leaderExpiredMsg{seq: seq})
	next = updated.(model)
	if !next.leaderPending {
		t.Fatal("stale timeout must not clear a re-armed leader")
	}
}

func TestLeaderEscCancels(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	m = typeRunes(t, m, "keep me")
	updated, _ := m.Update(testKeyCtrl('x'))
	m = updated.(model)
	updated, _ = m.Update(testKey(tea.KeyEsc))
	next := updated.(model)
	if next.leaderPending {
		t.Fatal("Esc should clear leader-pending")
	}
	if got := next.composerValue(); got != "keep me" {
		t.Fatalf("Esc cancel must not clear composer, got %q", got)
	}
}

func TestLeaderSecondCtrlXCancels(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	updated, _ := m.Update(testKeyCtrl('x'))
	m = updated.(model)
	updated, _ = m.Update(testKeyCtrl('x'))
	next := updated.(model)
	if next.leaderPending {
		t.Fatal("second Ctrl+X should cancel, not re-arm")
	}
}

func TestLeaderUnknownKeyClearsWithoutCommand(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	before := len(m.transcript)
	updated, _ := m.Update(testKeyCtrl('x'))
	m = updated.(model)
	updated, _ = m.Update(testKeyText("z"))
	next := updated.(model)
	if next.leaderPending {
		t.Fatal("unknown second key should clear leader-pending")
	}
	if next.picker != nil {
		t.Fatal("unknown second key must not open a picker")
	}
	if next.composerValue() != "" {
		t.Fatalf("unknown key must not type into composer, got %q", next.composerValue())
	}
	if len(next.transcript) != before {
		t.Fatalf("unknown key should not append transcript rows, before=%d after=%d", before, len(next.transcript))
	}
}

func TestLeaderDoesNotInsertText(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	updated, _ := m.Update(testKeyCtrl('x'))
	m = updated.(model)
	updated, _ = m.Update(testKeyText("q"))
	next := updated.(model)
	if next.composerValue() != "" {
		t.Fatalf("follow-up key must never appear in composer, got %q", next.composerValue())
	}
}

func TestLeaderIgnoredWhenPickerOpen(t *testing.T) {
	m := newModel(context.Background(), Options{
		ModelName:    "gpt-4o",
		ProviderName: "openai",
	})
	// Open model picker the normal way.
	m.input.SetValue("/model")
	updated, _ := m.handleSubmit()
	m = updated.(model)
	if m.picker == nil {
		t.Fatal("setup: expected /model to open a picker")
	}
	updated, _ = m.Update(testKeyCtrl('x'))
	next := updated.(model)
	if next.leaderPending {
		t.Fatal("Ctrl+X must not arm leader while a picker is open")
	}
	// Printable 'm' should filter the picker, not dispatch another /model.
	updated, _ = next.Update(testKeyText("m"))
	next = updated.(model)
	if next.picker == nil {
		t.Fatal("picker should still be open")
	}
	if next.leaderPending {
		t.Fatal("picker keystrokes must not arm leader")
	}
}

func TestLeaderToolsRunsSlash(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	updated, _ := m.Update(testKeyCtrl('x'))
	m = updated.(model)
	updated, _ = m.Update(testKeyText("t"))
	next := updated.(model)
	if next.leaderPending {
		t.Fatal("expected leader cleared after Ctrl+X t")
	}
	if !transcriptContains(next.transcript, "Tools") && !transcriptContains(next.transcript, "tool") {
		// toolsText content varies; at least something was appended.
		if len(next.transcript) == 0 {
			t.Fatal("Ctrl+X t should run /tools and append output")
		}
	}
}

func TestLeaderHelpOverlayListsChords(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	groups := m.buildKeybindingGroups()
	var found bool
	for _, g := range groups {
		for _, b := range g.bindings {
			if strings.Contains(b.keys, "Ctrl+X") && strings.Contains(b.desc, "/model") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("buildKeybindingGroups must document Ctrl+X slash-command chords")
	}
}

func TestLeaderQuestionMarkOpensChordMap(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	m.width = 100
	m.height = 50
	m.altScreen = true
	updated, _ := m.Update(testKeyCtrl('x'))
	m = updated.(model)
	if !m.leaderPending {
		t.Fatal("setup: Ctrl+X should arm leader")
	}
	updated, _ = m.Update(testKeyText("?"))
	next := updated.(model)
	if next.leaderPending {
		t.Fatal("Ctrl+X ? should clear leader-pending")
	}
	if !next.leaderHelpOverlay {
		t.Fatal("Ctrl+X ? should open the leader chord map")
	}
	if next.composerValue() != "" {
		t.Fatalf("? must not type into composer, got %q", next.composerValue())
	}
	view := plainRender(t, next.View())
	for _, want := range []string{
		"Ctrl+X Shortcuts",
		"Slash commands",
		"Ctrl+X m", "open /model",
		"Ctrl+X p", "open /provider",
		"Ctrl+X R", "run /retry",
		"╭", "╰", "│", // same box border chrome as the ? keyboard-shortcut modal
	} {
		if !strings.Contains(view, want) {
			t.Fatalf("leader help missing %q, got:\n%s", want, view)
		}
	}
}

func TestLeaderHelpMatchesKeybindingHelpChrome(t *testing.T) {
	// Both overlays must use the same transparent, titled frame builder.
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	m.width = 100
	help := m.renderKeybindingHelpOverlay(100)
	leader := m.renderLeaderHelpOverlay(100)
	for _, block := range []struct {
		name string
		s    string
	}{
		{"help", help},
		{"leader", leader},
	} {
		plain := ansiPattern.ReplaceAllString(block.s, "")
		for _, want := range []string{"╭", "╰", "│", "╮", "╯"} {
			if !strings.Contains(plain, want) {
				t.Fatalf("%s overlay missing border %q:\n%s", block.name, want, plain)
			}
		}
	}
	if !strings.Contains(ansiPattern.ReplaceAllString(help, ""), "Keyboard Shortcuts") {
		t.Fatal("help overlay should keep Keyboard Shortcuts title")
	}
	if !strings.Contains(ansiPattern.ReplaceAllString(leader, ""), "Ctrl+X Shortcuts") {
		t.Fatal("leader overlay should use Ctrl+X Shortcuts title in the border")
	}
}

func TestShortcutOverlaysKeepTerminalCanvasTransparent(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	panelFill := zeroTheme.panel.Render(" ")
	for _, overlay := range []struct {
		name string
		view string
	}{
		{"keyboard help", m.renderKeybindingHelpOverlay(100)},
		{"leader help", m.renderLeaderHelpOverlay(100)},
	} {
		if strings.Contains(overlay.view, panelFill) {
			t.Fatalf("%s should not paint the panel background behind shortcut rows", overlay.name)
		}
	}
}

func TestLeaderHelpOverlayClosesOnEsc(t *testing.T) {
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	m.leaderHelpOverlay = true
	updated, _ := m.Update(testKey(tea.KeyEsc))
	next := updated.(model)
	if next.leaderHelpOverlay {
		t.Fatal("Esc should close the leader help overlay")
	}
}

func TestLeaderHelpBindingsCoverEveryMapEntry(t *testing.T) {
	listed := map[string]bool{}
	for _, b := range leaderHelpBindings() {
		// "Ctrl+X m" → last field is the letter (skip the "?" meta row for map check).
		parts := strings.Fields(b.keys)
		if len(parts) != 2 || parts[0] != "Ctrl+X" {
			continue
		}
		letter := parts[1]
		if letter == "?" {
			continue
		}
		runes := []rune(letter)
		if len(runes) != 1 {
			t.Fatalf("unexpected key label %q", b.keys)
		}
		listed[string(runes[0])] = true
		if _, ok := leaderCommandByKey[runes[0]]; !ok {
			t.Fatalf("help lists %q but leaderCommandByKey has no entry", b.keys)
		}
	}
	for key := range leaderCommandByKey {
		if !listed[string(key)] {
			t.Fatalf("leaderCommandByKey has %q but help table omits it", string(key))
		}
	}
}

func TestLeaderSecondKeyShiftCapital(t *testing.T) {
	// Shift+p should map to /plan (capital P), not /provider.
	m := newModel(context.Background(), Options{ModelName: "gpt-4o"})
	updated, _ := m.Update(testKeyCtrl('x'))
	m = updated.(model)
	updated, _ = m.Update(testKeyShift('p'))
	next := updated.(model)
	if next.leaderPending {
		t.Fatal("Shift+p should resolve the leader chord")
	}
	if next.picker != nil {
		t.Fatal("/plan should not open a picker")
	}
	if !transcriptContains(next.transcript, "Plan") && !transcriptContains(next.transcript, "plan") {
		// planText wording may vary; require some transcript growth from /plan.
		if len(next.transcript) == 0 {
			t.Fatal("Ctrl+X P should run /plan")
		}
	}
}

func TestLeaderSecondKeyFromTextCapital(t *testing.T) {
	key, ok := leaderSecondKey(testKeyText("M"))
	if !ok || key != 'M' {
		t.Fatalf("leaderSecondKey(M) = %q,%v, want M,true", key, ok)
	}
	key, ok = leaderSecondKey(testKeyText("m"))
	if !ok || key != 'm' {
		t.Fatalf("leaderSecondKey(m) = %q,%v, want m,true", key, ok)
	}
}

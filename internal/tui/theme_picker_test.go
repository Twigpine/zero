package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// A committed theme is written to user config and reloaded at startup (via
// Options.SavedTheme -> resolveThemeMode), so a /theme choice survives restart.
func TestThemeChoicePersistsAcrossRestart(t *testing.T) {
	defer applyTheme(themeDark, true)
	cfgPath := filepath.Join(t.TempDir(), "config.json")

	m := newModel(context.Background(), Options{UserConfigPath: cfgPath})
	m, _ = m.handleThemeCommand("dracula")
	if m.themeMode != themeMode("dracula") {
		t.Fatalf("themeMode = %q, want dracula", m.themeMode)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("theme commit should have written config: %v", err)
	}
	var cfg struct {
		Preferences struct {
			Theme string `json:"theme"`
		} `json:"preferences"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	if cfg.Preferences.Theme != "dracula" {
		t.Fatalf("preferences.theme = %q, want dracula", cfg.Preferences.Theme)
	}

	restarted := newModel(context.Background(), Options{UserConfigPath: cfgPath, SavedTheme: "dracula"})
	if restarted.themeMode != themeMode("dracula") {
		t.Fatalf("restarted themeMode = %q, want dracula (from saved config)", restarted.themeMode)
	}
}

func TestResolveThemeModeConfigFallback(t *testing.T) {
	if got := resolveThemeMode("", "", "nord"); got != themeMode("nord") {
		t.Errorf("saved-only = %q, want nord", got)
	}
	if got := resolveThemeMode("dracula", "", "nord"); got != themeMode("dracula") {
		t.Errorf("flag should beat saved: got %q, want dracula", got)
	}
	if got := resolveThemeMode("", "gruvbox", "nord"); got != themeMode("gruvbox") {
		t.Errorf("env should beat saved: got %q, want gruvbox", got)
	}
	if got := resolveThemeMode("", "", "bogus-theme"); got != themeSystem {
		t.Errorf("unknown saved theme should fall back to system, got %q", got)
	}
	if got := resolveThemeMode("auto"); got != themeSystem {
		t.Errorf("legacy auto = %q, want system", got)
	}
}

func themePickerRowIndex(t *testing.T, p *commandPicker, name string) int {
	t.Helper()
	for i, item := range p.items {
		if item.Value == name {
			return i
		}
	}
	t.Fatalf("theme picker has no row for %q", name)
	return -1
}

// Bare `/theme` opens the popup picker with one row per presented theme mode,
// preselecting the committed choice. An explicit `/theme <mode>` keeps taking the
// text path so scripts remain predictable.
func TestThemePickerOpensOnBareTheme(t *testing.T) {
	defer applyTheme(themeDark, true)
	m := newModel(context.Background(), Options{})
	m.themeMode = themeMode("dracula")
	m.input.SetValue("/theme")

	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if cmd != nil {
		t.Fatalf("opening the theme picker should not emit a cmd, got %T", cmd)
	}
	if m.picker == nil || m.picker.kind != pickerTheme {
		t.Fatalf("expected the theme picker to open, got %#v", m.picker)
	}
	if len(m.picker.items) != len(themeModes) {
		t.Fatalf("picker has %d items, want %d", len(m.picker.items), len(themeModes))
	}
	if got := m.picker.items[0].Value; got != string(themeSystem) {
		t.Errorf("first theme = %q, want system", got)
	}
	if got := m.picker.items[m.picker.selected].Value; got != "dracula" {
		t.Errorf("preselected value = %q, want the active mode dracula", got)
	}
	for _, retired := range []string{string(themeDark), string(themeLight)} {
		for _, item := range m.picker.items {
			if item.Value == retired {
				t.Errorf("retired %q must not appear in the theme picker", retired)
			}
		}
	}
}

func TestThemeArgSkipsPicker(t *testing.T) {
	defer applyTheme(themeDark, true)
	m := newModel(context.Background(), Options{})
	m.input.SetValue("/theme dracula")

	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.picker != nil {
		t.Fatalf("explicit /theme dracula must not open a picker, got %#v", m.picker)
	}
	if m.themeMode != themeMode("dracula") {
		t.Fatalf("after /theme dracula, mode = %q", m.themeMode)
	}
}

// Moving through the picker changes only the small candidate preview. The active
// palette and committed preference stay exactly as they were until Enter.
func TestThemePickerPreviewIsContained(t *testing.T) {
	defer applyTheme(themeDark, true)
	m := newModel(context.Background(), Options{})
	m.themeMode = themeMode("dracula")
	applyTheme(m.themeMode, true)
	before, _, _, _ := zeroTheme.inkColor.RGBA()
	m.input.SetValue("/theme")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)

	for i := 0; i < 3; i++ {
		updated, _ = m.Update(testKey(tea.KeyDown))
		m = updated.(model)
	}
	after, _, _, _ := zeroTheme.inkColor.RGBA()
	if after != before {
		t.Fatal("moving through the theme picker changed the active palette")
	}
	if m.themeMode != themeMode("dracula") {
		t.Errorf("preview mutated the committed mode to %q", m.themeMode)
	}
	view := m.pickerOverlay(100)
	for _, want := range []string{"Preview", "no bugs, probably"} {
		if !strings.Contains(view, want) {
			t.Errorf("contained preview missing %q:\n%s", want, view)
		}
	}
}

func TestThemePickerCommitAppliesAndRecords(t *testing.T) {
	defer applyTheme(themeDark, true)
	m := newModel(context.Background(), Options{})
	m.themeMode = themeMode("dracula")
	applyTheme(m.themeMode, true)
	m.input.SetValue("/theme")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)

	m.picker.selected = themePickerRowIndex(t, m.picker, "dracula")
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if cmd == nil {
		t.Fatal("committing a fixed theme should schedule transient-notice expiry")
	}
	if m.picker != nil {
		t.Fatal("committing should close the picker")
	}
	if m.themeMode != themeMode("dracula") {
		t.Fatalf("committed mode = %q, want dracula", m.themeMode)
	}
	if got, want := colorHex(t, zeroTheme.inkColor), draculaPalette.ink; got != want {
		t.Errorf("committed dracula ink = %s, want %s", got, want)
	}
	if m.transientNotice.text != "Theme: Dracula" {
		t.Errorf("commit notice = %q, want Theme: Dracula", m.transientNotice.text)
	}
	if transcriptContains(m.transcript, "active theme") {
		t.Errorf("commit should not add a durable transcript result:\n%s", transcriptText(m.transcript))
	}
}

func TestThemePickerSystemCommitPreservesTerminalCanvas(t *testing.T) {
	defer applyTheme(themeDark, true)
	m := newModel(context.Background(), Options{})
	m.themeMode = themeMode("dracula")
	applyTheme(m.themeMode, true)
	m.input.SetValue("/theme")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	m.picker.selected = themePickerRowIndex(t, m.picker, string(themeSystem))
	updated, _ = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.themeMode != themeSystem {
		t.Fatalf("committed mode = %q, want system", m.themeMode)
	}
	if _, ok := zeroTheme.bgPanel.(lipgloss.NoColor); !ok {
		t.Fatalf("system theme background = %T, want lipgloss.NoColor", zeroTheme.bgPanel)
	}
}

// Themes are local styling choices. Even a named palette must not use Bubble
// Tea's terminal-wide colour controls, which would replace the user's terminal
// background, transparency, or wallpaper.
func TestThemeViewLeavesTerminalCanvasUntouched(t *testing.T) {
	defer applyTheme(themeDark, true)
	for _, mode := range []themeMode{themeSystem, themeMode("dracula"), themeMode("dune")} {
		t.Run(string(mode), func(t *testing.T) {
			m := newModel(context.Background(), Options{SavedTheme: string(mode)})
			m.altScreen = true
			view := m.View()
			if view.BackgroundColor != nil {
				t.Fatalf("%s theme set terminal background to %T", mode, view.BackgroundColor)
			}
			if view.ForegroundColor != nil {
				t.Fatalf("%s theme set terminal foreground to %T", mode, view.ForegroundColor)
			}
		})
	}
}

func TestThemePickerFilterAndCancelKeepActiveTheme(t *testing.T) {
	defer applyTheme(themeDark, true)
	m := newModel(context.Background(), Options{})
	m.themeMode = themeMode("dracula")
	applyTheme(m.themeMode, true)
	before, _, _, _ := zeroTheme.inkColor.RGBA()
	m.input.SetValue("/theme")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)

	updated, _ = m.Update(testKeyText("d"))
	m = updated.(model)
	if len(m.picker.items) == 0 || m.picker.items[m.picker.selected].Value != "dracula" {
		t.Fatalf("filter should select dracula, got %#v", m.picker.items)
	}
	updated, _ = m.Update(testKey(tea.KeyEsc))
	m = updated.(model)
	if m.picker != nil || m.themeMode != themeMode("dracula") {
		t.Fatalf("Esc changed picker or committed mode: picker=%#v mode=%q", m.picker, m.themeMode)
	}
	after, _, _, _ := zeroTheme.inkColor.RGBA()
	if after != before {
		t.Fatal("filtering or cancelling changed the active palette")
	}
}

package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/notify"
)

// A committed /notify choice is written to user config and reloaded at startup,
// so a /notify choice survives restart, just like /theme.
func TestNotifyChoicePersistsAcrossRestart(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")

	// First session: pick a notify pair via the text handler (the same commit
	// path the picker uses via choosePicker).
	m := newModel(context.Background(), Options{UserConfigPath: cfgPath})
	m, out := m.handleNotifyCommand("off always")
	if m.notifyMode != "off" {
		t.Fatalf("notifyMode = %q, want off", m.notifyMode)
	}
	if !strings.Contains(out, "Notify") {
		t.Fatalf("output should announce the change, got: %s", out)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("notify commit should have written config: %v", err)
	}
	var cfg struct {
		Notify config.NotifyConfig `json:"notify"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}
	if cfg.Notify.Mode != "off" || cfg.Notify.FocusMode != "always" {
		t.Fatalf("notify = %+v, want mode=off focusMode=always", cfg.Notify)
	}

	// Second session: the persisted notify block seeds startup (options.Notify
	// is populated by the resolver from the same file).
	restarted := newModel(context.Background(), Options{UserConfigPath: cfgPath, Notify: config.NotifyConfig{Mode: "off", FocusMode: "always"}})
	if restarted.notifyMode != "off" || restarted.notifyFocusMode != "always" {
		t.Fatalf("restarted = mode %q focus %q, want off/always (from saved config)", restarted.notifyMode, restarted.notifyFocusMode)
	}
}

// Mode-only `/notify <mode>` keeps the LIVE and PERSISTED policies separate
// (maintainer review, PR #1001): the live session preserves its in-session
// focus (initialized from the resolved pair, so a project's focus rule keeps
// applying), while the global write seeds from the USER'S OWN file — blank
// stays blank, and a project-derived value is never copied into the global
// config. The three states here deliberately disagree:
//
//	user file:   focusMode=focused, no mode
//	resolved:    both/unfocused (as a project override would produce)
//	live start:  both/unfocused
func TestNotifyModeOnlySplitsLiveAndPersistedFocus(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"notify":{"focusMode":"focused"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), Options{
		UserConfigPath: cfgPath,
		Notify:         config.NotifyConfig{Mode: "both", FocusMode: "unfocused"},
	})

	m, _ = m.handleNotifyCommand("bell")

	// LIVE: mode changes, focus keeps the in-session (resolved) value — the
	// user-file focus must NOT bleed into the session.
	if m.notifyMode != "bell" {
		t.Errorf("live mode = %q, want bell", m.notifyMode)
	}
	if m.notifyFocusMode != "unfocused" {
		t.Errorf("live focus = %q, want the in-session unfocused (not the user-file focused)", m.notifyFocusMode)
	}
	// PERSISTED: the mode is written; the focus seeded from the user's file
	// (their explicit focused), never the resolved/live value.
	persisted := readNotifyBlock(t, cfgPath)
	if persisted.Mode != "bell" {
		t.Errorf("persisted mode = %q, want bell", persisted.Mode)
	}
	if persisted.FocusMode != "focused" {
		t.Errorf("persisted focusMode = %q, want the user-file value focused", persisted.FocusMode)
	}

	// Blank stays blank: with nothing stored, a mode-only change must not pin
	// the default focus as an explicit choice, and live still keeps its value.
	blankPath := filepath.Join(t.TempDir(), "config.json")
	m = newModel(context.Background(), Options{
		UserConfigPath: blankPath,
		Notify:         config.NotifyConfig{Mode: "off", FocusMode: "focused"},
	})
	m, _ = m.handleNotifyCommand("off")
	if m.notifyFocusMode != "focused" {
		t.Errorf("live focus = %q, want the in-session focused preserved", m.notifyFocusMode)
	}
	persisted = readNotifyBlock(t, blankPath)
	if persisted.Mode != "off" {
		t.Errorf("persisted mode = %q, want off", persisted.Mode)
	}
	if persisted.FocusMode != "" {
		t.Errorf("persisted focusMode = %q, want blank (unspecified stays unspecified)", persisted.FocusMode)
	}
}

// The full contract matrix when the three notify states disagree (blank/different
// user file + project-resolved pair), per the maintainer review: display and
// preselection report the LIVE policy; partial updates change live and persist
// independently; restart precedence never copies project values into the
// global file.
func TestNotifyLivePolicyContractMatrix(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	// User file: nothing stored at all.
	if err := os.WriteFile(cfgPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Project-resolved pair: off/focused (a project override shape).
	m := newModel(context.Background(), Options{
		UserConfigPath: cfgPath,
		Notify:         config.NotifyConfig{Mode: "off", FocusMode: "focused"},
	})

	// `/notify list` reports the live resolved pair, not the (blank) file.
	state := m.notifyStateText()
	if !strings.Contains(state, "active mode: off") || !strings.Contains(state, "active focus: focused") {
		t.Errorf("list should report the live off/focused pair, got:\n%s", state)
	}

	// The picker preselects the live pair; Enter without navigation does not
	// change it.
	m.input.SetValue("/notify")
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	if cmd != nil {
		t.Fatalf("opening the picker should not emit a cmd, got %T", cmd)
	}
	m = updated.(model)
	if sel := m.picker.items[m.picker.selected]; sel.Value != "off focused" {
		t.Fatalf("preselected = %q, want the live pair %q", sel.Value, "off focused")
	}
	updated, _ = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.notifyMode != "off" || m.notifyFocusMode != "focused" {
		t.Errorf("Enter on preselected live pair changed live state: %q/%q", m.notifyMode, m.notifyFocusMode)
	}
	persisted := readNotifyBlock(t, cfgPath)
	if persisted.Mode != "off" || persisted.FocusMode != "focused" {
		t.Errorf("Enter on the live pair persisted %q/%q, want off/focused (explicit row commit)", persisted.Mode, persisted.FocusMode)
	}

	// Reset the file to blank and re-seed the session for the partial-update
	// checks (fresh states: blank file, live off/focused).
	if err := os.WriteFile(cfgPath, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m = newModel(context.Background(), Options{
		UserConfigPath: cfgPath,
		Notify:         config.NotifyConfig{Mode: "off", FocusMode: "focused"},
	})

	// `/notify bell` (mode-only): live becomes bell/focused; the file gets
	// mode bell and keeps its focus BLANK — the project's focused is not
	// copied into the global file.
	m, _ = m.handleNotifyCommand("bell")
	if m.notifyMode != "bell" || m.notifyFocusMode != "focused" {
		t.Errorf("after mode-only: live = %q/%q, want bell/focused", m.notifyMode, m.notifyFocusMode)
	}
	persisted = readNotifyBlock(t, cfgPath)
	if persisted.Mode != "bell" {
		t.Errorf("after mode-only: persisted mode = %q, want bell", persisted.Mode)
	}
	if persisted.FocusMode != "" {
		t.Errorf("after mode-only: persisted focusMode = %q, want blank (project value must not leak)", persisted.FocusMode)
	}

	// `/notify bell always` (explicit pair): both live fields change and both
	// explicit values persist.
	m, _ = m.handleNotifyCommand("bell always")
	if m.notifyMode != "bell" || m.notifyFocusMode != "always" {
		t.Errorf("after explicit pair: live = %q/%q, want bell/always", m.notifyMode, m.notifyFocusMode)
	}
	persisted = readNotifyBlock(t, cfgPath)
	if persisted.Mode != "bell" || persisted.FocusMode != "always" {
		t.Errorf("after explicit pair: persisted = %q/%q, want bell/always", persisted.Mode, persisted.FocusMode)
	}

	// Restart precedence: a fresh resolve of the user file (now bell/always,
	// no project) yields exactly the stored pair — nothing else leaked in.
	resolved, err := config.Resolve(config.ResolveOptions{UserConfigPath: cfgPath, Env: map[string]string{}})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.Notify.Mode != "bell" || resolved.Notify.FocusMode != "always" {
		t.Errorf("restart resolve = %q/%q, want bell/always", resolved.Notify.Mode, resolved.Notify.FocusMode)
	}
}

// `/notify bell unfocused` updates both fields in one call.
func TestNotifyCommandSetsModeAndFocus(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m, _ = m.handleNotifyCommand("bell unfocused")
	if m.notifyMode != "bell" {
		t.Errorf("notifyMode = %q, want bell", m.notifyMode)
	}
	if m.notifyFocusMode != "unfocused" {
		t.Errorf("notifyFocusMode = %q, want unfocused", m.notifyFocusMode)
	}
}

// The choice reaches the LIVE notifier immediately, so the change applies on
// the next permission prompt in this session (not only after a restart).
func TestNotifyCommandAppliesToLiveNotifier(t *testing.T) {
	var buf bytes.Buffer
	m := newModel(context.Background(), Options{Notify: config.NotifyConfig{Mode: "off", FocusMode: "always"}})
	m.notifier = notify.New(&buf, notify.Config{Mode: notify.ModeOff, FocusMode: notify.FocusAlways})
	m.notifier.SetFocused(true)

	m, _ = m.handleNotifyCommand("bell always")
	m.notifier.Notify(notify.Completion, "x")
	if buf.String() != "\x07" {
		t.Fatalf("live notifier should bell after /notify bell, got %q", buf.String())
	}

	m, _ = m.handleNotifyCommand("off always")
	m.notifier.Notify(notify.Completion, "x")
	if buf.String() != "\x07" {
		t.Fatalf("live notifier should go silent after /notify off, got %q", buf.String())
	}
}

// `/notify off always typo` (more than two tokens) is rejected, not silently
// accepted as a successful change.
func TestNotifyCommandRejectsTrailingArguments(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.notifyMode = "both"
	m, out := m.handleNotifyCommand("off always typo")
	if m.notifyMode != "both" {
		t.Errorf("trailing args should not mutate state, got %q", m.notifyMode)
	}
	if !strings.Contains(out, "Too many arguments") {
		t.Errorf("output should explain the rejection, got: %s", out)
	}
}

// `/notify list junk` is a usage error, not a successful list — matching
// /theme, /effort, and /style, which recognize only an exact `list`
// (maintainer review, PR #1001). Exact `/notify list` still shows state.
func TestNotifyCommandListRequiresExactlyOneToken(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.notifyMode = "off"
	m.notifyFocusMode = "focused"

	m, out := m.handleNotifyCommand("list junk")
	if !strings.Contains(out, "Too many arguments") {
		t.Errorf("`list junk` should be a usage error, got: %s", out)
	}
	if m.notifyMode != "off" || m.notifyFocusMode != "focused" {
		t.Errorf("`list junk` should not mutate state, got %q/%q", m.notifyMode, m.notifyFocusMode)
	}

	_, out = m.handleNotifyCommand("list")
	if !strings.Contains(out, "active mode: off") || !strings.Contains(out, "active focus: focused") {
		t.Errorf("exact `/notify list` should still show the live state, got: %s", out)
	}
}

// `/notify loud` (invalid) returns an error message; the model's notifyMode
// is NOT mutated, so a typo cannot accidentally turn the alert off.
func TestNotifyCommandRejectsInvalidMode(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.notifyMode = "both"
	m, out := m.handleNotifyCommand("loud")
	if m.notifyMode != "both" {
		t.Errorf("invalid mode should not mutate state, got %q", m.notifyMode)
	}
	if !strings.Contains(out, "Unknown mode") {
		t.Errorf("output should explain the error, got: %s", out)
	}
}

// `/notify bell sideways` fails validation before persisting, so neither field
// changes.
func TestNotifyCommandRejectsInvalidFocus(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.notifyMode = "bell"
	m.notifyFocusMode = "always"
	m, out := m.handleNotifyCommand("bell sideways")
	if m.notifyMode != "bell" || m.notifyFocusMode != "always" {
		t.Errorf("invalid focus should not mutate state, got mode=%q focus=%q", m.notifyMode, m.notifyFocusMode)
	}
	if !strings.Contains(out, "Unknown focus mode") {
		t.Errorf("output should explain the error, got: %s", out)
	}
}

// The picker enumerates the FULL mode x focus space (12 rows), so every valid
// pair is representable and Enter can never silently commit a different pair
// than the user's current one. Maintainer review, PR #1001: with a 4-row
// curated list, opening the picker on (off, always) preselected row 0
// (both, unfocused) and Enter changed the setting.
func TestNotifyPickerEnumeratesFullSpace(t *testing.T) {
	m := newModel(context.Background(), Options{})
	picker := m.newNotifyPicker()
	if len(picker.items) != 12 {
		t.Fatalf("picker has %d items, want 12 (4 modes x 3 focus modes)", len(picker.items))
	}
	seen := map[string]bool{}
	for _, item := range picker.items {
		if seen[item.Value] {
			t.Errorf("duplicate picker row %q", item.Value)
		}
		seen[item.Value] = true
	}
	for _, mode := range []string{"off", "bell", "notify", "both"} {
		for _, focus := range []string{"unfocused", "always", "focused"} {
			if !seen[mode+" "+focus] {
				t.Errorf("picker is missing row for valid pair %q %q", mode, focus)
			}
		}
	}
}

// Every pair the picker preselects must also be a row: Enter on an open
// picker must keep (or explicitly change) the user's actual setting. This is
// the maintainer's suggested regression: send Enter to an open picker from a
// pair that is NOT in a 4-row curated list — with full enumeration the
// preselected row IS the current pair and the commit is a no-op change.
func TestNotifyPickerEnterOnUnlistedPairKeepsSetting(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"notify":{"mode":"off","focusMode":"always"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), Options{
		UserConfigPath: cfgPath,
		Notify:         config.NotifyConfig{Mode: "off", FocusMode: "always"},
	})
	m.input.SetValue("/notify")
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if cmd != nil {
		t.Fatalf("opening the notify picker should not emit a cmd, got %T", cmd)
	}
	if m.picker == nil || m.picker.kind != pickerNotify {
		t.Fatalf("expected the notify picker to open, got %#v", m.picker)
	}
	// (off, always) must be preselected — it is a valid pair even though the
	// old curated list could not represent it.
	if sel := m.picker.items[m.picker.selected]; sel.Value != "off always" {
		t.Fatalf("preselected = %q, want the current pair %q", sel.Value, "off always")
	}

	// Enter commits the preselected row: the setting is unchanged.
	updated, _ = m.Update(testKey(tea.KeyEnter))
	m = updated.(model)
	if m.picker != nil {
		t.Fatal("picker should close on Enter")
	}
	persisted := readNotifyBlock(t, cfgPath)
	if persisted.Mode != "off" || persisted.FocusMode != "always" {
		t.Fatalf("Enter changed the setting: got %+v, want off/always unchanged", persisted)
	}
}

// The picker's Value strings are the same "<mode> <focus>" form the text
// handler accepts, so the commit path can be shared. This is the contract that
// lets choosePicker dispatch to handleNotifyCommand without translation.
func TestNotifyPickerValuesAreValidCommandArgs(t *testing.T) {
	m := newModel(context.Background(), Options{})
	picker := m.newNotifyPicker()
	if len(picker.items) == 0 {
		t.Fatal("picker has no items")
	}
	for _, item := range picker.items {
		tokens := strings.Fields(item.Value)
		if len(tokens) != 2 {
			t.Errorf("item %q has %d tokens, want 2 (mode focus)", item.Value, len(tokens))
			continue
		}
		if !isValidNotifyMode(tokens[0]) {
			t.Errorf("item %q: mode %q is not a valid notify mode", item.Value, tokens[0])
		}
		if !isValidNotifyFocusMode(tokens[1]) {
			t.Errorf("item %q: focus %q is not a valid focus mode", item.Value, tokens[1])
		}
	}
}

// The /notify state view reports the LIVE session policy (initialized from the
// resolved pair), not the stored file — the two can legitimately disagree when
// a project config overrides notify (maintainer review, PR #1001). A blank
// live focus renders as the unfocused default.
func TestNotifyStateTextShowsLivePair(t *testing.T) {
	m := newModel(context.Background(), Options{Notify: config.NotifyConfig{Mode: "bell", FocusMode: "always"}})
	state := m.notifyStateText()
	if !strings.Contains(state, "active mode: bell") {
		t.Errorf("state should show the live mode, got: %s", state)
	}
	if !strings.Contains(state, "active focus: always") {
		t.Errorf("state should show the live focus, got: %s", state)
	}

	// Blank live focus renders as the default, never an empty string.
	m = newModel(context.Background(), Options{Notify: config.NotifyConfig{Mode: "bell"}})
	state = m.notifyStateText()
	if !strings.Contains(state, "active focus: unfocused (default)") {
		t.Errorf("state should label a blank live focus as the default, got: %s", state)
	}
}

func readNotifyBlock(t *testing.T, path string) config.NotifyConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config %s: %v", path, err)
	}
	var cfg struct {
		Notify config.NotifyConfig `json:"notify"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode config %s: %v", path, err)
	}
	return cfg.Notify
}

// Maintainer regression (PR #1001): a blank stored mode must read as a
// DEFAULT on every first-party surface — the TUI says "(default)" like the
// CLI's `mode: (default)`, not a bare effective value the user cannot
// distinguish from a stored "both". The mode line mirrors focus, which
// already labels its built-in default ("unfocused (default)").
func TestNotifyStateLabelsEffectiveDefaultMode(t *testing.T) {
	// Nothing stored, nothing resolved: the TUI applies its effective
	// default ("both") and must say so.
	m := newModel(context.Background(), Options{})
	state := m.notifyStateText()
	if !strings.Contains(state, "active mode: both (default)") {
		t.Errorf("unconfigured state should label the effective mode as (default), got:\n%s", state)
	}

	// A stored/resolved explicit "both" renders bare — the user chose it.
	// (The focus line may still say "unfocused (default)"; only the MODE
	// line is under assertion.)
	m = newModel(context.Background(), Options{Notify: config.NotifyConfig{Mode: "both"}})
	state = m.notifyStateText()
	if strings.Contains(state, "active mode: both (default)") {
		t.Errorf("an explicit both must not carry the (default) label, got:\n%s", state)
	}
	if !strings.Contains(state, "active mode: both") {
		t.Errorf("explicit both should still show the mode, got:\n%s", state)
	}

	// A mode-only change from unconfigured commits an explicit choice: the
	// change line and later state output render it bare, never as a default.
	m = newModel(context.Background(), Options{})
	m, out := m.handleNotifyCommand("bell")
	if strings.Contains(out, "active mode: bell (default)") {
		t.Errorf("explicit /notify bell change line should render bare, got:\n%s", out)
	}
	if !strings.Contains(out, "active mode: bell") {
		t.Errorf("change line should show the new mode, got:\n%s", out)
	}
	if state := m.notifyStateText(); strings.Contains(state, "active mode: bell (default)") {
		t.Errorf("state after an explicit choice should render bare, got:\n%s", state)
	}
}

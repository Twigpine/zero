package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/Twigpine/zero/internal/config"
)

func TestParseBindingLabel(t *testing.T) {
	tests := []struct {
		input string
		want  string // expected Label()
	}{
		{"option+o", "Alt+O"},
		{"ctrl+o", "Ctrl+O"},
		{"ctrl+e", "Ctrl+E"},
		{"option+b", "Alt+B"},
		{"option+O", "Alt+O"},
		{"alt+o", "Alt+O"},
		{"cmd+o", "Cmd+O"},
		{"super+o", "Cmd+O"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			p := parseBinding(tt.input)
			got := p.Label()
			if got != tt.want {
				t.Errorf("parseBinding(%q).Label() = %q, want %q", tt.input, got, tt.want)
			}
			if p.isZero() {
				t.Errorf("parseBinding(%q).isZero() = true, want false", tt.input)
			}
		})
	}
}

func TestOptionOMatchesOptionO(t *testing.T) {
	p := parseBinding("option+o")
	matcher := p.Matcher()

	// Simulate iTerm2 with "Option as Meta" pressing Option+O
	// Terminal sends ESC o → KeyPressEvent{Code: 'o', Mod: ModAlt}
	msg := tea.KeyPressMsg(tea.Key{Code: 'o', Mod: tea.ModAlt})

	if !matcher(msg) {
		t.Errorf("option+o matcher should match {Code:'o', Mod:ModAlt}")
	}

	// Should NOT match plain 'o'
	msg2 := tea.KeyPressMsg(tea.Key{Code: 'o'})
	if matcher(msg2) {
		t.Errorf("option+o matcher should NOT match {Code:'o'} without ModAlt")
	}

	// Should NOT match ctrl+o
	msg3 := tea.KeyPressMsg(tea.Key{Code: 'o', Mod: tea.ModCtrl})
	if matcher(msg3) {
		t.Errorf("option+o matcher should NOT match {Code:'o', Mod:ModCtrl}")
	}
}

func TestOptionOMatchesComposedCharacters(t *testing.T) {
	p := parseBinding("option+o")
	matcher := p.Matcher()

	// On macOS without "Option as Meta", Option+O produces ø (U+00F8)
	msg := tea.KeyPressMsg(tea.Key{Code: 0xF8, Text: "ø"})
	if matcher(msg) {
		t.Logf("NOTE: option+o matcher also matches option+ø (macOS compose behavior)")
	}
}

func TestCtrlEMatchesDefault(t *testing.T) {
	p := parseBinding("ctrl+e")
	matcher := p.Matcher()

	// Terminal sends Ctrl+E → byte 0x05 → KeyPressEvent{Code: 'e', Mod: ModCtrl}
	msg := tea.KeyPressMsg(tea.Key{Code: 'e', Mod: tea.ModCtrl})

	if !matcher(msg) {
		t.Errorf("ctrl+e matcher should match {Code:'e', Mod:ModCtrl}")
	}
}

func TestCtrlBindingRejectsSupersetModifiers(t *testing.T) {
	// Configured ctrl+e must NOT match ctrl+alt+e or ctrl+shift+e
	// (Mod.Contains superset correctness for the ctrl-letter fast path).
	p := parseBinding("ctrl+e")
	matcher := p.Matcher()

	bad := []struct {
		name string
		msg  tea.KeyMsg
	}{
		{"ctrl+alt+e", tea.KeyPressMsg(tea.Key{Code: 'e', Mod: tea.ModCtrl | tea.ModAlt})},
		{"ctrl+shift+e", tea.KeyPressMsg(tea.Key{Code: 'e', Mod: tea.ModCtrl | tea.ModShift})},
		{"ctrl+alt+shift+e", tea.KeyPressMsg(tea.Key{Code: 'e', Mod: tea.ModCtrl | tea.ModAlt | tea.ModShift})},
		{"ctrl+cmd+e", tea.KeyPressMsg(tea.Key{Code: 'e', Mod: tea.ModCtrl | tea.ModSuper})},
	}

	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			if matcher(tt.msg) {
				t.Errorf("ctrl+e matcher should NOT match %s", tt.name)
			}
		})
	}
}

func TestAltBindingRejectsSupersetModifiers(t *testing.T) {
	// Configured alt+o must NOT match alt+shift+o (code path, exact mod).
	p := parseBinding("alt+o")
	matcher := p.Matcher()

	bad := []struct {
		name string
		msg  tea.KeyMsg
	}{
		{"alt+shift+o", tea.KeyPressMsg(tea.Key{Code: 'o', Mod: tea.ModAlt | tea.ModShift})},
		{"alt+ctrl+o", tea.KeyPressMsg(tea.Key{Code: 'o', Mod: tea.ModAlt | tea.ModCtrl})},
		{"alt+cmd+o", tea.KeyPressMsg(tea.Key{Code: 'o', Mod: tea.ModAlt | tea.ModSuper})},
	}

	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			if matcher(tt.msg) {
				t.Errorf("alt+o matcher should NOT match %s", tt.name)
			}
		})
	}
}

func TestPlainKeyBindingRejectsModifiers(t *testing.T) {
	// A configured plain-key binding (no modifiers, e.g. "o") should only
	// match when Mod is exactly 0, not when a modifier is held.
	p := parseBinding("o")
	matcher := p.Matcher()

	plain := tea.KeyPressMsg(tea.Key{Code: 'o'})
	if !matcher(plain) {
		t.Errorf("'o' matcher should match {Code:'o'} with no modifiers")
	}

	bad := []struct {
		name string
		msg  tea.KeyMsg
	}{
		{"ctrl+o", tea.KeyPressMsg(tea.Key{Code: 'o', Mod: tea.ModCtrl})},
		{"alt+o", tea.KeyPressMsg(tea.Key{Code: 'o', Mod: tea.ModAlt})},
		{"shift+o", tea.KeyPressMsg(tea.Key{Code: 'o', Mod: tea.ModShift})},
	}

	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			if matcher(tt.msg) {
				t.Errorf("'o' matcher should NOT match %s", tt.name)
			}
		})
	}
}

func TestParseBindingUTF8Character(t *testing.T) {
	// A 2-byte UTF-8 key like é (U+00E9) should parse as a single character,
	// not be rejected by a byte-length check.
	p := parseBinding("é")
	if p.isZero() {
		t.Errorf("parseBinding('é') should not be zero — it's a valid single character")
	}
	if p.code != 'é' {
		t.Errorf("parseBinding('é').code = %d (U+%04X), want %d (U+00E9)", p.code, p.code, 'é')
	}
	if p.text != "" {
		t.Errorf("parseBinding('é').text = %q, want empty string", p.text)
	}

	// Also verify the matcher works
	matcher := p.Matcher()
	msg := tea.KeyPressMsg(tea.Key{Code: 'é', Text: "é"})
	if !matcher(msg) {
		t.Errorf("'é' matcher should match {Code:'é', Text:'é'}")
	}

	// Should not match with a modifier
	msgAlt := tea.KeyPressMsg(tea.Key{Code: 'é', Text: "é", Mod: tea.ModAlt})
	if matcher(msgAlt) {
		t.Errorf("'é' matcher should NOT match with modifier")
	}
}

func TestDispatchRejectsSupersetModifiers(t *testing.T) {
	// Integration: model.keyMatch with a configured binding must reject
	// modifier supersets.
	cfg := config.KeyBindingsConfig{
		ToggleDetailed: "ctrl+o",
	}
	bindings := resolveKeyBindings(cfg)
	defaultFn := func(tea.KeyMsg) bool { return false }
	m := model{keyBindings: bindings}

	// Ctrl+O should match
	msgCtrlO := tea.KeyPressMsg(tea.Key{Code: 'o', Mod: tea.ModCtrl})
	if !m.keyMatch(bindings.toggleDetailed, msgCtrlO, defaultFn) {
		t.Errorf("keyMatch should match ctrl+o when configured")
	}

	// Ctrl+Alt+O should NOT match
	msgCtrlAltO := tea.KeyPressMsg(tea.Key{Code: 'o', Mod: tea.ModCtrl | tea.ModAlt})
	if m.keyMatch(bindings.toggleDetailed, msgCtrlAltO, defaultFn) {
		t.Errorf("keyMatch should NOT match ctrl+alt+o when ctrl+o is configured")
	}
}

func TestConfigToBindingPipeline(t *testing.T) {
	cfg := config.KeyBindingsConfig{
		ToggleDetailed: "option+o",
		ToggleMouse:    "ctrl+e",
		CycleReasoning: "ctrl+t",
		TogglePlan:     "ctrl+p",
		ToggleSidebar:  "option+b",
	}

	bindings := resolveKeyBindings(cfg)

	tests := []struct {
		name    string
		binding parsedBinding
		wantKey string
	}{
		{"toggleDetailed", bindings.toggleDetailed, "Alt+O"},
		{"toggleMouse", bindings.toggleMouse, "Ctrl+E"},
		{"cycleReasoning", bindings.cycleReasoning, "Ctrl+T"},
		{"togglePlan", bindings.togglePlan, "Ctrl+P"},
		{"toggleSidebar", bindings.toggleSidebar, "Alt+B"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.binding.isZero() {
				t.Errorf("%s binding is zero (unset) — config not loaded", tt.name)
			}
			if got := tt.binding.Label(); got != tt.wantKey {
				t.Errorf("%s.Label() = %q, want %q", tt.name, got, tt.wantKey)
			}
		})
	}
}

func TestHelpBindingSubstitution(t *testing.T) {
	// Verify that the help overlay correctly shows user-configured bindings
	// by checking that Label() returns non-empty for config-sourced bindings.
	cfg := config.KeyBindingsConfig{
		ToggleDetailed: "option+o",
		ToggleSidebar:  "option+b",
	}

	bindings := resolveKeyBindings(cfg)

	if l := bindings.toggleDetailed.Label(); l != "Alt+O" {
		t.Errorf("toggleDetailed.Label() = %q, want %q", l, "Alt+O")
	}
	if l := bindings.toggleSidebar.Label(); l != "Alt+B" {
		t.Errorf("toggleSidebar.Label() = %q, want %q", l, "Alt+B")
	}
}

// TestDispatchOptionO is an integration-style test: it verifies that pressing
// Option+O dispatches as toggleDetailed when configured.
func TestDispatchOptionO(t *testing.T) {
	cfg := config.KeyBindingsConfig{
		ToggleDetailed: "option+o",
	}
	bindings := resolveKeyBindings(cfg)

	// Simulate Option+O in iTerm2 with "Option as Meta" → ESC o → {Code:'o', Mod:ModAlt}
	msg := tea.KeyPressMsg(tea.Key{Code: 'o', Mod: tea.ModAlt})
	defaultFn := func(tea.KeyMsg) bool { return false }

	// This is the dispatch check the model uses
	m := model{keyBindings: bindings}
	if !m.keyMatch(bindings.toggleDetailed, msg, defaultFn) {
		t.Errorf("model.keyMatch should match option+o")
	}

	// Also verify default doesn't match
	msgCtrlO := tea.KeyPressMsg(tea.Key{Code: 'o', Mod: tea.ModCtrl})
	if m.keyMatch(bindings.toggleDetailed, msgCtrlO, defaultFn) {
		t.Errorf("model.keyMatch should NOT match ctrl+o when option+o is configured")
	}
}

// A configured binding equal to a reserved hardcoded chord must be reverted
// to default rather than silently making the hardcoded action unreachable
// (e.g. configuring toggleDetailed to ctrl+f would otherwise swallow the
// /model picker's "favorite" shortcut).
func TestSanitizeKeyBindingsDropsReservedCollision(t *testing.T) {
	cfg := config.KeyBindingsConfig{
		ToggleDetailed: "ctrl+f", // collides with the hardcoded favorite-model shortcut
		ToggleMouse:    "ctrl+e", // unaffected, should survive untouched
	}
	sanitized, warnings := sanitizeKeyBindings(resolveKeyBindings(cfg))

	if !sanitized.toggleDetailed.isZero() {
		t.Errorf("toggleDetailed should be reverted to default, got %q", sanitized.toggleDetailed.Label())
	}
	if sanitized.toggleMouse.isZero() || sanitized.toggleMouse.Label() != "Ctrl+E" {
		t.Errorf("toggleMouse should be untouched, got %q", sanitized.toggleMouse.Label())
	}
	if len(warnings) != 1 {
		t.Fatalf("want exactly 1 warning, got %d: %v", len(warnings), warnings)
	}
}

// Two configurable actions bound to the same chord must not both fire —
// sanitizeKeyBindings should keep the first and revert the rest to default.
func TestSanitizeKeyBindingsDropsMutualCollision(t *testing.T) {
	cfg := config.KeyBindingsConfig{
		ToggleDetailed: "ctrl+o",
		CycleReasoning: "ctrl+o", // collides with toggleDetailed above
	}
	sanitized, warnings := sanitizeKeyBindings(resolveKeyBindings(cfg))

	if sanitized.toggleDetailed.isZero() || sanitized.toggleDetailed.Label() != "Ctrl+O" {
		t.Errorf("toggleDetailed (first claimant) should keep ctrl+o, got %q", sanitized.toggleDetailed.Label())
	}
	if !sanitized.cycleReasoning.isZero() {
		t.Errorf("cycleReasoning (second claimant) should be reverted to default, got %q", sanitized.cycleReasoning.Label())
	}
	if len(warnings) != 1 {
		t.Fatalf("want exactly 1 warning, got %d: %v", len(warnings), warnings)
	}
}

// Non-colliding configured bindings must pass through unchanged with no
// warnings.
func TestSanitizeKeyBindingsNoCollisions(t *testing.T) {
	cfg := config.KeyBindingsConfig{
		ToggleDetailed: "option+o",
		ToggleSidebar:  "option+b",
	}
	sanitized, warnings := sanitizeKeyBindings(resolveKeyBindings(cfg))

	if len(warnings) != 0 {
		t.Fatalf("want no warnings, got %v", warnings)
	}
	if sanitized.toggleDetailed.Label() != "Alt+O" || sanitized.toggleSidebar.Label() != "Alt+B" {
		t.Fatalf("bindings should be unchanged: toggleDetailed=%q toggleSidebar=%q",
			sanitized.toggleDetailed.Label(), sanitized.toggleSidebar.Label())
	}
}

// A configured binding must not shadow another action that is still on its
// built-in default chord.
func TestSanitizeKeyBindingsDropsCollisionWithOtherDefault(t *testing.T) {
	cfg := config.KeyBindingsConfig{
		ToggleDetailed: "ctrl+t", // collides with cycleReasoning default
	}
	sanitized, warnings := sanitizeKeyBindings(resolveKeyBindings(cfg))

	if !sanitized.toggleDetailed.isZero() {
		t.Errorf("toggleDetailed should be reverted to default, got %q", sanitized.toggleDetailed.Label())
	}
	if !sanitized.cycleReasoning.isZero() {
		t.Errorf("cycleReasoning should remain default (zero), got %q", sanitized.cycleReasoning.Label())
	}
	if len(warnings) != 1 {
		t.Fatalf("want exactly 1 warning, got %d: %v", len(warnings), warnings)
	}
}

// Bare hardcoded keys handled directly in updateModel (e.g. Tab navigation)
// are reserved too and must not be shadowed by configurable bindings.
func TestSanitizeKeyBindingsDropsBareHardcodedCollision(t *testing.T) {
	cfg := config.KeyBindingsConfig{
		ToggleDetailed: "tab",
	}
	sanitized, warnings := sanitizeKeyBindings(resolveKeyBindings(cfg))

	if !sanitized.toggleDetailed.isZero() {
		t.Errorf("toggleDetailed should be reverted to default, got %q", sanitized.toggleDetailed.Label())
	}
	if len(warnings) != 1 {
		t.Fatalf("want exactly 1 warning, got %d: %v", len(warnings), warnings)
	}
}

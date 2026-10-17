package tui

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/config"
)

// parsedBinding is the parsed representation of a keybinding string such as
// "ctrl+o" or "alt+enter". The zero value is a sentinel meaning "use default".
type parsedBinding struct {
	ctrl  bool
	alt   bool
	shift bool
	cmd   bool   // ⌘ Command (macOS) / ⊞ Win → ModSuper
	code  rune   // tea.KeyCode or 0 for text-based matching
	text  string // for text-based matching (e.g. "?")
}

// isZero returns true when p is the nil sentinel (no binding configured).
func (p parsedBinding) isZero() bool {
	return p.code == 0 && p.text == ""
}

// defaultToggleMouseChord and defaultToggleSidebarChord are the built-in
// Ctrl+E/Ctrl+B chords that conflict with readline cursor navigation
// (move-to-end-of-line / move-to-beginning-of-line) while typing in the
// composer.
var (
	defaultToggleMouseChord   = parseBinding("ctrl+e")
	defaultToggleSidebarChord = parseBinding("ctrl+b")
)

// requiresEmptyComposer reports whether binding b resolves to conflicting, the
// hardcoded default chord it can fall back to. That happens either because b
// is unset (isZero, so keyMatch uses the default matcher) or because the user
// explicitly configured the identical chord (e.g. toggleMouse: "ctrl+e"),
// which parseBinding does not treat as zero. Only a binding that resolves to
// a genuinely different chord may fire while the composer has text; one that
// resolves to the conflicting default must still wait for it to be empty so
// readline navigation gets the keystroke instead.
func requiresEmptyComposer(b parsedBinding, conflicting parsedBinding) bool {
	return b.isZero() || b == conflicting
}

// canFireComposerGatedToggle reports whether a toggle bound to b (whose
// conflicting hardcoded default is conflicting) may fire given the current
// composer-empty state. Factored out of the toggleMouse/toggleSidebar dispatch
// cases in model.go, which both repeated this same condition inline.
func canFireComposerGatedToggle(b parsedBinding, conflicting parsedBinding, composerEmpty bool) bool {
	return !requiresEmptyComposer(b, conflicting) || composerEmpty
}

// Label returns a human-readable representation of the binding, e.g. "Ctrl+O"
// or "Cmd+Shift+Enter". Used in the help overlay. Returns empty string for
// zero (unset) bindings.
func (p parsedBinding) Label() string {
	if p.isZero() {
		return ""
	}
	var b strings.Builder
	if p.ctrl {
		b.WriteString("Ctrl+")
	}
	if p.alt {
		b.WriteString("Alt+")
	}
	if p.shift {
		b.WriteString("Shift+")
	}
	if p.cmd {
		b.WriteString("Cmd+")
	}
	if p.text != "" {
		b.WriteString(p.text)
	} else if p.code != 0 {
		switch p.code {
		case tea.KeyEnter:
			b.WriteString("Enter")
		case tea.KeyTab:
			b.WriteString("Tab")
		case tea.KeyEsc:
			b.WriteString("Esc")
		case tea.KeySpace:
			b.WriteString("Space")
		case tea.KeyBackspace:
			b.WriteString("Backspace")
		case tea.KeyDelete:
			b.WriteString("Delete")
		case tea.KeyUp:
			b.WriteString("↑")
		case tea.KeyDown:
			b.WriteString("↓")
		case tea.KeyLeft:
			b.WriteString("←")
		case tea.KeyRight:
			b.WriteString("→")
		case tea.KeyHome:
			b.WriteString("Home")
		case tea.KeyEnd:
			b.WriteString("End")
		case tea.KeyPgUp:
			b.WriteString("PgUp")
		case tea.KeyPgDown:
			b.WriteString("PgDn")
		case tea.KeyF1, tea.KeyF2, tea.KeyF3, tea.KeyF4, tea.KeyF5, tea.KeyF6,
			tea.KeyF7, tea.KeyF8, tea.KeyF9, tea.KeyF10, tea.KeyF11, tea.KeyF12:
			b.WriteString(fKeyLabel(p.code))
		default:
			// Printable character — uppercase for display
			if p.code >= 'a' && p.code <= 'z' {
				b.WriteRune(p.code - 32)
			} else {
				b.WriteRune(p.code)
			}
		}
	}
	return b.String()
}

// Matcher returns a function that matches a tea.KeyMsg against this binding.
// It is the hot path for the key dispatch — kept cheap intentionally.
func (p parsedBinding) Matcher() func(tea.KeyMsg) bool {
	if p.isZero() {
		// A zero binding should never be matched — the caller is expected to
		// check useDefault() first and fall through to the built-in check.
		return func(tea.KeyMsg) bool { return false }
	}

	// Build the required modifier mask from the parsed flags.
	var mod tea.KeyMod
	if p.ctrl {
		mod |= tea.ModCtrl
	}
	if p.alt {
		mod |= tea.ModAlt
	}
	if p.shift {
		mod |= tea.ModShift
	}
	if p.cmd {
		mod |= tea.ModSuper // ⌘ Command on macOS, ⊞ Win on Windows
	}

	// ctrl+letter fast path — use exact modifier equality so that a
	// configured ctrl+o does NOT fire on ctrl+alt+o or ctrl+shift+o.
	// Note: in raw terminal mode Bubble Tea may report ctrl+letter as
	// {Code:letter, Mod:ModCtrl} (handled below), or as a control char
	// code (e.g. 0x0F) without a modifier flag.  The latter is handled by
	// the text-based fallback in model.go for default bindings; for
	// configured bindings the code+mod path below is what the user
	// expressed.
	if mod == tea.ModCtrl && p.code >= 'a' && p.code <= 'z' {
		return func(msg tea.KeyMsg) bool {
			return msg.Key().Code == p.code && msg.Key().Mod == mod
		}
	}

	if p.text != "" {
		return func(msg tea.KeyMsg) bool {
			return msg.Key().Text == p.text && msg.Key().Mod == mod
		}
	}

	return func(msg tea.KeyMsg) bool {
		return msg.Key().Code == p.code && msg.Key().Mod == mod
	}
}

// parseBinding converts a user-written keybinding string (e.g. "ctrl+o") into
// a parsedBinding. The empty string returns zero parsedBinding (the "use
// default" sentinel).
func parseBinding(s string) parsedBinding {
	s = strings.TrimSpace(s)
	if s == "" {
		return parsedBinding{}
	}

	parts := strings.Split(strings.ToLower(s), "+")
	var p parsedBinding
	var keyPart string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		switch part {
		case "ctrl", "control":
			p.ctrl = true
		case "alt", "option":
			p.alt = true
		case "shift":
			p.shift = true
		case "cmd", "command":
			p.cmd = true
		case "super":
			// "super" matches the Bubble Tea naming; on macOS this is ⌘
			p.cmd = true
		default:
			keyPart = part
		}
	}

	if p.ctrl && len(keyPart) == 1 && keyPart[0] >= 'a' && keyPart[0] <= 'z' {
		// ctrl+<letter> — store the code so the match is exact
		p.code = []rune(keyPart)[0]
		p.text = ""
		return p
	}

	// Map named keys to their tea.KeyCode
	switch keyPart {
	case "enter", "return":
		p.code = tea.KeyEnter
	case "tab":
		p.code = tea.KeyTab
	case "esc", "escape":
		p.code = tea.KeyEsc
	case "space":
		p.code = tea.KeySpace
	case "backspace":
		p.code = tea.KeyBackspace
	case "delete":
		p.code = tea.KeyDelete
	case "up":
		p.code = tea.KeyUp
	case "down":
		p.code = tea.KeyDown
	case "left":
		p.code = tea.KeyLeft
	case "right":
		p.code = tea.KeyRight
	case "home":
		p.code = tea.KeyHome
	case "end":
		p.code = tea.KeyEnd
	case "pgup", "pageup":
		p.code = tea.KeyPgUp
	case "pgdown", "pagedown":
		p.code = tea.KeyPgDown
	case "f1":
		p.code = tea.KeyF1
	case "f2":
		p.code = tea.KeyF2
	case "f3":
		p.code = tea.KeyF3
	case "f4":
		p.code = tea.KeyF4
	case "f5":
		p.code = tea.KeyF5
	case "f6":
		p.code = tea.KeyF6
	case "f7":
		p.code = tea.KeyF7
	case "f8":
		p.code = tea.KeyF8
	case "f9":
		p.code = tea.KeyF9
	case "f10":
		p.code = tea.KeyF10
	case "f11":
		p.code = tea.KeyF11
	case "f12":
		p.code = tea.KeyF12
	case "?":
		p.text = "?"
		p.code = 0
	default:
		// Single character, any modifier context
		if utf8.RuneCountInString(keyPart) == 1 {
			p.code = []rune(keyPart)[0]
		}
		// else unrecognised — leave zero so it falls through to default
	}

	return p
}

// labelOr returns b.Label() when b is configured (non-zero), otherwise it
// returns the caller-supplied default label string.  This is the display-layer
// counterpart to keyMatch — dispatch falls through to the hardcoded default
// function, so the label displayed in help / hints must match that fallback.
func labelOr(b parsedBinding, defaultLabel string) string {
	if !b.isZero() {
		return b.Label()
	}
	return defaultLabel
}

// keyBindings holds the parsed, resolved key bindings the model uses at
// dispatch time. When a binding's parsedBinding is zero, the built-in default
// check in model.go's updateModel should be used.
type keyBindings struct {
	toggleDetailed parsedBinding
	toggleMouse    parsedBinding
	cycleReasoning parsedBinding
	togglePlan     parsedBinding
	toggleSidebar  parsedBinding
}

// fKeyLabel renders a function-key code as "F9" etc. tea.KeyF1..KeyF12 are
// sequential, so the offset from KeyF1 gives the number.
func fKeyLabel(code rune) string {
	n := int(code-tea.KeyF1) + 1
	return "F" + strconv.Itoa(n)
}

// resolveKeyBindings converts a user-facing KeyBindingsConfig into the
// dispatch-ready parsed form, using empty-is-default semantics.
func resolveKeyBindings(cfg config.KeyBindingsConfig) keyBindings {
	return keyBindings{
		toggleDetailed: parseBinding(string(cfg.ToggleDetailed)),
		toggleMouse:    parseBinding(string(cfg.ToggleMouse)),
		cycleReasoning: parseBinding(string(cfg.CycleReasoning)),
		togglePlan:     parseBinding(string(cfg.TogglePlan)),
		toggleSidebar:  parseBinding(string(cfg.ToggleSidebar)),
	}
}

// keyMatch returns true when msg matches either the user-configured binding b
// or (when b is zero/unset) the built-in default matcher defaultFn. This is
// the bridge between the config surface and the hot dispatch path in model.go.
func (m model) keyMatch(b parsedBinding, msg tea.KeyMsg, defaultFn func(tea.KeyMsg) bool) bool {
	if !b.isZero() {
		return b.Matcher()(msg)
	}
	return defaultFn(msg)
}

// reservedBindings lists hardcoded (non-configurable) chords handled directly in
// model.go's key dispatch. If a configurable binding uses one of these chords,
// one of the actions becomes unreachable (depending on switch order), so
// sanitizeKeyBindings reverts the configurable binding back to its default.
var reservedBindings = []struct {
	binding     parsedBinding
	description string
}{
	{parseBinding("ctrl+c"), "cancel / exit"},
	{parseBinding("esc"), "cancel / close"},
	{parseBinding("enter"), "submit"},
	{parseBinding("shift+tab"), "cycle permission mode"},
	{parseBinding("tab"), "navigation / completion"},
	{parseBinding("backspace"), "composer edit / attachment removal"},
	{parseBinding("up"), "history/navigation"},
	{parseBinding("down"), "history/navigation"},
	{parseBinding("pgup"), "transcript scroll"},
	{parseBinding("pgdown"), "transcript scroll"},
	{parseBinding("ctrl+f"), "favorite model (in the /model picker)"},
	{parseBinding("?"), "help overlay"},
}

// sanitizeKeyBindings drops (reverts to default) any configured binding that
// collides with a reserved hardcoded chord above, or with another
// configured binding, since either collision would silently make one of the
// two actions permanently unreachable. Returns the sanitized bindings plus a
// human-readable warning for each dropped binding, for the caller to surface
// as a startup notice.
func sanitizeKeyBindings(b keyBindings) (keyBindings, []string) {
	entries := []struct {
		name           string
		binding        *parsedBinding
		defaultBinding parsedBinding
	}{
		{"toggleDetailed", &b.toggleDetailed, parseBinding("ctrl+o")},
		{"toggleMouse", &b.toggleMouse, parseBinding("ctrl+e")},
		{"cycleReasoning", &b.cycleReasoning, parseBinding("ctrl+t")},
		{"togglePlan", &b.togglePlan, parseBinding("ctrl+p")},
		{"toggleSidebar", &b.toggleSidebar, parseBinding("ctrl+b")},
	}

	var warnings []string
	for _, e := range entries {
		if e.binding.isZero() {
			continue
		}
		for _, other := range entries {
			if other.name == e.name || !other.binding.isZero() {
				continue
			}
			if *e.binding == other.defaultBinding {
				warnings = append(warnings, fmt.Sprintf(
					"keybindings.%s (%s) conflicts with keybindings.%s default (%s); using the default instead.",
					e.name, e.binding.Label(), other.name, other.defaultBinding.Label()))
				*e.binding = parsedBinding{}
				break
			}
		}
	}

	for _, e := range entries {
		if e.binding.isZero() {
			continue
		}
		for _, reserved := range reservedBindings {
			if *e.binding == reserved.binding {
				warnings = append(warnings, fmt.Sprintf(
					"keybindings.%s (%s) conflicts with the built-in %s shortcut; using the default instead.",
					e.name, e.binding.Label(), reserved.description))
				*e.binding = parsedBinding{}
				break
			}
		}
	}

	claimedBy := map[parsedBinding]string{}
	for _, e := range entries {
		if e.binding.isZero() {
			continue
		}
		if other, ok := claimedBy[*e.binding]; ok {
			warnings = append(warnings, fmt.Sprintf(
				"keybindings.%s (%s) conflicts with keybindings.%s; using the default instead.",
				e.name, e.binding.Label(), other))
			*e.binding = parsedBinding{}
			continue
		}
		claimedBy[*e.binding] = e.name
	}

	return b, warnings
}

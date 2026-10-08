package tui

import (
	"strings"

	"github.com/Gitlawb/zero/internal/config"
)

// themeMode is the operator's palette preference.
type themeMode string

const (
	themeSystem themeMode = "system" // preserve the terminal canvas (default)
	themeAuto   themeMode = "auto"   // legacy alias for system
	themeDark   themeMode = "dark"   // migrated old saved preference
	themeLight  themeMode = "light"  // migrated old saved preference
)

// themeModes lists the values /theme presents, in picker order: `system` first,
// then named palettes. Former dark/light saved preferences migrate to System and
// no longer appear as choices.
var themeModes = append([]string{string(themeSystem)}, selectableThemeNames()...)

func selectableThemeNames() []string {
	names := make([]string, 0, len(themeRegistry))
	for _, entry := range themeRegistry {
		if entry.Name == string(themeDark) || entry.Name == string(themeLight) {
			continue
		}
		names = append(names, entry.Name)
	}
	return names
}

// resolveThemeMode picks the first accepted preference from candidates in
// precedence order — the caller passes them highest-first: the --theme flag, then
// ZERO_THEME, then the persisted config theme. A value is accepted if it is
// `system`, the legacy `auto` alias, or names a registered theme. Unrecognized/
// blank values are skipped, and an empty list (or all-unrecognized) falls back to
// system.
func resolveThemeMode(candidates ...string) themeMode {
	for _, v := range candidates {
		s := strings.ToLower(strings.TrimSpace(v))
		if s == "" {
			continue
		}
		if s == string(themeSystem) || s == string(themeAuto) || s == string(themeDark) || s == string(themeLight) {
			return themeSystem
		}
		if _, ok := lookupTheme(s); ok {
			return themeMode(s)
		}
	}
	return themeSystem
}

// validThemeMode reports whether s names a theme mode (for /theme validation):
// `system`, legacy `auto`, or any visible named palette.
func validThemeMode(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == string(themeSystem) || s == string(themeAuto) {
		return true
	}
	if s == string(themeDark) || s == string(themeLight) {
		return false
	}
	_, ok := lookupTheme(s)
	return ok
}

// ValidThemeArg reports whether s is an acceptable --theme / ZERO_THEME value.
// It accepts retired dark/light preferences so existing scripts and environment
// variables migrate to System, while validThemeMode keeps them out of the picker.
func ValidThemeArg(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == string(themeDark) || s == string(themeLight) {
		return true
	}
	return validThemeMode(s)
}

// applyTheme swaps the active palette (zeroTheme) and the globals derived from it
// — the streaming-fade ramp and the static render cache — so a committed switch
// repaints every subsequent render. `system` (and legacy `auto`) preserve the
// terminal canvas. Named palettes adapt their contrast direction to the terminal
// background without painting it. Must run on the Bubble Tea update goroutine (or
// before the program starts), like every other zeroTheme access.
func applyTheme(mode themeMode, terminalDark bool) themeMode {
	resolved, theme := themeForMode(mode, terminalDark)
	zeroTheme = theme
	rebuildStreamingFadePalette()
	if defaultRenderCache != nil {
		defaultRenderCache.clear() // old-palette entries must not be reused
	}
	return resolved
}

// themeForMode resolves a candidate without mutating zeroTheme. The /theme picker
// uses it to render a contained preview while the active UI remains untouched.
func themeForMode(mode themeMode, terminalDark bool) (themeMode, tuiTheme) {
	if mode == themeSystem || mode == themeAuto || mode == "" {
		return themeSystem, buildSystemThemeForTerminal(terminalDark)
	}
	if entry, ok := lookupTheme(string(mode)); ok {
		theme := buildTheme(paletteForTerminal(entry.Palette, entry.IsDark, terminalDark))
		if entry.IsDark == terminalDark {
			theme.codeTheme = codeThemeForMode(themeMode(entry.Name))
		}
		return themeMode(entry.Name), theme
	}
	return themeSystem, buildSystemThemeForTerminal(terminalDark)
}

// handleThemeCommand implements /theme [name]: `list` shows state, a registered
// theme name (or legacy `auto`) switches the active palette. Bare `/theme` opens
// the picker at the dispatch layer, so it never reaches here empty.
func (m model) handleThemeCommand(args string) (model, string) {
	arg := strings.ToLower(strings.TrimSpace(args))
	if arg == "" || arg == "list" {
		return m, m.themeStateText()
	}
	if !validThemeMode(arg) {
		return m, "Theme\nUnknown theme: " + arg + " (use /theme with no argument to pick from the list)"
	}
	m.themeMode = resolveThemeMode(arg)
	active := string(applyTheme(m.themeMode, m.hasDarkBg))
	lines := []string{
		"Theme",
		"active theme: " + active,
		"Already-printed scrollback keeps its previous colors; new output uses the new theme.",
	}
	if note := m.persistThemePreference(); note != "" {
		lines = append(lines, note)
	}
	return m, strings.Join(lines, "\n")
}

// themeAppliedNotice returns the compact confirmation used after a successful
// theme switch. The full state card remains available through /theme list.
func (m model) themeAppliedNotice() string {
	if m.themeMode == themeSystem {
		return "Theme: System"
	}
	if entry, ok := lookupTheme(string(m.themeMode)); ok {
		return "Theme: " + entry.Label
	}
	return "Theme: " + string(m.themeMode)
}

// persistThemePreference writes the committed theme to user config so it is applied
// again at startup (via Options.SavedTheme -> resolveThemeMode). Best-effort: returns
// a short note to surface on failure, or "" on success / when there is no config
// path (e.g. tests).
func (m model) persistThemePreference() string {
	if strings.TrimSpace(m.userConfigPath) == "" {
		return ""
	}
	if _, err := config.SetTheme(m.userConfigPath, string(m.themeMode)); err != nil {
		return "note: could not save theme preference (" + err.Error() + ")"
	}
	return ""
}

// themeStateText renders the /theme state view.
func (m model) themeStateText() string {
	return renderCommandOutput(commandOutput{
		Title:  "Theme",
		Status: commandStatusOK,
		Sections: []commandSection{{
			Title: "State",
			Lines: []string{
				"active theme: " + string(m.themeMode),
				"available: " + strings.Join(themeModes, ", "),
			},
		}},
		Hints: []string{"run /theme with no argument to open the picker, or /theme <name> to switch directly"},
	})
}

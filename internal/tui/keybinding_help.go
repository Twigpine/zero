// keybinding_help.go renders the `?` keyboard-shortcut overlay. Zero has a
// rich set of chord bindings (Ctrl+T effort, drill-in subchat,
// Shift+Tab permission mode, …) that are otherwise invisible — only learnable
// by reading the source. A single-key `?` overlay (opened on an empty composer)
// lists them grouped, so the keymap is discoverable the way the reference TUIs
// do it. The list is declarative and hand-curated to match the real handlers in
// model.go's Update switch; keep them in sync when a binding changes.
//
// Configurable bindings (toggleDetailed, toggleMouse, cycleReasoning,
// toggleSidebar) pull their key label from m.keyBindings so a user
// who remaps them in config.json sees the actual chords, not the defaults.
package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

// keybinding is one row in the help overlay: the key chord and what it does.
type keybinding struct {
	keys string
	desc string
}

// keybindingGroup is a titled cluster of related bindings.
type keybindingGroup struct {
	title    string
	bindings []keybinding
}

// buildKeybindingGroups returns the full, grouped shortcut list shown by the
// `?` overlay. Configurable bindings use the user's remapped labels (via
// m.keyBindings) when present; unconfigured bindings get their built-in labels.
// Sourced from the real key cases in model.go (Update) — not invented. When a
// binding is added/changed there, update this method too.
func (m model) buildKeybindingGroups() []keybindingGroup {
	ctrlCDescription := "cancel the run, then quit"
	if m.btw.active {
		ctrlCDescription = "return to the main session when the BTW run is idle"
	}
	return []keybindingGroup{
		{
			title: "Chat",
			bindings: []keybinding{
				{"Enter", "send the message"},
				{"Shift+Enter / Alt+Enter", "insert a newline (multi-line compose)"},
				{"Esc (\u00d72)", "cancel the run / dismiss a popup / clear the input"},
				{"Ctrl+C", ctrlCDescription},
				{"Ctrl+X then letter", "common slash commands (m=/model, p=/provider, r=/resume, \u2026)"},
				{"Ctrl+X ?", "list every Ctrl+X slash shortcut"},
				{"?", "show this help (on an empty input)"},
			},
		},
		{
			title: "Model & run controls",
			bindings: []keybinding{
				{labelOr(m.keyBindings.cycleReasoning, "Ctrl+T"), "cycle reasoning effort (auto \u2192 low \u2192 medium \u2192 high)"},
				{"Shift+Tab", "cycle permission mode (auto \u2194 ask)"},
			},
		},
		{
			title: "Navigation & scrollback",
			bindings: []keybinding{
				{"PgUp / PgDn", "scroll the transcript by a page"},
				{"\u2191 / \u2193", "scroll, or move within a popup / multi-line input"},
				{"Ctrl+P / Ctrl+N", "previous / next item in menus (emacs-style)"},
				{labelOr(m.keyBindings.toggleDetailed, "Ctrl+O"), "toggle the detailed (full-screen) transcript"},
				{labelOr(m.keyBindings.toggleSidebar, "Ctrl+B"), "show / hide run details"},
				{labelOr(m.keyBindings.toggleMouse, "Ctrl+E"), "release the mouse to drag-select & copy text"},
				{"Tab", "accept the autocomplete / picker selection"},
			},
		},
		{
			title: "Specialists & pickers",
			bindings: []keybinding{
				{"Click a specialist card", "drill into its sub-session"},
				{"\u2191 / Esc (in a sub-session)", "return to the main chat"},
				{"Ctrl+F (in /model)", "toggle the highlighted model as a favorite"},
				{"Click a tool card", "expand / collapse its output"},
				{"Right-click", "paste the clipboard"},
			},
		},
	}
}

// keybindingHelpFooter is the dismiss hint shown at the bottom of the overlay.
const keybindingHelpFooter = "? or Esc to close \u00b7 /help for slash commands"

// renderKeybindingHelpLines builds the overlay body lines (group titles +
// aligned key/description rows + footer), wrapped to the inner width. Exposed
// separately from the framed renderer so tests can assert on content without
// the border chrome.
func (m model) renderKeybindingHelpLines(innerWidth int) []string {
	groups := m.buildKeybindingGroups()
	keyColumn := keybindingKeyColumnWidth(groups)
	lines := make([]string, 0, 64)
	for index, group := range groups {
		if index > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, zeroTheme.accent.Render(group.title))
		for _, binding := range group.bindings {
			lines = append(lines, formatKeybindingLine(binding, keyColumn, innerWidth))
		}
	}
	lines = append(lines, "")
	lines = append(lines, zeroTheme.faint.Render(keybindingHelpFooter))
	return lines
}

// keybindingKeyColumnWidth returns the width of the key column: the widest key
// chord across all groups, so the descriptions align in a clean second column.
func keybindingKeyColumnWidth(groups []keybindingGroup) int {
	widest := 0
	for _, group := range groups {
		for _, binding := range group.bindings {
			if n := len([]rune(binding.keys)); n > widest {
				widest = n
			}
		}
	}
	return widest
}

// formatKeybindingLine renders one "  <keys>   <desc>" row: the key chord in
// the accent color, padded to keyColumn, then the muted description truncated
// to fit innerWidth.
func formatKeybindingLine(binding keybinding, keyColumn int, innerWidth int) string {
	keys := binding.keys
	pad := keyColumn - len([]rune(keys))
	if pad < 0 {
		pad = 0
	}
	keyCell := zeroTheme.ink.Render(keys) + strings.Repeat(" ", pad)
	// Indent(2) + keyCell + gap(2) consumed before the description.
	descBudget := innerWidth - 2 - keyColumn - 2
	if descBudget < 4 {
		descBudget = 4
	}
	desc := zeroTheme.muted.Render(truncateRunes(binding.desc, descBudget))
	return "  " + keyCell + "  " + desc
}

// the given terminal width. Vertical centering is handled by the caller's
// overlay compositing pipeline (overlayViewportLines in transcriptView).
func (m model) renderKeybindingHelpOverlay(width int) string {
	overlayWidth := keybindingHelpOverlayWidth(width)
	lines := m.renderKeybindingHelpLines(overlayWidth - 4)
	block := styledBlockFillTitle(overlayWidth, "Keyboard Shortcuts", lines, zeroTheme.line, lipgloss.NewStyle())
	return centerRenderedBlock(block, width)
}

// keybindingHelpOverlayWidth picks the overlay width: wide enough that the
// descriptions don't truncate next to the key column, capped, and never wider
// than the terminal.
func keybindingHelpOverlayWidth(terminalWidth int) int {
	width := 76
	if terminalWidth-4 < width {
		width = terminalWidth - 4
	}
	if width < 24 {
		width = 24
	}
	return width
}

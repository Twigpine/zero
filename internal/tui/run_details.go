package tui

import "charm.land/lipgloss/v2"

const runDetailsMaxItems = 5
const runDetailsMinWidth = 24

// runDetailsAvailable reports whether the current session has useful state to
// inspect without taking permanent width away from the conversation.
func (m model) runDetailsAvailable() bool {
	return !m.transcriptEmpty() && (m.sidebarHasContent() || m.sidebarTokenText() != "" || m.pending)
}

// runDetailsAllowed keeps the focused summary out of setup and selection flows
// that already own the keyboard.
func (m model) runDetailsAllowed() bool {
	return m.altScreen && m.height > 0 && m.width >= runDetailsMinWidth && !m.subchat.active && !m.transcriptEmpty() &&
		!m.setup.visible && m.providerWizard == nil && m.mcpAddWizard == nil &&
		m.mcpManager == nil && m.picker == nil && m.renamePrompt == nil && !m.suggestionsActive()
}

func (m model) runDetailsOverlay(width int) string {
	if !m.runDetailsOpen || width < runDetailsMinWidth || !m.runDetailsAllowed() {
		return ""
	}
	overlayWidth := minInt(72, maxInt(40, width-8))
	overlayWidth = minInt(overlayWidth, width)
	inner := maxInt(12, overlayWidth-4)
	lines := m.runDetailsLines(inner)
	return centerRenderedBlock(styledBlockFillTitle(overlayWidth, "Run details", lines, zeroTheme.lineStrong, lipgloss.NewStyle()), width)
}

func (m model) runDetailsLines(width int) []string {
	lines := make([]string, 0, 16)
	appendSection := func(header string, rows []string) {
		if len(rows) == 0 {
			return
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, header)
		if len(rows) > runDetailsMaxItems {
			rows = append(append([]string(nil), rows[:runDetailsMaxItems-1]...), "  "+zeroTheme.faint.Render("… more in transcript"))
		}
		lines = append(lines, rows...)
	}

	appendSection(m.sidebarAgentHeader(width), m.sidebarAgentLines(width))
	appendSection(m.sidebarPlanHeader(width), m.sidebarPlanLines(width))
	fileLines, _ := m.sidebarFileLines(width)
	appendSection(m.sidebarFilesHeader(width), fileLines)
	appendSection(sidebarHeader("ACTIVITY", width), m.sidebarActivityLines(width, runDetailsMaxItems))
	if tokens := m.sidebarTokenText(); tokens != "" {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, zeroTheme.faint.Render(tokens))
	}
	if len(lines) == 0 {
		return []string{zeroTheme.faint.Render("No active run details yet.")}
	}
	lines = append(lines, "", zeroTheme.faint.Render("Esc or Ctrl+B closes"))
	return lines
}

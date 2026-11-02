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

// runDetailsVisible reports whether Run details is the overlay currently
// displayed in the viewport. Higher-priority overlays (terminal attach, speech
// API-key prompt, keybinding help, leader help) and the detailed transcript
// view take precedence over Run details in View/transcriptView.
func (m model) runDetailsVisible() bool {
	if !m.runDetailsOpen || !m.runDetailsAllowed() || m.transcriptDetailed ||
		m.helpOverlay || m.leaderHelpOverlay || m.sttKeyPrompt != nil || m.terminalAttach != nil {
		return false
	}
	width := chatWidth(m.width)
	if m.terminalAttachOverlay(width) != "" || m.sttKeyPromptOverlay(width) != "" {
		return false
	}
	return m.runDetailsOverlay(width) != ""
}

func (m model) runDetailsOverlay(width int) string {
	if !m.runDetailsOpen || width < runDetailsMinWidth || !m.runDetailsAllowed() {
		return ""
	}
	overlayWidth := minInt(72, maxInt(40, width-8))
	overlayWidth = minInt(overlayWidth, width)
	inner := maxInt(12, overlayWidth-4)
	lines := m.runDetailsLayout(inner).lines
	return centerRenderedBlock(styledBlockFillTitle(overlayWidth, "Run details", lines, zeroTheme.lineStrong, lipgloss.NewStyle()), width)
}

type runDetailsLayout struct {
	lines     []string
	fileHits  []fileHit
	fileStart int
}

func (m model) runDetailsLayout(width int) runDetailsLayout {
	var out runDetailsLayout
	out.fileStart = -1
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
	fileLines, hits := m.sidebarFileLines(width)
	if len(fileLines) > 0 {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, m.sidebarFilesHeader(width))
		out.fileStart = len(lines)
		kept := fileLines
		if len(kept) > runDetailsMaxItems {
			kept = append(append([]string(nil), fileLines[:runDetailsMaxItems-1]...), "  "+zeroTheme.faint.Render("… more in transcript"))
			var filtered []fileHit
			for _, h := range hits {
				if h.lineOffset < runDetailsMaxItems-1 {
					filtered = append(filtered, h)
				}
			}
			hits = filtered
		}
		lines = append(lines, kept...)
		out.fileHits = hits
	}
	appendSection(sidebarHeader("ACTIVITY", width), m.sidebarActivityLines(width, runDetailsMaxItems))
	if tokens := m.sidebarTokenText(); tokens != "" {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, zeroTheme.faint.Render(tokens))
	}
	if len(lines) == 0 {
		out.lines = []string{zeroTheme.faint.Render("No active run details yet.")}
		return out
	}
	lines = append(lines, "", zeroTheme.faint.Render("Esc or Ctrl+B closes"))
	out.lines = lines
	return out
}

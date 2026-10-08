package tui

import (
	"fmt"
	"strings"
)

func (m model) toggleDetailedTranscript() model {
	m.transcriptDetailed = !m.transcriptDetailed
	m.clearSuggestions()
	if m.fileView.active {
		m = m.exitFileView()
	}
	m.picker = nil
	return m
}

// detailedTranscriptView renders the full (uncapped) transcript viewport.
// Tool output that would be truncated in the live view appears in full here.
func (m model) detailedTranscriptView() string {
	width := chatWidth(m.width)
	items := m.transcriptBodyItems(width, "", true)

	header := detailedTranscriptHeader(width) + "\n" + zeroTheme.line.Render(strings.Repeat("-", width))
	footer := m.detailedTranscriptFooter(width)

	if m.altScreen && m.height > 0 {
		return m.scrollableTranscriptItemsView(header, items, footer, width, "")
	}
	// Popup/fallback: render the full body without viewport clipping.
	layout := layoutTranscriptBodyItems(items)
	return header + "\n" + layout.String() + "\n" + footer
}

// detailedTranscriptFooter builds the one-line footer for the detailed transcript
// view: copy status when active, otherwise the key hint with jump-to-bottom cue.
func (m model) detailedTranscriptFooter(width int) string {
	if copyStatus := strings.TrimSpace(m.copyStatus); copyStatus != "" {
		return rightAlignedLine(zeroTheme.ink.Render(copyStatus), width)
	}

	detailKey := labelOr(m.keyBindings.toggleDetailed, "Ctrl+O")
	hint := zeroTheme.faint.Render(fmt.Sprintf("Esc close | %s toggle", detailKey))
	if jt := m.jumpToBottomHint(); jt != "" {
		return fitStyledLine(joinHeaderLine(hint, jt, width), width)
	}
	return fitStyledLine(hint, width)
}

func detailedTranscriptHeader(width int) string {
	title := zeroTheme.ink.Bold(true).Render("Transcript")
	hint := zeroTheme.faint.Render("detailed")
	return fitStyledLine(joinHeaderLine(title, hint, width), width)
}

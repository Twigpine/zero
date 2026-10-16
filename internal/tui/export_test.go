// Test seams: helpers only test code uses, kept out of the production binary.
package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/Gitlawb/zero/internal/dictation"
	"github.com/Gitlawb/zero/internal/tools"
)

func renderMarkdownInline(text string) string {
	segments := parseMarkdownInline(text)
	var builder strings.Builder
	for _, segment := range segments {
		builder.WriteString(renderMarkdownInlineSegment(segment))
	}
	return builder.String()
}

func completePathQuery(value string, cursorPos int, selectedPath string) (string, int) {
	return completePathQueryWithTrailingSpace(value, cursorPos, selectedPath, true)
}

// matchCommandSuggestions returns commands whose canonical name or any alias has
// the typed prefix (case-insensitive), preserving commandDefinitions order and
// capped at maxCommandSuggestions. A command matched via an alias is still listed
// by its canonical name (completing always inserts the canonical form).
func matchCommandSuggestions(token string) []commandSuggestion {
	return matchCommandSuggestionsWithFilter(token, func(commandDefinition) bool { return true })
}

func formatCommandHelpLines() []string {
	return formatGroupedCommandHelpLines()
}

func formatGroupedCommandHelpLines() []string {
	lines := make([]string, 0, len(commandDefinitions)+len(commandGroupOrder()))
	for _, group := range commandGroupOrder() {
		groupLines := commandHelpLinesForGroup(group)
		if len(groupLines) == 0 {
			continue
		}
		lines = append(lines, string(group)+":")
		lines = append(lines, groupLines...)
	}
	return lines
}

// runningPlanModel supplies plan state for tests that exercise contextual
// details without reviving the former persistent plan presentation.
func runningPlanModel(t *testing.T, steps int) model {
	t.Helper()
	m := newModel(t.Context(), Options{ModelName: "gpt-4"})
	m.width = 100
	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return base.Add(15 * time.Second) }
	items := make([]tools.PlanItem, steps)
	for i := range items {
		status := "pending"
		if i == 0 {
			status = "in_progress"
		}
		items[i] = tools.PlanItem{Content: fmt.Sprintf("Step number %d here", i+1), Status: status}
	}
	m.plan.updateFromItems(items, base)
	return m
}

func listCommandNames() []string {
	names := make([]string, 0, len(commandDefinitions))
	for _, command := range commandDefinitions {
		names = append(names, command.name)
		names = append(names, command.aliases...)
	}
	return names
}

// renderImageChips builds a compact "[Image #1] [Image #2]" row for the pending
// image attachments, or "" when there are none, so the long file name never
// clutters the input. Kept plain so the renderer can wrap/style it consistently.
func renderImageChips(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	chips := make([]string, 0, len(labels))
	for i := range labels {
		chips = append(chips, fmt.Sprintf("[Image #%d]", i+1))
	}
	return strings.Join(chips, " ")
}

func (m model) chatMaxScrollOffset() int {
	_, maxOffset := m.chatScrollMetrics()
	return maxOffset
}

func (m model) scrollableTranscriptLayoutView(header string, body transcriptBodyLayout, footer string, width int, overlay string) string {
	frame := m.scrollableTranscriptFrame(header, footer)
	window := transcriptViewportForLayout(body, frame, m.chatScrollOffset).window()

	bodyWindow := body.visibleLines(window)
	return m.renderScrollableTranscriptWindow(frame, bodyWindow, window, width, overlay)
}

func (m model) overlayMouseTop(overlayHeight int, width int) int {
	return m.overlayMouseRect(overlayHeight, width).y
}

func (c *staticRenderCache) retainedCharacters() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.retained
}

func (c *staticRenderCache) stats() renderCacheStats {
	if c == nil {
		return renderCacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statsData
}

func renderSelectableList(options selectableListOptions) string {
	if len(options.Items) == 0 {
		return ""
	}
	width := options.Width
	if width <= 0 {
		width = 80
	}
	maxVisible := options.MaxVisible
	if maxVisible <= 0 || maxVisible > len(options.Items) {
		maxVisible = len(options.Items)
	}
	selected := clampInt(options.Selected, 0, len(options.Items)-1)
	start := selectableListStart(len(options.Items), maxVisible, selected)
	visible := options.Items[start : start+maxVisible]

	labelWidth := 0
	for _, item := range visible {
		if w := lipgloss.Width(item.Label); w > labelWidth {
			labelWidth = w
		}
	}

	lines := make([]string, 0, maxVisible+1)
	for index, item := range visible {
		absoluteIndex := start + index
		surface := zeroTheme.onPanel
		marker := surface(zeroTheme.faintest).Render("  ")
		if absoluteIndex == selected {
			surface = zeroTheme.onSel
			marker = surface(zeroTheme.accent).Render("❯ ")
		}

		label := surface(zeroTheme.ink).Render(item.Label)
		pad := surface(zeroTheme.ink).Render(strings.Repeat(" ", maxInt(0, labelWidth-lipgloss.Width(item.Label))))
		line := marker + label + pad
		if strings.TrimSpace(item.Description) != "" {
			descWidth := width - lipgloss.Width(marker) - labelWidth - 2
			desc := truncateRunes(item.Description, maxInt(0, descWidth))
			if desc != "" {
				line += surface(zeroTheme.faint).Render("  " + desc)
			}
		}
		lines = append(lines, fitStyledLine(line, width))
	}

	if hidden := len(options.Items) - len(visible); hidden > 0 {
		lines = append(lines, fitStyledLine(zeroTheme.faint.Render(fmt.Sprintf("  %d more", hidden)), width))
	}
	return strings.Join(lines, "\n")
}

// addTokens adds tokens to the running total for the specialist with
// childSessionID. Unknown specialists are ignored.
func (t *specialistTracker) addTokens(childSessionID string, tokens int) {
	for index := range t.specialists {
		if t.specialists[index].childSessionID == childSessionID {
			t.specialists[index].tokenCount += tokens
			return
		}
	}
}

// hasRunning reports whether any tracked specialist is still running.
func (t *specialistTracker) hasRunning() bool {
	for index := range t.specialists {
		if t.specialists[index].status == specialistRunning {
			return true
		}
	}
	return false
}

func borderedBlock(width int, lines []string) string {
	return styledBlock(width, lines, zeroTheme.line)
}

// tailLines returns the last tailCap content lines, including the in-progress one.
func (d *streamingDecoder) tailLines() []string {
	out := append([]string(nil), d.tail...)
	if len(d.cur) > 0 {
		out = append(out, string(d.cur))
	}
	if len(out) > d.tailCap {
		out = out[len(out)-d.tailCap:]
	}
	return out
}

// newSTTDownloadPicker builds the model-download chooser, seeded with the
// curated shortlist. The full model list from the release is fetched
// asynchronously and merged in (see fetchSTTModelsCmd / handleSTTModelsFetched).
func (m model) newSTTDownloadPicker() *commandPicker {
	return newSTTDownloadPickerFrom(dictation.ModelVariants(), true, m.dictation.downloadRoot, m.engineDownloaded(), m.dictation.cfg.LocalModelPath)
}

func toolBodyRendererFor(name string) toolBodyRenderer {
	return defaultToolBodyRegistry.rendererFor(name)
}

// renderSelectableSpecialistRow renders a specialist card and marks every line
// as a clickable specialistCard selectable line carrying the childSessionID.
// A left-click or Enter on any card line drills into that specialist's subchat.
func (m model) renderSelectableSpecialistRow(rowIndex int, row transcriptRow, width int, rc rowContext, startBodyY int) (string, []transcriptSelectableLine) {
	return m.renderSelectableSpecialistRowFn(rowIndex, row, width, rc, startBodyY, m.renderRow)
}

func (m model) renderSelectableToolResultRow(rowIndex int, row transcriptRow, width int, rc rowContext, startBodyY int) (string, []transcriptSelectableLine) {
	opts := cardRenderOptions{bodyCap: cardBodyMaxLines, cwd: m.cwd}
	return m.renderSelectableToolResultRowFn(rowIndex, row, width, rc, startBodyY, m.renderRow, opts)
}

func (m model) transcriptBody(width int, emptyOverlay string) (string, []transcriptSelectableLine) {
	layout := m.transcriptBodyLayout(width, emptyOverlay)
	return layout.String(), layout.selectable
}

func (m model) transcriptBodyLayout(width int, emptyOverlay string) transcriptBodyLayout {
	return layoutTranscriptBodyItems(m.transcriptBodyItems(width, emptyOverlay, false))
}

func (m model) transcriptViewportStart(body string, width int) (int, int, int) {
	frame := m.scrollableTranscriptFrame(m.pinnedTitleBar(width), m.footerView(width))
	return transcriptViewportStartForFrame(body, frame, m.chatScrollOffset)
}

func (l transcriptBodyLayout) visibleLines(window transcriptViewportWindow) []string {
	start := clampInt(window.start, 0, len(l.lines))
	end := clampInt(window.end, start, len(l.lines))
	return append([]string(nil), l.lines[start:end]...)
}

func transcriptViewportStartForFrame(body string, frame transcriptFrameLayout, scrollOffset int) (int, int, int) {
	window := transcriptViewportForBody(body, frame, scrollOffset).window()
	return window.start, window.height, frame.bodyRect.y
}

func transcriptViewportForBody(body string, frame transcriptFrameLayout, offset int) transcriptViewport {
	return newTranscriptViewport(len(viewLines(body)), frame.bodyRect.height, offset)
}

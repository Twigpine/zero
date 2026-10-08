package tui

import (
	"fmt"
	"image/color"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/lipgloss/v2"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/tools"
)

func displayValue(value string, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// pickerBusyText explains that a settings picker (/model, /effort)
// can't be opened while a run is in flight. Opening it then would silently refuse
// the selection once the run lands, so the no-arg command no-ops into this notice.
func pickerBusyText(name string) string {
	label := strings.TrimPrefix(name, "/")
	return renderCommandOutput(commandOutput{
		Title:  label,
		Status: commandStatusWarning,
		Sections: []commandSection{{
			Title: "Busy",
			Lines: []string{"Can't change " + label + " while a run is in progress."},
		}},
		Hints: []string{"press Esc twice to cancel the run, then try again"},
	})
}

func helpText() string {
	// Render /help as a styled command card (accent group headers, two-tone
	// command rows) rather than a flat grey system note.
	return commandCardTranscriptPrefix + formatGroupedCommandHelp()
}

// rowContext carries the cross-row knowledge renderRow needs: which tool
// calls already have results (their call rows collapse into the result card),
// each call's argument hints for the card head, and which calls were
// auto-approved (by permission mode or a stored grant). All maps are keyed by
// run-scoped ids (rcKey): some providers synthesize ToolCallIDs that repeat
// across turns (e.g. Gemini's gemini_tool_N), so a bare id could attribute a
// decision or a result to a different run's call.
type rowContext struct {
	resolved map[string]bool
	hints    map[string]string
	args     map[string]string
	auto     map[string]bool
	decided  map[string]bool
}

func rcKey(runID int, id string) string {
	return strconv.Itoa(runID) + ":" + id
}

func buildRowContext(rows []transcriptRow) rowContext {
	if len(rows) == 0 {
		// Nil maps are safe for all rowContext lookups. This is the steady-state
		// frontier-at-tail path, so avoid allocating maps on every View.
		return rowContext{}
	}
	rc := rowContext{
		resolved: map[string]bool{},
		hints:    map[string]string{},
		args:     map[string]string{},
		auto:     map[string]bool{},
		decided:  map[string]bool{},
	}
	prompted := map[string]bool{}
	for _, row := range rows {
		switch row.kind {
		case rowToolCall:
			if row.id != "" {
				rc.hints[rcKey(row.runID, row.id)] = strings.TrimSpace(row.detail)
				rc.args[rcKey(row.runID, row.id)] = strings.TrimSpace(row.arg)
			}
		case rowToolResult:
			if row.id != "" {
				rc.resolved[rcKey(row.runID, row.id)] = true
			}
		case rowPermission:
			event := row.permission
			if event == nil || event.ToolCallID == "" {
				continue
			}
			key := rcKey(row.runID, event.ToolCallID)
			switch event.Action {
			case agent.PermissionActionPrompt:
				prompted[key] = true
				delete(rc.auto, key)
			case agent.PermissionActionAllow:
				rc.decided[key] = true
				// "auto" means approved without asking: a mode/policy allow or a
				// stored grant match. Any allow that followed a prompt — including a
				// first-time "always" — was a manual decision, not auto.
				if !prompted[key] {
					rc.auto[key] = true
				}
			case agent.PermissionActionDeny:
				rc.decided[key] = true
			case agent.PermissionActionCancel:
				rc.decided[key] = true
			}
		}
	}
	return rc
}

// isHiddenPlumbingTool reports whether a tool is an internal mechanism whose
// human-facing result is rendered elsewhere. Task/TaskOutput are represented by
// specialist cards, and tool_search only loads schemas. Their successful cards
// are suppressed so the transcript remains a narrative of the actual work.
func isHiddenPlumbingTool(name string) bool {
	switch name {
	case "Task", "TaskOutput", "tool_search":
		return true
	}
	return false
}

// toolCallCardSuppressedInTranscript also hides an in-flight update_plan call.
// Its completed result is deliberately kept: renderPlanUpdateCard turns it into
// the durable, readable checklist in the transcript.
func toolCallCardSuppressedInTranscript(name string) bool {
	return isHiddenPlumbingTool(name) || name == "update_plan"
}

// skip reports whether a row renders nothing itself: a tool call whose result
// arrived collapses into the result's card; a permission prompt that has been
// decided collapses away; silent auto-approvals collapse, while manual approval
// decisions remain as audit rows.
func (rc rowContext) skip(row transcriptRow) bool {
	switch row.kind {
	case rowToolCall:
		// Pure-plumbing calls do not have useful standalone content. A successful
		// update_plan is the exception at result time, where it becomes a plan
		// checklist rather than a generic tool card.
		if toolCallCardSuppressedInTranscript(row.tool) {
			return true
		}
		return row.id != "" && rc.resolved[rcKey(row.runID, row.id)]
	case rowToolResult:
		// Hide only successful plumbing results; failures must still surface their
		// specific error even when a dedicated summary normally owns the tool.
		return isHiddenPlumbingTool(row.tool) && row.status != tools.StatusError
	case rowPermission:
		event := row.permission
		if event == nil || event.ToolCallID == "" {
			return false
		}
		key := rcKey(row.runID, event.ToolCallID)
		switch event.Action {
		case agent.PermissionActionPrompt:
			return rc.decided[key]
		case agent.PermissionActionAllow:
			return rc.auto[key]
		}
	}
	return false
}

// cardRenderOptions carries per-render knobs for tool cards: the body-line cap
// (small for the live region, generous for the permanent scrollback flush) and
// the workspace root used to absolutize paths for OSC 8 file hyperlinks.
type cardRenderOptions struct {
	bodyCap  int
	cwd      string
	expanded bool
	// fileSelected marks a tool-result card whose mutation touched the file
	// selected in the FILES sidebar; the card border tints accent so the
	// selection reads in the transcript.
	fileSelected bool
}

// flushCardBodyMaxLines is the body cap for cards flushed to scrollback. The
// small live cap exists only to keep the managed region tidy; history can hold
// full output — most importantly the complete diffs of edited code, which the
// user reviews by scrolling up.
const flushCardBodyMaxLines = 400

func (m model) renderRow(row transcriptRow, width int, rc rowContext) string {
	return m.renderRowMode(row, width, rc, false)
}

func (m model) renderRowDetailed(row transcriptRow, width int, rc rowContext) string {
	opts := cardRenderOptions{bodyCap: 0, cwd: m.cwd}
	if defaultRenderCache != nil {
		if key, stable := m.renderRowCacheKey(row, width, rc, opts, false); key != "" {
			return defaultRenderCache.render(key, stable, func() string {
				return m.renderRowModeUncached(row, width, rc, opts)
			})
		}
	}
	return m.renderRowModeUncached(row, width, rc, opts)
}

// renderRowMode renders a transcript row either for the live region (flush ==
// false: tight body caps, spinner-capable) or for its one-time scrollback
// flush (flush == true: deep body caps so edited code stays reviewable).
func (m model) renderRowMode(row transcriptRow, width int, rc rowContext, flush bool) string {
	opts := cardRenderOptions{bodyCap: cardBodyMaxLines, cwd: m.cwd}
	if flush {
		opts.bodyCap = flushCardBodyMaxLines
	}
	if defaultRenderCache != nil {
		if key, stable := m.renderRowCacheKey(row, width, rc, opts, flush); key != "" {
			return defaultRenderCache.render(key, stable, func() string {
				return m.renderRowModeUncached(row, width, rc, opts)
			})
		}
	}
	return m.renderRowModeUncached(row, width, rc, opts)
}

func (m model) renderRowModeUncached(row transcriptRow, width int, rc rowContext, opts cardRenderOptions) string {
	// Resolved per-row (not at the opts construction sites) so every render
	// path — live, flush, detailed — carries the FILES selection tint; the
	// cache key includes the same predicate (renderRowCacheKey).
	opts.fileSelected = m.rowTouchesSelectedFile(row)
	switch row.kind {
	case rowUser:
		return renderUserRow(row, width)
	case rowAssistant:
		return renderAssistantRow(row, width)
	case rowReasoning:
		return renderReasoningRow(row, width)
	case rowSystem:
		if row.tool == "peer" {
			return renderPeerMessageRow(row.text, width)
		}
		if payload, ok := planCardTranscriptPayload(row.text); ok {
			return renderPlanCardRow(payload, width)
		}
		if payload, ok := commandCardTranscriptPayload(row.text); ok {
			return renderCommandCardRow(payload, width)
		}
		if row.tool == "sessions" {
			return renderSessionsCards(row.text, width)
		}
		if row.tool == "mcp" {
			return renderMCPManagerCard(row.text, width)
		}
		if row.id == compactStatusRowID && strings.HasPrefix(strings.TrimSpace(row.text), "Compressing session") {
			return renderCompactRunningCard(row.text, width)
		}
		if row.id == compactStatusRowID && strings.HasPrefix(strings.TrimSpace(row.text), "Compression complete") {
			return renderCompactCompleteCard(row.text, width)
		}
		if row.id == doctorStatusRowID && strings.HasPrefix(strings.TrimSpace(row.text), "Checking provider") {
			return renderDoctorRunningCard(row.text, width)
		}
		if row.id == doctorStatusRowID {
			return renderDoctorResultCard(row.text, width)
		}
		return renderSystemNote(row.text, width)
	case rowError:
		return renderErrorRow(row, width)
	case rowToolCall:
		return m.renderRunningToolCard(row, width, rc, opts)
	case rowToolResult:
		if isInternalToolArgumentError(row) {
			return ""
		}
		if row.tool == "update_plan" && row.status != tools.StatusError {
			if card, ok := renderPlanUpdateCard(row.detail, width); ok {
				return card
			}
		}
		return renderToolResultCard(row, width, rc, opts)
	case rowPermission:
		return renderPermissionRow(row, width)
	case rowAskUser:
		return renderAskUserRow(row, width)
	case rowSpecialist:
		if row.specialistInfo != nil {
			return m.renderSpecialistCard(*row.specialistInfo, width)
		}
		return ""
	case rowRecap:
		return renderRecapRow(row, width)
	default:
		return row.text
	}
}

// renderRecapRow renders the post-turn "※ recap: …" footnote — a faint one-line
// recap that lands below the answer's done-line (it is a separate transcript row
// appended after the final answer). Uses the same faint metadata style as the
// "worked for …" done-line.
func renderRecapRow(row transcriptRow, width int) string {
	return fitStyledLine(zeroTheme.faint.Render("※ recap: "+strings.TrimSpace(row.text)), width)
}

func isInternalToolArgumentError(row transcriptRow) bool {
	if row.status != tools.StatusError {
		return false
	}
	detail := strings.TrimSpace(row.detail)
	return strings.HasPrefix(detail, "Error: Failed to parse arguments for ") ||
		row.tool == "ask_user" && strings.HasPrefix(detail, "Error: Invalid arguments for ask_user:")
}

// hyperlink wraps already-styled text in an OSC 8 terminal hyperlink so
// supporting terminals (iTerm2, WezTerm, kitty, Ghostty, …) make it clickable
// — cmd/ctrl+click on an edited file opens it. The sequences are zero-width
// for lipgloss/x-ansi width math, and truncateStyledLine skips and re-closes
// them via ansiSequenceEnd.
func hyperlink(url string, text string) string {
	if url == "" || text == "" {
		return text
	}
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

// fileURL builds the file:// link target for a workspace path. The path is
// percent-encoded (spaces especially) — a raw space inside an OSC 8 URI makes
// some terminals terminate the sequence early and print the remainder.
func fileURL(cwd string, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	link := url.URL{Scheme: "file", Path: path}
	return link.String()
}

// looksLikePath reports whether a tool-card target plausibly names a file —
// the only targets worth turning into hyperlinks (bash commands and grep
// patterns also flow through the target column).
func looksLikePath(value string) bool {
	if value == "" || strings.ContainsAny(value, " \t") {
		return false
	}
	return strings.Contains(value, "/") || filepath.Ext(value) != ""
}

// userHomeDir is overridable in tests; os.UserHomeDir in production.
var userHomeDir = os.UserHomeDir

// displayPath shortens an absolute path for the transcript so a tool card shows
// `examples/calc/calc.go` instead of `D:\…\examples\calc\calc.go`. Built-in
// tools already emit workspace-relative paths; this mainly tames MCP tools and
// any tool that surfaces an absolute path. Display-only: never mutate the path
// sent to a tool or stored in the session. The ladder mirrors the reference
// agents — relative under the workspace, `~`-relative under home, else the
// trailing segments with a `…/` prefix:
//
//	under cwd      → examples/calc/calc.go
//	under $HOME    → ~/projects/zero/main.go
//	elsewhere      → …/other/calc.go   (last displayPathTailSegments segments)
//	already short  → returned unchanged (relative input, no separators, etc.)
//
// Output always uses forward slashes so it reads the same on every platform.
const displayPathTailSegments = 3

func displayPath(cwd string, p string) string {
	p = strings.TrimSpace(p)
	if p == "" || !filepath.IsAbs(p) {
		// Relative inputs (the built-in-tool common case) are already short and
		// workspace-anchored; just normalize separators.
		return filepath.ToSlash(p)
	}
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		if rel, err := filepath.Rel(cwd, p); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
			return filepath.ToSlash(rel)
		}
	}
	if home, err := userHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") && rel != "." {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	slashed := filepath.ToSlash(p)
	segments := strings.Split(strings.Trim(slashed, "/"), "/")
	if len(segments) <= displayPathTailSegments {
		return slashed
	}
	return "…/" + strings.Join(segments[len(segments)-displayPathTailSegments:], "/")
}

// sayMeasure is the narrow prose wrap width for compact secondary text.
func sayMeasure(width int) int {
	measure := width - 4
	if measure > 74 {
		measure = 74
	}
	if measure < 16 {
		measure = 16
	}
	return measure
}

// assistantMeasureCap bounds assistant PROSE to a readable line length on wide
// terminals — long measures hurt readability past the ~90-100 col sweet spot.
// Looser than sayMeasure's 74 (this is the main answer); tables and code blocks
// still use the full chat width (the separate tableMeasure arg).
const assistantMeasureCap = 96

// assistantMeasure is the main answer prose wrap width: the chat width, capped at
// assistantMeasureCap, with a 16-col floor. Left-aligned (the cap just shortens
// lines; it does not center).
func assistantMeasure(width int) int {
	measure := width
	if measure > assistantMeasureCap {
		measure = assistantMeasureCap
	}
	if measure < 16 {
		measure = 16
	}
	return measure
}

// wrapPlainText word-wraps unstyled text to the measure, preserving explicit
// newlines AND each line's leading indentation (wrapped continuations keep the
// same indent), so code blocks and aligned lists in assistant answers survive.
// Words longer than the available measure are hard-split so no emitted line
// can exceed it.
func wrapPlainText(text string, measure int) []string {
	if measure < 1 {
		measure = 1
	}
	out := []string{}
	for _, paragraph := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(paragraph) == "" {
			out = append(out, "")
			continue
		}
		body := strings.TrimLeft(paragraph, " \t")
		// Tabs render unpredictably across terminals; a fixed 4-cell indent
		// keeps the width math exact (same policy as the tool cards).
		indent := strings.ReplaceAll(paragraph[:len(paragraph)-len(body)], "\t", "    ")
		if len(indent) >= measure {
			indent = strings.Repeat(" ", measure/2)
		}
		available := measure - len(indent)
		// Preformatted: a body with an internal run of >=2 spaces is aligned
		// content (columns, a table, indented code) where word-wrapping via
		// strings.Fields would collapse the runs and destroy the alignment. Split it
		// verbatim by display width instead, preserving every space. A line that
		// already fits returns unchanged. Leading indent (handled above) and
		// explicit newlines (the outer split) are unaffected.
		if strings.Contains(body, "  ") {
			for _, segment := range splitPreservingWidth(body, available) {
				out = append(out, indent+segment)
			}
			continue
		}
		line := ""
		for _, word := range strings.Fields(body) {
			for lipgloss.Width(word) > available {
				if line != "" {
					out = append(out, indent+line)
					line = ""
				}
				head, tail := splitAtWidth(word, available)
				out = append(out, indent+head)
				word = tail
			}
			switch {
			case line == "":
				line = word
			case lipgloss.Width(line)+1+lipgloss.Width(word) <= available:
				line += " " + word
			default:
				out = append(out, indent+line)
				line = word
			}
		}
		if line != "" {
			out = append(out, indent+line)
		}
	}
	return out
}

// splitAtWidth cuts text at the largest rune boundary whose display width
// fits the measure. CJK and emoji runes are double-width, so slicing by rune
// count would either panic or emit lines up to twice the measure.
func splitAtWidth(text string, measure int) (string, string) {
	used := 0
	for index, glyph := range text {
		glyphWidth := lipgloss.Width(string(glyph))
		if used+glyphWidth > measure {
			if index == 0 {
				// A single glyph wider than the measure still has to go somewhere.
				_, size := utf8.DecodeRuneInString(text)
				return text[:size], text[size:]
			}
			return text[:index], text[index:]
		}
		used += glyphWidth
	}
	return text, ""
}

// splitPreservingWidth breaks text into segments that each fit the measure in
// display width, preserving ALL characters (whitespace included) — the verbatim
// counterpart to the word-wrapper, used for aligned/columnar lines so their
// column spacing survives. A line that already fits returns a single segment.
func splitPreservingWidth(text string, measure int) []string {
	if measure < 1 {
		measure = 1
	}
	var segments []string
	for lipgloss.Width(text) > measure {
		head, tail := splitAtWidth(text, measure)
		if head == "" {
			// splitAtWidth always advances by >=1 rune for measure>=1; the guard
			// just keeps a degenerate input from spinning.
			break
		}
		segments = append(segments, head)
		text = tail
	}
	return append(segments, text)
}

func renderUserRow(row transcriptRow, width int) string {
	contentWidth := userPromptContentWidth(width)
	wrapped := wrapPlainText(row.text, maxInt(1, contentWidth))
	lines := make([]string, 0, len(wrapped)+2)
	// A single plain blank line delimits the turn — no full-width painted band.
	// The ▌ accent gutter alone marks it as the user's, matching the clean
	// reference agents instead of a heavy chat bubble.
	lines = append(lines, "")
	if attachment := renderUserAttachmentSummary(row.attachments); attachment != "" {
		lines = append(lines, renderUserPromptStyledLine(zeroTheme.muted.Render(attachment), contentWidth))
	}
	for _, line := range wrapped {
		lines = append(lines, renderUserPromptStyledLine(zeroTheme.ink.Bold(true).Render(line), contentWidth))
	}
	return strings.Join(lines, "\n")
}

func renderUserAttachmentSummary(summary transcriptAttachmentSummary) string {
	visibleImages := min(summary.images, attachmentSummaryVisibleItems)
	visibleDocuments := min(summary.documents, attachmentSummaryVisibleItems)
	parts := make([]string, 0, visibleImages+visibleDocuments+1)
	for index := 1; index <= visibleImages; index++ {
		parts = append(parts, fmt.Sprintf("[Image #%d]", index))
	}
	for index := 1; index <= visibleDocuments; index++ {
		parts = append(parts, fmt.Sprintf("[Document #%d]", index))
	}
	if hidden := summary.images + summary.documents - visibleImages - visibleDocuments; hidden > 0 {
		parts = append(parts, fmt.Sprintf("[+%d attachments]", hidden))
	}
	return strings.Join(parts, " ")
}

const userPromptPrefix = "▌  "

func userPromptContentWidth(width int) int {
	if width <= 0 {
		return 0
	}
	prefixWidth := lipgloss.Width(userPromptPrefix)
	return maxInt(0, width-prefixWidth)
}

func renderUserPromptStyledLine(styledText string, contentWidth int) string {
	if contentWidth <= 0 {
		return zeroTheme.userPrompt.Render("▌")
	}
	fitted := fitStyledLine(styledText, contentWidth)
	return zeroTheme.userPrompt.Render("▌") + "  " + fitted
}

// renderAssistantRow draws final answers as plain response text plus completion
// metadata; a non-final assistant row (e.g. a rehydrated inline notice) renders
// as interim-style prose.
func renderAssistantRow(row transcriptRow, width int) string {
	tableMeasure := width
	if !row.final {
		// Interim prose is the agent's connective narration ("Now the stylesheet.").
		// Render it as plain prose so the transcript doesn't grow a separate
		// activity rail beside the tool rows.
		narr := renderAssistantMarkdownText(row.text, assistantMeasure(width), tableMeasure, true)
		out := make([]string, 0, len(narr))
		for _, line := range narr {
			styled := styleAssistantMarkdownLine(line, zeroTheme.ink)
			out = append(out, fitStyledLine(styled, width))
		}
		return strings.Join(out, "\n")
	}
	// Committed final answer: highlighting runs here (once, behind the render cache).
	lines := renderAssistantMarkdownText(row.text, assistantMeasure(width), tableMeasure, true)
	for index := range lines {
		lines[index] = styleAssistantMarkdownLine(lines[index], zeroTheme.ink)
	}
	if row.turnElapsed >= longTurnBookend {
		lines = append(lines, doneLine(row))
	}
	return strings.Join(lines, "\n")
}

func renderReasoningRow(row transcriptRow, width int) string {
	// Committed rows live in the scrollable history, so show the whole body (no cap).
	return renderReasoningBlock(row.text, row.expanded, width, false, row.turnElapsed, 0)
}

// renderReasoningBlock renders a reasoning ("Thinking…") block. When expanded and
// maxBodyLines > 0, the body is capped to the LATEST maxBodyLines (with a faint
// "… N earlier" marker), so a live thought stays ~half-screen and its clickable
// toggle header stays on-screen instead of filling the terminal. maxBodyLines = 0
// shows the whole body.
func renderReasoningBlock(text string, expanded bool, width int, running bool, elapsed time.Duration, maxBodyLines int) string {
	text = strings.TrimSpace(text)
	if strings.TrimSpace(text) == "" {
		return ""
	}
	header := reasoningHeaderLine(text, expanded, running, elapsed)
	if !expanded {
		return fitStyledLine(header, width)
	}
	lines := []string{fitStyledLine(header, width)}
	body := renderReasoningBodyLines(text, width)
	if maxBodyLines > 0 && len(body) > maxBodyLines {
		hidden := len(body) - maxBodyLines
		lines = append(lines, fitStyledLine("  "+reasoningHiddenMarker(hidden), width))
		body = body[hidden:]
	}
	for _, line := range body {
		lines = append(lines, fitStyledLine("  "+styleAssistantMarkdownLine(line, zeroTheme.sayText), width))
	}
	return strings.Join(lines, "\n")
}

// reasoningHiddenMarker is the faint line shown in place of capped reasoning body
// lines; the display and selectable paths share it so they stay line-aligned.
func reasoningHiddenMarker(hidden int) string {
	return zeroTheme.faint.Render(fmt.Sprintf("… %d earlier lines · Ctrl+O for all", hidden))
}

func renderReasoningBodyLines(text string, width int) []string {
	measure := maxInt(16, sayMeasure(width)-2)
	// Reasoning bodies can stream and rarely carry code: keep them plain.
	return renderAssistantMarkdownText(strings.TrimSpace(text), measure, measure, false)
}

func reasoningHeaderLine(text string, expanded bool, running bool, elapsed time.Duration) string {
	return zeroTheme.faint.Render(reasoningHeaderText(text, expanded, running, elapsed))
}

func reasoningHeaderText(text string, expanded bool, running bool, elapsed time.Duration) string {
	icon, label := reasoningHeaderParts(text, expanded, running, elapsed)
	if icon == "" {
		return label
	}
	return icon + " " + label
}

func reasoningHeaderParts(_ string, expanded bool, running bool, elapsed time.Duration) (string, string) {
	if running {
		return "", "Thinking"
	}
	icon := "▸"
	if expanded {
		icon = "▾"
	}
	label := "Thought"
	if elapsed > 0 {
		label = fmt.Sprintf("Thought for %s", formatElapsedSeconds(elapsed))
	}
	return icon, label
}

func formatElapsedSeconds(elapsed time.Duration) string {
	return fmt.Sprintf("%.1fs", elapsed.Seconds())
}

// longTurnBookend is the floor a turn must cross to earn a "worked for …"
// terminator. Short turns get none — the next user prompt is the separator,
// matching the clean reference agents — so trivial replies stay uncluttered.
const longTurnBookend = 60 * time.Second

// doneLine is the faint bookend for a long successful turn ("worked for 1m 5s").
// It carries no tool count (the tool cards above already show that) and never
// marks errors (the bordered error note already signals failure).
func doneLine(row transcriptRow) string {
	return zeroTheme.faint.Render("worked for " + formatElapsedSeconds(row.turnElapsed))
}

// renderSystemNote draws a system notice as plain, lightly-marked lines — not a
// heavy box. A run cancellation reads amber ("stopped"); every other notice is
// calm muted info. Multi-line notices keep the marker on the first line and
// indent the continuation so the block still reads as one note.
func renderSystemNote(text string, width int) string {
	trimmed := strings.TrimSpace(text)
	marker, style := zeroTheme.faint.Render("·"), zeroTheme.muted
	if isCancellationNotice(trimmed) {
		marker, style = zeroTheme.amber.Render("⊘"), zeroTheme.amber
	}
	srcLines := strings.Split(trimmed, "\n")
	out := make([]string, 0, len(srcLines))
	for i, line := range srcLines {
		prefix := marker + " "
		if i > 0 {
			prefix = "  " // continuation lines align under the first word
		}
		out = append(out, fitStyledLine(prefix+style.Render(line), width))
	}
	return strings.Join(out, "\n")
}

// renderPeerMessageRow keeps accepted cross-session input lightweight while
// making its origin distinct from direct user input. The body aligns beneath
// the dimmed source label, matching the visual rhythm of other agent messages.
func renderPeerMessageRow(text string, width int) string {
	header, body, _ := strings.Cut(strings.TrimSpace(text), "\n")
	lines := []string{fitStyledLine(zeroTheme.faint.Render("› "+header), width)}
	if body == "" {
		return strings.Join(lines, "\n")
	}
	for _, line := range wrapPlainText(body, maxInt(16, width-2)) {
		lines = append(lines, fitStyledLine("  "+zeroTheme.ink.Render(line), width))
	}
	return strings.Join(lines, "\n")
}

// isCancellationNotice reports whether a system notice is the run-cancelled
// marker (single line, written by the cancel path), so it renders amber.
func isCancellationNotice(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	return !strings.Contains(t, "\n") && strings.HasPrefix(t, "run cancelled")
}

func renderCommandCardRow(text string, width int) string {
	raw := strings.Split(strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n")
	if len(raw) == 0 {
		return renderSystemNote(text, width)
	}

	title := strings.TrimSpace(raw[0])
	lines := make([]string, 0, len(raw)-1)
	for _, line := range raw[1:] {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			lines = append(lines, "")
		case isCommandCardStatusLine(trimmed):
			// "status: info" and the like are structural noise — the card border
			// and title already convey state; only surface a non-ok status.
			if styled := styledCommandCardStatus(trimmed); styled != "" {
				lines = append(lines, styled)
			}
		case isCommandCardActionsLine(trimmed):
			lines = append(lines, zeroTheme.accent.Render("actions: ")+zeroTheme.ink.Render(strings.TrimSpace(strings.TrimPrefix(trimmed, "actions:"))))
		case isCommandCardHintLine(trimmed):
			lines = append(lines, zeroTheme.faint.Render(line))
		case isIndentedCommandCardRow(line):
			// A content row (indented): a "/cmd … - description" gets two-tone
			// styling (bright name, muted description); a "key  value" field or a
			// "- bullet" keeps the value readable rather than dim grey.
			lines = append(lines, styleCommandCardContentRow(line))
		default:
			// A non-indented, non-empty line is a group header (Model, Session…).
			lines = append(lines, zeroTheme.accent.Bold(true).Render(line))
		}
	}
	return styledBlockFillTitle(width, title, lines, zeroTheme.accent, lipgloss.NewStyle())
}

// renderPlanCardRow renders the plan-step detail card with a deliberately
// minimal look: a dim grey border (not the loud lime command-card border), a
// calm status-tinted title, and soft group headers instead of bright accent
// ones. The payload structure is identical to a command card, so it reuses the
// same line classification — only the colours differ.
func renderPlanCardRow(text string, width int) string {
	raw := strings.Split(strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n")
	if len(raw) == 0 {
		return renderSystemNote(text, width)
	}

	title := strings.TrimSpace(raw[0])
	lines := make([]string, 0, len(raw)-1)
	for _, line := range raw[1:] {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			lines = append(lines, "")
		case isCommandCardStatusLine(trimmed):
			// The border + title already convey state; drop the structural
			// "status: …" line entirely for the minimal card.
		case isCommandCardHintLine(trimmed):
			lines = append(lines, zeroTheme.faint.Render(line))
		case isIndentedCommandCardRow(line):
			lines = append(lines, styleCommandCardContentRow(line))
		default:
			// A section group header (What we did / Files changed …): soft grey,
			// not the loud lime accent the command cards use.
			lines = append(lines, zeroTheme.muted.Bold(true).Render(line))
		}
	}
	return styledBlockFillTitleStyled(width, title, lines, zeroTheme.faintest, lipgloss.NewStyle(), planCardTitleStyle(title))
}

// planCardTitleStyle tints the plan card's title by the step state encoded in its
// suffix (· done / · failed / · in progress / · up next), staying calm: success
// green, failure red, otherwise plain ink/faint — no loud lime.
func planCardTitleStyle(title string) lipgloss.Style {
	switch {
	case strings.HasSuffix(title, "· done"):
		return zeroTheme.green
	case strings.HasSuffix(title, "· failed"):
		return zeroTheme.red
	case strings.HasSuffix(title, "· in progress"):
		return zeroTheme.ink
	default: // · up next (pending)
		return zeroTheme.faint
	}
}

// isIndentedCommandCardRow reports whether a line is an indented content row
// (a command, field, or bullet) rather than a group header.
func isIndentedCommandCardRow(line string) bool {
	return strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")
}

// styleCommandCardContentRow two-tones an indented content row. A command row
// ("  /name [args] - description") renders the "/name [args]" half in bright ink
// and the description in muted; a plain field/bullet row keeps the leading
// marker bright and the rest readable. Indentation is preserved.
func styleCommandCardContentRow(line string) string {
	indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
	body := strings.TrimLeft(line, " \t")

	// "/cmd … - description": split on the FIRST " - " so the command (and its
	// arg/alias syntax) stays bright and the prose dims.
	if strings.HasPrefix(body, "/") {
		if name, desc, ok := strings.Cut(body, " - "); ok {
			return indent + zeroTheme.ink.Bold(true).Render(name) + zeroTheme.muted.Render(" — "+desc)
		}
		return indent + zeroTheme.ink.Bold(true).Render(body)
	}
	// "- bullet" list item: keep it readable (not dim grey).
	if strings.HasPrefix(body, "- ") {
		return indent + zeroTheme.ink.Render(body)
	}
	// "key   value" field row: the value carries the information, so keep the
	// whole row in readable ink rather than the old faint grey.
	return indent + zeroTheme.ink.Render(body)
}

// isCommandCardStatusLine reports whether trimmed is a "status: <state>" line.
func isCommandCardStatusLine(trimmed string) bool {
	return strings.HasPrefix(trimmed, "status: ")
}

// styledCommandCardStatus returns a styled status line for a non-ok/non-info
// state (warning/blocked surface in their tint), or "" to drop a neutral
// "status: ok"/"status: info" entirely.
func styledCommandCardStatus(trimmed string) string {
	state := strings.TrimSpace(strings.TrimPrefix(trimmed, "status:"))
	switch state {
	case "warning":
		return zeroTheme.amber.Render(trimmed)
	case "blocked":
		return zeroTheme.red.Render(trimmed)
	default: // ok, info — no signal worth the line
		return ""
	}
}

func isCommandCardActionsLine(trimmed string) bool {
	return strings.HasPrefix(trimmed, "actions:")
}

func isCommandCardHintLine(trimmed string) bool {
	return strings.HasPrefix(trimmed, "hint:")
}

func renderMCPManagerCard(text string, width int) string {
	raw := strings.Split(strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n")
	lines := make([]string, 0, len(raw))
	for index, line := range raw {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			lines = append(lines, "")
		case index == 0:
			lines = append(lines, zeroTheme.accent.Bold(true).Render(line))
		case index == 1:
			lines = append(lines, zeroTheme.ink.Bold(true).Render(line))
		case isMCPManagerHeading(trimmed):
			lines = append(lines, zeroTheme.accent.Bold(true).Render(line))
		case strings.Contains(trimmed, "zero mcp "):
			lines = append(lines, zeroTheme.ink.Render(line))
		case strings.HasPrefix(trimmed, "›") || strings.HasPrefix(trimmed, "- "):
			lines = append(lines, zeroTheme.ink.Render(line))
		default:
			lines = append(lines, zeroTheme.muted.Render(line))
		}
	}
	return styledBlock(width, lines, zeroTheme.accent)
}

func isMCPManagerHeading(value string) bool {
	switch value {
	case "User MCPs", "Built-in MCPs", "Tools", "Permissions", "OAuth", "Actions":
		return true
	default:
		return false
	}
}

func renderCompactRunningCard(text string, width int) string {
	raw := strings.Split(strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n")
	lines := make([]string, 0, len(raw)+1)
	for index, line := range raw {
		switch index {
		case 0:
			lines = append(lines, zeroTheme.amber.Bold(true).Render(line))
		case 1:
			lines = append(lines, zeroTheme.muted.Render(line))
		case 2:
			lines = append(lines, zeroTheme.amber.Bold(true).Render(line))
		default:
			lines = append(lines, zeroTheme.faint.Render(line))
		}
		if index == 0 {
			lines = append(lines, "")
		}
	}
	return styledBlock(width, lines, zeroTheme.amber)
}

func renderCompactCompleteCard(text string, width int) string {
	raw := strings.Split(strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n")
	lines := make([]string, 0, len(raw)+1)
	for index, line := range raw {
		switch index {
		case 0:
			lines = append(lines, zeroTheme.green.Bold(true).Render(line))
		case 1:
			lines = append(lines, zeroTheme.ink.Render(line))
		default:
			lines = append(lines, zeroTheme.muted.Render(line))
		}
		if index == 0 {
			lines = append(lines, "")
		}
	}
	return styledBlock(width, lines, zeroTheme.green)
}

func renderDoctorRunningCard(text string, width int) string {
	raw := strings.Split(strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n")
	lines := make([]string, 0, len(raw)+1)
	for index, line := range raw {
		switch index {
		case 0:
			lines = append(lines, zeroTheme.accent.Bold(true).Render(line))
		case 1:
			lines = append(lines, zeroTheme.muted.Render(line))
		case 2:
			lines = append(lines, zeroTheme.accent.Bold(true).Render(line))
		default:
			lines = append(lines, zeroTheme.faint.Render(line))
		}
		if index == 0 {
			lines = append(lines, "")
		}
	}
	return styledBlock(width, lines, zeroTheme.accent)
}

func renderDoctorResultCard(text string, width int) string {
	raw := strings.Split(strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n")
	border := doctorResultBorderStyle(text)
	lines := make([]string, 0, len(raw))
	for index, line := range raw {
		trimmed := strings.TrimSpace(line)
		switch {
		case index == 0:
			lines = append(lines, border.Bold(true).Render(line))
		case strings.HasPrefix(trimmed, "status:"):
			lines = append(lines, border.Render(line))
		case isDoctorResultHeading(trimmed):
			lines = append(lines, zeroTheme.accent.Bold(true).Render(line))
		case strings.HasPrefix(trimmed, "- [pass]"):
			lines = append(lines, zeroTheme.green.Render(line))
		case strings.HasPrefix(trimmed, "- [warn]"):
			lines = append(lines, zeroTheme.amber.Render(line))
		case strings.HasPrefix(trimmed, "- [fail]"):
			lines = append(lines, zeroTheme.red.Render(line))
		case strings.HasPrefix(trimmed, "hint:"):
			lines = append(lines, zeroTheme.faint.Render(line))
		case strings.HasPrefix(trimmed, "/") ||
			strings.HasPrefix(trimmed, "WSL2") ||
			strings.HasPrefix(trimmed, "install "):
			lines = append(lines, zeroTheme.ink.Render(line))
		default:
			lines = append(lines, zeroTheme.muted.Render(line))
		}
		if index == 0 {
			lines = append(lines, "")
		}
	}
	return styledBlock(width, lines, border)
}

func doctorResultBorderStyle(text string) lipgloss.Style {
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		switch strings.TrimSpace(line) {
		case "status: ok":
			return zeroTheme.green
		case "status: blocked":
			return zeroTheme.red
		case "status: warning":
			return zeroTheme.amber
		}
	}
	return zeroTheme.accent
}

func isDoctorResultHeading(value string) bool {
	switch value {
	case "Summary", "Provider", "Platform", "Other", "Backend", "Actions":
		return true
	default:
		return false
	}
}

func renderErrorRow(row transcriptRow, width int) string {
	note := noteBox(row.text, width, zeroTheme.cardErr, zeroTheme.red)
	// A recognized failure carries a one-line next step. Render it just below the
	// red box in the faint metadata style so it reads as guidance, not more error
	// text (and to avoid nesting ANSI styles inside noteBox's per-line red wrap).
	if hint := strings.TrimSpace(row.hint); hint != "" {
		note += "\n" + fitStyledLine(zeroTheme.faint.Render("→ "+hint), width)
	}
	return note
}

// noteBox is the bordered one-note container behind system and error rows.
func noteBox(text string, width int, borderStyle lipgloss.Style, textStyle lipgloss.Style) string {
	raw := strings.Split(strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n"), "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		lines = append(lines, textStyle.Render(line))
	}
	return styledBlock(width, lines, borderStyle)
}

func renderAskUserRow(row transcriptRow, width int) string {
	line := fitStyledLine(zeroTheme.accent.Render("ask zero")+"  "+zeroTheme.ink.Render(strings.TrimPrefix(row.text, "ask_user: ")), width)
	if detail := strings.TrimSpace(row.detail); detail != "" {
		line += "\n" + wrapDetailBlock(detail, width)
	}
	return line
}

// renderPermissionRow draws the transcript record of a permission event. A
// decided prompt and an auto-approved allow are skipped upstream, so this
// sees: undecided prompts (one amber line + detail), manual decisions (the
// spec's collapsed one-liner), and denials.
func renderPermissionRow(row transcriptRow, width int) string {
	event := row.permission
	if event == nil {
		return zeroTheme.amber.Render("permission") + "  " + zeroTheme.ink.Render(row.text)
	}

	name := event.ToolName
	if name == "" {
		name = row.tool
	}
	displayName := permissionToolDisplayName(name)
	dot := zeroTheme.faintest.Render(" · ")

	switch event.Action {
	case agent.PermissionActionAllow:
		label := "allowed once"
		if event.DecisionAction == agent.PermissionDecisionAlwaysAllowPrefix {
			label = "always prefix"
		} else if event.DecisionAction == agent.PermissionDecisionAllowPrefix {
			label = "allowed prefix"
		} else if event.DecisionAction == agent.PermissionDecisionAllowForSession ||
			(event.Grant != nil && event.Grant.Session) {
			label = "allowed for session"
		} else if event.DecisionAction == agent.PermissionDecisionAlwaysAllow ||
			event.Grant != nil || event.GrantMatched {
			label = "always"
		}
		line := zeroTheme.green.Render(label) + dot + zeroTheme.green.Render(displayName)
		if scope := strings.TrimSpace(event.Scope); scope != "" {
			line += dot + zeroTheme.muted.Render(permissionEventScopeLabel(event)+":"+scope)
		}
		return fitStyledLine(line, width)
	case agent.PermissionActionDeny:
		line := zeroTheme.red.Render("denied") + dot + zeroTheme.red.Render(displayName)
		if scope := strings.TrimSpace(event.Scope); scope != "" {
			line += dot + zeroTheme.muted.Render(permissionEventScopeLabel(event)+":"+scope)
		}
		if reason := permissionDisplayReason(event.Reason); reason != "" {
			line += zeroTheme.faint.Render(" — " + truncateRunes(reason, maxInt(16, width-lipgloss.Width(displayName)-16)))
		}
		out := fitStyledLine(line, width)
		if detail := strings.TrimSpace(row.detail); detail != "" {
			out += "\n" + wrapDetailBlock(detail, width)
		}
		return out
	case agent.PermissionActionCancel:
		line := zeroTheme.red.Render("cancelled") + dot + zeroTheme.red.Render(displayName)
		if scope := strings.TrimSpace(event.Scope); scope != "" {
			line += dot + zeroTheme.muted.Render(permissionEventScopeLabel(event)+":"+scope)
		}
		if reason := permissionDisplayReason(event.Reason); reason != "" {
			line += zeroTheme.faint.Render(" — " + truncateRunes(reason, maxInt(16, width-lipgloss.Width(displayName)-16)))
		}
		out := fitStyledLine(line, width)
		if detail := strings.TrimSpace(row.detail); detail != "" {
			out += "\n" + wrapDetailBlock(detail, width)
		}
		return out
	default:
		line := zeroTheme.amber.Render("permission") + "  " + zeroTheme.ink.Render(displayName) + "  " + zeroTheme.amber.Render("prompt")
		if scope := strings.TrimSpace(event.Scope); scope != "" {
			line += "  " + zeroTheme.muted.Render(permissionEventScopeLabel(event)+":"+scope)
		}
		out := fitStyledLine(line, width)
		if detail := strings.TrimSpace(row.detail); detail != "" {
			out += "\n" + wrapDetailBlock(detail, width)
		}
		return out
	}
}

// wrapDetailBlock wraps a metadata detail blob to the terminal and indents it
// two cells, so no permission/ask row can emit a line wider than the frame.
func wrapDetailBlock(detail string, width int) string {
	lines := wrapPlainText(detail, maxInt(16, width-2))
	for index := range lines {
		lines[index] = "  " + zeroTheme.muted.Render(lines[index])
	}
	return strings.Join(lines, "\n")
}

// renderFocusedPermissionPrompt draws the modal permission card and reports the
// card-relative Y offset of each option line (in permissionOptions order) so the
// caller can register those lines as clickable.
func renderFocusedPermissionPrompt(request agent.PermissionRequest, cursor int, typing bool, feedback string, width int) (string, []int) {
	name := strings.TrimSpace(request.ToolName)
	if name == "" {
		name = "tool"
	}
	// The card body carries no background fill, matching every other prompt card
	// (ask_user, spec review, plan) — see the lipgloss.NewStyle() fills below. The
	// permission card used to tint its whole body with permBg, an amber-family
	// wash that reads as a warm slab on cool themes (e.g. a brown-yellow box over
	// dracula's purples) and made it the one outlier. The amber PERMISSION badge
	// and the amber-mixed border still carry the "this is a permission gate"
	// signal; the body no longer clashes with the surrounding theme.
	fill := func(style lipgloss.Style) lipgloss.Style { return style }

	top := zeroTheme.permBadge.Render(" PERMISSION ")

	body := fill(zeroTheme.amber).Bold(true).Render(name)
	if request.ToolName == peerPermissionToolName {
		top = zeroTheme.permBadge.Render(" PEER MESSAGE ")
		body = fill(zeroTheme.amber).Bold(true).Render("Held message from another session")
	} else if request.ToolName == tools.RequestPermissionsToolName {
		body = fill(zeroTheme.amber).Bold(true).Render("Grant requested permissions?")
	} else if request.SideEffect != "" {
		body += fill(zeroTheme.ink).Render("  " + request.SideEffect)
	}
	lines := []string{top, body}
	if reason := permissionDisplayReason(request.Reason); reason != "" {
		reasonWidth := width
		if widthTier(width) != tierTiny {
			reasonWidth = maxInt(1, width-4)
		}
		for _, line := range wrapPlainText(reason, reasonWidth) {
			lines = append(lines, fill(zeroTheme.muted).Render(line))
		}
	}
	// Surface exactly what the grant covers (file/dir/host) so "always" is a
	// clear, bounded choice rather than a blind tool-wide yes.
	if scope := strings.TrimSpace(request.Scope); scope != "" {
		lines = append(lines, fill(zeroTheme.muted).Render(permissionScopeLine(request, scope)))
	}

	lines = append(lines, "")

	// Feedback mode: the option list is replaced by a free-text field, like the
	// ask_user "type your own answer" surface. What is typed is sent to the model
	// as the denial reason, so it reads the instruction and adjusts.
	if typing {
		lines = append(lines, fill(zeroTheme.muted).Render("Tell Zero what to do differently:"))
		lines = append(lines, zeroTheme.userPrompt.Render("❯ ")+fill(zeroTheme.ink).Render(feedback)+fill(zeroTheme.accent).Render("▌"))
		lines = append(lines, "")
		lines = append(lines, fill(zeroTheme.faint).Render("enter · send to Zero    esc · back to options"))
		return styledBlockFill(width, lines, zeroTheme.permBorder, zeroTheme.permBg), nil
	}

	// Each option is its own line so a click anywhere on that row selects it (no
	// per-column hit-testing). The highlighted row gets a ▸ marker and a reverse
	// label; the rest stay quiet. styledBlockFill prepends exactly one top-border
	// line, so an option at content index i renders at card line i+1 — the offset
	// returned for click registration.
	options := permissionOptions(request)
	cursor = clampPermissionCursor(cursor, request)
	offsets := make([]int, len(options))
	for index, option := range options {
		offsets[index] = 1 + len(lines)
		hotkey := fill(zeroTheme.faint).Render(" [" + option.hotkey + "]")
		optionLabel := permissionOptionLabel(option, request)
		if index == cursor {
			// onSel, not badge. zeroTheme.badge is the brand chip (" 0 ", " ASK ",
			// " SPEC REVIEW ") — a full-brightness accent fill meant for short
			// labels. Using it for a selected ROW painted a bright accent slab
			// across the permission card, fighting the card's amber warning palette
			// and ignoring the card tint every other line composes onto. selBg is
			// the tint tuned for exactly this job ("separates from the panel while
			// ink label contrast stays ~9.4:1"), and onSel is what every other
			// selectable list in the TUI uses for its highlighted row.
			marker := fill(zeroTheme.accent).Render("▸ ")
			label := zeroTheme.onSel(zeroTheme.ink).Bold(true).Render(" " + optionLabel + " ")
			lines = append(lines, marker+label+hotkey)
		} else {
			label := fill(zeroTheme.ink).Render(optionLabel)
			lines = append(lines, "  "+label+hotkey)
		}
	}

	lines = append(lines, "")
	footer := "↑↓ move · enter or click to confirm · [esc] cancel run"
	switch request.ToolName {
	case tools.RequestPermissionsToolName:
		footer = "↑↓ move · enter or click to confirm · [esc] continue without permissions"
	case peerPermissionToolName:
		footer = "↑↓ move · enter or click to confirm · [esc] deny"
	}
	lines = append(lines, fill(zeroTheme.faint).Render(footer))

	return styledBlockFill(width, lines, zeroTheme.permBorder, lipgloss.NewStyle()), offsets
}

func permissionScopeLine(request agent.PermissionRequest, scope string) string {
	if request.ToolName == peerPermissionToolName {
		return "from: " + scope
	}
	if request.ToolName == tools.RequestPermissionsToolName {
		return "permissions: " + scope
	}
	if request.SideEffect == string(tools.SideEffectNetwork) {
		return "target: " + scope
	}
	return "scope: " + scope
}

func permissionOptionLabel(option permissionOption, request agent.PermissionRequest) string {
	if request.ToolName == peerPermissionToolName {
		switch option.choice {
		case permissionDecisionDeny:
			return "Deny — drop this message"
		case permissionDecisionAllow:
			return "Deliver this message to Zero"
		default:
			return option.label
		}
	}
	if request.ToolName == tools.RequestPermissionsToolName {
		switch option.choice {
		case permissionDecisionAllow:
			return "Grant for this task"
		case permissionDecisionAllowStrict:
			return "Grant for this task and ask model to review commands"
		case permissionDecisionAllowForSession:
			return "Grant for this session"
		case permissionDecisionDeny, permissionDecisionCancel:
			return "Continue without permissions"
		default:
			return option.label
		}
	}
	switch option.choice {
	case permissionDecisionAllow:
		if request.SideEffect == string(tools.SideEffectNetwork) {
			return "Yes, just this once"
		}
		return "Yes, proceed"
	case permissionDecisionAllowForSession:
		if request.SideEffect == string(tools.SideEffectNetwork) {
			return "Yes, and allow this host for this conversation"
		}
		if request.SideEffect == string(tools.SideEffectWrite) && strings.TrimSpace(request.Scope) != "" {
			return "Yes, and don't ask again for these files in this session"
		}
		return "Yes, and don't ask again for this command in this session"
	case permissionDecisionAllowPrefix:
		if len(request.CommandPrefix) > 0 {
			return "Yes, and allow `" + strings.Join(request.CommandPrefix, " ") + "` in this session"
		}
		return "Yes, and allow this command prefix in this session"
	case permissionDecisionAlwaysAllowPrefix:
		if len(request.CommandPrefix) > 0 {
			return "Yes, and always allow `" + strings.Join(request.CommandPrefix, " ") + "`"
		}
		return "Yes, and always allow this command prefix"
	case permissionDecisionAlwaysAllow:
		if request.SideEffect == string(tools.SideEffectNetwork) {
			return "Yes, and allow this host in the future"
		}
		if strings.TrimSpace(request.Scope) != "" {
			return "Yes, and don't ask again for this scope"
		}
		return option.label
	case permissionDecisionDeny:
		return "No, continue without running it"
	case permissionDecisionCancel:
		return "No, and tell Zero what to do differently"
	default:
		return option.label
	}
}

func permissionEventScopeLabel(event *agent.PermissionEvent) string {
	if event != nil && event.SideEffect == string(tools.SideEffectNetwork) {
		return "target"
	}
	return "scope"
}

// renderAskUserQuestionnaire draws the ask-user prompt that replaces the composer
// box: a tab row (one per question + a trailing Confirm tab for multi-question
// prompts), the active question's picker or free-text field, and a key-hint footer.
// `input` is the live composer value, echoed in free-text mode.
func renderAskUserQuestionnaire(prompt pendingAskUserPrompt, input string, width int) string {
	questions := prompt.request.Questions
	if len(questions) == 0 {
		return styledBlockFill(width, []string{zeroTheme.badge.Render(" ASK ")}, zeroTheme.lineStrong, lipgloss.NewStyle())
	}
	// The questionnaire replaces the composer, so it paints on the terminal canvas
	// (black) like the composer box — not a gray card. fill is an identity wrapper
	// so existing fill(style) call sites render bare foregrounds on that canvas.
	fill := func(style lipgloss.Style) lipgloss.Style { return style }
	confirm := len(questions)
	active := prompt.active
	if active < 0 {
		active = 0
	}
	if active > confirm {
		active = confirm
	}
	multi := len(questions) > 1

	var lines []string
	lines = append(lines, renderAskUserWaitingState(strings.TrimSpace(prompt.request.Header), width, fill))

	// Tab row (only for multi-question prompts): each question's short title + a
	// trailing Confirm tab; the active tab is a lime badge, answered ones get a ✓.
	if multi {
		tabs := make([]string, 0, len(questions)+1)
		for index, question := range questions {
			title := askUserTabTitle(question, index)
			switch {
			case index == active:
				tabs = append(tabs, zeroTheme.badge.Render(" "+title+" "))
			case prompt.states[index].answered:
				tabs = append(tabs, fill(zeroTheme.muted).Render(" ✓ "+title+" "))
			default:
				tabs = append(tabs, fill(zeroTheme.faint).Render(" "+title+" "))
			}
		}
		if active == confirm {
			tabs = append(tabs, zeroTheme.badge.Render(" Confirm "))
		} else {
			tabs = append(tabs, fill(zeroTheme.faint).Render(" Confirm "))
		}
		lines = append(lines, strings.Join(tabs, " "))
	}

	// Confirm tab: a review of the collected answers.
	if active == confirm {
		lines = append(lines, "")
		lines = append(lines, fill(zeroTheme.ink).Render("Review and submit:"))
		for index, question := range questions {
			answer := strings.TrimSpace(prompt.states[index].answer)
			rendered := fill(zeroTheme.ink).Render(answer)
			if answer == "" {
				rendered = fill(zeroTheme.faint).Render("(no answer)")
			}
			lines = append(lines, "  "+fill(zeroTheme.muted).Render(askUserTabTitle(question, index)+": ")+rendered)
		}
		lines = append(lines, "")
		lines = append(lines, fill(zeroTheme.faint).Render("⇆ tab · enter submit · esc dismiss"))
		return styledBlockFill(width, lines, zeroTheme.lineStrong, lipgloss.NewStyle())
	}

	question := questions[active]
	state := prompt.states[active]
	lines = append(lines, "")
	lines = append(lines, fill(zeroTheme.ink).Render(question.Question))

	if len(question.Options) > 0 && !state.typing {
		// Picker: numbered options (with optional descriptions) + a trailing "type
		// your own" entry; the highlighted row gets a lime ▸ marker and a badge label,
		// the recommended option is tagged inline.
		selectable := askUserSelectableCount(question)
		cursor := clampAskUserCursor(state.cursor, selectable)
		for index, option := range question.Options {
			label := fmt.Sprintf("%d. %s", index+1, option)
			if option == question.Recommended {
				label += "  (recommended)"
			}
			if index == cursor {
				lines = append(lines, fill(zeroTheme.accent).Render("▸ ")+zeroTheme.badge.Render(" "+label+" "))
			} else {
				lines = append(lines, "  "+fill(zeroTheme.ink).Render(label))
			}
			if index < len(question.OptionDescriptions) && strings.TrimSpace(question.OptionDescriptions[index]) != "" {
				lines = append(lines, "     "+fill(zeroTheme.faint).Render(question.OptionDescriptions[index]))
			}
		}
		typeOwn := fmt.Sprintf("%d. %s", len(question.Options)+1, askUserTypeMyOwnLabel)
		if cursor >= len(question.Options) {
			lines = append(lines, fill(zeroTheme.accent).Render("▸ ")+zeroTheme.badge.Render(" "+typeOwn+" "))
		} else {
			lines = append(lines, "  "+fill(zeroTheme.muted).Render(typeOwn))
		}
		lines = append(lines, "")
		footer := "↑↓ select · enter confirm · esc dismiss"
		if multi {
			footer = "⇆ tab · ↑↓ select · enter confirm · esc dismiss"
		}
		lines = append(lines, fill(zeroTheme.faint).Render(footer))
		return styledBlockFill(width, lines, zeroTheme.lineStrong, lipgloss.NewStyle())
	}

	// Free-text mode: the typed answer is echoed here (this region IS the input now).
	if question.MultiSelect && len(question.Options) > 0 {
		lines = append(lines, fill(zeroTheme.muted).Render("suggested: "+strings.Join(question.Options, ", ")))
	}
	lines = append(lines, zeroTheme.userPrompt.Render("❯ ")+fill(zeroTheme.ink).Render(input)+fill(zeroTheme.accent).Render("▌"))
	footer := "enter submit · esc dismiss"
	switch {
	case !question.MultiSelect && len(question.Options) > 0:
		footer = "enter submit · esc back to options"
	case multi:
		footer = "⇆ tab · enter submit · esc dismiss"
	}
	lines = append(lines, fill(zeroTheme.faint).Render(footer))
	return styledBlockFill(width, lines, zeroTheme.lineStrong, lipgloss.NewStyle())
}

// renderAskUserWaitingState makes the paused handoff explicit inside the prompt
// that owns the keyboard. It is intentionally steady: this is user-blocked work,
// not background progress, so a spinner would imply Zero can advance without an
// answer. The state label wins over the optional title on narrow terminals.
func renderAskUserWaitingState(title string, width int, fill func(lipgloss.Style) lipgloss.Style) string {
	state := zeroTheme.accent.Render("●") + " " + fill(zeroTheme.faint).Render("waiting for your answer")
	available := maxInt(1, width-4)
	if title == "" {
		return fitStyledLine(state, available)
	}
	heading := fill(zeroTheme.ink).Render(title)
	if gap := available - lipgloss.Width(heading) - lipgloss.Width(state); gap >= 2 {
		return heading + strings.Repeat(" ", gap) + state
	}
	return fitStyledLine(state, available)
}

// --- Tool cards -------------------------------------------------------------

// cardBodyMaxLines caps every card body; hidden lines collapse into a
// "… N more lines" trailer.
const cardBodyMaxLines = 16

// cardBody is what a result-shape renderer hands back: body lines, an
// optional footer embedded in the bottom border, optional extra head metadata
// (e.g. a read's line range), and whether the rendered body exposes an
// expand/collapse interaction.
type cardBody struct {
	lines     []string
	footer    string
	headTag   string
	canToggle bool
}

// renderRunningToolCard draws the head-only card for a tool call that has no
// result yet: spinner glyph while ITS run is live, a static placeholder for
// orphans (cancelled/errored turns, rehydrated history) — keying off the
// global pending flag alone would re-animate dead cards on every later run.
func (m model) renderRunningToolCard(row transcriptRow, width int, rc rowContext, opts cardRenderOptions) string {
	glyph := zeroTheme.faintest.Render("…")
	active := m.pending && row.runID != 0 && row.runID == m.activeRunID
	if active {
		glyph = zeroTheme.accent.Render("›")
	}
	// The call row carries its own argHints; rc.hints/args only matter for
	// result rows, whose detail is the tool output.
	hint := strings.TrimSpace(row.detail)
	if hint == "" {
		hint = rc.hints[rcKey(row.runID, row.id)]
	}
	arg := strings.TrimSpace(row.arg)
	if arg == "" {
		arg = rc.args[rcKey(row.runID, row.id)]
	}
	// Running cards keep the normal name color; the static accent marker identifies
	// the active operation while the turn-level status owns animation.
	head := toolCardHead(toolRowName(row), hint, arg, "", "", "", true, zeroTheme.ink, rc.auto[rcKey(row.runID, row.id)], width, opts)
	if active {
		return renderLeftRuleCard(width, []string{glyph + " " + head}, zeroTheme.cardRun)
	}
	return toolCard(head, glyph, nil, "", zeroTheme.cardRun, width)
}

func renderToolResultCard(row transcriptRow, width int, rc rowContext, opts cardRenderOptions) string {
	name := toolRowName(row)
	failed := row.status == tools.StatusError
	glyph := zeroTheme.green.Render("•")
	nameStyle := zeroTheme.green
	borderStyle := zeroTheme.line
	if opts.fileSelected {
		// The selected FILES row's edit card: accent border, same as the
		// sidebar's ▸ marker, so click → highlight reads as one gesture.
		borderStyle = zeroTheme.accent
	}
	if failed {
		glyph = zeroTheme.red.Render("•")
		nameStyle = zeroTheme.red
		borderStyle = zeroTheme.cardErr
	}
	key := rcKey(row.runID, row.id)
	headTarget := rc.hints[key]
	headArg := rc.args[key]
	if !failed && isExploreTool(name) {
		headTarget = ""
		headArg = ""
	}
	// A successful call whose only output is a one-line confirmation ("Created
	// examples/calc.go (45 bytes).", "Successfully created directory …") restates
	// what the head already shows (action + target + status dot), so drop the body and let
	// the card collapse to a single line — matching the reference agents' density.
	// Only for clean OK results: errors and anything multi-line keep their body.
	if !failed && opts.bodyCap > 0 && !toolCardAlwaysExpands(name) && looksLikeRedundantConfirmation(row.detail) {
		head := toolCardHead(name, headTarget, headArg, "", row.detail, row.text, false, nameStyle, rc.auto[key], width, opts)
		return toolCard(head, glyph, nil, "", borderStyle, width)
	}
	// Collapse long, noisy output (web-search/MCP/read dumps) by default so the
	// transcript stays scannable; the model still received the full output. Click
	// the card to expand (▸ → ▾) while it is live; collapsed rows flush to
	// scrollback clean. Skipped for: the uncapped detailed view (opts.bodyCap==0),
	// diff tools whose body must stay reviewable, and short output.
	collapsedFooter := ""
	if opts.bodyCap > 0 && !toolCardAlwaysExpands(name) && (failed || (!isExploreTool(name) && !isLocalControlTool(name))) {
		collapsedFooter = collapsedToolFooter(row.detail)
	}
	if collapsedFooter != "" && !row.expanded {
		head := toolCardHead(name, headTarget, headArg, toolResultBudgetTag(row.meta), row.detail, row.text, false, nameStyle, rc.auto[key], width, opts)
		return toolCard(head, glyph, nil, collapsedFooter, borderStyle, width)
	}
	bodyOpts := opts
	bodyOpts.expanded = row.expanded
	body := toolCardBody(name, rc.hints[key], rc.args[key], row.detail, width, bodyOpts, failed)
	head := toolCardHead(name, headTarget, headArg, joinToolHeadTags(body.headTag, toolResultBudgetTag(row.meta)), row.detail, row.text, false, nameStyle, rc.auto[key], width, opts)
	footer := body.footer
	if collapsedFooter != "" && row.expanded && footer == "" {
		footer = "▾ collapse"
	}
	return toolCard(head, glyph, body.lines, footer, borderStyle, width)
}

func joinToolHeadTags(tags ...string) string {
	var present []string
	for _, tag := range tags {
		if tag = strings.TrimSpace(tag); tag != "" {
			present = append(present, tag)
		}
	}
	return strings.Join(present, " · ")
}

func toolResultBudgetTag(meta map[string]string) string {
	if len(meta) == 0 {
		return ""
	}
	reduced := meta["command_output_reduced"] == "true" || meta["truncated"] == "true"
	if !reduced {
		return ""
	}
	original, originalErr := strconv.Atoi(meta["output_budget_estimated_original_tokens"])
	retained, retainedErr := strconv.Atoi(meta["output_budget_estimated_retained_tokens"])
	if commandOriginal, err := strconv.Atoi(meta["command_output_original_tokens"]); err == nil {
		original, originalErr = commandOriginal, nil
	}
	if commandRetained, err := strconv.Atoi(meta["command_output_retained_tokens"]); err == nil {
		retained, retainedErr = commandRetained, nil
	}
	tag := "compact output"
	if originalErr == nil && retainedErr == nil && original > retained {
		tag = fmt.Sprintf("compact %s→%s tok", compactCount(original), compactCount(retained))
	}
	if strings.TrimSpace(meta["spill_path"]) != "" {
		tag += " · raw saved"
	}
	return tag
}

func compactCount(value int) string {
	if value < 1000 {
		return strconv.Itoa(value)
	}
	return fmt.Sprintf("%.1fk", float64(value)/1000)
}

// confirmationVerbPattern matches a single-line success confirmation that only
// restates the action + target: a leading verb ("Created", "Overwrote",
// "Successfully created directory", "Wrote", "Deleted", …) optionally followed
// by a path/detail. Kept deliberately narrow — anything it doesn't recognize
// keeps its body, so the worst case is the status quo, never lost output.
var confirmationVerbPattern = regexp.MustCompile(`(?i)^(successfully\s+\w+|created|overwrote|wrote|updated|edited|deleted|removed|renamed|moved|copied|appended)\b`)

// looksLikeRedundantConfirmation reports whether a tool's output is a single
// short line that merely confirms a mutation (so the card body would just echo
// the head). Multi-line output, or anything not starting with a known
// confirmation verb, is NOT redundant and keeps its body.
func looksLikeRedundantConfirmation(detail string) bool {
	trimmed := strings.TrimSpace(detail)
	if trimmed == "" || strings.Contains(trimmed, "\n") {
		return false
	}
	return confirmationVerbPattern.MatchString(trimmed)
}

// toolCardAlwaysExpands reports tools whose body is a code diff that must stay
// visible for review (mirrors the deeper flush cap intent) rather than collapse.
func toolCardAlwaysExpands(name string) bool {
	switch name {
	case "edit_file", "apply_patch", "write_file":
		return true
	}
	return false
}

// collapsedToolFooter summarizes the hidden output for a collapsed tool card, or
// "" when the output is short enough to render inline. Only output longer than
// the live body cap (the noisy "… N more lines" case, e.g. a web-search dump)
// collapses by default.
func collapsedToolFooter(detail string) string {
	trimmed := strings.TrimRight(detail, "\n")
	if strings.TrimSpace(trimmed) == "" {
		return ""
	}
	n := strings.Count(trimmed, "\n") + 1
	if n <= cardBodyMaxLines {
		return ""
	}
	return fmt.Sprintf("▸ %d lines — click to expand", n)
}

func toolRowName(row transcriptRow) string {
	if row.tool != "" {
		return row.tool
	}
	name := strings.TrimPrefix(row.text, "tool call: ")
	return strings.TrimPrefix(name, "tool result: ")
}

func isExploreTool(name string) bool {
	switch name {
	case "read_file", "read_minified_file", "list_directory", "grep", "glob":
		return true
	}
	return false
}

func isLocalControlTool(name string) bool {
	switch name {
	case "browser_install", "browser_launch", "browser_connect", "browser_open", "browser_snapshot",
		"browser_click", "browser_type", "browser_press", "browser_action",
		"desktop_windows", "desktop_snapshot", "desktop_action",
		"terminal_session", "capture_artifact":
		return true
	}
	return false
}

// toolDisplayName is the human-facing label for a tool card head. Core tools use
// action-oriented labels, and MCP tools are cleaned from "mcp_<server>_<tool>"
// to "<tool>" (e.g. mcp_exa_web_search_exa → "web search").
func toolDisplayName(name string) string {
	switch name {
	case "write_file":
		return "Create"
	case "edit_file":
		return "Edit"
	case "apply_patch":
		return "Patch"
	case "read_file", "read_minified_file":
		return "Read"
	case "list_directory":
		return "List"
	case "grep":
		return "Search"
	case "glob":
		return "Find"
	case "bash", "exec_command":
		return "Run"
	case "write_stdin":
		return "Send input"
	case "browser_install":
		return "Install browser"
	case "browser_launch":
		return "Launch browser"
	case "browser_connect":
		return "Connect browser"
	case "browser_open":
		return "Open browser"
	case "browser_snapshot":
		return "Browser snapshot"
	case "browser_click":
		return "Click"
	case "browser_type":
		return "Type"
	case "browser_press":
		return "Press key"
	case "browser_action":
		return "Browser action"
	case "desktop_windows":
		return "Desktop windows"
	case "desktop_snapshot":
		return "Desktop snapshot"
	case "desktop_action":
		return "Desktop action"
	case "terminal_session":
		return "Terminal session"
	case "capture_artifact":
		return "Capture"
	}
	if !strings.HasPrefix(name, "mcp_") {
		return name
	}
	rest := strings.TrimPrefix(name, "mcp_")
	server := rest
	if i := strings.Index(rest, "_"); i >= 0 {
		server = rest[:i]
		rest = rest[i+1:]
	} else {
		rest = ""
	}
	rest = strings.TrimSuffix(rest, "_"+server) // exa: web_search_exa → web_search
	if rest == "" {
		rest = server
	}
	return strings.ReplaceAll(rest, "_", " ")
}

func toolCardActionLabel(name string, detail string, running bool) string {
	if running {
		switch name {
		case "write_file":
			return "Adding"
		case "edit_file":
			return "Editing"
		case "apply_patch":
			return "Patching"
		case "read_file", "read_minified_file":
			return "Reading"
		case "list_directory":
			return "Listing"
		case "grep":
			return "Searching"
		case "glob":
			return "Finding"
		case "bash", "exec_command":
			return "Running"
		case "write_stdin":
			return "Sending input"
		case "browser_install":
			return "Installing"
		case "browser_launch":
			return "Launching"
		case "browser_connect":
			return "Connecting"
		case "browser_open":
			return "Opening"
		case "browser_snapshot", "desktop_snapshot", "capture_artifact":
			return "Capturing"
		case "browser_click":
			return "Clicking"
		case "browser_type":
			return "Typing"
		case "browser_press":
			return "Pressing"
		case "browser_action", "desktop_action":
			return "Interacting"
		case "desktop_windows":
			return "Listing"
		case "terminal_session":
			return "Running"
		case "update_plan":
			return "Planning"
		case "tool_search":
			return "Preparing tools"
		case "Task":
			return "Delegating"
		default:
			return toolDisplayName(name)
		}
	}
	switch name {
	case "read_file", "read_minified_file", "list_directory", "grep", "glob":
		return "Explored"
	case "write_file":
		meta := diffCardMetadata(detail)
		trimmed := strings.TrimSpace(detail)
		switch {
		case strings.Contains(trimmed, "Overwrote "):
			return "Updated"
		case strings.Contains(trimmed, "Created "):
			return "Added"
		case meta.adds > 0 && meta.dels == 0:
			return "Added"
		case meta.adds > 0 || meta.dels > 0:
			return "Updated"
		default:
			return "Create"
		}
	case "edit_file":
		if strings.TrimSpace(detail) != "" {
			return "Edited"
		}
		return "Edit"
	case "apply_patch":
		if strings.TrimSpace(detail) != "" {
			return "Patched"
		}
		return "Patch"
	case "bash", "exec_command":
		return "Ran"
	case "write_stdin":
		return "Sent input"
	case "browser_install":
		return "Installed"
	case "browser_launch":
		return "Launched"
	case "browser_connect":
		return "Connected"
	case "browser_open":
		return "Opened"
	case "browser_snapshot":
		return "Captured"
	case "browser_click":
		return "Clicked"
	case "browser_type":
		return "Typed"
	case "browser_press":
		return "Pressed"
	case "browser_action":
		return "Ran action"
	case "desktop_windows":
		return "Listed"
	case "desktop_snapshot":
		return "Captured"
	case "desktop_action":
		return "Ran desktop action"
	case "terminal_session":
		return "Ran"
	case "capture_artifact":
		return "Captured"
	default:
		return toolDisplayName(name)
	}
}

func toolCardHeadTarget(name string, target string, detail string) string {
	target = strings.TrimSpace(target)
	if target != "" {
		return target
	}
	if name == "write_file" || name == "edit_file" || name == "apply_patch" || looksLikeDiff(detail) {
		if meta := diffCardMetadata(detail); meta.path != "" {
			return meta.path
		}
	}
	if target := localControlHeadTarget(name, detail); target != "" {
		return target
	}
	return ""
}

func localControlHeadTarget(name string, detail string) string {
	switch name {
	case "browser_snapshot":
		return "snapshot"
	case "desktop_windows":
		return "windows"
	case "desktop_snapshot":
		return "desktop"
	case "browser_install":
		return "runtime"
	case "capture_artifact":
		return artifactCapturedPath(detail)
	default:
		return ""
	}
}

func permissionToolDisplayName(name string) string {
	if isLocalControlTool(name) {
		return toolDisplayName(name)
	}
	return name
}

// toolCardHead composes the card head: user-facing action label,
// middle-truncated target (hyperlinked when it names a file), the faintest arg
// column, optional extra tag, and the auto marker. The status glyph is added by
// toolCard so it leads the head text.
func toolCardHead(name string, target string, arg string, headTag string, detail string, actionDetail string, running bool, nameStyle lipgloss.Style, auto bool, width int, opts cardRenderOptions) string {
	// Color the action label by state (accent running / green done / red failed)
	// so the head row reads at a glance, reinforcing the leading status glyph.
	if actionDetail == "" {
		actionDetail = detail
	}
	head := nameStyle.Bold(true).Render(toolCardActionLabel(name, actionDetail, running))
	if target = singleLineToolHeadText(toolCardHeadTarget(name, target, detail)); target != "" {
		// Show a shortened, workspace-relative path, but keep the hyperlink
		// pointing at the original absolute path so the file still opens.
		shown := target
		if looksLikePath(target) {
			shown = displayPath(opts.cwd, target)
		}
		styled := zeroTheme.toolTarget.Render(middleTruncate(shown, maxInt(16, width/2)))
		if looksLikePath(target) {
			styled = hyperlink(fileURL(opts.cwd, target), styled)
		}
		head += " " + styled
	}
	// The arg column is the first thing the width tiers drop (below 100 cols).
	if arg = singleLineToolHeadText(arg); arg != "" && widthTier(width) == tierFull {
		head += "  " + zeroTheme.toolArg.Render(truncateRunes(arg, maxInt(12, width/3)))
	}
	if headTag != "" {
		if strings.Contains(headTag, "\x1b") {
			head += "  " + headTag
		} else {
			head += "  " + zeroTheme.faint.Render(headTag)
		}
	}
	_ = auto // the permission mode is shown in the composer divider; a per-card [auto] badge is redundant noise
	return head
}

func singleLineToolHeadText(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return ""
	}
	parts := []string{}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r", "\n"), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, " ")
}

// toolCard draws a compact completed-or-static tool block: the status glyph
// leads the head row, body lines follow directly below, and an optional footer
// closes the block. The one live card is rendered separately with a breathing
// rail, so history remains visually quiet.
func toolCard(head string, glyph string, body []string, footer string, _ lipgloss.Style, width int) string {
	if width < 24 {
		width = 24
	}
	innerWidth := width

	// Head line: LEADING status glyph + head. Leading the row with the
	// glyph (status dot / spinner running) puts state in the first cell the
	// eye lands on, instead of right-aligning it to the far card edge. The glyph
	// (and the space after it) is reserved out of the head's width budget so a long
	// head truncates cleanly; the row is right-padded to the full card width.
	glyphWidth := lipgloss.Width(glyph)
	leading := ""
	leadingWidth := 0
	if glyphWidth > 0 {
		leading = glyph + " "
		leadingWidth = glyphWidth + 1
	}
	headBudget := maxInt(1, width-leadingWidth)
	head = fitStyledLine(head, headBudget)
	headPad := maxInt(0, width-leadingWidth-lipgloss.Width(head))
	headLine := leading + head + strings.Repeat(" ", headPad)

	lines := make([]string, 0, len(body)+2)
	lines = append(lines, headLine)
	for _, line := range body {
		fitted := fitStyledLine(line, innerWidth)
		pad := strings.Repeat(" ", maxInt(0, width-lipgloss.Width(fitted)))
		lines = append(lines, fitted+pad)
	}

	if strings.TrimSpace(footer) != "" {
		fittedFooter := fitStyledLine(footer, width)
		pad := strings.Repeat(" ", maxInt(0, width-lipgloss.Width(fittedFooter)))
		lines = append(lines, fittedFooter+pad)
	}
	return strings.Join(lines, "\n")
}

// toolCardBody delegates result-shape selection to the tool body registry.
func toolCardBody(name string, hint string, arg string, detail string, width int, opts cardRenderOptions, failed bool) cardBody {
	return defaultToolBodyRegistry.render(toolBodyRequest{
		name:   name,
		hint:   hint,
		arg:    arg,
		detail: detail,
		width:  width,
		opts:   opts,
		failed: failed,
	})
}

// capCardLines applies the body cap, appending the hidden-count trailer when
// lines were dropped.
func capCardLines(lines []string, cap int) []string {
	if cap <= 0 || len(lines) <= cap {
		return lines
	}
	hidden := len(lines) - cap
	lines = lines[:cap]
	return append(lines, zeroTheme.faint.Render(fmt.Sprintf("… %d more lines", hidden)))
}

func genericCardBody(detail string, opts cardRenderOptions) cardBody {
	raw := strings.Split(detail, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		lines = append(lines, zeroTheme.muted.Render(line))
	}
	return cardBody{lines: capCardLines(lines, opts.bodyCap)}
}

// hunkHeaderPattern extracts the old/new ranges from a unified-diff hunk
// header so the gutter can show real line numbers and distinguish hunk content
// that happens to resemble a file header.
var hunkHeaderPattern = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

type diffHunkState struct {
	oldLine      int
	newLine      int
	oldRemaining int
	newRemaining int
}

func parseDiffHunkHeader(line string) (diffHunkState, bool) {
	match := hunkHeaderPattern.FindStringSubmatch(line)
	if match == nil {
		return diffHunkState{}, false
	}
	oldLine, oldErr := strconv.Atoi(match[1])
	newLine, newErr := strconv.Atoi(match[3])
	if oldErr != nil || newErr != nil {
		return diffHunkState{}, false
	}
	oldRemaining, newRemaining := 1, 1
	if match[2] != "" {
		oldRemaining, oldErr = strconv.Atoi(match[2])
	}
	if match[4] != "" {
		newRemaining, newErr = strconv.Atoi(match[4])
	}
	if oldErr != nil || newErr != nil || oldRemaining < 0 || newRemaining < 0 {
		return diffHunkState{}, false
	}
	return diffHunkState{oldLine: oldLine, newLine: newLine, oldRemaining: oldRemaining, newRemaining: newRemaining}, true
}

func (state diffHunkState) active() bool {
	return state.oldRemaining > 0 || state.newRemaining > 0
}

func (state *diffHunkState) consume(line string) {
	if len(line) == 0 {
		// Some producers omit the leading space on a blank context line. Treat
		// that non-standard but harmless form exactly like normal context so the
		// parser stays aligned with the hunk ranges.
		state.oldRemaining--
		state.newRemaining--
		return
	}
	switch line[0] {
	case ' ':
		state.oldRemaining--
		state.newRemaining--
	case '-':
		state.oldRemaining--
	case '+':
		state.newRemaining--
	}
}

func (state *diffHunkState) consumeContext(count int) {
	state.oldRemaining -= count
	state.newRemaining -= count
	state.oldLine += count
	state.newLine += count
}

func isDiffFileHeader(line string) bool {
	return strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ")
}

type diffMetadata struct {
	path    string
	newFile bool
	adds    int
	dels    int
}

func diffCardMetadata(detail string) diffMetadata {
	meta := diffMetadata{}
	hunk := diffHunkState{}
	for _, line := range strings.Split(detail, "\n") {
		if parsed, ok := parseDiffHunkHeader(line); ok {
			hunk = parsed
			continue
		}
		if hunk.active() && isDiffHunkBodyLine(line) {
			if len(line) > 0 {
				switch line[0] {
				case '+':
					meta.adds++
				case '-':
					meta.dels++
				}
			}
			hunk.consume(line)
			continue
		}
		switch {
		case strings.HasPrefix(line, "+++ "):
			meta.path = diffViewerSourcePath(line)
		case strings.HasPrefix(line, "--- "):
			if strings.TrimSpace(strings.TrimPrefix(line, "--- ")) == "/dev/null" {
				meta.newFile = true
			}
		}
	}
	return meta
}

func diffHeadTag(meta diffMetadata) string {
	return diffCountTag(meta.adds, meta.dels)
}

func diffCountTag(adds int, dels int) string {
	if adds == 0 && dels == 0 {
		return ""
	}
	return zeroTheme.faint.Render("(") +
		zeroTheme.diffAdd.Render(fmt.Sprintf("+%d", adds)) +
		zeroTheme.faint.Render(" ") +
		zeroTheme.diffDel.Render(fmt.Sprintf("-%d", dels)) +
		zeroTheme.faint.Render(")")
}

func diffCardBody(detail string, width int, opts cardRenderOptions) cardBody {
	rawLines := strings.Split(detail, "\n")
	displayLines := compactDiffViewerContext(rawLines)
	meta := diffCardMetadata(detail)
	innerWidth := width
	lines := []string{}

	// The line-number gutter drops below 80 cols (the 60–79 tier). With it,
	// gutter(4) + sign(3) + textBudget == innerWidth; without, sign(3) + text.
	gutter := widthTier(width) >= tierMedium
	gutterWidth := 0
	if gutter {
		gutterWidth = 4
	}
	textBudget := maxInt(8, innerWidth-3-gutterWidth)
	highlightedLines := highlightedDiffLines(rawLines, meta)
	hunk := diffHunkState{}
	for i := 0; i < len(displayLines); i++ {
		displayLine := displayLines[i]
		line := displayLine.text
		switch {
		case displayLine.hiddenContext > 0:
			lines = append(lines, zeroTheme.diffMeta.Render(fmt.Sprintf("… %d unchanged lines", displayLine.hiddenContext)))
			hunk.consumeContext(displayLine.hiddenContext)
		case isDiffFileHeader(line) && !hunk.active():
			// Path and counts live in the tool head row.
		case strings.HasPrefix(line, "diff --git ") && !hunk.active():
			// A subsequent file section is metadata, not context from the preceding
			// hunk. Reset so its headers remain hidden until its next hunk starts.
			hunk = diffHunkState{}
		case strings.HasPrefix(line, "@@"):
			if parsed, ok := parseDiffHunkHeader(line); ok {
				hunk = parsed
			}
			// The line ranges drive the gutters below but do not compete with source
			// content in a compact tool card.
		case !hunk.active():
			// The card head already carries the target path and change counts. Hide
			// Git transport metadata so the viewer opens on the first useful hunk.
			// Preserve unusual pre-hunk output as muted context rather than losing
			// diagnostics from nonstandard diff-producing commands.
			if !isDiffViewerPreamble(line) {
				lines = append(lines, zeroTheme.diffMeta.Render(truncateRunes(line, innerWidth)))
			}
		case strings.HasPrefix(line, `\`):
			// "\ No newline at end of file" has no source-line position.
			lines = append(lines, zeroTheme.diffMeta.Render(truncateRunes(line, innerWidth)))
		case strings.HasPrefix(line, "+"):
			if styled, ok := highlightedLines[displayLine.rawIndex]; ok {
				lines = append(lines, diffBodyStyledLine(hunk.newLine, "+", styled, true, textBudget, gutter))
			} else {
				text := truncateRunes(strings.TrimPrefix(line, "+"), textBudget)
				lines = append(lines, diffBodyLine(hunk.newLine, "+", text, true, textBudget, gutter))
			}
			hunk.newLine++
			hunk.consume(line)
		case strings.HasPrefix(line, "-"):
			// Isolated 1:1 replacement (one "-" immediately followed by one "+"):
			// highlight only the changed span on each side so a one-token edit reads
			// instantly. When a lexer is available the syntax-highlighter carries
			// that same word span; unknown paths retain the established plain-text
			// word-diff fallback.
			if styled, ok := highlightedLines[displayLine.rawIndex]; ok {
				lines = append(lines, diffBodyStyledLine(hunk.oldLine, "−", styled, false, textBudget, gutter))
				hunk.oldLine++
				hunk.consume(line)
				continue
			} else if isIsolatedReplacement(rawLines, displayLine.rawIndex) {
				delText := truncateRunes(strings.TrimPrefix(line, "-"), textBudget)
				addText := truncateRunes(strings.TrimPrefix(rawLines[displayLine.rawIndex+1], "+"), textBudget)
				if delRow, addRow, ok := renderWordDiffPair(hunk.oldLine, hunk.newLine, delText, addText, textBudget, gutter); ok {
					lines = append(lines, delRow, addRow)
					hunk.oldLine++
					hunk.newLine++
					hunk.consume(line)
					hunk.consume(rawLines[displayLine.rawIndex+1])
					i++ // consume the paired "+"
					continue
				}
			}
			text := truncateRunes(strings.TrimPrefix(line, "-"), textBudget)
			lines = append(lines, diffBodyLine(hunk.oldLine, "−", text, false, textBudget, gutter))
			hunk.oldLine++
			hunk.consume(line)
		default:
			if styled, ok := highlightedLines[displayLine.rawIndex]; ok {
				lines = append(lines, diffContextStyledLine(hunk.newLine, styled, textBudget, gutter))
			} else {
				text := truncateRunes(strings.TrimPrefix(line, " "), textBudget)
				row := "   " + zeroTheme.muted.Render(text)
				if gutter {
					row = zeroTheme.faintest.Render(fmt.Sprintf("%4d", hunk.newLine)) + row
				}
				lines = append(lines, row)
			}
			hunk.oldLine++
			hunk.newLine++
			hunk.consume(line)
		}
	}
	return cardBody{lines: capCardLines(lines, opts.bodyCap), headTag: diffHeadTag(meta)}
}

func isDiffViewerPreamble(line string) bool {
	for _, prefix := range []string{
		"diff --git ", "index ", "similarity index ", "dissimilarity index ",
	} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

const (
	diffViewerHighlightMaxLines     = 10_000
	diffViewerHighlightMaxBytes     = 512 * 1024
	diffViewerHighlightMaxLineBytes = 4 * 1024
)

type diffHighlightBudget struct {
	lines int
	bytes int
}

func (budget *diffHighlightBudget) reserve(lines int, bytes int) bool {
	if lines > diffViewerHighlightMaxLines || bytes > diffViewerHighlightMaxBytes ||
		budget.lines+lines > diffViewerHighlightMaxLines || budget.bytes+bytes > diffViewerHighlightMaxBytes {
		return false
	}
	budget.lines += lines
	budget.bytes += bytes
	return true
}

// highlightedDiffLines lexes each unified-diff hunk as one source unit. This
// preserves lexer state through multiline comments, strings, and declarations,
// while still returning independently renderable rows. Large diffs fall back to
// the regular viewer so expanding a tool card remains responsive.
func highlightedDiffLines(rawLines []string, meta diffMetadata) map[int]string {
	highlighted := make(map[int]string)
	budget := diffHighlightBudget{}
	path := ""
	for index := 0; index < len(rawLines); {
		line := rawLines[index]
		switch {
		case isDiffFileHeader(line):
			if candidate := diffViewerSourcePath(line); candidate != "" {
				path = candidate
			}
			index++
			continue
		case !strings.HasPrefix(line, "@@"):
			index++
			continue
		}
		hunk, ok := parseDiffHunkHeader(line)
		if !ok {
			index++
			continue
		}
		start := index + 1
		index = start
		for index < len(rawLines) && hunk.active() && isDiffHunkBodyLine(rawLines[index]) {
			hunk.consume(rawLines[index])
			index++
		}
		hunkPath := path
		if hunkPath == "" {
			hunkPath = meta.path
		}
		if hunkPath == "" || !highlightDiffHunk(rawLines[start:index], start, hunkPath, highlighted, &budget) {
			return nil
		}
	}
	return highlighted
}

func diffViewerSourcePath(line string) string {
	path := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "--- "), "+++ "))
	if tab := strings.IndexByte(path, '\t'); tab >= 0 {
		path = path[:tab]
	}
	if path == "" || path == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(path, "a/") {
		return strings.TrimPrefix(path, "a/")
	}
	return strings.TrimPrefix(path, "b/")
}

func isDiffHunkBodyLine(line string) bool {
	return line == "" || line[0] == ' ' || line[0] == '+' || line[0] == '-' || line[0] == '\\'
}

func highlightDiffHunk(rawLines []string, rawStart int, path string, highlighted map[int]string, budget *diffHighlightBudget) bool {
	content := make([]string, 0, len(rawLines))
	backgrounds := make([]color.Color, 0, len(rawLines))
	rawIndexes := make([]int, 0, len(rawLines))
	contentIndexes := make(map[int]int, len(rawLines))
	bytes := 0
	for offset, line := range rawLines {
		if len(line) == 0 || line[0] == '\\' {
			continue
		}
		prefix := line[0]
		if prefix != ' ' && prefix != '+' && prefix != '-' {
			continue
		}
		contentIndexes[rawStart+offset] = len(content)
		content = append(content, line[1:])
		rawIndexes = append(rawIndexes, rawStart+offset)
		bytes += len(line)
		if len(line) > diffViewerHighlightMaxLineBytes {
			return false
		}
		switch prefix {
		case '+':
			backgrounds = append(backgrounds, zeroTheme.addLine.GetBackground())
		case '-':
			backgrounds = append(backgrounds, zeroTheme.delLine.GetBackground())
		default:
			backgrounds = append(backgrounds, nil)
		}
	}
	if len(content) == 0 {
		return true
	}
	if !budget.reserve(len(content), bytes) {
		return false
	}

	spans := diffHunkWordSpans(rawLines, rawStart, contentIndexes)
	styled, ok := highlightCodeForPathWithLineBackgrounds(content, path, 1<<20, backgrounds, spans)
	if !ok || len(styled) != len(content) {
		return false
	}
	for contentIndex, text := range styled {
		highlighted[rawIndexes[contentIndex]] = text
	}
	return true
}

func diffHunkWordSpans(rawLines []string, rawStart int, contentIndexes map[int]int) []highlightSpan {
	var spans []highlightSpan
	for offset, line := range rawLines {
		rawIndex := rawStart + offset
		if !isDiffDelContent(line) || offset+1 >= len(rawLines) || !isDiffAddContent(rawLines[offset+1]) {
			continue
		}
		if offset > 0 && isDiffDelContent(rawLines[offset-1]) {
			continue
		}
		if offset+2 < len(rawLines) && isDiffAddContent(rawLines[offset+2]) {
			continue
		}
		before := []rune(line[1:])
		after := []rune(rawLines[offset+1][1:])
		prefix, beforeEnd, afterEnd := changedSpan(before, after)
		changed := maxInt(beforeEnd-prefix, afterEnd-prefix)
		if changed == 0 || float64(changed)/float64(maxInt(len(before), len(after))) > 0.6 {
			continue
		}
		if lineIndex, ok := contentIndexes[rawIndex]; ok {
			spans = append(spans, highlightSpan{line: lineIndex, start: prefix, end: beforeEnd, background: zeroTheme.delLineWord.GetBackground()})
		}
		if lineIndex, ok := contentIndexes[rawIndex+1]; ok {
			spans = append(spans, highlightSpan{line: lineIndex, start: prefix, end: afterEnd, background: zeroTheme.addLineWord.GetBackground()})
		}
	}
	return spans
}

// diffBodyLine paints one changed row: optional gutter number, sign column,
// and text padded to the full budget, all on the add/del tint so the row
// reads as one solid band edge to edge.
func diffBodyLine(number int, sign string, text string, added bool, textBudget int, gutter bool) string {
	if pad := textBudget - lipgloss.Width(text); pad > 0 {
		text += strings.Repeat(" ", pad)
	}
	numCol := ""
	if gutter {
		num := fmt.Sprintf("%4d", number)
		if added {
			numCol = zeroTheme.addLineNum.Render(num)
		} else {
			numCol = zeroTheme.delLineNum.Render(num)
		}
	}
	if added {
		return numCol + zeroTheme.addSign.Render(" "+sign+" ") + zeroTheme.addLine.Render(text)
	}
	return numCol + zeroTheme.delSign.Render(" "+sign+" ") + zeroTheme.delLine.Render(text)
}

func diffBodyStyledLine(number int, sign string, styledText string, added bool, textBudget int, gutter bool) string {
	lineStyle, signStyle, numStyle := zeroTheme.delLine, zeroTheme.delSign, zeroTheme.delLineNum
	if added {
		lineStyle, signStyle, numStyle = zeroTheme.addLine, zeroTheme.addSign, zeroTheme.addLineNum
	}
	styledText = fitStyledLine(styledText, textBudget)
	if pad := textBudget - lipgloss.Width(styledText); pad > 0 {
		styledText += lineStyle.Render(strings.Repeat(" ", pad))
	}
	numCol := ""
	if gutter {
		numCol = numStyle.Render(fmt.Sprintf("%4d", number))
	}
	return numCol + signStyle.Render(" "+sign+" ") + styledText
}

func diffContextStyledLine(number int, styledText string, textBudget int, gutter bool) string {
	styledText = fitStyledLine(styledText, textBudget)
	if pad := textBudget - lipgloss.Width(styledText); pad > 0 {
		styledText += zeroTheme.muted.Render(strings.Repeat(" ", pad))
	}
	row := "   " + styledText
	if gutter {
		row = zeroTheme.faintest.Render(fmt.Sprintf("%4d", number)) + row
	}
	return row
}

func isDiffAddContent(s string) bool {
	return strings.HasPrefix(s, "+") && !strings.HasPrefix(s, "+++ ")
}
func isDiffDelContent(s string) bool {
	return strings.HasPrefix(s, "-") && !strings.HasPrefix(s, "--- ")
}

// isIsolatedReplacement reports whether rawLines[i] (already a deleted content
// line) is a single delete immediately followed by a single add — a 1:1 swap
// where an intra-line word diff is meaningful. It rejects multi-line delete or
// add blocks (where pairing would be wrong).
func isIsolatedReplacement(rawLines []string, i int) bool {
	if i+1 >= len(rawLines) || !isDiffAddContent(rawLines[i+1]) {
		return false
	}
	if i > 0 && isDiffDelContent(rawLines[i-1]) {
		return false // part of a multi-line delete block
	}
	if i+2 < len(rawLines) && isDiffAddContent(rawLines[i+2]) {
		return false // part of a multi-line add block
	}
	return true
}

// changedSpan returns the rune index p where a and b first diverge, plus the
// exclusive ends in a and b after the common suffix. The changed middle is
// a[p:aEnd] / b[p:bEnd]; equal strings yield p==aEnd==bEnd.
func changedSpan(a, b []rune) (p, aEnd, bEnd int) {
	n := minInt(len(a), len(b))
	for p < n && a[p] == b[p] {
		p++
	}
	s := 0
	for s < n-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	return p, len(a) - s, len(b) - s
}

// renderWordDiffPair renders a deleted/added pair with only the changed span
// highlighted. ok is false (caller falls back to whole-line tinting) when the
// change covers more than 60% of the longer line — a near-rewrite, where
// per-word highlighting is just noise.
func renderWordDiffPair(oldLine, newLine int, delText, addText string, textBudget int, gutter bool) (string, string, bool) {
	del := []rune(delText)
	add := []rune(addText)
	p, delEnd, addEnd := changedSpan(del, add)
	longer := maxInt(len(del), len(add))
	changed := maxInt(delEnd-p, addEnd-p)
	if longer == 0 || changed <= 0 || float64(changed)/float64(longer) > 0.6 {
		return "", "", false
	}
	delRow := diffBodyLineSpanned(oldLine, "−", del, false, p, delEnd, textBudget, gutter)
	addRow := diffBodyLineSpanned(newLine, "+", add, true, p, addEnd, textBudget, gutter)
	return delRow, addRow, true
}

// diffBodyLineSpanned is diffBodyLine with the [spanStart,spanEnd) rune range of
// text painted on the brighter changed-span bg.
func diffBodyLineSpanned(number int, sign string, text []rune, added bool, spanStart, spanEnd, textBudget int, gutter bool) string {
	if spanStart < 0 {
		spanStart = 0
	}
	if spanEnd > len(text) {
		spanEnd = len(text)
	}
	if spanEnd < spanStart {
		spanEnd = spanStart
	}
	lineStyle, wordStyle, signStyle, numStyle := zeroTheme.delLine, zeroTheme.delLineWord, zeroTheme.delSign, zeroTheme.delLineNum
	if added {
		lineStyle, wordStyle, signStyle, numStyle = zeroTheme.addLine, zeroTheme.addLineWord, zeroTheme.addSign, zeroTheme.addLineNum
	}
	pre := string(text[:spanStart])
	mid := string(text[spanStart:spanEnd])
	post := string(text[spanEnd:])
	if pad := textBudget - lipgloss.Width(string(text)); pad > 0 {
		post += strings.Repeat(" ", pad)
	}
	body := lineStyle.Render(pre) + wordStyle.Render(mid) + lineStyle.Render(post)
	numCol := ""
	if gutter {
		numCol = numStyle.Render(fmt.Sprintf("%4d", number))
	}
	return numCol + signStyle.Render(" "+sign+" ") + body
}

func exploreCardBody(name string, hint string, arg string, detail string, width int, opts cardRenderOptions, failed bool) cardBody {
	if failed {
		return genericCardBody(detail, opts)
	}
	summary := exploreCardLine(name, hint, arg, detail, width, opts, "└")
	if !opts.expanded && opts.bodyCap > 0 {
		footer := ""
		if strings.TrimSpace(detail) != "" {
			footer = zeroTheme.faint.Render("▸ details")
		}
		return cardBody{lines: []string{summary}, footer: footer, canToggle: footer != ""}
	}
	body := exploreDetailCardBody(name, detail, width, opts)
	lines := append([]string{summary}, body.lines...)
	footer := body.footer
	canToggle := body.canToggle
	if opts.expanded && opts.bodyCap > 0 && footer == "" {
		footer = zeroTheme.faint.Render("▾ collapse")
		canToggle = true
	}
	return cardBody{lines: lines, footer: footer, canToggle: canToggle}
}

func exploreDetailCardBody(name string, detail string, width int, opts cardRenderOptions) cardBody {
	switch name {
	case "grep":
		return grepCardBody(detail, width, opts)
	default:
		return genericCardBody(detail, opts)
	}
}

func exploreCardLine(name string, hint string, arg string, detail string, width int, opts cardRenderOptions, marker string) string {
	if marker == "" {
		marker = "└"
	}
	action := exploreChildAction(name)
	target := exploreTarget(name, hint, arg, detail)
	line := zeroTheme.faint.Render("  "+marker+" ") + zeroTheme.green.Render(action)
	if target != "" {
		shown := target
		isPath := exploreTargetLooksLikePath(name, target)
		if isPath {
			shown = displayPath(opts.cwd, target)
		}
		styled := zeroTheme.toolTarget.Render(middleTruncate(shown, maxInt(8, width-lipgloss.Width(action)-6)))
		if isPath {
			styled = hyperlink(fileURL(opts.cwd, target), styled)
		}
		line += " " + styled
	}
	return line
}

func exploreChildAction(name string) string {
	switch name {
	case "read_file", "read_minified_file":
		return "Read"
	case "list_directory":
		return "List"
	case "grep":
		return "Search"
	case "glob":
		return "Find"
	default:
		return toolDisplayName(name)
	}
}

func exploreTargetLooksLikePath(name string, target string) bool {
	switch name {
	case "read_file", "read_minified_file", "list_directory":
		return looksLikePath(target)
	default:
		return false
	}
}

func exploreTarget(name string, hint string, arg string, detail string) string {
	switch name {
	case "read_file", "read_minified_file":
		if target := strings.TrimSpace(hint); target != "" {
			return target
		}
		return readDetailPath(detail)
	case "grep":
		if target := strings.TrimSpace(arg); target != "" {
			return target
		}
		return strings.TrimSpace(hint)
	case "glob":
		if target := strings.TrimSpace(arg); target != "" {
			return target
		}
		return strings.TrimSpace(hint)
	case "list_directory":
		return strings.TrimSpace(hint)
	default:
		return strings.TrimSpace(hint)
	}
}

func readDetailPath(detail string) string {
	for _, line := range strings.Split(detail, "\n") {
		if strings.HasPrefix(line, "File: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "File: "))
		}
	}
	return ""
}

func localControlCardBody(name string, hint string, detail string, width int, opts cardRenderOptions, failed bool) cardBody {
	if failed {
		return genericCardBody(detail, opts)
	}
	switch name {
	case "browser_snapshot", "desktop_windows", "desktop_snapshot", "capture_artifact":
		return cardBody{}
	case "browser_open":
		if summary := browserOpenSummary(detail, hint); summary != "" {
			return cardBody{lines: []string{localControlChildLine(summary, width)}}
		}
		return cardBody{}
	default:
		if summary := firstLocalControlSummaryLine(detail); summary != "" {
			return cardBody{lines: []string{localControlChildLine(summary, width)}}
		}
		return cardBody{}
	}
}

func localControlChildLine(text string, width int) string {
	prefix := "  └ "
	budget := maxInt(8, width-lipgloss.Width(prefix))
	return zeroTheme.faint.Render(prefix) + zeroTheme.muted.Render(truncateDisplayWidth(text, budget))
}

func browserOpenSummary(detail string, target string) string {
	target = strings.TrimRight(strings.TrimSpace(target), "/")
	for _, line := range strings.Split(detail, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "✓"))
		if line == "" {
			continue
		}
		normalized := strings.TrimRight(line, "/")
		if target != "" && normalized == target {
			continue
		}
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			continue
		}
		return line
	}
	return ""
}

func firstLocalControlSummaryLine(detail string) string {
	for _, line := range strings.Split(detail, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "✓"))
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "Artifact captured:"):
			continue
		default:
			return line
		}
	}
	return ""
}

func artifactCapturedPath(detail string) string {
	for _, line := range strings.Split(detail, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Artifact captured:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Artifact captured:"))
		}
	}
	return ""
}

func bashCardBody(command string, detail string, width int, opts cardRenderOptions) cardBody {
	footer := ""
	output := []commandOutputLine{}
	section := "stdout"
	for _, line := range strings.Split(detail, "\n") {
		switch {
		case line == "stdout:":
			section = "stdout"
		case line == "stderr:":
			section = "stderr"
		case strings.HasPrefix(line, "exit_code: "):
			code := strings.TrimPrefix(line, "exit_code: ")
			if code != "0" {
				footer = zeroTheme.red.Render("exit " + code)
			}
		default:
			style := zeroTheme.muted
			if section == "stderr" {
				style = zeroTheme.delText
			}
			output = append(output, commandOutputLine{text: line, style: style})
		}
	}
	lines := renderCommandOutputLines(output, width, opts)
	return cardBody{lines: capCardLines(lines, opts.bodyCap), footer: footer}
}

func execCommandCardBody(command string, detail string, width int, opts cardRenderOptions) cardBody {
	footer := ""
	output := []commandOutputLine{}
	section := ""
	interrupted := false
	for _, line := range strings.Split(detail, "\n") {
		switch {
		case line == "output:":
			section = "output"
		case line == "interrupted: true":
			interrupted = true
			footer = zeroTheme.green.Render("interrupted")
		case strings.HasPrefix(line, "exit_code: "):
			code := strings.TrimPrefix(line, "exit_code: ")
			if code == "0" {
				// Successful completion is already visible from the green status dot.
			} else if interrupted {
				footer = zeroTheme.green.Render("interrupted")
			} else {
				footer = zeroTheme.red.Render("exit " + code)
			}
		case strings.HasPrefix(line, "session_id: "):
			footer = zeroTheme.faint.Render("session " + strings.TrimSpace(strings.TrimPrefix(line, "session_id: ")))
		case strings.HasPrefix(line, "Use write_stdin "):
			continue
		default:
			style := zeroTheme.muted
			if section == "" && strings.HasPrefix(line, "Command is still running.") {
				style = zeroTheme.faint
			}
			output = append(output, commandOutputLine{text: line, style: style})
		}
	}
	lines := renderCommandOutputLines(output, width, opts)
	return cardBody{lines: capCardLines(lines, opts.bodyCap), footer: footer}
}

type commandOutputLine struct {
	text  string
	style lipgloss.Style
}

func renderCommandOutputLines(output []commandOutputLine, width int, opts cardRenderOptions) []string {
	output = trimTrailingEmptyCommandOutput(output)
	if len(output) == 0 {
		return nil
	}

	if len(output) == 1 {
		prefix := "  └ "
		budget := maxInt(8, width-lipgloss.Width(prefix))
		text := truncateDisplayWidth(output[0].text, budget)
		return capCardLines([]string{zeroTheme.faint.Render(prefix) + output[0].style.Render(text)}, opts.bodyCap)
	}

	lines := make([]string, 0, len(output))
	prefix := "  │ "
	budget := maxInt(8, width-lipgloss.Width(prefix))
	for _, item := range output {
		text := truncateDisplayWidth(item.text, budget)
		lines = append(lines, zeroTheme.faint.Render(prefix)+item.style.Render(text))
	}
	return capCardLines(lines, opts.bodyCap)
}

func trimTrailingEmptyCommandOutput(output []commandOutputLine) []commandOutputLine {
	for len(output) > 0 && strings.TrimSpace(output[len(output)-1].text) == "" {
		output = output[:len(output)-1]
	}
	return output
}

func truncateDisplayWidth(text string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if lipgloss.Width(text) <= budget {
		return text
	}
	if budget <= 1 {
		return "…"
	}
	head, _ := splitAtWidth(text, budget-1)
	return head + "…"
}

// renderSessionsCards draws the /resume list as stacked cards: id (accent) +
// age (faint) on the top row, title (ink), then the meta line (faint with
// faintest dots). Records arrive as sessionsCardFieldSep-joined fields; a
// record without separators is a plain trailing hint.
func renderSessionsCards(payload string, width int) string {
	blocks := []string{}
	for _, record := range strings.Split(payload, "\n") {
		fields := strings.Split(record, sessionsCardFieldSep)
		if len(fields) < 4 {
			blocks = append(blocks, fitStyledLine(zeroTheme.faint.Render(record), width))
			continue
		}
		id, age, title, meta := fields[0], fields[1], fields[2], fields[3]
		innerWidth := width - 4
		top := joinHeaderLine(zeroTheme.onPanel(zeroTheme.accent).Render(id), zeroTheme.onPanel(zeroTheme.faint).Render(age), innerWidth)
		metaParts := strings.Split(meta, " · ")
		for index := range metaParts {
			metaParts[index] = zeroTheme.onPanel(zeroTheme.faint).Render(metaParts[index])
		}
		lines := []string{
			top,
			zeroTheme.onPanel(zeroTheme.ink).Render(title),
			strings.Join(metaParts, zeroTheme.onPanel(zeroTheme.faintest).Render(" · ")),
		}
		blocks = append(blocks, styledBlockFill(width, lines, zeroTheme.line, zeroTheme.panel))
	}
	return strings.Join(blocks, "\n")
}

// grepMatchPattern matches the grep tool's "path:line: text" content rows.
var grepMatchPattern = regexp.MustCompile(`^(.+?:\d+):\s?(.*)$`)

func grepCardBody(detail string, width int, opts cardRenderOptions) cardBody {
	innerWidth := width
	raw := strings.Split(detail, "\n")
	lines := make([]string, 0, len(raw))
	matches := 0
	for _, line := range raw {
		if match := grepMatchPattern.FindStringSubmatch(line); match != nil {
			matches++
			location := zeroTheme.grepLoc.Render(match[1])
			// match[1] is "path:line" — link the file so a hit is one click away.
			if path, _, ok := strings.Cut(match[1], ":"); ok && path != "" {
				location = hyperlink(fileURL(opts.cwd, path), location)
			}
			budget := maxInt(8, innerWidth-lipgloss.Width(match[1])-2)
			lines = append(lines, location+"  "+zeroTheme.muted.Render(truncateDisplayWidth(match[2], budget)))
			continue
		}
		lines = append(lines, zeroTheme.muted.Render(line))
	}
	footer := ""
	if matches > 0 {
		footer = zeroTheme.faint.Render(fmt.Sprintf("%d matches", matches))
	}
	return cardBody{lines: capCardLines(lines, opts.bodyCap), footer: footer}
}

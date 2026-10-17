// specialist_card.go renders specialist/subagent cards in the transcript.
//
// A specialist card summarises one spawned sub-agent (worker, explorer, code
// review, ...): its name, task description, elapsed time, tool-call count, and
// token usage. The SpecialistTracker holds the live state that the transcript
// view consults each render; the session store feeds it via start/complete/
// incrementToolCount/addTokens as specialist events arrive.
package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/Twigpine/zero/internal/streamjson"
)

// specialistStatus is the lifecycle state of a single specialist invocation.
type specialistStatus int

const (
	specialistRunning specialistStatus = iota
	specialistCompleted
	specialistError
)

// specialistInfo is the rendered view of one specialist invocation.
type specialistInfo struct {
	name           string
	description    string
	childSessionID string
	status         specialistStatus
	startedAt      time.Time
	completedAt    time.Time
	exitCode       int
	errorMsg       string
	toolCount      int // number of tool calls made by this specialist
	tokenCount     int // total tokens consumed
	currentTool    string
	currentDetail  string
}

// specialistTracker holds the live state for every specialist the parent agent
// has spawned in the current turn. Lookups are by childSessionID.
type specialistTracker struct {
	specialists []specialistInfo
}

// start adds a new specialist entry. If childSessionID already exists the
// existing entry is updated in place (so a duplicate start event is idempotent).
func (t *specialistTracker) start(name, description, childSessionID string, now time.Time) {
	for index := range t.specialists {
		if t.specialists[index].childSessionID == childSessionID {
			t.specialists[index].name = name
			t.specialists[index].description = description
			t.specialists[index].status = specialistRunning
			t.specialists[index].startedAt = now
			t.specialists[index].completedAt = time.Time{}
			t.specialists[index].exitCode = 0
			t.specialists[index].errorMsg = ""
			return
		}
	}
	t.specialists = append(t.specialists, specialistInfo{
		name:           name,
		description:    description,
		childSessionID: childSessionID,
		status:         specialistRunning,
		startedAt:      now,
	})
}

// complete marks the specialist with childSessionID as finished, recording the
// terminal status, exit code, and any error message. Specialists that are not
// tracked are ignored.
func (t *specialistTracker) complete(childSessionID string, status specialistStatus, exitCode int, errorMsg string, now time.Time) {
	for index := range t.specialists {
		if t.specialists[index].childSessionID == childSessionID {
			t.specialists[index].status = status
			t.specialists[index].exitCode = exitCode
			t.specialists[index].errorMsg = errorMsg
			t.specialists[index].completedAt = now
			return
		}
	}
}

// incrementToolCount bumps the tool-call counter for the specialist with
// childSessionID. Unknown specialists are ignored.
func (t *specialistTracker) incrementToolCount(childSessionID string) {
	for index := range t.specialists {
		if t.specialists[index].childSessionID == childSessionID {
			t.specialists[index].toolCount++
			return
		}
	}
}

// setCurrentTool updates the live tool-call progress for the specialist with
// childSessionID. Used by specialistProgressMsg to show ↳ toolName detail.
func (t *specialistTracker) setCurrentTool(childSessionID, toolName, detail string) {
	for index := range t.specialists {
		if t.specialists[index].childSessionID == childSessionID {
			t.specialists[index].currentTool = toolName
			t.specialists[index].currentDetail = detail
			return
		}
	}
}

// clear resets the tracker to an empty state.
func (t *specialistTracker) clear() {
	t.specialists = nil
}

// reconcileSessionID rewrites the childSessionID of the entry currently keyed
// by oldID to newID. This bridges the tool-call-ID (used as a temporary key at
// specialist start time) to the real session ID (known only when the child
// process reports it on completion). No-op if oldID is not found.
func (t *specialistTracker) reconcileSessionID(oldID, newID string) {
	for index := range t.specialists {
		if t.specialists[index].childSessionID == oldID {
			t.specialists[index].childSessionID = newID
			return
		}
	}
}

// getBySessionID returns the info for childSessionID and whether it was found.
func (t *specialistTracker) getBySessionID(childSessionID string) (specialistInfo, bool) {
	for index := range t.specialists {
		if t.specialists[index].childSessionID == childSessionID {
			return t.specialists[index], true
		}
	}
	return specialistInfo{}, false
}

// all returns a copy of the specialists slice so callers may iterate without
// the underlying array mutating underneath them.
func (t *specialistTracker) all() []specialistInfo {
	if len(t.specialists) == 0 {
		return nil
	}
	out := make([]specialistInfo, len(t.specialists))
	copy(out, t.specialists)
	return out
}

// specialistStatusString returns the lowercase human label for a status.
func specialistStatusString(s specialistStatus) string {
	switch s {
	case specialistRunning:
		return "running"
	case specialistCompleted:
		return "completed"
	case specialistError:
		return "error"
	default:
		return "error"
	}
}

// parseSpecialistStatus maps the status string carried by specialist events to
// the internal specialistStatus enum. Unknown values default to error so a
// malformed event never reads as a silent success.
func parseSpecialistStatus(s string) specialistStatus {
	switch s {
	case "running":
		return specialistRunning
	case "completed":
		return specialistCompleted
	default:
		return specialistError
	}
}

// parseTaskCallArgs extracts the specialist name and description from a Task
// tool call's JSON arguments. The name comes from the "name" field and the
// description from the "description" field (falling back to "prompt").
func parseTaskCallArgs(rawArgs string) (name, description string) {
	name = firstArgValue(rawArgs, []string{"name"})
	description = firstArgValue(rawArgs, []string{"description", "prompt"})
	return name, description
}

// formatTokenCount renders an integer token count with comma thousands
// separators: 1840 -> "1,840", 5210 -> "5,210".
func formatTokenCount(n int) string {
	if n < 0 {
		n = -n
	}
	digits := strconv.Itoa(n)
	if len(digits) <= 3 {
		return digits
	}
	var b strings.Builder
	first := len(digits) % 3
	if first > 0 {
		b.WriteString(digits[:first])
		if len(digits) > first {
			b.WriteByte(',')
		}
	}
	for i := first; i < len(digits); i += 3 {
		b.WriteString(digits[i : i+3])
		if i+3 < len(digits) {
			b.WriteByte(',')
		}
	}
	return b.String()
}

// formatSpecialistElapsed renders a duration as the compact "Ns" / "NmNs" form
// shown on specialist card headers (e.g. 18s, 45s, 1m5s). Durations under a
// second round up to 1s so a freshly started card never shows "0s".
func formatSpecialistElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	seconds := int(d.Seconds())
	if seconds < 1 {
		return "1s"
	}
	if seconds < 60 {
		return strconv.Itoa(seconds) + "s"
	}
	minutes := seconds / 60
	remainder := seconds % 60
	if remainder == 0 {
		return strconv.Itoa(minutes) + "m"
	}
	return strconv.Itoa(minutes) + "m" + strconv.Itoa(remainder) + "s"
}

// renderSpecialistCard renders one specialist as a left-rule card of the given
// width: a status-tinted │ on the left, no top/right/bottom borders. The card
// has a header (icon + name + description + elapsed) and a body line (status +
// tool calls + tokens). An optional error detail line is shown when the
// specialist errored. The whole card is mouse-clickable to drill into its
// subchat (wired in transcript_selection.go); no text hint is rendered.
// Widths below the minimum are clamped to 30.
func (m model) renderSpecialistCard(info specialistInfo, width int) string {
	if width < 30 {
		width = 30
	}

	// Elapsed: live while running, frozen at completion once the specialist is
	// done.
	var elapsed time.Duration
	if info.status == specialistRunning {
		elapsed = m.now().Sub(info.startedAt)
	} else if !info.completedAt.IsZero() {
		elapsed = info.completedAt.Sub(info.startedAt)
	} else {
		elapsed = m.now().Sub(info.startedAt)
	}
	elapsedStr := formatSpecialistElapsed(elapsed)

	// Description truncation. The header reserves room for the icon, the name,
	// the two " · " separators, the elapsed string, and a safety margin. Clamp
	// to zero so very long names never underflow.
	descMax := width - len(info.name) - 25
	if descMax < 0 {
		descMax = 0
	}
	description := truncateRunes(info.description, descMax)

	// Header line: icon + name + " · " + description + " · " + elapsed.
	var header string
	switch info.status {
	case specialistRunning:
		icon := m.spinnerGlyph()
		header = zeroTheme.accent.Render(fmt.Sprintf("%s%s · %s · %s", icon, info.name, description, elapsedStr))
	case specialistCompleted:
		header = zeroTheme.green.Render(fmt.Sprintf("✓ %s · %s · %s", info.name, description, elapsedStr))
	case specialistError:
		header = zeroTheme.red.Render(fmt.Sprintf("✗ %s · %s · %s", info.name, description, elapsedStr))
	default:
		header = zeroTheme.accent.Render(fmt.Sprintf("• %s · %s · %s", info.name, description, elapsedStr))
	}

	// Body line: "  status · N tool calls · M,NNN tokens".
	toolLabel := "tool calls"
	statusLabel := specialistStatusString(info.status)
	if info.status == specialistError {
		statusLabel = fmt.Sprintf("error (exit code %d)", info.exitCode)
	}
	// The token total is only populated when usage was bridged from the child; omit
	// the segment when it is zero rather than advertise a misleading "0 tokens" (M18).
	bodyText := fmt.Sprintf("  %s · %d %s", statusLabel, info.toolCount, toolLabel)
	if info.tokenCount > 0 {
		bodyText += fmt.Sprintf(" · %s tokens", formatTokenCount(info.tokenCount))
	}
	var body string
	if info.status == specialistError {
		body = zeroTheme.red.Render(bodyText)
	} else {
		body = zeroTheme.muted.Render(bodyText)
	}
	// Surface the otherwise-invisible drill-in affordance: a left-click or Enter on
	// the card opens its subchat (transcript_selection.go). A faint hint makes that
	// discoverable instead of hidden; it truncates first on narrow cards.
	body += zeroTheme.faint.Render("   · enter to open")

	lines := []string{header, body}

	// Live tool-call progress while running.
	if info.status == specialistRunning && info.currentTool != "" {
		progressLine := fmt.Sprintf("  ↳ %s", info.currentTool)
		if info.currentDetail != "" {
			progressLine += " " + info.currentDetail
		}
		lines = append(lines, zeroTheme.muted.Render(progressLine))
	}

	// Optional error detail line.
	if info.status == specialistError && strings.TrimSpace(info.errorMsg) != "" {
		errMax := width - 4
		if errMax < 1 {
			errMax = 1
		}
		errMsg := truncateRunes(strings.TrimSpace(info.errorMsg), errMax)
		lines = append(lines, zeroTheme.red.Render("  "+errMsg))
	}

	// Left-rule card: status-tinted │ on the left, no box borders.
	rule := specialistBorderStyle(info.status)
	return renderLeftRuleCard(width, lines, rule)
}

// specialistBorderStyle picks the card border style for a specialist status:
// the running tint while in flight, the error tint on failure, and the default
// line once completed cleanly.
func specialistBorderStyle(status specialistStatus) lipgloss.Style {
	switch status {
	case specialistRunning:
		return zeroTheme.cardRun
	case specialistError:
		return zeroTheme.cardErr
	default:
		return zeroTheme.line
	}
}

// specialistTitleFor returns the display title (name + " · " + description) for
// the specialist with the given childSessionID, for the subchat nav bar. Returns
// "" when the specialist is not found in the tracker. Falls back to the
// specialist info carried by transcript rows when the tracker has been cleared.
func (m model) specialistTitleFor(childSessionID string) string {
	info, ok := m.specialists.getBySessionID(childSessionID)
	if ok {
		return info.name + " · " + info.description
	}
	for _, row := range m.transcript {
		if row.kind == rowSpecialist && row.specialistInfo != nil && row.specialistInfo.childSessionID == childSessionID {
			return row.specialistInfo.name + " · " + row.specialistInfo.description
		}
	}
	return ""
}

// toolCallSummary extracts a short detail string from a stream-json tool_call
// event's arguments, for the live progress line in specialist cards.
func toolCallSummary(event streamjson.Event) string {
	args, ok := event.Args.(map[string]any)
	if !ok {
		return ""
	}
	switch event.Name {
	case "read_file", "read_minified_file", "list_directory", "write_file", "edit_file":
		if path, ok := args["path"].(string); ok {
			return truncateRunes(path, 50)
		}
	case "grep":
		if pattern, ok := args["pattern"].(string); ok {
			return truncateRunes(pattern, 40)
		}
	case "glob":
		if pattern, ok := args["pattern"].(string); ok {
			return truncateRunes(pattern, 40)
		}
	case "bash", "exec_command":
		if cmd, ok := args["command"].(string); ok {
			return truncateRunes(singleLineToolHeadText(cmd), 40)
		}
		if cmd, ok := args["cmd"].(string); ok {
			return truncateRunes(singleLineToolHeadText(cmd), 40)
		}
	case "write_stdin":
		sessionID := toolCallIntArg(args, "session_id")
		chars, _ := args["chars"].(string)
		switch chars {
		case "":
			return fmt.Sprintf("poll session %d", sessionID)
		case "\x03":
			return fmt.Sprintf("interrupt session %d", sessionID)
		default:
			return fmt.Sprintf("send input to session %d", sessionID)
		}
	case "update_plan":
		return "plan"
	}
	return ""
}

func toolCallIntArg(args map[string]any, key string) int {
	switch value := args[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	case string:
		parsed, _ := strconv.Atoi(value)
		return parsed
	default:
		return 0
	}
}

// truncateRunes is provided by view.go; specialist_card.go relies on it for
// rune-safe description and error-message truncation.

// renderLeftRuleCard renders lines with a single status-tinted left rule and
// no other borders. Each line is prefixed with "│ " in the rule style; the
// content is padded to the given width so cards align. No top/bottom/right
// borders — lighter than styledBlock, matching the borderless inline tool
// render style used by reference TUIs.
func renderLeftRuleCard(width int, lines []string, ruleStyle lipgloss.Style) string {
	if width < 4 {
		width = 4
	}
	inner := width - 2 // "│ " takes 2 cells
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		fitted := fitStyledLine(line, inner)
		pad := strings.Repeat(" ", maxInt(0, inner-lipgloss.Width(fitted)))
		out = append(out, ruleStyle.Render("│ ")+fitted+pad)
	}
	return strings.Join(out, "\n")
}

// renderSpecialistSummary renders a one-line rollup shown above the specialist
// cards: live spinner, total/running/completed/error counts, and total tokens.
// Returns "" when there are no specialists.
func renderSpecialistSummary(specialists []specialistInfo, spinnerView string) string {
	if len(specialists) == 0 {
		return ""
	}
	running, completed, errors, totalTokens := 0, 0, 0, 0
	for _, sp := range specialists {
		totalTokens += sp.tokenCount
		switch sp.status {
		case specialistRunning:
			running++
		case specialistCompleted:
			completed++
		case specialistError:
			errors++
		}
	}
	summary := fmt.Sprintf("  %s %d specialists · %d running · %d done",
		spinnerView, len(specialists), running, completed)
	if errors > 0 {
		summary += fmt.Sprintf(" · %d error", errors)
		if errors > 1 {
			summary += "s"
		}
	}
	summary += " · " + formatTokenCount(totalTokens) + " tokens"
	// summary is "  " + spinnerView + " N specialists ...". The spinner sits
	// at byte offset 2 (after the 2-space indent), so the muted tail must skip
	// both the indent and the spinner's bytes to avoid splitting a multi-byte
	// rune and losing the indent.
	tailStart := 2 + len(spinnerView)
	return zeroTheme.accent.Render(spinnerView) + zeroTheme.muted.Render(summary[tailStart:])
}

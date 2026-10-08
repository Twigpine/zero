package tui

import (
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// leaderTimeout is how long Ctrl+X waits for a follow-up key before the chord
// cancels itself. Short enough to not feel sticky; long enough to find the
// second key without racing.
const leaderTimeout = 2 * time.Second

// leaderCommandByKey maps the second key of a Ctrl+X chord to a builtin slash
// command. Case-sensitive so m/M and p/P stay distinct. Only bare commands (no
// args) — same as typing the slash and pressing Enter with an empty argument.
var leaderCommandByKey = map[rune]string{
	'm': "/model",
	'p': "/provider",
	'P': "/plan",
	'M': "/stt-model",
	'v': "/voice",
	'c': "/clear",
	'C': "/context",
	's': "/stop",
	'i': "/image",
	'r': "/resume",
	'u': "/rewind",
	't': "/tools",
	'R': "/retry",
}

// leaderExpiredMsg clears leader-pending when the follow-up window elapses.
// seq must match m.leaderSeq or the tick is stale (a later Ctrl+X re-armed).
type leaderExpiredMsg struct {
	seq int
}

func (m model) canArmLeader() bool {
	return m.noBlockingModal() && !m.transcriptDetailed && !m.subchat.active && !m.helpOverlay && !m.leaderHelpOverlay
}

func (m model) armLeader() (model, tea.Cmd) {
	m.leaderPending = true
	m.leaderSeq++
	seq := m.leaderSeq
	return m, tea.Tick(leaderTimeout, func(time.Time) tea.Msg {
		return leaderExpiredMsg{seq: seq}
	})
}

func (m model) clearLeader() model {
	m.leaderPending = false
	// Bump seq so an in-flight timeout tick becomes a no-op.
	m.leaderSeq++
	return m
}

// handleLeaderKey consumes the key while leader-pending is armed. Never inserts
// into the composer; either runs a mapped slash command, opens the chord map
// (?), or cancels the chord. Ctrl+C is handled before this is called so
// exit/cancel still works.
func (m model) handleLeaderKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Second Ctrl+X cancels (does not re-arm).
	if keyCtrl(msg, 'x') {
		return m.clearLeader(), nil
	}
	if keyIs(msg, tea.KeyEsc) {
		return m.clearLeader(), nil
	}
	// Ctrl+X ? opens the full leader-chord map (discoverability for every wired
	// slash shortcut). Not a letter binding, so handle before leaderSecondKey.
	if keyText(msg) == "?" && !keyAlt(msg) && !keyHasMod(msg, tea.ModCtrl) {
		m = m.clearLeader()
		m.leaderHelpOverlay = true
		return m, nil
	}
	key, ok := leaderSecondKey(msg)
	if !ok {
		return m.clearLeader(), nil
	}
	slash, mapped := leaderCommandByKey[key]
	if !mapped {
		return m.clearLeader(), nil
	}
	m = m.clearLeader()
	return m.executeSlash(slash)
}

// leaderHelpBindings is the stable, display-ordered table of every wired
// Ctrl+X chord. Keep in sync with leaderCommandByKey.
func leaderHelpBindings() []keybinding {
	return []keybinding{
		{"Ctrl+X m", "open /model"},
		{"Ctrl+X p", "open /provider"},
		{"Ctrl+X P", "run /plan"},
		{"Ctrl+X M", "open /stt-model"},
		{"Ctrl+X v", "toggle /voice"},
		{"Ctrl+X c", "run /clear"},
		{"Ctrl+X C", "run /context"},
		{"Ctrl+X s", "run /stop"},
		{"Ctrl+X i", "run /image"},
		{"Ctrl+X r", "open /resume"},
		{"Ctrl+X u", "run /rewind"},
		{"Ctrl+X t", "run /tools"},
		{"Ctrl+X R", "run /retry"},
		{"Ctrl+X ?", "show this list"},
	}
}

const leaderHelpFooter = "? or Esc to close \u00b7 letter after Ctrl+X runs the command"

// renderLeaderHelpLines builds the body of the Ctrl+X ? overlay — same shape as
// renderKeybindingHelpLines (group title + key rows + footer) so the two modals
// share layout and column alignment.
func renderLeaderHelpLines(innerWidth int) []string {
	groups := []keybindingGroup{{
		title:    "Slash commands",
		bindings: leaderHelpBindings(),
	}}
	keyColumn := keybindingKeyColumnWidth(groups)
	lines := make([]string, 0, 32)
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
	lines = append(lines, zeroTheme.faint.Render(leaderHelpFooter))
	return lines
}

// renderLeaderHelpOverlay frames the Ctrl+X ? chord map exactly like the
// general ? keyboard-shortcut overlay. Its interior stays transparent so a
// terminal's own canvas remains visible without jagged panel-fill padding.
func (m model) renderLeaderHelpOverlay(width int) string {
	overlayWidth := keybindingHelpOverlayWidth(width)
	lines := renderLeaderHelpLines(overlayWidth - 4)
	block := styledBlockFillTitle(overlayWidth, "Ctrl+X Shortcuts", lines, zeroTheme.line, lipgloss.NewStyle())
	return centerRenderedBlock(block, width)
}

// leaderSecondKey extracts the letter for a leader follow-up. Prefers printable
// text ("m" / "M"); falls back to code+Shift so test harnesses and terminals
// that report only ModShift still work.
func leaderSecondKey(msg tea.KeyMsg) (rune, bool) {
	if keyHasMod(msg, tea.ModCtrl) || keyAlt(msg) {
		return 0, false
	}
	text := keyText(msg)
	if runes := []rune(text); len(runes) == 1 && unicode.IsLetter(runes[0]) {
		return runes[0], true
	}
	code := keyCode(msg)
	if code >= 'a' && code <= 'z' {
		if keyShift(msg) {
			return code - ('a' - 'A'), true
		}
		return code, true
	}
	if code >= 'A' && code <= 'Z' {
		return code, true
	}
	return 0, false
}

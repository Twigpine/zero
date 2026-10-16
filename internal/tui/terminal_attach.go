package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Gitlawb/zero/internal/execution"
)

// terminalAttachState is the modal "attach to a live terminal" overlay: while
// it is set, keystrokes are forwarded verbatim to the session's PTY stdin and
// the overlay shows a rolling tail of its output. The bytes the user types are
// never inspected, stored, or logged — the PTY's own line discipline (e.g.
// sudo's no-echo prompt) handles masking.
type terminalAttachState struct {
	sessionID      int
	command        string
	output         string
	ptyCols        int
	ptyRows        int
	bracketedPaste bool
}

const terminalAttachTickInterval = 100 * time.Millisecond

type terminalAttachTickMsg struct{}

func terminalAttachTickCmd() tea.Cmd {
	return tea.Tick(terminalAttachTickInterval, func(time.Time) tea.Msg {
		return terminalAttachTickMsg{}
	})
}

// terminalAutoAttachState watches a run for a brand-new tty exec session so the
// attach overlay can open on it without the user typing /attach.
type terminalAutoAttachState struct {
	runID    int
	known    map[int]bool
	deadline time.Time
}

const terminalAutoAttachTimeout = 60 * time.Second

// interactiveExecStartMsg signals that the active run just invoked exec_command
// with tty:true; the session registers with the process manager moments later,
// so the tick polls for it. The baseline session set is captured in OnToolCall,
// before the tool runs: Program.Send only queues the message, so snapshotting
// in Update could already see the new session and exclude it as pre-existing.
type interactiveExecStartMsg struct {
	runID int
	known map[int]bool
}

// The tick carries the run it belongs to so the /btw router can deliver it to
// the hidden parent model instead of dropping it on the visible side model.
type terminalAutoAttachTickMsg struct {
	runID int
}

func terminalAutoAttachTickCmd(runID int) tea.Cmd {
	return tea.Tick(terminalAttachTickInterval, func(time.Time) tea.Msg {
		return terminalAutoAttachTickMsg{runID: runID}
	})
}

// execCallWantsTTY reports whether an exec_command tool call's arguments JSON
// requests a PTY. Malformed JSON means false.
func execCallWantsTTY(arguments string) bool {
	var args struct {
		TTY bool `json:"tty"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return false
	}
	return args.TTY
}

// openTerminalAttach attaches to an interactive exec session. Callers check
// TTY eligibility first; a session that is already gone or exited surfaces as a
// transient notice instead of an empty overlay.
func (m model) openTerminalAttach(id int) (model, tea.Cmd) {
	state := &terminalAttachState{sessionID: id}
	if controller, ok := m.execSessionController(); ok {
		snapshot, found := controller.ExecSession(id)
		switch {
		case !found:
			return m.showTransientNoticeInline(fmt.Sprintf("Terminal session %d ended.", id), transientNoticeInfo), nil
		case snapshot.Status != "running":
			exitCode := 0
			if snapshot.ExitCode != nil {
				exitCode = *snapshot.ExitCode
			}
			return m.showTransientNoticeInline(fmt.Sprintf("Terminal session %d finished (exit %d).", id, exitCode), transientNoticeInfo), nil
		default:
			state.command = snapshot.Command
			state.output = snapshot.RecentOutput
			state.bracketedPaste = bracketedPasteMode(snapshot.RecentOutput, false)
		}
	}
	m.terminalAttach = state
	if m.terminalAttachSeen == nil {
		m.terminalAttachSeen = map[int]bool{}
	}
	m.terminalAttachSeen[id] = true
	m.clearSuggestions()
	m = m.resizeAttachedTerminalPTY()
	return m, terminalAttachTickCmd()
}

// resizeAttachedTerminalPTY reports the overlay's output area to the PTY so
// the session lays out at the size it is actually displayed at. Errors are
// ignored: platforms without PTY support have nothing to resize.
func (m model) resizeAttachedTerminalPTY() model {
	state := m.terminalAttach
	if state == nil {
		return m
	}
	width := chatWidth(m.width)
	// The box border and inset take 4 cells; one more is the cursor
	// reservation, matching the width renderTerminalTail draws at.
	cols := maxInt(1, width-5)
	rows := m.terminalAttachViewportRows(width)
	if cols == state.ptyCols && rows == state.ptyRows {
		return m
	}
	controller, ok := m.execSessionController()
	if !ok {
		return m
	}
	state.ptyCols, state.ptyRows = cols, rows
	_ = controller.ResizeExecSession(state.sessionID, cols, rows)
	return m
}

// refreshTerminalAttach polls the session snapshot for the overlay tail. When
// the session exits or disappears the overlay closes itself with a notice;
// detaching also stops the tick because the handler drops it while
// terminalAttach is nil.
func (m model) refreshTerminalAttach() (model, tea.Cmd) {
	state := m.terminalAttach
	if state == nil {
		return m, nil
	}
	controller, ok := m.execSessionController()
	if !ok {
		m.terminalAttach = nil
		return m, nil
	}
	snapshot, found := controller.ExecSession(state.sessionID)
	if !found {
		m.terminalAttach = nil
		return m.showTransientNoticeInline(fmt.Sprintf("Terminal session %d ended.", state.sessionID), transientNoticeInfo), nil
	}
	state.output = snapshot.RecentOutput
	state.bracketedPaste = bracketedPasteMode(snapshot.RecentOutput, state.bracketedPaste)
	if snapshot.Status != "running" {
		m.terminalAttach = nil
		exitCode := 0
		if snapshot.ExitCode != nil {
			exitCode = *snapshot.ExitCode
		}
		return m.showTransientNoticeInline(fmt.Sprintf("Terminal session %d finished (exit %d).", state.sessionID, exitCode), transientNoticeInfo), nil
	}
	return m, terminalAttachTickCmd()
}

// pollTerminalAutoAttach runs the auto-attach watcher: while the run is live it
// looks for a new tty session and opens the overlay on it, giving up at the
// deadline (a permission prompt can hold the session start for a while).
func (m model) pollTerminalAutoAttach(msg terminalAutoAttachTickMsg) (model, tea.Cmd) {
	state := m.terminalAutoAttach
	if state == nil || msg.runID != state.runID {
		return m, nil
	}
	if m.activeRunID != state.runID || !m.pending || m.now().After(state.deadline) {
		m.terminalAutoAttach = nil
		return m, nil
	}
	controller, ok := m.execSessionController()
	if !ok {
		return m, terminalAutoAttachTickCmd(state.runID)
	}
	for _, session := range controller.ExecSessions() {
		if !session.TTY || session.Status != "running" || state.known[session.ID] || m.terminalAttachSeen[session.ID] {
			continue
		}
		if !m.terminalAttachCanOpen() || m.terminalAttach != nil {
			// A modal (permission prompt, picker, …) or an existing attach owns
			// the viewport; keep watching so the overlay opens once it clears.
			return m, terminalAutoAttachTickCmd(state.runID)
		}
		m.terminalAutoAttach = nil
		return m.openTerminalAttach(session.ID)
	}
	return m, terminalAutoAttachTickCmd(state.runID)
}

// terminalAttachCanOpen reports whether the attach overlay would actually be
// rendered right now: beyond the modal prompts, the subchat drill-in, file
// view, setup wizard, help/run-details overlays, the detailed transcript, and
// active suggestions each replace or cover the viewport it draws into, so
// opening then would capture keys into an invisible overlay.
func (m model) terminalAttachCanOpen() bool {
	return m.noBlockingModalExceptAttach() &&
		!m.helpOverlay && !m.leaderHelpOverlay && !m.runDetailsOpen &&
		!m.subchat.active && !m.fileView.active && !m.setup.visible &&
		!m.transcriptDetailed && !m.suggestionsActive()
}

// noBlockingModalExceptAttach is noBlockingModal without the attach overlay's
// own term, for the auto-attach watcher deciding whether another modal owns
// the viewport.
func (m model) noBlockingModalExceptAttach() bool {
	return m.pendingPermission == nil && m.pendingAskUser == nil && m.pendingSpecReview == nil &&
		m.providerWizard == nil && m.mcpAddWizard == nil && m.mcpManager == nil && m.picker == nil &&
		m.sttKeyPrompt == nil && m.renamePrompt == nil
}

// writeTerminalAttachInput forwards bytes to the session stdin. The bytes are
// written through untouched and never echoed into the transcript, a notice, or
// the overlay. Errors only update overlay state or a generic notice.
func (m model) writeTerminalAttachInput(state *terminalAttachState, data []byte) model {
	controller, ok := m.execSessionController()
	if !ok {
		m.terminalAttach = nil
		return m
	}
	err := controller.WriteExecSessionInput(state.sessionID, data)
	switch {
	case errors.Is(err, execution.ErrProcessNotFound):
		m.terminalAttach = nil
		return m.showTransientNoticeInline(fmt.Sprintf("Terminal session %d ended.", state.sessionID), transientNoticeInfo)
	case errors.Is(err, execution.ErrProcessStdinDisabled):
		m.terminalAttach = nil
		return m.showTransientNoticeInline(fmt.Sprintf("Terminal session %d no longer accepts input.", state.sessionID), transientNoticeWarning)
	case err != nil:
		return m.showTransientNoticeInline("Could not send input to session "+strconv.Itoa(state.sessionID)+".", transientNoticeWarning)
	}
	return m
}

// handleTerminalAttachKey owns every keystroke while attached: Esc detaches,
// anything else maps to PTY bytes and goes to the process (so Ctrl+C reaches
// it as 0x03 instead of triggering the TUI's own exit confirmation).
func (m model) handleTerminalAttachKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	state := m.terminalAttach
	if keyIs(msg, tea.KeyEsc) {
		m.terminalAttach = nil
		return m.showTransientNoticeInline(
			fmt.Sprintf("Detached from session %d — it keeps running; /attach %d to return.", state.sessionID, state.sessionID),
			transientNoticeInfo), nil
	}
	data, ok := ptyInputBytes(msg)
	if !ok {
		return m, nil
	}
	return m.writeTerminalAttachInput(state, data), nil
}

// handleTerminalAttachPaste forwards a paste to the PTY. Bubble Tea strips
// the outer terminal's bracketed-paste markers, so when the child enabled
// bracketed paste itself the framing is re-added around the content.
func (m model) handleTerminalAttachPaste(content string) (tea.Model, tea.Cmd) {
	state := m.terminalAttach
	if content == "" {
		return m, nil
	}
	data := []byte(content)
	if state.bracketedPaste {
		data = append(append([]byte("\x1b[200~"), data...), "\x1b[201~"...)
	}
	return m.writeTerminalAttachInput(state, data), nil
}

// bracketedPasteMode tracks the child's bracketed-paste mode from its output:
// the last enable/disable sequence wins, and output without either keeps the
// previous state since RecentOutput is a rolling window that can lose it.
func bracketedPasteMode(output string, current bool) bool {
	on := strings.LastIndex(output, "\x1b[?2004h")
	off := strings.LastIndex(output, "\x1b[?2004l")
	switch {
	case on < 0 && off < 0:
		return current
	case on > off:
		return true
	default:
		return false
	}
}

// ptyInputBytes maps a keystroke to the byte sequence a terminal would send.
// Esc is deliberately unmapped: it detaches instead of reaching the process.
func ptyInputBytes(msg tea.KeyMsg) ([]byte, bool) {
	key := msg.Key()
	if keyHasMod(msg, tea.ModCtrl) {
		if key.Code >= 'a' && key.Code <= 'z' {
			return []byte{byte(key.Code - 'a' + 1)}, true
		}
		return nil, false
	}
	if keyAlt(msg) {
		if text := keyText(msg); text != "" {
			return append([]byte{0x1b}, text...), true
		}
		return nil, false
	}
	switch key.Code {
	case tea.KeyEnter:
		return []byte("\r"), true
	case tea.KeyTab:
		return []byte("\t"), true
	case tea.KeyBackspace:
		return []byte{0x7f}, true
	case tea.KeyDelete:
		return []byte("\x1b[3~"), true
	case tea.KeyUp:
		return []byte("\x1b[A"), true
	case tea.KeyDown:
		return []byte("\x1b[B"), true
	case tea.KeyRight:
		return []byte("\x1b[C"), true
	case tea.KeyLeft:
		return []byte("\x1b[D"), true
	case tea.KeyHome:
		return []byte("\x1b[H"), true
	case tea.KeyEnd:
		return []byte("\x1b[F"), true
	case tea.KeyPgUp:
		return []byte("\x1b[5~"), true
	case tea.KeyPgDown:
		return []byte("\x1b[6~"), true
	}
	if text := keyText(msg); text != "" {
		return []byte(text), true
	}
	return nil, false
}

// renderTerminalTail turns raw PTY output into display lines: carriage returns
// overwrite their line from column 0 (progress-bar style), ANSI sequences are
// stripped, remaining control runes are dropped (tab expands to spaces), and
// the result is the last rows lines top-aligned and padded to exactly rows.
func renderTerminalTail(raw string, width, rows int) []string {
	if rows < 0 {
		rows = 0
	}
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	source := strings.Split(raw, "\n")
	lines := make([]string, 0, len(source))
	for _, line := range source {
		lines = append(lines, sanitizeTerminalLine(line, width))
	}
	if len(lines) > rows {
		lines = lines[len(lines)-rows:]
	}
	for len(lines) < rows {
		lines = append(lines, "")
	}
	return lines
}

func sanitizeTerminalLine(line string, width int) string {
	var resolved []rune
	for _, segment := range strings.Split(line, "\r") {
		runes := []rune(segment)
		if len(runes) < len(resolved) {
			runes = append(runes, resolved[len(runes):]...)
		}
		resolved = runes
	}
	stripped := ansi.Strip(string(resolved))
	var builder strings.Builder
	for _, r := range stripped {
		switch {
		case r == '\t':
			builder.WriteString("    ")
		case unicode.IsControl(r):
		default:
			builder.WriteRune(r)
		}
	}
	if width <= 0 {
		return builder.String()
	}
	return fitStyledLine(builder.String(), width)
}

// terminalAttachViewportRows is the number of output rows the attached
// terminal gets: the transcript viewport's body height minus the box's two
// border lines and its footer hint line, so the bordered overlay fills the
// viewport exactly. The same frame math the compositor uses keeps the two in
// sync — and footerView already collapses to the status line while attached.
func (m model) terminalAttachViewportRows(width int) int {
	if m.height <= 0 {
		return 10
	}
	frame := m.scrollableTranscriptFrame(m.pinnedTitleBar(width), m.footerView(width))
	return maxInt(6, frame.bodyRect.height-3)
}

func (m model) terminalAttachOverlay(width int) string {
	state := m.terminalAttach
	if state == nil {
		return ""
	}
	command := compactCommandOutputText(state.command)
	if command == "" {
		command = "command"
	}
	// styledBlockFillTitle drops the title when it can't fit the top rule, so
	// truncate the command to whatever the border can carry.
	titlePrefix := zeroTheme.accent.Render("●") + " Terminal  "
	titleSuffix := "  [running]"
	titleBudget := width - 5 - lipgloss.Width(titleSuffix)
	title := titlePrefix + truncateRunes(command, maxInt(0, titleBudget-lipgloss.Width(titlePrefix))) + titleSuffix
	innerWidth := maxInt(1, width-4)
	// Reserve one cell so the cursor never pushes a full line past the edge.
	tail := renderTerminalTail(state.output, innerWidth-1, m.terminalAttachViewportRows(width))
	cursor := -1
	for index := len(tail) - 1; index >= 0; index-- {
		if strings.TrimSpace(ansi.Strip(tail[index])) != "" {
			cursor = index
			break
		}
	}
	if cursor < 0 && len(tail) > 0 {
		cursor = 0
	}
	if cursor >= 0 {
		tail[cursor] += zeroTheme.accent.Render("▌")
	}
	footer := fmt.Sprintf("Esc detach · Ctrl+C goes to the process · /stop %d to kill", state.sessionID)
	lines := make([]string, 0, len(tail)+1)
	lines = append(lines, tail...)
	lines = append(lines, zeroTheme.faint.Render(footer))
	return styledBlockFillTitle(width, title, lines, zeroTheme.lineStrong, lipgloss.NewStyle())
}

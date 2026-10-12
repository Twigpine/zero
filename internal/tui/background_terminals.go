package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/tools"
)

func (m model) execSessionController() (tools.ExecSessionController, bool) {
	if m.registry == nil {
		return nil, false
	}
	tool, ok := m.registry.Get(tools.ExecCommandToolName)
	if !ok {
		return nil, false
	}
	controller, ok := tool.(tools.ExecSessionController)
	return controller, ok
}

func (m model) backgroundTerminalSessions() []tools.ExecSessionSnapshot {
	controller, ok := m.execSessionController()
	if !ok {
		return nil
	}
	return controller.ExecSessions()
}

func (m model) backgroundTerminalSummary() string {
	sessions := m.backgroundTerminalSessions()
	if len(sessions) == 0 {
		return ""
	}
	plural := ""
	if len(sessions) != 1 {
		plural = "s"
	}
	summary := fmt.Sprintf("%d background terminal%s running · /ps to view · /stop to close", len(sessions), plural)
	for _, session := range sessions {
		if session.TTY {
			return summary + " · /attach to type into it"
		}
	}
	return summary
}

func (m model) stopAllBackgroundTerminalSessions() []int {
	controller, ok := m.execSessionController()
	if !ok {
		return nil
	}
	return controller.StopAllExecSessions()
}

func (m model) backgroundTerminalsText() string {
	sessions := m.backgroundTerminalSessions()
	card := commandCard{
		Title:   "Background Terminals",
		Summary: []string{fmt.Sprintf("%d running", len(sessions))},
	}
	if len(sessions) == 0 {
		card.Sections = []commandCardSection{{
			Rows: []commandRow{{Text: "No background terminals running."}},
		}}
		return renderCommandCardTranscript(card)
	}
	rows := make([]commandRow, 0, len(sessions))
	now := m.now()
	for _, session := range sessions {
		rows = append(rows, commandRow{Text: formatBackgroundTerminalRow(session, now)})
	}
	card.Sections = []commandCardSection{{
		Title: "running",
		Rows:  rows,
	}}
	card.Actions = []string{"/stop <session_id>", "/stop"}
	for _, session := range sessions {
		if session.TTY {
			card.Actions = append([]string{"/attach <session_id>"}, card.Actions...)
			break
		}
	}
	return renderCommandCardTranscript(card)
}

func (m model) stopBackgroundTerminalsText(input string) string {
	controller, ok := m.execSessionController()
	if !ok {
		return renderCommandCardTranscript(commandCard{
			Title:   "Background Terminals",
			Summary: []string{"unavailable"},
			Sections: []commandCardSection{{
				Rows: []commandRow{{Text: "exec_command is not registered."}},
			}},
		})
	}
	input = strings.TrimSpace(input)
	if input == "" {
		stopped := m.stopAllBackgroundTerminalSessions()
		return renderStopBackgroundTerminalsCard(stopped, "")
	}
	id, err := strconv.Atoi(input)
	if err != nil || id <= 0 {
		return renderCommandCardTranscript(commandCard{
			Title:   "Background Terminals",
			Summary: []string{"invalid session id"},
			Sections: []commandCardSection{{
				Rows: []commandRow{{Text: "Usage: /stop [session_id]"}},
			}},
		})
	}
	if !controller.StopExecSession(id) {
		return renderCommandCardTranscript(commandCard{
			Title:   "Background Terminals",
			Summary: []string{"not found"},
			Sections: []commandCardSection{{
				Rows: []commandRow{{Text: fmt.Sprintf("No running background terminal with session_id %d.", id)}},
			}},
		})
	}
	return renderStopBackgroundTerminalsCard([]int{id}, "")
}

// attachTerminalCommand implements /attach: with an explicit session id it
// validates and opens the attach overlay; bare, it picks the one interactive
// session or lists the choices.
func (m model) attachTerminalCommand(input string) (model, tea.Cmd) {
	controller, ok := m.execSessionController()
	if !ok {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: renderCommandCardTranscript(commandCard{
			Title:   "Background Terminals",
			Summary: []string{"unavailable"},
			Sections: []commandCardSection{{
				Rows: []commandRow{{Text: "exec_command is not registered."}},
			}},
		})})
		return m, nil
	}
	// The overlay composites into the main transcript viewport; in a view that
	// replaces it (subchat, detailed transcript, another overlay) it would
	// capture keystrokes while invisible, so refuse to open there.
	if !m.terminalAttachCanOpen() {
		return m.showTransientNoticeInline("Leave the current view first", transientNoticeInfo), nil
	}
	input = strings.TrimSpace(input)
	if input != "" {
		id, err := strconv.Atoi(input)
		if err != nil || id <= 0 {
			return m.appendAttachCard("invalid session id", "Usage: /attach [session_id]"), nil
		}
		snapshot, found := controller.ExecSession(id)
		if !found {
			return m.appendAttachCard("not found", fmt.Sprintf("No running terminal session %d.", id)), nil
		}
		if !snapshot.TTY {
			return m.appendAttachCard("not interactive", fmt.Sprintf("Session %d has no terminal — it was started without tty:true (or this platform has no PTY), so it can't take input. Use /stop %d to end it.", id, id)), nil
		}
		return m.openTerminalAttach(id)
	}
	var interactive []tools.ExecSessionSnapshot
	for _, session := range controller.ExecSessions() {
		if session.TTY {
			interactive = append(interactive, session)
		}
	}
	switch len(interactive) {
	case 0:
		return m.showTransientNoticeInline("No interactive terminal sessions running.", transientNoticeInfo), nil
	case 1:
		return m.openTerminalAttach(interactive[0].ID)
	}
	rows := make([]commandRow, 0, len(interactive))
	now := m.now()
	for _, session := range interactive {
		rows = append(rows, commandRow{Text: formatBackgroundTerminalRow(session, now)})
	}
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: renderCommandCardTranscript(commandCard{
		Title:   "Background Terminals",
		Summary: []string{fmt.Sprintf("%d interactive sessions", len(interactive))},
		Sections: []commandCardSection{{
			Title: "running",
			Rows:  rows,
		}},
		Actions: []string{"/attach <session_id>"},
	})})
	return m, nil
}

func (m model) appendAttachCard(summary string, text string) model {
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: renderCommandCardTranscript(commandCard{
		Title:   "Background Terminals",
		Summary: []string{summary},
		Sections: []commandCardSection{{
			Rows: []commandRow{{Text: text}},
		}},
	})})
	return m
}

func renderStopBackgroundTerminalsCard(stopped []int, note string) string {
	card := commandCard{Title: "Background Terminals"}
	if len(stopped) == 0 {
		card.Summary = []string{"none running"}
		card.Sections = []commandCardSection{{
			Rows: []commandRow{{Text: "No background terminals running."}},
		}}
		return renderCommandCardTranscript(card)
	}
	values := make([]string, 0, len(stopped))
	for _, id := range stopped {
		values = append(values, strconv.Itoa(id))
	}
	card.Summary = []string{"stopping " + strings.Join(values, ", ")}
	rows := []commandRow{{Text: "Stopping background terminal sessions."}}
	if strings.TrimSpace(note) != "" {
		rows = append(rows, commandRow{Text: note})
	}
	card.Sections = []commandCardSection{{Rows: rows}}
	return renderCommandCardTranscript(card)
}

func formatBackgroundTerminalRow(session tools.ExecSessionSnapshot, now time.Time) string {
	age := formatTerminalAge(now.Sub(session.StartedAt))
	cwd := strings.TrimSpace(session.RelativeCwd)
	if cwd == "" {
		cwd = shortenPath(session.Cwd)
	}
	command := compactCommandOutputText(session.Command)
	if command == "" {
		command = "command"
	}
	preview := compactCommandOutputText(session.RecentOutput)
	prefix := fmt.Sprintf("%d · %s · %s · %s", session.ID, session.Status, age, cwd)
	if session.OutputTruncated {
		prefix += " · output truncated"
	}
	if preview != "" {
		return prefix + " · " + command + " · " + preview
	}
	return prefix + " · " + command
}

func formatTerminalAge(duration time.Duration) string {
	if duration < 0 {
		duration = 0
	}
	switch {
	case duration < time.Minute:
		return fmt.Sprintf("%ds", int(duration.Seconds()))
	case duration < time.Hour:
		return fmt.Sprintf("%dm", int(duration.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(duration.Hours()))
	}
}

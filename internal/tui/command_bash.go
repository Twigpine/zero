package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/Twigpine/zero/internal/tools"
)

// bashEscapeTimeout bounds a "!cmd" shell escape so a hung command can't freeze
// the agent indefinitely.
const bashEscapeTimeout = 30 * time.Second

// bashResultMsg carries the combined output of a "!cmd" shell escape back to the
// model for display in the transcript.
type bashResultMsg struct {
	command string
	output  string
}

// runBashEscape runs a user-typed "!cmd" in the workspace and returns its
// combined output as a message. The user typed it explicitly, so it runs
// directly (a deliberate shell escape, outside the agent sandbox), bounded by a
// timeout and executed asynchronously so the UI stays responsive.
func runBashEscape(cwd, command string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), bashEscapeTimeout)
		defer cancel()

		cmd := tools.NewHostShellCommandContext(ctx, command)
		if strings.TrimSpace(cwd) != "" {
			cmd.Dir = cwd
		}
		out, err := cmd.CombinedOutput()
		text := strings.TrimRight(string(out), "\n")

		switch {
		case ctx.Err() == context.DeadlineExceeded:
			text = appendNote(text, "[timed out after "+bashEscapeTimeout.String()+"]")
		case err != nil:
			text = appendNote(text, "[exit error: "+err.Error()+"]")
		}
		if strings.TrimSpace(text) == "" {
			text = "(no output)"
		}
		return bashResultMsg{command: command, output: text}
	}
}

// escapeShell returns the platform shell and arguments for tests and display.
// Execution itself uses tools.NewHostShellCommandContext so the cmd.exe
// fallback receives its required raw Windows command line.
func escapeShell(command string) (string, []string) {
	return tools.HostShellCommand(command)
}

func appendNote(text, note string) string {
	if text == "" {
		return note
	}
	return text + "\n" + note
}

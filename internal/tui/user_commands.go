package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/usercommands"
)

// handleUserCommand resolves a "/name args" that wasn't a builtin command
// against the file-sourced user commands (.zero/commands/<name>.md). When a
// match is found it expands the command's template with the args and launches a
// normal agent turn, returning handled=true. handled=false means no user
// command matched, so the caller falls through to "unknown command".
//
// raw is the full slash input as parseCommand captured it for commandUnknown
// (e.g. "/release v1.2"), so we re-split it here rather than thread the parsed
// name/args through the builtin parser (which doesn't know about file commands).
func (m model) handleUserCommand(raw string) (model, tea.Cmd, bool) {
	name, args := splitUserCommand(raw)
	if name == "" {
		return m, nil, false
	}
	cmd, ok := m.lookupUserCommand(name)
	if !ok {
		return m, nil, false
	}
	prompt := usercommands.Expand(cmd.Template, args)
	if strings.TrimSpace(prompt) == "" {
		return m, nil, false
	}
	// Same run-state guards as a plain prompt: a user command invoked mid-run is
	// queued (as its expanded prompt), not raced into a second concurrent turn.
	return m.launchOrDeferExpandedPrompt(raw, prompt)
}

// lookupUserCommand returns the loaded user command with the given (lowercased)
// name. Linear scan — the set is small (a handful of files).
func (m model) lookupUserCommand(name string) (usercommands.Command, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, cmd := range m.userCommands {
		if cmd.Name == name {
			return cmd, true
		}
	}
	return usercommands.Command{}, false
}

// splitUserCommand splits "/name rest of args" into the bare name (no leading
// slash, lowercased) and the trailing argument string.
func splitUserCommand(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "/") {
		return "", ""
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return "", ""
	}
	name := strings.ToLower(strings.TrimPrefix(fields[0], "/"))
	args := strings.TrimSpace(raw[len(fields[0]):])
	return name, args
}

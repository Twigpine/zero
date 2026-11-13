package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/agentinit"
)

// handleInitCommand runs the guided /init flow: build a bootstrap prompt seeded
// with repo facts (agentinit.BuildPrompt) and launch a normal agent turn that
// investigates the repo and writes AGENTS.md. It reuses launchPrompt, so the
// run, tools, and AGENTS.md write path are exactly the standard ones.
func (m model) handleInitCommand() (tea.Model, tea.Cmd) {
	if m.exiting {
		return m, nil
	}
	prompt := agentinit.BuildPrompt(m.ctx, m.cwd, m.now())
	return m.launchPrompt(prompt)
}

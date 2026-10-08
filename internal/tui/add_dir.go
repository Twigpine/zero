package tui

import (
	"strings"
)

// handleAddDirCommand processes "/add-dir [path]". A bare "/add-dir" lists the
// current write roots; with a path it grants session-scoped write access via
// the sandbox engine's shared scope, so the policy gate, the OS profile of the
// next bash command, and the file tools all widen immediately.
func (m model) handleAddDirCommand(arg string) model {
	engine := m.agentOptions.Sandbox
	if engine == nil || engine.Scope() == nil {
		return m.appendSystemNotice("add-dir: sandbox scope is unavailable in this session.")
	}
	scope := engine.Scope()
	trimmed := strings.TrimSpace(arg)
	if trimmed == "" {
		return m.appendSystemNotice("Write roots:\n  " + strings.Join(scope.Roots(), "\n  ") + "\nUsage: /add-dir <path>  (grants are session-only; use sandbox.additionalWriteRoots in the global config to persist)")
	}
	root, err := scope.Add(trimmed)
	if err != nil {
		return m.appendSystemNotice("add-dir: " + err.Error())
	}
	return m.showTransientNoticeInline("Write access added: "+root+" (this session only).", transientNoticeSuccess)
}

// appendSystemNotice appends a system transcript line and returns the updated
// model. Shared by the inline command handlers (/add-dir, /image) so notice
// plumbing lives in one place.
func (m model) appendSystemNotice(text string) model {
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
	return m
}

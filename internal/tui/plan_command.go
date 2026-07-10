package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/planmode"
	"github.com/Twigpine/zero/internal/tools"
)

type currentPlanReader interface {
	CurrentPlan() []tools.PlanItem
}

// handlePlanCommand toggles plan mode on the current session, in the style of
// openclaude's /plan:
//
//	/plan            toggle plan mode on/off; when on, show the current plan
//	/plan open       open the session's plan file in $VISUAL/$EDITOR
//	/plan off        exit plan mode (alias: /plan exit)
//
// Plan mode is read-only: tool advertisement (see agent.toolAdvertisedInPlan)
// only exposes read tools, update_plan, and ask_user, so the agent cannot
// mutate the workspace while planning.
func (m model) handlePlanCommand(text string) (tea.Model, tea.Cmd) {
	if _, ok := m.registry.Get("update_plan"); !ok {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "No plan is active."})
		return m, nil
	}

	arg := strings.ToLower(strings.TrimSpace(text))
	switch arg {
	case "off", "exit":
		if m.permissionMode != agent.PermissionModePlan {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Plan mode is not active."})
			return m, nil
		}
		m.permissionMode = agent.PermissionModeAuto
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Exited plan mode. The agent can now implement."})
		return m, nil
	case "open":
		return m.openPlanInEditor()
	}

	// No subcommand: toggle plan mode, then surface the current plan.
	if m.permissionMode == agent.PermissionModePlan {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: m.planText()})
		return m, nil
	}
	if m.pending || m.exiting {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "Cannot enter plan mode while a run is active."})
		return m, nil
	}
	m.permissionMode = agent.PermissionModePlan
	textToShow := planEnterText(m) + "\n\n" + m.planText()
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: textToShow})
	return m, nil
}

// planModeCommandUnavailable reports whether a local (non-tool) TUI command
// must be blocked while plan mode is active. Plan mode's tool-advertisement
// gate only covers agent tool calls; these commands run entirely inside the
// TUI process and would mutate the workspace or spawn a host process outside
// that gate: /rewind restores files from a checkpoint, /export writes a
// transcript file to disk, /sandbox-setup runs native platform setup,
// /spec forks a drafting session, /mcp <sub> mutates server configuration, and
// /init's whole job is writing AGENTS.md (which plan mode then denies).
// Bare /mcp (empty text) only opens the read-only manager view, so it stays
// available. Modeled on btwCommandUnavailable's shape for the analogous BTW guard.
func planModeCommandUnavailable(command parsedCommand) bool {
	switch command.kind {
	case commandRewind, commandExport, commandSandboxSetup, commandSpec, commandInit:
		return true
	case commandMCP:
		// Bare /mcp only opens the read-only manager view; subcommands mutate config.
		return strings.TrimSpace(command.text) != ""
	default:
		return false
	}
}

// openPlanInEditor writes the session plan file (if missing) and suspends the
// TUI to launch $VISUAL/$EDITOR on it, resuming on exit.
func (m model) openPlanInEditor() (tea.Model, tea.Cmd) {
	if m.permissionMode != agent.PermissionModePlan {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Enter plan mode (/plan) before opening the plan file."})
		return m, nil
	}
	path, err := planmode.PlanFilePath(m.cwd, m.activeSession.SessionID)
	if err != nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "plan path error: " + err.Error()})
		return m, nil
	}
	if _, ok := fileExists(path); !ok {
		if _, err := planmode.WritePlan(m.cwd, m.activeSession.SessionID, ""); err != nil {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "plan write error: " + err.Error()})
			return m, nil
		}
	}
	editor := strings.TrimSpace(os.Getenv("VISUAL"))
	if editor == "" {
		editor = strings.TrimSpace(os.Getenv("EDITOR"))
	}
	if editor == "" {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Set $VISUAL or $EDITOR to open the plan file:\n" + path})
		return m, nil
	}
	if m.program == nil {
		// No live program (e.g. under test): just report the path.
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Plan file: " + path})
		return m, nil
	}
	parts := strings.Fields(editor)
	cmd := exec.Command(parts[0], append(parts[1:], path)...) //nolint:gosec // editor path from $VISUAL/$EDITOR
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return nil
	})
}

func planEnterText(m model) string {
	path, err := planmode.PlanFilePath(m.cwd, m.activeSession.SessionID)
	planNote := ""
	if err == nil && path != "" {
		planNote = "\nPlan file: " + path
	}
	return "Entered plan mode. The agent can inspect the workspace and shape the plan with update_plan, but cannot edit files or run commands until you exit.\n" +
		"Use /plan to view the plan, /plan open to edit it, or /plan off to implement." + planNote
}
}

func (m model) planText() string {
	// Prefer the session plan file when present.
	if path, err := planmode.PlanFilePath(m.cwd, m.activeSession.SessionID); err == nil && path != "" {
		if content, exists, err := planmode.ReadPlan(m.cwd, m.activeSession.SessionID); err == nil && exists {
			header := "Current Plan (plan mode)"
			if path != "" {
				header += "\n" + path
			}
			return header + "\n" + strings.TrimRight(content, "\n")
		}
	}

	// Fall back to the update_plan list the agent has been building.
	tool, ok := m.registry.Get("update_plan")
	if !ok {
		return "Plan mode is active. No plan written yet. Use update_plan to outline steps, or /plan open to draft the plan file."
	}
	reader, ok := tool.(currentPlanReader)
	if !ok {
		return "Plan mode is active. No plan written yet."
	}
	plan := reader.CurrentPlan()
	if len(plan) == 0 {
		return "Plan mode is active. No plan written yet. Use update_plan to outline steps, or /plan open to draft the plan file."
	}

	lines := make([]string, 0, len(plan)+1)
	lines = append(lines, "Current Plan")
	for index, item := range plan {
		line := fmt.Sprintf("%d. [%s] %s", index+1, item.Status, item.Content)
		if item.Notes != "" {
			line += "\n   Notes: " + item.Notes
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func fileExists(path string) (struct{}, bool) {
	_, err := os.Stat(path)
	if err != nil {
		return struct{}{}, false
	}
	return struct{}{}, true
}

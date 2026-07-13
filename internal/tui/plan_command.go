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

// handlePlanCommand toggles plan mode on the current session:
//
//	/plan            toggle plan mode on/off; entering shows the current plan
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
		m = m.exitPlanMode()
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Exited plan mode. The agent can now implement."})
		return m, nil
	case "open":
		updated, err := m.ensureActiveSession("")
		if err != nil {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "session error: " + err.Error()})
			return m, nil
		}
		return updated.openPlanInEditor()
	}

	// No subcommand: toggle plan mode. A bare /plan while already in plan mode
	// exits it (matching the advertised on/off toggle); entering it shows the
	// plan that was just seeded.
	if m.permissionMode == agent.PermissionModePlan {
		m = m.exitPlanMode()
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Exited plan mode. The agent can now implement."})
		return m, nil
	}
	if m.pending || m.exiting {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "Cannot enter plan mode while a run is active."})
		return m, nil
	}
	updated, err := m.ensureActiveSession("")
	if err != nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "session error: " + err.Error()})
		return m, nil
	}
	m = updated
	m.permissionModeBeforePlan = m.permissionMode
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

// exitPlanMode restores the permission mode that was active before /plan
// entered plan mode. Shared by /plan off, the bare-/plan toggle, and session
// switches (/new, /resume), which must not leave a stale plan-mode grant (or a
// stale "restore to" mode) attached to a session other than the one that set it.
func (m model) exitPlanMode() model {
	if m.permissionMode == agent.PermissionModePlan {
		m.permissionMode = agent.PermissionModeAuto
		if m.permissionModeBeforePlan != "" {
			m.permissionMode = m.permissionModeBeforePlan
		}
	}
	m.permissionModeBeforePlan = ""
	return m
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
	_, exists, err := planmode.ReadPlan(m.cwd, m.activeSession.SessionID)
	if err != nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "plan read error: " + err.Error()})
		return m, nil
	}
	if !exists {
		// Seed the file with the agent's in-memory update_plan draft (if any)
		// rather than leaving it blank: once the file exists, planText prefers
		// it over the draft, so starting empty would shadow real plan content
		// the agent already captured.
		if _, err := planmode.WritePlan(m.cwd, m.activeSession.SessionID, m.formatPlanDraft()); err != nil {
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
	parts := strings.Fields(editor)
	cmd := exec.Command(parts[0], append(parts[1:], path)...) //nolint:gosec // editor path from $VISUAL/$EDITOR
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		if err != nil {
			return planEditorFinishedMsg{err: err}
		}
		return nil
	})
}

// planEditorFinishedMsg reports a failed $VISUAL/$EDITOR run launched by
// /plan open so the transcript can surface it.
type planEditorFinishedMsg struct {
	err error
}

func planEnterText(m model) string {
	planNote := ""
	if path, err := planmode.PlanFilePath(m.cwd, m.activeSession.SessionID); err == nil {
		planNote = "\nPlan file: " + path
	}
	return "Entered plan mode. The agent can inspect the workspace and shape the plan with update_plan, but cannot edit files or run commands until you exit.\n" +
		"Use /plan open to edit the plan, or /plan (again) / /plan off to implement." + planNote
}
}

func (m model) planText() string {
	// Prefer the session plan file when present. update_plan persists to this
	// file on every call (see model.go's OnToolResult hook), so it is the
	// durable source of truth once anything has been captured; the in-memory
	// draft below is only a fallback for a plan that predates any write.
	path, pathErr := planmode.PlanFilePath(m.cwd, m.activeSession.SessionID)
	content, exists, readErr := planmode.ReadPlan(m.cwd, m.activeSession.SessionID)
	switch {
	case readErr != nil:
		// A real I/O/permission failure, not just a not-yet-created file:
		// surface it instead of silently falling back to the in-memory draft,
		// which would hide the failure entirely.
		return "plan file read error: " + readErr.Error()
	case exists:
		header := "Current Plan (plan mode)"
		if pathErr == nil {
			header += "\n" + path
		}
		return header + "\n" + strings.TrimRight(content, "\n")
	}

	// Fall back to the update_plan list the agent has been building.
	if draft := m.formatPlanDraft(); draft != "" {
		return "Current Plan\n" + draft
	}
	return "Plan mode is active. No plan written yet. Use update_plan to outline steps, or /plan open to draft the plan file."
}

// formatPlanDraft renders the agent's in-memory update_plan items as plain
// text, or "" if nothing has been captured yet. Shared by planText's fallback
// and openPlanInEditor's file-seeding so a newly created plan file starts from
// the agent's real draft instead of blank.
func (m model) formatPlanDraft() string {
	tool, ok := m.registry.Get("update_plan")
	if !ok {
		return ""
	}
	reader, ok := tool.(currentPlanReader)
	if !ok {
		return ""
	}
	return formatPlanItems(reader.CurrentPlan())
}

// formatPlanItems renders update_plan items as plain text, or "" if there are
// none. Shared by formatPlanDraft (in-memory fallback for display) and the
// OnToolResult hook in model.go that persists every update_plan call to disk.
func formatPlanItems(items []tools.PlanItem) string {
	if len(items) == 0 {
		return ""
	}
	lines := make([]string, 0, len(items))
	for index, item := range items {
		line := fmt.Sprintf("%d. [%s] %s", index+1, item.Status, item.Content)
		if item.Notes != "" {
			line += "\n   Notes: " + item.Notes
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

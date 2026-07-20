package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	tea "charm.land/bubbletea/v2"
	"mvdan.cc/sh/v3/shell"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/planmode"
	"github.com/Twigpine/zero/internal/tools"
)

// numberedStatusRe matches the "N. [status] " prefix that formatPlanItems
// writes, capturing the status so a user-edited plan file (seeded from that
// format) can be re-parsed back into plan items without losing progress.
var numberedStatusRe = regexp.MustCompile(`^\d+\.\s*(?:\[([^\]]*)\]\s*)?`)

type currentPlanReader interface {
	CurrentPlan() []tools.PlanItem
}

// planFileReloader syncs a user-edited plan file back into the in-memory plan.
// The in-memory update_plan is the execution source of truth; the file is its
// seed and on-disk target, so after /plan open the edited file is reloaded here.
type planFileReloader interface {
	SetPlan([]tools.PlanItem)
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
	case "":
		// Bare /plan: the toggle logic below the switch handles it.
	default:
		// An unrecognized subcommand (a typo like "openx", or "status") must
		// not fall through to the bare toggle: while plan mode is active that
		// would silently exit the read-only boundary and re-enable
		// implementation.
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: fmt.Sprintf("Unknown /plan subcommand %q. Usage: /plan, /plan open, /plan off", arg)})
		return m, nil
	case "off", "exit":
		if m.permissionMode != agent.PermissionModePlan {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Plan mode is not active."})
			return m, nil
		}
		m = m.exitPlanMode()
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Exited plan mode. The agent can now implement."})
		return m, nil
	case "open":
		if m.pending || m.exiting {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "Cannot open the plan file while a run is active."})
			return m, nil
		}
		// Validate plan mode is active before ensureActiveSession, not after:
		// openPlanInEditor rejects this same condition, but by then a session
		// would already have been created for what should be a pure no-op
		// error, leaving a persistent empty session behind in /resume.
		if m.permissionMode != agent.PermissionModePlan {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Enter plan mode (/plan) before opening the plan file."})
			return m, nil
		}
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

// resetPlanForSessionSwitch clears the in-memory plan (both the update_plan
// tool's state and the sticky plan panel) so a session switch doesn't leak
// the previous session's plan into a session that never drafted it. Callers
// must also call exitPlanMode; unlike that call, a plain /plan off/toggle
// within the same session must NOT go through this path, since exiting plan
// mode there is exactly the hand-off into implementing the plan just drafted.
func (m model) resetPlanForSessionSwitch() model {
	if writer, ok := m.registry.Get("update_plan"); ok {
		if reloader, ok := writer.(planFileReloader); ok {
			reloader.SetPlan(nil)
		}
	}
	m.plan.clear()
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
	// The editor is launched on a staged copy outside the workspace, not on
	// path directly: see planmode.StageForEditor for why handing $EDITOR a
	// workspace-relative path would leave a symlink-swap containment race.
	stagedPath, cleanup, err := planmode.StageForEditor(m.cwd, m.activeSession.SessionID)
	if err != nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "plan stage error: " + err.Error()})
		return m, nil
	}
	// $VISUAL/$EDITOR commonly quote an executable path containing spaces
	// (e.g. `"/Applications/Visual Studio Code.app/.../code" --wait`);
	// strings.Fields would split that mid-path. shell.Fields applies POSIX
	// shell word-splitting, so quoted segments and any $VAR references in the
	// value are handled the way a shell would.
	parts, err := shell.Fields(editor, os.Getenv)
	if err != nil || len(parts) == 0 {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "invalid $VISUAL/$EDITOR value: " + editor})
		cleanup()
		return m, nil
	}
	cmd := exec.Command(parts[0], append(parts[1:], stagedPath)...) //nolint:gosec // editor path from $VISUAL/$EDITOR
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	workspaceRoot := m.cwd
	sessionID := m.activeSession.SessionID
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		defer cleanup()
		if err != nil {
			return planEditorFinishedMsg{err: err}
		}
		if commitErr := planmode.CommitStagedEdit(workspaceRoot, sessionID, stagedPath); commitErr != nil {
			return planEditorFinishedMsg{err: commitErr}
		}
		return planEditorFinishedMsg{err: nil}
	})
}

// planEditorFinishedMsg reports a failed $VISUAL/$EDITOR run launched by
// /plan open so the transcript can surface it.
type planEditorFinishedMsg struct {
	err error
}

// reloadPlanFromFile reads the session plan file (if any) and syncs its
// content into the in-memory update_plan, so edits the user makes in $EDITOR
// become the plan that drives execution. The file is only the on-disk target;
// the in-memory plan stays the source of truth. A missing or unreadable file
// is left as-is (the in-memory plan remains authoritative). Returns the parsed
// items and true on success, so the caller can also refresh the sticky plan
// panel, which reloadPlanFromFile cannot do itself as a value-receiver method.
func (m model) reloadPlanFromFile() ([]tools.PlanItem, bool) {
	content, ok, err := planmode.ReadPlan(m.cwd, m.activeSession.SessionID)
	if err != nil || !ok {
		return nil, false
	}
	items := parsePlanFileLines(content)
	if writer, ok := m.registry.Get("update_plan"); ok {
		if reloader, ok := writer.(planFileReloader); ok {
			reloader.SetPlan(items)
		}
	}
	return items, true
}

// parsePlanFileLines converts the plain-text plan file the user edits in
// $EDITOR back into plan items. A numbered line ("N. [status] ...") starts a
// new step; an optional leading "[status]" is parsed back into the item's
// Status (matching formatPlanItems) so completed/in-progress steps survive an
// edit instead of resetting to pending.
//
// Indentation is authoritative and is decided BEFORE anything else: an
// indented line (formatPlanItems writes every continuation with a leading
// "   ") always folds into the current item, even when its text happens to
// look like a numbered step ("2. validate") — deciding by content first
// would shatter such a continuation into a bogus new pending step. Within an
// item, the first indented "Notes: ..." line switches from Content to Notes;
// an indented continuation whose text itself begins with "Notes:" (or a
// backslash) is escaped by formatPlanItems with a leading backslash, which
// this parser strips, so real content is distinguishable from the notes
// delimiter. A whitespace-only indented line is a preserved blank
// continuation line; a fully blank line is a separator and is dropped. A
// non-numbered line with NO leading indentation is a freeform new step (e.g.
// one the user typed without bothering to number or indent it).
func parsePlanFileLines(content string) []tools.PlanItem {
	items := make([]tools.PlanItem, 0)
	inNotes := false
	for _, raw := range strings.Split(content, "\n") {
		raw = strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(raw)
		indented := len(raw) > 0 && (raw[0] == ' ' || raw[0] == '\t')
		if !indented || len(items) == 0 {
			if trimmed == "" {
				continue
			}
			if match := numberedStatusRe.FindStringSubmatch(trimmed); match != nil {
				status := "pending"
				if match[1] != "" {
					status = tools.NormalizePlanStatus(match[1])
				}
				items = append(items, tools.PlanItem{
					Content: strings.TrimSpace(trimmed[len(match[0]):]),
					Status:  status,
				})
				inNotes = false
				continue
			}
			items = append(items, tools.PlanItem{Content: trimmed, Status: "pending"})
			inNotes = false
			continue
		}
		last := &items[len(items)-1]
		if !inNotes {
			if notes, ok := strings.CutPrefix(trimmed, "Notes:"); ok {
				last.Notes = strings.TrimSpace(notes)
				inNotes = true
				continue
			}
		}
		line := unescapePlanContinuation(trimmed)
		if inNotes {
			if last.Notes == "" {
				last.Notes = line
			} else {
				last.Notes += "\n" + line
			}
			continue
		}
		last.Content += "\n" + line
	}
	return items
}

// escapePlanContinuation guards a continuation line whose literal text would
// otherwise be parsed as structure: a line beginning with "Notes:" (the notes
// delimiter) or with a backslash (the escape itself) gets one leading
// backslash, which unescapePlanContinuation strips on reload.
func escapePlanContinuation(line string) string {
	if strings.HasPrefix(strings.TrimSpace(line), "Notes:") || strings.HasPrefix(line, `\`) {
		return `\` + line
	}
	return line
}

func unescapePlanContinuation(line string) string {
	if strings.HasPrefix(line, `\`) {
		return line[1:]
	}
	return line
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
	// Prefer the durable plan file when present. update_plan persists to the
	// per-user plan store on every call (see model.go's OnToolResult hook), so
	// it is the source of truth once anything has been captured; the in-memory
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
//
// A multi-line Content or Notes is rendered with each continuation line
// indented ("   "), matching what parsePlanFileLines expects: it is the
// indentation, not just the "Notes:" marker, that tells a reload apart a
// continuation of the current item from a freeform new step.
func formatPlanItems(items []tools.PlanItem) string {
	if len(items) == 0 {
		return ""
	}
	lines := make([]string, 0, len(items))
	for index, item := range items {
		contentLines := strings.Split(item.Content, "\n")
		line := fmt.Sprintf("%d. [%s] %s", index+1, item.Status, contentLines[0])
		// Continuations are indented (which is what makes them continuations
		// to parsePlanFileLines, even when the text looks like "2. validate")
		// and escaped where their literal text would read as structure.
		for _, cont := range contentLines[1:] {
			line += "\n   " + escapePlanContinuation(cont)
		}
		if item.Notes != "" {
			noteLines := strings.Split(item.Notes, "\n")
			line += "\n   Notes: " + noteLines[0]
			for _, cont := range noteLines[1:] {
				line += "\n   " + escapePlanContinuation(cont)
			}
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// planSnapshotFromResult decodes the plan items a successful update_plan call
// carried in its result meta (tools.PlanSnapshotMeta). ok=false when the
// snapshot is absent or undecodable — the caller then skips panel/file
// updates rather than re-reading the shared tool, whose state may already
// belong to another session by the time the result callback runs.
func planSnapshotFromResult(result agent.ToolResult) ([]tools.PlanItem, bool) {
	encoded, ok := result.Meta[tools.PlanSnapshotMeta]
	if !ok {
		return nil, false
	}
	var items []tools.PlanItem
	if err := json.Unmarshal([]byte(encoded), &items); err != nil {
		return nil, false
	}
	return items, true
}

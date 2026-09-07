package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
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

// planItemsEqual reports whether two plan snapshots carry the same content.
// ID is deliberately excluded: parsePlanFileLines rebuilds items from the file
// text and does not preserve the in-memory IDs, so comparing them would report
// every reload as a change even when the user edited nothing.
func planItemsEqual(left, right []tools.PlanItem) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if strings.TrimSpace(left[index].Content) != strings.TrimSpace(right[index].Content) ||
			tools.NormalizePlanStatus(left[index].Status) != tools.NormalizePlanStatus(right[index].Status) ||
			strings.TrimSpace(left[index].Notes) != strings.TrimSpace(right[index].Notes) {
			return false
		}
	}
	return true
}

// planFileReloader syncs a user-edited plan file back into the in-memory plan.
// The in-memory update_plan is the execution source of truth; the file is its
// seed and on-disk target, so after /plan open the edited file is reloaded here.
type planFileReloader interface {
	SetPlan([]tools.PlanItem)
}

// handlePlanCommand manages the current session's plan mode:
//
//	/plan            show the current plan status
//	/plan on         enter read-only plan mode
//	/plan open       open the session's plan file in $VISUAL/$EDITOR
//	/plan off        exit plan mode (alias: /plan exit)
//
// Plan mode is read-only: tool advertisement (see
// tools.ToolAdvertisedForPermissionMode) only exposes read tools,
// update_plan, and ask_user, so the agent cannot mutate the workspace while
// planning.
func (m model) handlePlanCommand(text string) (tea.Model, tea.Cmd) {
	arg := strings.ToLower(strings.TrimSpace(text))
	switch arg {
	case "", "status":
		if _, ok := m.registry.Get("update_plan"); !ok {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "No plan is active."})
			return m, nil
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: m.planText()})
		return m, nil
	case "on":
		if m.pending || m.exiting {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "Cannot enter plan mode while a run is active."})
			return m, nil
		}
		if m.permissionMode == agent.PermissionModePlan {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Plan mode\nAlready active. " + planEnterText(m)})
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
		reloadWarning := ""
		if items, ok, reloadErr := m.reloadPlanFromFile(); reloadErr != nil {
			reloadWarning = "\nplan reload error: " + reloadErr.Error()
		} else if ok {
			m.plan.updateFromItems(items, m.now())
		}
		// Armed /loop ticks and /goal continuations cannot make progress
		// while tools are read-only. Pause them here so the idle ticker and
		// end-of-turn launcher do not spend tokens on no-op turns.
		pausedLoops := 0
		m, pausedLoops = m.pauseLoopsForPlan()
		if pausedLoops > 0 || m.hasArmedGoalContinuation() {
			reloadWarning += "\nAutomatic /loop and /goal continuations are paused while Plan mode is active."
		}
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Plan mode\n" + planEnterText(m) + reloadWarning})
		return m.syncPeerIdentity(), nil
	case "off", "exit":
		if m.permissionMode != agent.PermissionModePlan {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Plan mode\nNot currently active."})
			return m, nil
		}
		if m.pending || m.exiting {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "Cannot exit plan mode while a run is active. Press Esc to cancel it first."})
			return m, nil
		}
		m = m.exitPlanMode()
		m = m.resumeLoopsAfterPlan()
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Plan mode\nExited. Permission mode restored to " + string(m.permissionMode) + "."})
		m, queuedCmd := m.launchQueuedMessageIfReady()
		if queuedCmd != nil {
			return m, queuedCmd
		}
		return m.launchGoalContinuationIfReady()
	case "open":
		if m.pending || m.exiting {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "Cannot open the plan file while a run is active."})
			return m, nil
		}
		if m.permissionMode != agent.PermissionModePlan {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Enter plan mode (/plan on) before opening the plan file."})
			return m, nil
		}
		updated, err := m.ensureActiveSession("")
		if err != nil {
			m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "session error: " + err.Error()})
			return m, nil
		}
		return updated.openPlanInEditor()
	default:
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: fmt.Sprintf("Unknown /plan subcommand %q. Usage: /plan [status|on|open|off]", arg)})
		return m, nil
	}
}

// planModeCommandUnavailable reports whether a local TUI command would mutate
// the workspace or start a host process outside the plan-mode tool gate.
func planModeCommandUnavailable(command parsedCommand) bool {
	switch command.kind {
	case commandRewind, commandExport, commandSandboxSetup, commandInit:
		return true
	case commandMCP:
		return strings.TrimSpace(command.text) != ""
	default:
		return false
	}
}

// planModeBlocksContinuations reports whether automatic /loop ticks and
// /goal continuations must stay idle. Plan mode cannot run implementation
// turns, so firing them would only burn tokens.
func (m model) planModeBlocksContinuations() bool {
	return m.permissionMode == agent.PermissionModePlan
}

// exitPlanMode restores the permission mode that was active before /plan
// entered plan mode. Shared by /plan off, the bare-/plan toggle, and session
// switches (/new, /resume), which must not leave a stale plan-mode grant (or a
// stale "restore to" mode) attached to a session other than the one that set it.
// When no prior mode was recorded (legacy / incomplete state), fall back to Ask
// rather than Auto so exit does not silently re-enable unrestricted tools.
func (m model) exitPlanMode() model {
	if m.permissionMode == agent.PermissionModePlan {
		if m.permissionModeBeforePlan != "" {
			m.permissionMode = m.permissionModeBeforePlan
		} else {
			m.permissionMode = agent.PermissionModeAsk
		}
	}
	m.permissionModeBeforePlan = ""
	return m.syncPeerIdentity()
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
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: "Enter plan mode (/plan on) before opening the plan file."})
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
		content := formatPlanFile(parsePlanFileLines(m.formatPlanDraft()))
		if _, err := planmode.WritePlan(m.cwd, m.activeSession.SessionID, content); err != nil {
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
	stagedPath, finish, err := stagePlanForEditor(m.ctx, m.cwd, m.activeSession.SessionID)
	if err != nil {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "plan stage error: " + err.Error()})
		return m, nil
	}
	// $VISUAL/$EDITOR commonly quote an executable path containing spaces
	// (e.g. `"/Applications/Visual Studio Code.app/.../code" --wait`);
	// strings.Fields would split that mid-path. shell.Fields applies POSIX
	// shell word-splitting, so quoted segments and any $VAR references in the
	// value are handled the way a shell would. Unquoted Windows paths such as
	// C:\Windows\notepad.exe must not go through POSIX escapes (backslash
	// would drop path separators); see splitEditorCommand.
	parts, err := splitEditorCommand(editor)
	if err != nil || len(parts) == 0 {
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendError, text: "invalid $VISUAL/$EDITOR value: " + editor})
		finish(false)
		return m, nil
	}
	cmd := exec.Command(parts[0], append(parts[1:], stagedPath)...) //nolint:gosec // editor path from $VISUAL/$EDITOR
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	workspaceRoot := m.cwd
	sessionID := m.activeSession.SessionID
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return finishPlanEditor(workspaceRoot, sessionID, stagedPath, finish, err)
	})
}

func stagePlanForEditor(ctx context.Context, workspace, sessionID string) (string, func(bool), error) {
	content, _, err := planmode.ReadPlan(workspace, sessionID)
	if err != nil {
		return "", nil, err
	}
	// Convert before staging so unchanged editor bytes remain a no-op. The
	// baseline comparison protects a competing writer's accepted plan.
	if _, current := planFileBody(content); !current {
		if _, err := planmode.WritePlanIfUnchanged(ctx, workspace, sessionID, formatPlanFile(parsePlanFileLines(content)), content); err != nil {
			return "", nil, fmt.Errorf("convert plan format: %w", err)
		}
	}
	path, finish, err := planmode.StageEditor(workspace, sessionID)
	if err != nil {
		return "", nil, err
	}
	staged, err := os.ReadFile(path)
	if _, current := planFileBody(string(staged)); err != nil || !current {
		finish(false)
		if err != nil {
			return "", nil, fmt.Errorf("read staged plan: %w", err)
		}
		return "", nil, fmt.Errorf("plan format changed concurrently; reopen the editor")
	}
	return path, finish, nil
}

// planEditorFinishedMsg reports a failed $VISUAL/$EDITOR run launched by
// /plan open so the transcript can surface it.
func finishPlanEditor(workspaceRoot, sessionID, stagedPath string, finish func(bool), editorErr error) planEditorFinishedMsg {
	if editorErr != nil {
		finish(false)
		return planEditorFinishedMsg{err: editorErr}
	}
	outcome, err := planmode.CommitStagedEditResult(workspaceRoot, sessionID, stagedPath)
	if err != nil {
		recovery, recoveryErr := planmode.PreserveStagedEdit(stagedPath)
		if recoveryErr != nil {
			recovery = stagedPath
			err = fmt.Errorf("%w; recovery rename failed: %v", err, recoveryErr)
		}
		finish(true)
		return planEditorFinishedMsg{err: fmt.Errorf("%w; saved edits are recoverable at %s", err, recovery)}
	}
	finish(false)
	return planEditorFinishedMsg{outcome: &outcome}
}

type planEditorFinishedMsg struct {
	outcome *planmode.EditResult
	err     error
}

// splitEditorCommand parses $VISUAL/$EDITOR into argv. Single-quoted values and
// backslash-free values use POSIX shell.Fields (spaces inside quotes, $VAR
// expansion). Other Windows commands containing backslashes keep the
// separators literal via windowsEditorFields so `C:\Windows\notepad.exe` (or a
// relative `.\tools\editor.exe`) is not mangled by POSIX escape processing.
func splitEditorCommand(editor string) ([]string, error) {
	return splitEditorCommandFor(runtime.GOOS, editor)
}

func splitEditorCommandFor(goos, editor string) ([]string, error) {
	editor = strings.TrimSpace(editor)
	if editor == "" {
		return nil, fmt.Errorf("empty editor")
	}
	if goos == "windows" && strings.Contains(editor, `\`) && !strings.HasPrefix(editor, "'") {
		parts, err := windowsEditorFields(editor)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			return nil, fmt.Errorf("empty editor")
		}
		return parts, nil
	}
	return shell.Fields(editor, os.Getenv)
}

// windowsEditorFields splits a Windows command line with literal backslashes.
// Double-quoted segments keep internal spaces; outside quotes, whitespace splits.
func windowsEditorFields(s string) ([]string, error) {
	var parts []string
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
		case (c == ' ' || c == '\t') && !inQuote:
			if b.Len() > 0 {
				parts = append(parts, b.String())
				b.Reset()
			}
		default:
			b.WriteByte(c)
		}
	}
	if inQuote {
		return nil, fmt.Errorf("unterminated double quote")
	}
	if b.Len() > 0 {
		parts = append(parts, b.String())
	}
	return parts, nil
}

// reloadPlanFromFile reads the session plan file (if any) and syncs its
// content into the in-memory update_plan, so edits the user makes in $EDITOR
// become the plan that drives execution. The file is only the on-disk target;
// the in-memory plan stays the source of truth. A missing file returns
// ok=false with a nil error (in-memory plan remains authoritative). A real
// ReadPlan failure (I/O, symlink refusal) returns the error so the editor
// round-trip can surface it instead of going silent. Returns the parsed items
// and true on success, so the caller can also refresh the sticky plan panel,
// which reloadPlanFromFile cannot do itself as a value-receiver method.
func (m model) reloadPlanFromFile() ([]tools.PlanItem, bool, error) {
	content, ok, err := planmode.ReadPlan(m.cwd, m.activeSession.SessionID)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	items := parsePlanFileLines(content)
	items = tools.CanonicalizePlanItems(items)
	if writer, ok := m.registry.Get("update_plan"); ok {
		if reloader, ok := writer.(planFileReloader); ok {
			reloader.SetPlan(items)
		}
	}
	return items, true, nil
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
// item, the first indented "Notes: ..." line switches from Content to Notes.
// Versioned files preserve backslashes literally and use "| " to quote a
// structural continuation; unversioned files retain legacy backslash decoding.
// A whitespace-only indented line is a preserved blank
// continuation line; a fully blank line is a separator and is dropped. A
// non-numbered line with NO leading indentation is a freeform new step (e.g.
// one the user typed without bothering to number or indent it).
func parsePlanFileLines(content string) []tools.PlanItem {
	content, literalContinuations := planFileBody(content)
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
		var lineBody string
		switch {
		case strings.HasPrefix(raw, "   "):
			lineBody = raw[3:]
		case strings.HasPrefix(raw, "\t"):
			lineBody = raw[1:]
		default:
			lineBody = strings.TrimLeft(raw, " \t")
		}

		last := &items[len(items)-1]
		var line string
		var quoted bool
		if literalContinuations {
			line, quoted = strings.CutPrefix(lineBody, "| ")
		} else {
			line = unescapePlanContinuation(lineBody)
		}
		if !inNotes && !quoted {
			if notes, ok := strings.CutPrefix(strings.TrimSpace(lineBody), "Notes:"); ok {
				last.Notes = strings.TrimSpace(notes)
				inNotes = true
				continue
			}
		}
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
	if strings.HasPrefix(line, `\\`) {
		return line[1:]
	}
	if strings.HasPrefix(line, `\`) && strings.HasPrefix(strings.TrimSpace(line[1:]), "Notes:") {
		return line[1:]
	}
	return line
}

func planEnterText(m model) string {
	planNote := ""
	if path, err := planmode.PlanFilePath(m.cwd, m.activeSession.SessionID); err == nil {
		planNote = "\nPlan file: " + path
	}
	return "Active: read-only planning for this session. Write and shell tools are hidden. /plan off restores the previous permission mode. Starting or switching sessions exits Plan mode; returning from /btw restores the parent session's mode." + planNote
}

func (m model) planText() string {
	// Prefer the durable plan file when present. update_plan persists to the
	// per-user plan store on every call (see model.go's OnToolResult hook), so
	// it is the source of truth once anything has been captured; the in-memory
	// draft below is only a fallback for a plan that predates any write.
	path, pathErr := planmode.PlanFilePath(m.cwd, m.activeSession.SessionID)
	content, exists, readErr := planmode.ReadPlan(m.cwd, m.activeSession.SessionID)
	if readErr != nil {
		// Keep the accepted session snapshot visible without presenting it as
		// a successful durable read.
		text := "plan file read error: " + readErr.Error()
		if accepted := m.formatPlanDraft(); accepted != "" {
			text += "\nLast accepted plan in this session:\n" + accepted
		}
		return text
	}

	modeLabel := "inactive"
	if m.permissionMode == agent.PermissionModePlan {
		modeLabel = "active"
	}

	if exists {
		content = formatPlanItems(tools.CanonicalizePlanItems(parsePlanFileLines(content)))
		header := fmt.Sprintf("Current Plan (plan mode %s)", modeLabel)
		if pathErr == nil {
			header += "\n" + path
		}
		return header + "\n" + strings.TrimRight(content, "\n")
	}

	// Fall back to the update_plan list the agent has been building.
	if draft := m.formatPlanDraft(); strings.TrimSpace(draft) != "" {
		return fmt.Sprintf("Current Plan (plan mode %s; draft in memory)\n%s", modeLabel, draft)
	}

	if m.permissionMode == agent.PermissionModePlan {
		return "Plan mode is active. No plan written yet. Use update_plan to outline steps, or /plan open to draft the plan file."
	}
	return "Plan mode is inactive. No plan written. Use /plan on to enter plan mode."
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
// none. This is the display/legacy representation; formatPlanFile writes the
// explicitly versioned representation used for durable storage and editing.
//
// A multi-line Content or Notes is rendered with each continuation line
// indented ("   "), matching what parsePlanFileLines expects: it is the
// indentation, not just the "Notes:" marker, that tells a reload apart a
// continuation of the current item from a freeform new step.
func formatPlanItems(items []tools.PlanItem) string {
	return formatPlanItemsVersion(items, false)
}

// The marker makes legacy backslash escapes distinguishable from literal
// editor text. V2 quotes structural continuation lines with "| " instead.
const planFileFormatMarker = "<!-- zero-plan-format: 2 -->"

func planFileBody(content string) (string, bool) {
	first, rest, _ := strings.Cut(content, "\n")
	if strings.TrimSuffix(first, "\r") == planFileFormatMarker {
		return rest, true
	}
	return content, false
}

func formatPlanFile(items []tools.PlanItem) string {
	return planFileFormatMarker + "\n" + formatPlanItemsVersion(items, true)
}

func formatPlanItemsVersion(items []tools.PlanItem, literalContinuations bool) string {
	if len(items) == 0 {
		return ""
	}
	lines := make([]string, 0, len(items))
	escape := escapePlanContinuation
	if literalContinuations {
		escape = func(line string) string {
			if strings.HasPrefix(line, "| ") || strings.HasPrefix(strings.TrimSpace(line), "Notes:") {
				return "| " + line
			}
			return line
		}
	}
	for index, item := range items {
		contentLines := strings.Split(item.Content, "\n")
		line := fmt.Sprintf("%d. [%s] %s", index+1, item.Status, contentLines[0])
		// Continuations are indented (which is what makes them continuations
		// to parsePlanFileLines, even when the text looks like "2. validate")
		// and escaped where their literal text would read as structure.
		for _, cont := range contentLines[1:] {
			line += "\n   " + escape(cont)
		}
		if item.Notes != "" {
			noteLines := strings.Split(item.Notes, "\n")
			line += "\n   Notes: " + noteLines[0]
			for _, cont := range noteLines[1:] {
				line += "\n   " + escape(cont)
			}
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// planSnapshotFromResult extracts the immutable plan items a successful update_plan
// call carried in its typed PlanSnapshot field. ok=false when the snapshot is
// absent or empty — the caller then skips panel/file updates rather than re-reading
// the shared tool, whose state may already belong to another session by the time
// the result callback runs.
func planSnapshotFromResult(result agent.ToolResult) ([]tools.PlanItem, bool) {
	if result.PlanSnapshot != nil {
		return append([]tools.PlanItem{}, result.PlanSnapshot...), true
	}
	return nil, false
}

// sessionToolResultMeta copies result.Meta for session event logging, omitting
// PlanSnapshotMeta if present so the plan body is not persisted twice (durable plan file
// plus event log).
func sessionToolResultMeta(meta map[string]string) map[string]string {
	if len(meta) == 0 {
		return nil
	}
	out := make(map[string]string, len(meta))
	for k, v := range meta {
		if k == tools.PlanSnapshotMeta {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

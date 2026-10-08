package tui

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/tools"
)

type rowKind int

const (
	rowWelcome rowKind = iota
	rowUser
	rowAssistant
	rowReasoning
	rowToolCall
	rowToolResult
	rowPermission
	rowAskUser
	rowSystem
	rowError
	rowSpecialist
	rowRecap
)

type transcriptRow struct {
	kind       rowKind
	id         string
	text       string
	tool       string       // tool name, for tool call/result rows
	status     tools.Status // result status, for tool result rows
	detail     string       // raw multi-line output (e.g. a diff to render as a card)
	hint       string       // one-line actionable hint, rendered faintly below error rows
	arg        string       // secondary argument hint (pattern/command), for tool call rows
	meta       map[string]string
	runID      int // owning run, for tool call rows (0 = rehydrated/unknown)
	permission *agent.PermissionEvent
	askUser    *agent.AskUserRequest
	expanded   bool // collapsible transcript rows, e.g. provider thoughts

	// changedFiles lists the workspace-relative paths a mutating tool result
	// wrote (from tools.Result.ChangedFiles; restored from the session payload on
	// resume). The sidebar FILES section derives its roster from these.
	changedFiles    []string
	changeSummaries []execution.Change

	// specialistInfo holds the specialist card data for rowSpecialist rows.
	// Nil for all other row kinds.
	specialistInfo *specialistInfo

	// Final-answer metadata, set at append time. Interim assistant text streams
	// through model.streamingText and never lands in the transcript, so a
	// rowAssistant marked final IS the turn's answer — the renderer must not
	// re-parse text to tell the two apart. turnTools/turnElapsed feed the done
	// line; zero values mean "unknown" and the segment is omitted.
	final       bool
	turnTools   int
	turnElapsed time.Duration

	// attachments records what accompanied a user turn without retaining the
	// original bytes in transcript memory or the session log.
	attachments transcriptAttachmentSummary
}

type transcriptAttachmentSummary struct {
	images    int
	documents int
}

const (
	persistedAttachmentCountLimit = 64
	attachmentSummaryVisibleItems = 4
)

func (summary transcriptAttachmentSummary) empty() bool {
	return summary.images <= 0 && summary.documents <= 0
}

type transcriptActionKind int

const (
	actionAppendUser transcriptActionKind = iota
	actionAppendAssistant
	actionAppendSystem
	actionAppendError
	actionClear
)

type transcriptAction struct {
	kind        transcriptActionKind
	text        string
	attachments transcriptAttachmentSummary
}

func initialTranscript() []transcriptRow {
	return []transcriptRow{{
		kind: rowWelcome,
		text: "Welcome to Zero. Type /help for commands.",
	}}
}

func reduceTranscript(rows []transcriptRow, action transcriptAction) []transcriptRow {
	switch action.kind {
	case actionClear:
		return initialTranscript()
	case actionAppendUser:
		return appendTranscriptRow(rows, transcriptRow{kind: rowUser, text: action.text, attachments: action.attachments})
	case actionAppendAssistant:
		return appendRow(rows, rowAssistant, action.text)
	case actionAppendSystem:
		return appendRow(rows, rowSystem, action.text)
	case actionAppendError:
		return appendRow(rows, rowError, action.text)
	default:
		return rows
	}
}

func appendRow(rows []transcriptRow, kind rowKind, text string) []transcriptRow {
	return appendTranscriptRow(rows, transcriptRow{kind: kind, text: text})
}

func appendTranscriptRow(rows []transcriptRow, row transcriptRow) []transcriptRow {
	if hasTranscriptRow(rows, row) {
		return rows
	}
	// In-place append is safe: every transcript mutation happens on the Bubble
	// Tea update goroutine (agent goroutines only Send messages), so no other
	// model copy can append into the same backing array concurrently. The old
	// full-slice copy made appends O(n) and rehydration O(n²).
	return append(rows, row)
}

// appendTranscriptRowsDedup appends newRows to rows in a single pass, skipping
// keyed rows already present (by transcriptRowKey). It is the bulk equivalent of
// calling appendTranscriptRow per row, but builds the key set ONCE instead of
// re-scanning every existing row on each append — turning rehydration of a
// tool-heavy session from O(n²) into O(n). Semantics are identical: unkeyed rows
// always append; a keyed row is skipped if its key already appeared (in rows, or
// earlier within newRows).
func appendTranscriptRowsDedup(rows []transcriptRow, newRows []transcriptRow) []transcriptRow {
	if len(newRows) == 0 {
		return rows
	}
	seen := make(map[string]struct{}, len(rows)+len(newRows))
	for _, existing := range rows {
		if key := transcriptRowKey(existing); key != "" {
			seen[key] = struct{}{}
		}
	}
	for _, row := range newRows {
		if key := transcriptRowKey(row); key != "" {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}
		rows = append(rows, row)
	}
	return rows
}

// isSwarmStatusTool reports whether a tool is one of the swarm polling tools
// whose repeated identical result cards should be collapsed in the transcript.
func isSwarmStatusTool(name string) bool {
	return name == "swarm_status" || name == "swarm_collect"
}

// collapseRepeatedStatusCard drops the immediately-preceding identical swarm
// status card (its call+result pair) when newResult repeats it verbatim, so a
// model that re-checks swarm_status/swarm_collect doesn't flood the chat with
// the same card. It only fires when the tail is exactly
// [prev call][prev identical result][new call] — two back-to-back checks with
// nothing between them — so it never removes a card that has intervening content
// or a changed state. The new call row at the tail is kept; the caller appends
// the new result row afterwards.
func collapseRepeatedStatusCard(rows []transcriptRow, newResult transcriptRow) []transcriptRow {
	if newResult.kind != rowToolResult || !isSwarmStatusTool(newResult.tool) {
		return rows
	}
	n := len(rows)
	if n < 3 {
		return rows
	}
	newCall, prevResult, prevCall := rows[n-1], rows[n-2], rows[n-3]
	if newCall.kind != rowToolCall || newCall.tool != newResult.tool {
		return rows
	}
	if prevResult.kind != rowToolResult || prevResult.tool != newResult.tool || prevResult.detail != newResult.detail {
		return rows
	}
	if prevCall.kind != rowToolCall || prevCall.tool != newResult.tool {
		return rows
	}
	// Drop the older call+result pair (n-3, n-2); keep the new call row (n-1).
	return append(rows[:n-3], newCall)
}

func hasTranscriptRow(rows []transcriptRow, row transcriptRow) bool {
	key := transcriptRowKey(row)
	if key == "" {
		return false
	}
	for _, existing := range rows {
		if transcriptRowKey(existing) == key {
			return true
		}
	}
	return false
}

// transcriptRowKey is run-scoped (runID baked into every key): some providers
// synthesize ToolCallIDs that repeat across runs (e.g. Gemini's gemini_tool_N),
// and a bare-id key silently dropped later runs' tool rows as "duplicates".
// Repeats WITHIN one run are disambiguated upstream by the per-run ordinal
// suffix the runner appends to row ids (see effectiveToolRowID).
func transcriptRowKey(row transcriptRow) string {
	switch row.kind {
	case rowToolCall, rowToolResult:
		if row.id != "" {
			return fmt.Sprintf("%d:%d:%s", row.kind, row.runID, row.id)
		}
	case rowReasoning:
		if row.id != "" {
			return fmt.Sprintf("%d:%d:%s", row.kind, row.runID, row.id)
		}
	case rowPermission:
		if row.permission != nil && row.permission.ToolCallID != "" {
			return fmt.Sprintf("%d:%d:%s:%s", row.kind, row.runID, row.permission.ToolCallID, row.permission.Action)
		}
	case rowAskUser:
		// Prefer row.id (set to the ToolCallID): it survives rehydration even when
		// row.askUser is nil, so a reloaded ask_user row still dedupes correctly.
		if row.id != "" {
			return fmt.Sprintf("%d:%d:%s", row.kind, row.runID, row.id)
		}
		if row.askUser != nil && row.askUser.ToolCallID != "" {
			return fmt.Sprintf("%d:%d:%s", row.kind, row.runID, row.askUser.ToolCallID)
		}
	}
	return ""
}

func reasoningTranscriptRow(id string, runID int, text string) (transcriptRow, bool) {
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return transcriptRow{}, false
	}
	return transcriptRow{kind: rowReasoning, id: id, runID: runID, text: text}, true
}

func previousVisibleTranscriptKind(rows []transcriptRow, before int, rc rowContext) (rowKind, bool) {
	if before > len(rows) {
		before = len(rows)
	}
	for index := before - 1; index >= 0; index-- {
		row := rows[index]
		if row.kind == rowWelcome || rc.skip(row) {
			continue
		}
		return row.kind, true
	}
	return rowWelcome, false
}

// effectiveToolRowID disambiguates a provider tool-call id that repeats within
// a run: the first occurrence keeps the raw id (the common case), repeats get
// an ordinal suffix. Session payloads are unaffected — they persist the
// provider's original ids.
func effectiveToolRowID(id string, seq int) string {
	if id == "" || seq <= 1 {
		return id
	}
	return fmt.Sprintf("%s#%d", id, seq)
}

func askUserTranscriptRow(request agent.AskUserRequest) transcriptRow {
	return transcriptRow{
		kind:    rowAskUser,
		id:      request.ToolCallID,
		text:    askUserRowText(request),
		detail:  askUserDetailText(request),
		askUser: &request,
	}
}

func askUserRowText(request agent.AskUserRequest) string {
	parts := []string{"ask_user:"}
	if header := strings.TrimSpace(request.Header); header != "" {
		parts = append(parts, header)
	} else {
		parts = append(parts, fmt.Sprintf("%d question(s)", len(request.Questions)))
	}
	return strings.Join(parts, " ")
}

func askUserDetailText(request agent.AskUserRequest) string {
	lines := make([]string, 0, len(request.Questions))
	for index, question := range request.Questions {
		line := fmt.Sprintf("%d. %s", index+1, question.Question)
		if len(question.Options) > 0 {
			line += "  (" + strings.Join(question.Options, ", ") + ")"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func askUserSessionPayload(request agent.AskUserRequest) map[string]any {
	questions := make([]map[string]any, 0, len(request.Questions))
	for _, question := range request.Questions {
		entry := map[string]any{"question": question.Question}
		if len(question.Options) > 0 {
			entry["options"] = question.Options
		}
		if question.MultiSelect {
			entry["multiSelect"] = true
		}
		questions = append(questions, entry)
	}
	payload := map[string]any{
		"role":       "ask_user",
		"toolCallId": request.ToolCallID,
		"questions":  questions,
	}
	if header := strings.TrimSpace(request.Header); header != "" {
		payload["header"] = header
	}
	return payload
}

func permissionTranscriptRow(event agent.PermissionEvent) transcriptRow {
	return transcriptRow{
		kind:       rowPermission,
		id:         event.ToolCallID,
		text:       permissionRowText(event),
		tool:       event.ToolName,
		detail:     permissionDetailText(event),
		permission: &event,
	}
}

// permissionEventIsNoteworthy reports whether a permission event carries
// user-facing information worth a visible transcript row. A silently
// auto-approved call (auto mode, or a previously-granted scope that just
// matched) is NOT noteworthy: the tool card already shows the action, target,
// and result, so a separate "always · <tool> · target:<path>" row is pure
// noise — the reference agents only surface approval when the user is actually
// prompted or makes an explicit durable choice. The underlying audit event is
// still recorded regardless (see the OnPermission handler and resume rebuild);
// this only gates the rendered row, so the session log / `zero sessions` stay
// complete.
//
// Used in BOTH the live path (model.go OnPermission) and the resume rebuild
// (session.go) so a resumed session shows exactly the rows the live view did.
func permissionEventIsNoteworthy(event agent.PermissionEvent) bool {
	switch event.Action {
	case agent.PermissionActionPrompt, agent.PermissionActionDeny, agent.PermissionActionCancel:
		// A real prompt was shown, or the call was blocked/cancelled — always show.
		return true
	}
	// A non-empty DecisionAction means the user actually decided (allow once,
	// allow for session, always, prefix, …): the agent only sets it when a
	// decision was made, leaving it empty for a silent auto-approve. So any real
	// decision is worth one visible row, even a plain "allow once".
	if strings.TrimSpace(string(event.DecisionAction)) != "" {
		return true
	}
	// A safety-relevant block stays visible even if somehow allowed.
	if event.Block != nil {
		return true
	}
	// Everything else (plain auto-approve, or a pre-granted scope auto-matching)
	// is silent — the tool card speaks for it.
	return false
}

func permissionEventFromRequest(request agent.PermissionRequest) agent.PermissionEvent {
	return agent.PermissionEvent{
		ToolCallID:     request.ToolCallID,
		ToolName:       request.ToolName,
		Action:         request.Action,
		Permission:     request.Permission,
		PermissionMode: request.PermissionMode,
		Autonomy:       request.Autonomy,
		SideEffect:     request.SideEffect,
		Reason:         request.Reason,
		Scope:          request.Scope,
		Risk:           request.Risk,
		Block:          request.Block,
		GrantMatched:   request.GrantMatched,
		Grant:          request.Grant,
	}
}

func permissionRowText(event agent.PermissionEvent) string {
	parts := []string{"permission:"}
	if event.ToolName != "" {
		parts = append(parts, event.ToolName)
	}
	if event.Action != "" {
		parts = append(parts, string(event.Action))
	}
	return strings.Join(parts, " ")
}

func permissionDetailText(event agent.PermissionEvent) string {
	parts := []string{}
	if event.DecisionAction != "" {
		parts = append(parts, permissionDecisionDetail(event.DecisionAction))
	}
	if event.GrantMatched {
		parts = append(parts, "approved by saved permission")
	}
	if event.Action == agent.PermissionActionPrompt {
		if reason := permissionDisplayReason(event.Reason); reason != "" {
			parts = append(parts, reason)
		}
	}
	if event.Block != nil {
		parts = append(parts, permissionBlockDetail(event))
	}
	return strings.Join(parts, "  ")
}

func permissionDecisionDetail(decision agent.PermissionDecisionAction) string {
	switch decision {
	case agent.PermissionDecisionAllow:
		return "approved once"
	case agent.PermissionDecisionAllowStrict:
		return "approved with review"
	case agent.PermissionDecisionAllowForSession:
		return "approved for this session"
	case agent.PermissionDecisionAllowPrefix:
		return "approved command prefix for this session"
	case agent.PermissionDecisionAlwaysAllowPrefix:
		return "always approved command prefix"
	case agent.PermissionDecisionAlwaysAllow:
		return "always approved"
	case agent.PermissionDecisionDeny:
		return "denied by user"
	case agent.PermissionDecisionCancel:
		return "cancelled by user"
	default:
		return strings.ReplaceAll(string(decision), "_", " ")
	}
}

func permissionBlockDetail(event agent.PermissionEvent) string {
	if event.Block == nil {
		return ""
	}
	parts := []string{"blocked: " + permissionBlockLabel(string(event.Block.Code))}
	if path := strings.TrimSpace(event.Block.Path); path != "" {
		parts = append(parts, "path: "+path)
	}
	reason := permissionDisplayReason(event.Block.Reason)
	eventReason := permissionDisplayReason(event.Reason)
	if reason != "" && reason != eventReason {
		parts = append(parts, reason)
	}
	return strings.Join(parts, "  ")
}

func permissionBlockLabel(code string) string {
	switch code {
	case "outside_workspace":
		return "outside workspace"
	case "symlink_traversal":
		return "symlink traversal"
	case "network":
		return "network access"
	case "destructive_command":
		return "destructive command"
	case "persistent_deny":
		return "saved deny"
	case "denied_permission":
		return "permission denied"
	case "context_canceled":
		return "request cancelled"
	case "denied":
		return "not allowed"
	default:
		return strings.ReplaceAll(code, "_", " ")
	}
}

func permissionDisplayReason(reason string) string {
	reason = strings.TrimSpace(reason)
	switch reason {
	case "network access requires approval":
		return "Network access requires approval."
	case sandbox.ReasonEscalatedSandboxRequired:
		return "This command needs to run outside the sandbox."
	case "sandbox output is limited to the sandbox PID namespace; host/global state requires approval":
		return "Host/global state is hidden by the sandbox, so this command needs to run outside it."
	case "workspace write is allowed":
		return "Workspace write is allowed."
	default:
		return reason
	}
}

func truncateTUIOutput(output string, limit int) string {
	output = strings.TrimSpace(strings.ReplaceAll(output, "\r\n", "\n"))
	output = strings.ReplaceAll(output, "\n", " ")
	if limit <= 0 || len(output) <= limit {
		return output
	}
	// Cut on a rune boundary: a bare byte slice can split a multi-byte UTF-8
	// sequence and emit invalid UTF-8 into the transcript and session log.
	return cutRunes(output, limit) + " [truncated]"
}

// cutRunes truncates text to at most limit bytes without splitting a UTF-8
// rune (the cut lands on the last rune boundary at or before limit).
func cutRunes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

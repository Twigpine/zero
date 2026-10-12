package acp

import (
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/tools"
)

// translate.go maps ZERO's agent events onto ACP session/update payloads. The
// mapping functions are pure (no I/O) so they can be unit-tested directly; the
// notifier wires them to a live JSON-RPC connection.

func agentMessageChunk(delta string) ContentChunk {
	return ContentChunk{SessionUpdate: UpdateAgentMessageChunk, Content: TextBlock(delta)}
}

func replayMessageChunk(role, messageID, text string) ContentChunk {
	update := UpdateAgentMessageChunk
	if role == "user" {
		update = UpdateUserMessageChunk
	}
	return ContentChunk{SessionUpdate: update, MessageID: messageID, Content: TextBlock(text)}
}

func agentThoughtChunk(delta string) ContentChunk {
	return ContentChunk{SessionUpdate: UpdateAgentThoughtChunk, Content: TextBlock(delta)}
}

// toolKindFor maps a ZERO tool name to the closest ACP ToolKind so editors can
// pick an icon/affordance. Unknown tools fall back to "other".
func toolKindFor(name string) string {
	switch name {
	case "read_file", "read_minified_file", "list_directory":
		return ToolKindRead
	case "glob", "grep":
		return ToolKindSearch
	case "write_file", "edit_file", "apply_patch":
		return ToolKindEdit
	case "bash", "exec_command", "write_stdin":
		return ToolKindExecute
	case "web_fetch", "web_search":
		return ToolKindFetch
	case "update_plan":
		return ToolKindThink
	default:
		return ToolKindOther
	}
}

// toolTitle builds a concise human title, e.g. "read_file src/main.go".
func toolTitle(name, rawArgs string) string {
	if browser, ok := browserToolDetails(name); ok {
		return browserToolTitle(browser.Command, rawArgs)
	}
	if hint := primaryArgHint(rawArgs); hint != "" {
		return name + " " + hint
	}
	return name
}

// browserToolDetails identifies ZERO's local browser helpers without treating
// similarly named MCP tools as browser automation. The descriptor intentionally
// contains no request data: ACP tool input is already protocol-visible, but a
// durable UI must not need to retain text, local CDP targets, or full URLs just
// to recognise the browser operation.
func browserToolDetails(name string) (*BrowserToolDetails, bool) {
	const prefix = "browser_"
	command, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return nil, false
	}
	switch command {
	case "install", "launch", "connect", "open", "snapshot", "click", "type", "press", "action":
		return &BrowserToolDetails{Version: 1, Command: command}, true
	default:
		return nil, false
	}
}

const zeroBrowserMetaKey = "github.com/Gitlawb/zero/browser"

// attachBrowserToolDetails stores ZERO's browser descriptor in ACP's reserved
// extension channel. Keeping this in one helper prevents start, result, and
// permission payloads from drifting onto different wire shapes.
func attachBrowserToolDetails(update *ToolCallUpdate, name string) {
	browser, ok := browserToolDetails(name)
	if !ok {
		return
	}
	raw, err := json.Marshal(browser)
	if err != nil {
		return
	}
	update.Meta = map[string]json.RawMessage{zeroBrowserMetaKey: raw}
}

// browserToolTitle avoids putting browser_type text, an attached DevTools
// endpoint, or a URL query/fragment in a tool-card title. Those values can
// carry credentials or session data; the UI only needs the operation and, for
// navigation, a human-recognisable origin.
func browserToolTitle(command, rawArgs string) string {
	switch command {
	case "action":
		action, ok := exactJSONStringArg(rawArgs, "command")
		if !ok {
			return "browser action"
		}
		if action, ok := tools.NormalizedBrowserActionCommand(action); ok {
			return "browser action " + action
		}
		return "browser action"
	case "open":
		rawURL, ok := exactJSONStringArg(rawArgs, "url")
		if !ok {
			return "browser open"
		}
		normalized, err := tools.NormalizeBrowserOpenURL(rawURL)
		if err != nil {
			return "browser open"
		}
		u, err := url.Parse(normalized)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return "browser open"
		}
		origin := u.Scheme + "://" + u.Host
		if !browserTitleTextSafe(origin) {
			return "browser open"
		}
		return "browser open " + truncateHint(origin)
	default:
		return "browser " + command
	}
}

// browserTitleTextSafe validates text after URL parsing has decoded escaped
// UTF-8 in the host. Valid UTF-8 alone is not presentation-safe: whitespace, control,
// format/bidi, and line/paragraph separator runes can reorder or split the
// permission label shown to a user. The execution URL remains unchanged.
func browserTitleTextSafe(text string) bool {
	if !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return false
		}
	}
	return true
}

// exactJSONStringArg mirrors ZERO's map-based tool argument decoding: only the
// exact JSON key is considered, and a non-string value is invalid. In
// particular, an incidental "URL" key must not change a permission title when
// browser_open will only read "url".
func exactJSONStringArg(rawArgs, key string) (string, bool) {
	var args map[string]json.RawMessage
	if json.Unmarshal([]byte(rawArgs), &args) != nil {
		return "", false
	}
	raw, ok := args[key]
	if !ok {
		return "", false
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

// primaryArgHint extracts the most relevant argument (path/pattern/command) from
// raw JSON arguments. Best-effort; returns "" when it can't parse.
func primaryArgHint(rawArgs string) string {
	if strings.TrimSpace(rawArgs) == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(rawArgs), &m); err != nil {
		return ""
	}
	for _, key := range []string{"path", "file_path", "pattern", "query", "command", "url", "cwd"} {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			return truncateHint(v)
		}
	}
	return ""
}

func truncateHint(s string) string {
	s = strings.TrimSpace(s)
	const max = 60
	if utf8.RuneCountInString(s) > max {
		return string([]rune(s)[:max]) + "…"
	}
	return s
}

// rawInput returns the tool arguments as a raw JSON object when they parse, else
// nil (so a malformed/empty arg string never produces invalid JSON on the wire).
func rawInput(args string) json.RawMessage {
	if strings.TrimSpace(args) == "" || !json.Valid([]byte(args)) {
		return nil
	}
	return json.RawMessage(args)
}

// toolCallStart maps an advertised ZERO tool call to the initial ACP "tool_call"
// update (status in_progress — ZERO executes immediately after advertising).
func toolCallStart(call agent.ToolCall) ToolCallUpdate {
	upd := ToolCallUpdate{
		SessionUpdate: UpdateToolCall,
		ToolCallID:    call.ID,
		Title:         toolTitle(call.Name, call.Arguments),
		Kind:          toolKindFor(call.Name),
		Status:        ToolStatusInProgress,
		RawInput:      rawInput(call.Arguments),
	}
	attachBrowserToolDetails(&upd, call.Name)
	return upd
}

// toolCallResult maps a finished ZERO tool result to a "tool_call_update".
func toolCallResult(result agent.ToolResult) ToolCallUpdate {
	status := ToolStatusCompleted
	if result.Status == tools.StatusError {
		status = ToolStatusFailed
	}
	upd := ToolCallUpdate{
		SessionUpdate: UpdateToolCallUpdate,
		ToolCallID:    result.ToolCallID,
		Status:        status,
	}
	if content := toolResultContent(result); len(content) > 0 {
		upd.Content = content
	}
	if locs := toolResultLocations(result); len(locs) > 0 {
		upd.Locations = locs
	}
	attachBrowserToolDetails(&upd, result.Name)
	return upd
}

func toolResultContent(result agent.ToolResult) []ToolCallContent {
	content := make([]ToolCallContent, 0, 1+len(result.FileDiffs))
	text := strings.TrimRight(result.Output, "\n")
	if text == "" {
		text = result.Display.Summary
	}
	if text == "" {
		return appendToolResultDiffs(content, result.FileDiffs)
	}
	content = append(content, ToolContent(TextBlock(text)))
	return appendToolResultDiffs(content, result.FileDiffs)
}

func appendToolResultDiffs(content []ToolCallContent, diffs []tools.FileDiff) []ToolCallContent {
	for _, diff := range diffs {
		if !filepath.IsAbs(diff.Path) || (!diff.OldExists && !diff.NewExists) {
			continue
		}
		var oldText *string
		if diff.OldExists {
			old := diff.OldText
			oldText = &old
		}
		var newText *string
		if diff.NewExists {
			updated := diff.NewText
			newText = &updated
		}
		content = append(content, ToolCallContent{Type: "diff", Path: diff.Path, OldText: oldText, NewText: newText})
	}
	return content
}

func toolResultLocations(result agent.ToolResult) []ToolCallLocation {
	locs := make([]ToolCallLocation, 0, len(result.FileDiffs)+len(result.ChangedFiles))
	seen := make(map[string]bool, len(result.FileDiffs)+len(result.ChangedFiles))
	for _, diff := range result.FileDiffs {
		path := diff.Path
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		locs = append(locs, ToolCallLocation{Path: path})
	}
	// FileDiff.Path is canonical absolute path data while ChangedFiles is
	// normally workspace-relative. Without the trusted workspace root these
	// coordinate systems cannot be correlated safely: a suffix match would let
	// /workspace/sub/a.go consume the fallback for a distinct root a.go. The
	// shared seen set deduplicates only identities already exactly comparable.
	for _, f := range result.ChangedFiles {
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		locs = append(locs, ToolCallLocation{Path: f})
	}
	return locs
}

// planUpdate maps ZERO's plan items to an ACP "plan" update.
func planUpdate(items []tools.PlanItem) PlanUpdate {
	entries := make([]PlanEntry, 0, len(items))
	for _, it := range items {
		entries = append(entries, PlanEntry{
			Content:  it.Content,
			Priority: PlanPriorityMedium,
			Status:   planStatusToACP(it.Status),
		})
	}
	return PlanUpdate{SessionUpdate: UpdatePlan, Entries: entries}
}

// planStatusToACP maps ZERO's plan status (pending/in_progress/completed/failed)
// to ACP's PlanEntryStatus (which has no "failed"; a failed step is terminal, so
// it maps to completed).
func planStatusToACP(s string) string {
	switch s {
	case "in_progress":
		return PlanStatusInProgress
	case "completed", "failed":
		return PlanStatusCompleted
	default:
		return PlanStatusPending
	}
}

// promptText concatenates the text content blocks of an inbound prompt.
func promptText(blocks []ContentBlock) string {
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// notifier sends translated updates over a connection for one session.
type notifier struct {
	conn      *Conn
	sessionID string
}

func (n *notifier) send(update any) {
	_ = n.conn.Notify(MethodSessionUpdate, SessionNotification{SessionID: n.sessionID, Update: update})
}

func (n *notifier) text(delta string) {
	if delta != "" {
		n.send(agentMessageChunk(delta))
	}
}

func (n *notifier) thought(delta string) {
	if delta != "" {
		n.send(agentThoughtChunk(delta))
	}
}

func (n *notifier) toolCall(call agent.ToolCall)       { n.send(toolCallStart(call)) }
func (n *notifier) toolResult(result agent.ToolResult) { n.send(toolCallResult(result)) }

func (n *notifier) plan(items []tools.PlanItem) {
	if len(items) > 0 {
		n.send(planUpdate(items))
	}
}

func (n *notifier) currentMode(modeID string) {
	n.send(CurrentModeUpdate{SessionUpdate: UpdateCurrentMode, CurrentModeID: modeID})
}

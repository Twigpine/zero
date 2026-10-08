package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/usage"
)

const tuiSessionTitleLimit = 80

type pendingSessionEvent struct {
	Type    sessions.EventType
	Payload any
}

func (m model) ensureActiveSession(prompt string) (model, error) {
	if m.activeSession.SessionID != "" {
		return m, nil
	}

	title := strings.TrimSpace(m.pendingSessionTitle)
	manuallyNamed := title != ""
	if !manuallyNamed {
		title = tuiSessionTitle(prompt)
	}
	session, err := m.sessionStore.Create(sessions.CreateInput{
		Title:    title,
		Cwd:      m.cwd,
		ModelID:  m.modelName,
		Provider: m.providerName,
	})
	if err != nil {
		return m, err
	}
	m.activeSession = session
	m.pendingSessionTitle = ""
	m.sessionEvents = []sessions.Event{}
	if manuallyNamed {
		if m.titledSessions == nil {
			m.titledSessions = map[string]bool{}
		}
		m.titledSessions[session.SessionID] = true
	}
	m = m.syncPeerIdentity()
	return m, nil
}

// startNewSession abandons the visible conversation and the agent's in-context
// history and begins a fresh session in place. The current session already lives
// on disk (its events are persisted as they happen), so it stays resumable via
// /resume; here we only clear the in-memory conversation and the per-session
// usage/compaction counters, then let the next prompt lazily create a new session
// through ensureActiveSession — the same seam a cold start uses. Model, provider,
// permission mode, and response style are intentionally preserved: /new starts a
// clean conversation, not a clean configuration.
func (m model) startNewSession() model {
	previousID := m.activeSession.SessionID

	m.activeSession = sessions.Metadata{}
	m.pendingSessionTitle = ""
	m.sessionEvents = nil

	// Reset the per-session usage + compaction display so the new session starts
	// from zero instead of inheriting the previous conversation's token/cost totals.
	if m.usageTracker != nil {
		m.usageTracker.Reset()
	}
	m.lastUsage = usage.Normalized{}
	m.lastUsageSeen = false
	m.unpricedRequests = 0
	m.unpricedTokens = 0
	m.compactRequests = 0
	m.compactFrame = 0
	m.lastCompactResult = nil
	m.lastCompactError = ""
	m.turnLatencySum = 0
	m.turnLatencyCount = 0
	m.turnTTFTSum = 0
	m.turnTTFTCount = 0

	// Staged input belongs to the previous conversation. Attachments and a queued
	// message are only consumed at prompt-submit, so without clearing them here the
	// fresh session's first prompt would silently inherit the old session's images,
	// documents, or queued text.
	m.pendingImages = nil
	m.pendingImageLabels = nil
	m.pendingImageThumbnails = nil
	m.pendingDocuments = nil
	m.queuedMessage = ""
	// The remembered /retry attachment snapshot belongs to the previous session
	// too — dropping it keeps a post-/new /retry from re-staging old images or
	// documents. lastPrompt is composer history (like inputHistory) and stays.
	m.lastImages = nil
	m.lastImageLabels = nil
	m.lastDocuments = nil

	note := "Started a new session."
	if previousID != "" {
		note = "New session started. Previous session saved as " + previousID +
			" — resume it anytime with /resume " + previousID + "."
	}
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionClear})
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: note})
	// Scrollback above can't be un-printed; a faint divider marks the boundary and
	// the flush frontier restarts for the fresh transcript (mirrors /clear, /resume).
	m.resetFlushFrontier("· new session ·")
	// Loops belong to the previous session; stop them so they don't fire the old
	// conversation's prompt into the fresh one.
	if updated, cleared := m.clearLoopsForSessionSwitch(); cleared > 0 {
		m = updated
		m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: fmt.Sprintf("Stopped %d loop(s) tied to the previous session.", cleared)})
	}
	m = m.syncPeerIdentity()
	return m
}

func (m model) appendSessionEvent(eventType sessions.EventType, payload any) (model, error) {
	if m.activeSession.SessionID == "" {
		return m, nil
	}

	event, err := m.sessionStore.AppendEvent(m.activeSession.SessionID, sessions.AppendEventInput{
		Type:    eventType,
		Payload: payload,
	})
	if err != nil {
		return m, err
	}
	m.activeSession.UpdatedAt = event.CreatedAt
	m.activeSession.EventCount = event.Sequence
	m.activeSession.LastEventType = event.Type
	m.sessionEvents = append(m.sessionEvents, event)
	return m, nil
}

func (m model) appendSessionEvents(events []pendingSessionEvent) (model, []transcriptRow) {
	rows := []transcriptRow{}
	if m.activeSession.SessionID == "" || len(events) == 0 {
		return m, rows
	}
	inputs := make([]sessions.AppendEventInput, 0, len(events))
	for _, event := range events {
		inputs = append(inputs, sessions.AppendEventInput{Type: event.Type, Payload: event.Payload})
	}
	appended, err := m.sessionStore.AppendEvents(m.activeSession.SessionID, inputs)
	if err != nil {
		rows = append(rows, transcriptRow{kind: rowError, text: "session record error: " + err.Error()})
		return m, rows
	}
	if len(appended) > 0 {
		last := appended[len(appended)-1]
		m.activeSession.UpdatedAt = last.CreatedAt
		m.activeSession.EventCount = last.Sequence
		m.activeSession.LastEventType = last.Type
		m.sessionEvents = append(m.sessionEvents, appended...)
	}
	return m, rows
}

// appendSessionEventsTo persists events into a specific (non-active) session —
// the late flush of a run cancelled before a /resume switched sessions. The
// active session's in-memory metadata is deliberately untouched.
func (m model) appendSessionEventsTo(sessionID string, events []pendingSessionEvent) []transcriptRow {
	rows := []transcriptRow{}
	if m.sessionStore == nil || sessionID == "" {
		return rows
	}
	inputs := make([]sessions.AppendEventInput, 0, len(events))
	for _, event := range events {
		inputs = append(inputs, sessions.AppendEventInput{Type: event.Type, Payload: event.Payload})
	}
	if _, err := m.sessionStore.AppendEvents(sessionID, inputs); err != nil {
		rows = append(rows, transcriptRow{kind: rowError, text: "session record error: " + err.Error()})
	}
	return rows
}

// flushableSessionEvents selects the events worth persisting from a run that was
// cancelled mid-flight. The cancel path already records a single "Run cancelled."
// error, so the goroutine's trailing EventError (the ctx-cancellation error) is
// dropped to avoid a duplicate; everything else it accumulated before the cancel
// — tool calls/results, permission events, usage, and the EventSessionCheckpoint
// blobs that /rewind depends on — is kept.
func flushableSessionEvents(events []pendingSessionEvent) []pendingSessionEvent {
	flushable := make([]pendingSessionEvent, 0, len(events))
	for _, event := range events {
		if event.Type == sessions.EventError {
			continue
		}
		flushable = append(flushable, event)
	}
	return flushable
}

func tuiSessionTitle(prompt string) string {
	// cutRunes keeps the cut on a rune boundary — a bare byte slice could split
	// a multi-byte rune and persist invalid UTF-8 into the session metadata.
	title := cutRunes(strings.Join(strings.Fields(prompt), " "), tuiSessionTitleLimit)
	if title == "" {
		return "Zero TUI session"
	}
	return title
}

func (m model) handleResumeCommand(args string) (model, string) {
	args = strings.TrimSpace(args)
	if args == "" {
		return m, m.resumeText()
	}

	session, err := m.resolveResumeSession(args)
	if err != nil {
		return m, "Sessions\n" + err.Error()
	}
	events, err := m.resumeEvents(session.SessionID)
	if err != nil {
		return m, "Sessions\nerror: " + err.Error()
	}

	// Capture the current session id before switching so loops are only torn down
	// on a real change — `/resume latest` or `/resume <currentID>` can resolve to
	// the already-active session, whose loops belong to it, not a "previous" one.
	previousID := m.activeSession.SessionID
	m.activeSession = *session
	m.pendingSessionTitle = ""
	m.sessionEvents = append([]sessions.Event{}, events...)
	if m.providerName == "" {
		m.providerName = session.Provider
	}
	if m.modelName == "" {
		m.modelName = session.ModelID
	}
	loopsCleared := 0
	if session.SessionID != previousID {
		m, loopsCleared = m.clearLoopsForSessionSwitch()
	}

	rows := initialTranscript()
	rows = appendRow(rows, rowSystem, m.formatResumeSummary(*session, len(events)))
	if loopsCleared > 0 {
		rows = appendRow(rows, rowSystem, fmt.Sprintf("Stopped %d loop(s) tied to the previous session.", loopsCleared))
	}
	rows = appendTranscriptRowsDedup(rows, transcriptRowsFromSessionEvents(events))
	m.transcript = rows
	// Every rehydrated row is settled by construction, so resetting the flush
	// frontier sends the whole resumed history to native scrollback in one
	// batch — scrollable, selectable, and O(1) for every later frame.
	m.resetFlushFrontier("· resumed ·")
	m = m.syncPeerIdentity()
	return m, ""
}

func (m model) sessionPrompt(prompt string) string {
	if m.activeSession.SessionID == "" || len(m.sessionEvents) == 0 {
		return prompt
	}
	formatted := sessions.FormatExecPrompt(prompt, sessions.PreparedExec{
		Mode:          sessions.ModeResume,
		Session:       m.activeSession,
		ContextEvents: append([]sessions.Event{}, m.sessionEvents...),
	})
	if m.activeSession.SessionKind == sessions.SessionKindSide {
		return btwContextBoundary + "\n\n" + formatted
	}
	return formatted
}

func (m model) resolveResumeSession(args string) (*sessions.Metadata, error) {
	if strings.EqualFold(args, "latest") {
		// Latest *resumable* conversation IN THIS WORKSPACE, so "latest" never lands
		// on a child, a spec sub-run, or a session from another project (matching the
		// workspace-scoped picker). An explicit `/resume <id>` below still resolves
		// any session regardless of workspace.
		latest, err := m.latestResumableInWorkspace()
		if err != nil {
			return nil, err
		}
		if latest == nil {
			return nil, errors.New("no zero sessions available to resume")
		}
		return latest, nil
	}

	session, err := m.sessionStore.Get(args)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, fmt.Errorf("zero session not found: %s", args)
	}
	if !sessions.IsResumableKind(session.SessionKind) {
		return nil, fmt.Errorf("zero session is not resumable: %s", args)
	}
	return session, nil
}

// resumeEvents reads a session's events for resume, preferring the rehydrated
// (compaction-aware) view so a resumed session honors a prior /compact — matching
// the CLI's `zero exec --resume` (readExecContextEvents) and the in-TUI /compact
// reload. Falls back to the raw log if rehydration fails.
func (m model) resumeEvents(sessionID string) ([]sessions.Event, error) {
	events, err := m.sessionStore.ReadRehydratedEvents(sessionID)
	if err == nil {
		return events, nil
	}
	raw, rawErr := m.sessionStore.ReadEvents(sessionID)
	if rawErr != nil {
		// Surface the raw-read failure (the actual fallback error), not the earlier
		// rehydration error, so the caller sees why the fallback itself failed.
		return nil, rawErr
	}
	return raw, nil
}

// formatResumeSummary reports what the resumed conversation will actually continue
// with (the active model/provider), noting the session's recorded model/provider
// when it differs — resume keeps the current model rather than switching.
func (m model) formatResumeSummary(session sessions.Metadata, eventCount int) string {
	modelLine := "model: " + displayValue(m.modelName, "none")
	if recorded := strings.TrimSpace(session.ModelID); recorded != "" && !strings.EqualFold(recorded, m.modelName) {
		modelLine += "  (recorded: " + recorded + ")"
	}
	providerLine := "provider: " + displayValue(m.providerName, "none")
	if recorded := strings.TrimSpace(session.Provider); recorded != "" && !strings.EqualFold(recorded, m.providerName) {
		providerLine += "  (recorded: " + recorded + ")"
	}
	lines := []string{
		"id: " + session.SessionID,
		"title: " + displayValue(session.Title, "untitled"),
		modelLine,
		providerLine,
		fmt.Sprintf("events: %d", eventCount),
	}
	if session.Goal != nil {
		goalLine := "goal: " + string(session.Goal.Status) + " — " + session.Goal.Objective
		if session.Goal.Status == sessions.GoalStatusActive {
			goalLine += " (run /goal resume to continue)"
		}
		lines = append(lines, goalLine)
	}
	return renderCommandOutput(commandOutput{
		Title:  "Resumed Zero session",
		Status: commandStatusOK,
		Sections: []commandSection{{
			Title: "Session",
			Lines: lines,
		}},
	})
}

// newSessionPicker builds the interactive /resume picker (mirrors /model & /provider):
// one row per resumable session — age + title (Label), session id (Value), and a
// project/model/size line (Detail). Returns nil when there are no resumable
// sessions so the caller falls back to the text path.
func (m model) newSessionPicker() *commandPicker {
	if m.sessionStore == nil {
		return nil
	}
	metas, err := m.sessionStore.ListResumable()
	if err != nil || len(metas) == 0 {
		return nil
	}
	now := m.now()
	items := make([]pickerItem, 0, len(metas))
	for _, meta := range metas {
		// Workspace-scoped: hide sessions from other project directories so /resume
		// lists this workspace's history, not every project's. Checked BEFORE the
		// per-session event read below, so a large global history doesn't pay 50
		// full file reads to build one workspace's list. Sessions with no recorded
		// Cwd (older runs) stay visible rather than vanishing.
		if !sessionMatchesWorkspace(meta.Cwd, m.cwd) {
			continue
		}
		// A zero-event session has nothing to resume — skip it without a file read.
		if meta.EventCount == 0 {
			continue
		}
		// Skip empty/failed runs (no assistant output, no tool calls) — e.g. the
		// same prompt retried while the model wasn't responding. They have nothing
		// to resume and otherwise flood the list with identical rows. Still on disk.
		if !m.sessionHasResumableContent(meta.SessionID) {
			continue
		}
		// Lead with a fixed-width timestamp so titles form one scannable column.
		// The raw id remains the selection/search value but stays out of the row:
		// rendering it consumed half the picker and truncated the useful title.
		// Session metadata is persisted, user-controlled text — a title from
		// --session-title or /rename can legally contain newlines, which would
		// break the picker's fixed row geometry.
		label := displayValue(sanitizeCardField(meta.Title), "untitled")
		if when := relativeAge(meta.UpdatedAt, now); when != "" {
			label = sessionPickerLabel(when, label)
		}
		items = append(items, pickerItem{
			Label:  label,
			Value:  meta.SessionID,
			Detail: m.sessionPickerDetail(meta),
		})
	}
	if len(items) == 0 {
		return nil // every resumable session was an empty/failed run
	}
	return &commandPicker{
		kind:     pickerSession,
		title:    "Resume a session",
		items:    items,
		allItems: append([]pickerItem{}, items...),
		selected: 0,
	}
}

const sessionPickerTimeWidth = len("2006-01-02")

func sessionPickerLabel(when, title string) string {
	return fmt.Sprintf("%-*s  %s", sessionPickerTimeWidth, when, title)
}

// sessionPickerDetail composes the faint second line under a /resume row:
// the session's project directory (~/-contracted), the model it ran on, and a
// short status chip when the session is not a plain mainline run (forks,
// tagged sessions such as btw side-chats). Missing fields are simply omitted —
// the picker is workspace-scoped, so the project column is a confirmation,
// not a disambiguator.
func (m model) sessionPickerDetail(meta sessions.Metadata) string {
	parts := make([]string, 0, 3)
	if project := displayPath(m.cwd, meta.Cwd); project != "" {
		parts = append(parts, project)
	}
	if modelID := strings.TrimSpace(meta.ModelID); modelID != "" {
		parts = append(parts, modelID)
	}
	if status := sessionPickerStatus(meta); status != "" {
		parts = append(parts, status)
	}
	return sanitizeCardField(strings.Join(parts, " · "))
}

// sessionPickerStatus is the concise status chip for a /resume row: an
// explicit tag when the session carries one, otherwise a marker for forked
// sessions. Plain mainline sessions get none — their shape is the default.
func sessionPickerStatus(meta sessions.Metadata) string {
	if tag := strings.TrimSpace(meta.Tag); tag != "" {
		return tag
	}
	if meta.SessionKind == sessions.SessionKindFork {
		return "fork"
	}
	return ""
}

// sessionHasResumableContent reports whether a session has anything worth
// resuming: a tool call/result, or a non-user message with real content (not the
// no-output guardrail stop). Empty/failed runs return false and are hidden from
// the picker (they stay on disk). Errors fail open (the session is kept).
// latestResumableInWorkspace returns the most-recently-updated resumable session
// belonging to the current workspace, or nil when none exist. ListResumable is
// ordered latest-first, so the first qualifying match is the latest. It applies
// the SAME filters as newSessionPicker — workspace membership, non-empty
// metadata, and real resumable content — so `/resume latest` never lands on a
// zero-event or empty/failed run the picker would have hidden.
func (m model) latestResumableInWorkspace() (*sessions.Metadata, error) {
	metas, err := m.sessionStore.ListResumable()
	if err != nil {
		return nil, err
	}
	for i := range metas {
		if !sessionMatchesWorkspace(metas[i].Cwd, m.cwd) {
			continue
		}
		if metas[i].EventCount == 0 {
			continue
		}
		if !m.sessionHasResumableContent(metas[i].SessionID) {
			continue
		}
		return &metas[i], nil
	}
	return nil, nil
}

// sessionMatchesWorkspace reports whether a session recorded in sessionCwd
// belongs to the current workspaceCwd. A session with no recorded Cwd (older
// runs) or an unknown current workspace is kept visible rather than filtered out,
// so the scoping never hides history it can't confidently place elsewhere. On
// Windows the comparison is case-insensitive, since the filesystem is and the
// same workspace can be spelled with different casing (C:\Proj vs c:\proj).
func sessionMatchesWorkspace(sessionCwd, workspaceCwd string) bool {
	sessionCwd = strings.TrimSpace(sessionCwd)
	workspaceCwd = strings.TrimSpace(workspaceCwd)
	if sessionCwd == "" || workspaceCwd == "" {
		return true
	}
	a := filepath.Clean(sessionCwd)
	b := filepath.Clean(workspaceCwd)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func (m model) sessionHasResumableContent(sessionID string) bool {
	events, err := m.sessionStore.ReadEvents(sessionID)
	if err != nil {
		return true
	}
	return eventsHaveResumableContent(events)
}

// eventsHaveResumableContent reports whether already-loaded events contain
// anything worth resuming: a tool call/result, or a non-user message with real
// content (not the no-output guardrail stop). It is the pure core of
// sessionHasResumableContent so callers that already hold the events (e.g. the
// session picker refresh) don't re-read them.
func eventsHaveResumableContent(events []sessions.Event) bool {
	for _, event := range events {
		switch event.Type {
		case sessions.EventToolCall, sessions.EventToolResult:
			return true
		case sessions.EventMessage:
			payload := sessionPayload(event)
			if strings.EqualFold(payloadString(payload, "role"), "user") {
				continue
			}
			content := strings.TrimSpace(payloadString(payload, "content"))
			if content != "" && !agent.IsNoProgressStop(content) {
				return true
			}
		}
	}
	return false
}

// openSessionPicker opens the /resume picker; ok is false when there is nothing to
// resume (the caller then falls back to the text list / "none" message).
func (m model) openSessionPicker() (model, bool) {
	picker := m.newSessionPicker()
	if picker == nil {
		return m, false
	}
	m.picker = picker
	return m, true
}

func transcriptRowsFromSessionEvents(events []sessions.Event) []transcriptRow {
	rows := []transcriptRow{}
	// Rehydrated rows all carry runID 0, so repeated provider tool-call ids
	// (e.g. Gemini's per-turn gemini_tool_N) get the same per-occurrence
	// disambiguation the live runner applies — without it, dedup would drop
	// every tool card after the first occurrence of an id.
	callSeq := map[string]int{}
	// Pre-pass: collect the tool-call ids of Task delegations that actually started
	// a specialist (each renders as a card below). Only those Task tool-call/result
	// rows are redundant and skipped; a Task that failed before a specialist started
	// has no card, so its rows are kept — otherwise the failed delegation vanishes
	// on resume (M10).
	specialistToolCalls := map[string]bool{}
	for _, event := range events {
		if event.Type == sessions.EventSpecialistStart {
			if id := payloadString(sessionPayload(event), "toolCallId"); id != "" {
				specialistToolCalls[id] = true
			}
		}
	}
	for _, event := range events {
		payload := sessionPayload(event)
		switch event.Type {
		case sessions.EventMessage:
			role := strings.ToLower(payloadString(payload, "role"))
			if role == "user" && payloadString(payload, "origin") == "cross_session" {
				from := payloadString(payload, "from")
				content := payloadString(payload, "displayContent")
				if content == "" {
					content = payloadString(payload, "content")
				}
				rows = append(rows, peerTranscriptRow(from, content))
				continue
			}
			switch role {
			case "ask_user":
				rows = append(rows, askUserTranscriptRow(askUserRequestFromPayload(payload)))
				continue
			case "ask_user_answers":
				if text := askUserAnswersText(payload); text != "" {
					rows = append(rows, transcriptRow{kind: rowSystem, text: text})
				}
				continue
			}
			content := payloadString(payload, "content")
			if content == "" {
				continue
			}
			switch role {
			case "user":
				rows = append(rows, transcriptRow{kind: rowUser, text: content, attachments: attachmentSummaryFromPayload(payload)})
			case "assistant":
				// A persisted assistant message was a turn's final answer. Tool/timing
				// counters were not recorded; the completion line omits those segments.
				rows = append(rows, transcriptRow{kind: rowAssistant, text: content, final: true})
			default:
				rows = append(rows, transcriptRow{kind: rowSystem, text: content})
			}
		case sessions.EventToolCall:
			name := payloadString(payload, "name")
			if name == "" {
				name = "unknown"
			}
			// Extract the id exactly as the tool-result branch and the specialist
			// pre-pass do (toolCallId first, then id) so call and result rows key
			// callSeq/effectiveToolRowID on the same string and the specialist-skip
			// lookup matches — otherwise a payload that carries toolCallId (not id)
			// desyncs call→result dedup and the M10 skip (L20).
			id := firstNonEmptyString(payloadString(payload, "toolCallId"), payloadString(payload, "id"))
			if name == "Task" && specialistToolCalls[id] {
				// A specialist card renders this delegation; skip the redundant
				// "tool call: Task" row. A Task with no specialist (it failed before
				// one started) keeps its row so the failure stays visible (M10).
				continue
			}
			callSeq[id]++
			rows = append(rows, transcriptRow{
				kind:   rowToolCall,
				id:     effectiveToolRowID(id, callSeq[id]),
				text:   "tool call: " + name,
				tool:   name,
				detail: argHint(payloadString(payload, "arguments")),
				arg:    argHintSecondary(payloadString(payload, "arguments")),
			})
		case sessions.EventPermission, sessions.EventPermissionRequest, sessions.EventPermissionDecision:
			// Mirror the live path: a silently auto-approved call recorded its
			// audit event but rendered no row, so don't resurrect one on resume.
			event := permissionEventFromPayload(payload)
			if permissionEventIsNoteworthy(event) {
				rows = append(rows, permissionTranscriptRow(event))
			}
		case sessions.EventToolResult:
			name := payloadString(payload, "name")
			if name == "" {
				name = "unknown"
			}
			id := firstNonEmptyString(payloadString(payload, "toolCallId"), payloadString(payload, "id"))
			if name == "Task" && specialistToolCalls[id] {
				// The specialist card carries this Task's result; skip the redundant
				// "tool result: Task" row. A Task with no specialist keeps its result
				// row so a failed delegation's error stays visible (M10).
				continue
			}
			status := tools.Status(payloadString(payload, "status"))
			if status == "" {
				status = tools.StatusOK
			}
			output := payloadString(payload, "output")
			detail := payloadString(payload, "displayPreview")
			if detail == "" {
				detail = output
			}
			rows = append(rows, transcriptRow{
				kind:            rowToolResult,
				id:              effectiveToolRowID(id, callSeq[id]),
				text:            fmt.Sprintf("tool result: %s %s %s", name, status, truncateTUIOutput(output, tuiToolOutputLimit)),
				tool:            name,
				status:          status,
				detail:          detail,
				meta:            payloadStringMap(payload, "meta"),
				changedFiles:    payloadStringSlice(payload, "changedFiles"),
				changeSummaries: payloadExecutionChanges(payload, "changeSummaries"),
			})
		case sessions.EventError:
			if message := payloadString(payload, "message"); message != "" {
				rows = append(rows, transcriptRow{kind: rowError, text: message})
			}
		case sessions.EventCompaction:
			if summary := payloadString(payload, "summary"); summary != "" {
				rows = append(rows, transcriptRow{kind: rowSystem, text: summary})
			}
		case sessions.EventSessionFork:
			parentID := payloadString(payload, "parentSessionId")
			if parentID != "" {
				rows = append(rows, transcriptRow{kind: rowSystem, text: "forked from session: " + parentID})
			}
		case sessions.EventSpecialistStart:
			info := specialistInfoFromPayload(payload)
			if info != nil {
				rows = append(rows, transcriptRow{kind: rowSpecialist, specialistInfo: info})
			}
		case sessions.EventSpecialistStop:
			info := specialistInfoFromPayload(payload)
			if info != nil {
				// Reconcile: update the existing Start row with the same
				// childSessionID instead of appending a duplicate. On resume
				// this prevents two cards per specialist (running + completed).
				found := false
				for i := range rows {
					if rows[i].kind == rowSpecialist && rows[i].specialistInfo != nil &&
						rows[i].specialistInfo.childSessionID == info.childSessionID {
						rows[i].specialistInfo = info
						found = true
						break
					}
				}
				if !found {
					rows = append(rows, transcriptRow{kind: rowSpecialist, specialistInfo: info})
				}
			}
		}
	}
	return rows
}

func sessionPayload(event sessions.Event) map[string]any {
	payload := map[string]any{}
	if len(event.Payload) == 0 {
		return payload
	}
	_ = json.Unmarshal(event.Payload, &payload)
	return payload
}

func permissionEventFromPayload(payload map[string]any) agent.PermissionEvent {
	name := payloadString(payload, "name")
	if name == "" {
		name = payloadString(payload, "toolName")
	}
	event := agent.PermissionEvent{
		ToolCallID:        firstNonEmptyString(payloadString(payload, "toolCallId"), payloadString(payload, "id")),
		ToolName:          name,
		Action:            agent.PermissionAction(payloadString(payload, "action")),
		DecisionAction:    agent.PermissionDecisionAction(payloadString(payload, "decisionAction")),
		Permission:        payloadString(payload, "permission"),
		PermissionGranted: payloadBool(payload, "permissionGranted"),
		PermissionMode:    agent.PermissionMode(payloadString(payload, "permissionMode")),
		Autonomy:          payloadString(payload, "autonomy"),
		SideEffect:        payloadString(payload, "sideEffect"),
		Reason:            payloadString(payload, "reason"),
		Scope:             payloadString(payload, "scope"),
		DecisionReason:    payloadString(payload, "decisionReason"),
		GrantMatched:      payloadBool(payload, "grantMatched"),
	}
	if risk, ok := payloadMap(payload, "risk"); ok {
		event.Risk = sandbox.Risk{
			Level:  sandbox.RiskLevel(payloadString(risk, "level")),
			Reason: payloadString(risk, "reason"),
		}
	}
	if block, ok := payloadMap(payload, "block"); ok {
		event.Block = &sandbox.Block{
			Code:        sandbox.BlockCode(payloadString(block, "code")),
			ToolName:    payloadString(block, "toolName"),
			Action:      sandbox.Action(payloadString(block, "action")),
			Risk:        event.Risk,
			Path:        payloadString(block, "path"),
			Reason:      payloadString(block, "reason"),
			Recoverable: payloadBool(block, "recoverable"),
		}
		if nestedRisk, ok := payloadMap(block, "risk"); ok {
			event.Block.Risk = sandbox.Risk{
				Level:  sandbox.RiskLevel(payloadString(nestedRisk, "level")),
				Reason: payloadString(nestedRisk, "reason"),
			}
		}
	}
	return event
}

// askUserRequestFromPayload rebuilds the request persisted by
// askUserSessionPayload, so ask_user exchanges survive /resume instead of
// silently vanishing from rehydrated history.
func askUserRequestFromPayload(payload map[string]any) agent.AskUserRequest {
	request := agent.AskUserRequest{
		ToolCallID: payloadString(payload, "toolCallId"),
		Header:     payloadString(payload, "header"),
	}
	raw, ok := payload["questions"].([]any)
	if !ok {
		return request
	}
	for _, entry := range raw {
		fields, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		question := agent.AskUserQuestion{
			Question:    payloadString(fields, "question"),
			MultiSelect: payloadBool(fields, "multiSelect"),
		}
		if options, ok := fields["options"].([]any); ok {
			for _, option := range options {
				if text, ok := option.(string); ok {
					question.Options = append(question.Options, text)
				}
			}
		}
		request.Questions = append(request.Questions, question)
	}
	return request
}

// askUserAnswersText renders persisted ask_user answers for rehydration.
func askUserAnswersText(payload map[string]any) string {
	raw, ok := payload["answers"].([]any)
	if !ok {
		return ""
	}
	lines := make([]string, 0, len(raw))
	for index, entry := range raw {
		text, _ := entry.(string)
		if strings.TrimSpace(text) == "" {
			text = "(skipped)"
		}
		lines = append(lines, fmt.Sprintf("%d. %s", index+1, text))
	}
	if len(lines) == 0 {
		return ""
	}
	return "Answers\n" + strings.Join(lines, "\n")
}

func payloadString(payload map[string]any, key string) string {
	value := payload[key]
	switch typed := value.(type) {
	case string:
		return typed
	case float64, bool:
		return fmt.Sprint(typed)
	case nil:
		return ""
	default:
		data, err := json.Marshal(typed)
		if err != nil {
			return ""
		}
		return string(data)
	}
}

// payloadStringSlice reads a []string persisted into a session payload (JSON
// round-trips it as []any), skipping non-string entries. Nil when absent.
func payloadStringSlice(payload map[string]any, key string) []string {
	switch typed := payload[key].(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, v := range typed {
			if s, ok := v.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		return nil
	}
}

func payloadStringMap(payload map[string]any, key string) map[string]string {
	value, ok := payloadMap(payload, key)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(value))
	for name, raw := range value {
		if text, ok := raw.(string); ok {
			out[name] = text
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func payloadExecutionChanges(payload map[string]any, key string) []execution.Change {
	value, ok := payload[key]
	if !ok {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var changes []execution.Change
	if err := json.Unmarshal(data, &changes); err != nil {
		return nil
	}
	return changes
}

func payloadBool(payload map[string]any, key string) bool {
	value := payload[key]
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	default:
		return false
	}
}

func attachmentSummaryFromPayload(payload map[string]any) transcriptAttachmentSummary {
	attachments, ok := payloadMap(payload, "attachments")
	if !ok {
		return transcriptAttachmentSummary{}
	}
	return transcriptAttachmentSummary{
		images:    payloadNonNegativeInt(attachments, "images"),
		documents: payloadNonNegativeInt(attachments, "documents"),
	}
}

func payloadNonNegativeInt(payload map[string]any, key string) int {
	value, ok := payload[key].(float64)
	if !ok || value <= 0 || value != math.Trunc(value) {
		return 0
	}
	if value > persistedAttachmentCountLimit {
		return persistedAttachmentCountLimit
	}
	return int(value)
}

func payloadMap(payload map[string]any, key string) (map[string]any, bool) {
	value, ok := payload[key].(map[string]any)
	return value, ok
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// specialistInfoFromPayload builds a specialistInfo from a specialist_start or
// specialist_stop session event payload. Returns nil if the payload lacks a
// childSessionId (the minimum required field).
func specialistInfoFromPayload(payload map[string]any) *specialistInfo {
	childSessionID := payloadString(payload, "childSessionId")
	if childSessionID == "" {
		return nil
	}
	info := &specialistInfo{
		name:           payloadString(payload, "specialist"),
		description:    payloadString(payload, "description"),
		childSessionID: childSessionID,
	}
	statusStr := payloadString(payload, "status")
	switch statusStr {
	case "running":
		info.status = specialistRunning
	case "success":
		info.status = specialistCompleted
	case "completed":
		info.status = specialistCompleted
	default:
		info.status = parseSpecialistStatus(statusStr)
	}
	if errMsg := payloadString(payload, "error"); errMsg != "" {
		info.errorMsg = errMsg
	}
	return info
}

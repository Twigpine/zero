package tui

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/agentsessions"
	"github.com/Twigpine/zero/internal/execution"
	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/usage"
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
	m.transcript = m.withStartupNotices(reduceTranscript(m.transcript, transcriptAction{kind: actionClear}))
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

// resumeWhileRunningText is the one refusal every resume route shares.
const resumeWhileRunningText = "Sessions\nCannot resume sessions while a run is active."

type foreignSessionImportedMsg struct {
	result        agentsessions.ImportResult
	originSession string
	err           error
}

type sessionPickerLoadedMsg struct {
	picker *commandPicker
	text   string
	// originSession and generation bind this result to the /resume request
	// that produced it. Discovery is asynchronous; by the time it answers, a
	// prompt may have started a run or another /resume may have superseded it,
	// and installing the picker anyway let a late result switch sessions
	// mid-run (see updateModel). The completion path for foreign imports has
	// carried originSession for the same reason; the local picker did not.
	originSession string
	generation    uint64
}

// sessionPickerCmd keeps discovery of external agent stores off Bubble Tea's
// Update loop. A transcript can name an unavailable workspace and vendor stores
// can be slow even when local, so /resume must remain responsive while their
// bounded indexes are read.
//
// The returned model must be kept: it records the request generation the
// result will have to match.
func (m model) sessionPickerCmd() (model, tea.Cmd) {
	m.sessionPickerGeneration++
	generation := m.sessionPickerGeneration
	originSession := m.activeSession.SessionID
	snapshot := model{
		sessionStore:     m.sessionStore,
		agentSessionsEnv: m.agentSessionsEnv,
		cwd:              m.cwd,
		now:              m.now,
	}
	return m, func() tea.Msg {
		picker, foreignProblems, localErr := snapshot.buildSessionPicker()
		warning := sessionPickerWarning(localErr, foreignProblems)
		msg := sessionPickerLoadedMsg{originSession: originSession, generation: generation}
		if picker != nil {
			msg.picker, msg.text = picker, warning
			return msg
		}
		if warning != "" {
			msg.text = warning
			return msg
		}
		msg.text = snapshot.resumeText()
		return msg
	}
}

// sessionPickerWarning keeps partial discovery failures visible without
// turning them into a failed picker. Errors may contain paths or transcript
// identifiers from foreign stores, so this is also the single display
// boundary that sanitizes every local and foreign problem before it reaches
// the terminal transcript.
func sessionPickerWarning(localErr error, foreignProblems []error) string {
	warnings := []string{}
	if localErr != nil {
		warnings = append(warnings, "Warning: could not read local Zero sessions; showing external sessions only: "+agentsessions.DisplayField(localErr.Error()))
	}
	for _, problem := range foreignProblems {
		if problem == nil {
			continue
		}
		warnings = append(warnings, "Warning: could not read some external sessions: "+agentsessions.DisplayField(problem.Error()))
	}
	if len(warnings) == 0 {
		return ""
	}
	return "Sessions\n" + strings.Join(warnings, "\n")
}

// sessionPickerResultIsCurrent reports whether a discovery result still
// describes a request whose preconditions hold: no run has started, the same
// session is active, and no newer /resume has replaced it.
func (m model) sessionPickerResultIsCurrent(msg sessionPickerLoadedMsg) bool {
	return !m.pending &&
		msg.generation == m.sessionPickerGeneration &&
		msg.originSession == m.activeSession.SessionID
}

// startResumeCommand keeps foreign transcript I/O off Bubble Tea's Update
// loop. Local Zero resumes stay synchronous; a foreign reference returns a
// command whose result is applied by finishForeignSessionImport.
func (m model) startResumeCommand(args string) (model, string, tea.Cmd) {
	args = strings.TrimSpace(args)
	// CHECKED HERE, WHERE THE MUTATION STARTS, not only at /resume dispatch.
	// The command's guard runs when discovery is requested; a picker choice
	// lands later, after the picker has arrived, and by then a prompt may have
	// started a run. Resuming then swaps activeSession under a live run whose
	// completion appends its events to whichever session is active — the
	// earlier run's transcript spliced into another conversation. Refusing
	// before any mutation is the only order that works; a generation check on
	// the result alone cannot cover a run that starts while the same request is
	// still current.
	//
	// BEFORE THE BRANCH, NOT INSIDE THE LOCAL ARM. This check used to sit under
	// `!strings.Contains(args, ":")`, so a foreign reference walked straight
	// past it: `/resume claude-code:<id>` during a run set sessionImportInFlight
	// and dispatched the import, which reads the foreign transcript, CREATES A
	// DURABLE ZERO SESSION and calls InvalidateDiscovery. finishForeignSessionImport
	// then declined to switch to it — so the side effects the local route
	// refuses outright all happened anyway, and the user got a session they
	// never reached. A refusal that only covers the cheapest route is not the
	// shared rule this constant claims it is. Reported by @jatmn.
	if m.pending {
		return m, resumeWhileRunningText, nil
	}
	if !strings.Contains(args, ":") {
		next, text := m.handleResumeCommand(args)
		return next, text, nil
	}
	if m.sessionImportInFlight {
		return m, "Sessions\na foreign session import is already in progress", nil
	}
	m.sessionImportInFlight = true
	return m, "", m.importForeignSessionCmd(args)
}

func (m model) handleResumeCommand(args string) (model, string) {
	args = strings.TrimSpace(args)
	if args == "" {
		return m, m.resumeText()
	}
	return m.resumeZeroSession(args, "")
}

func (m model) resumeZeroSession(args string, importNote string) (model, string) {

	session, err := m.resolveResumeSession(args)
	if err != nil {
		return m, "Sessions\n" + agentsessions.DisplayField(err.Error())
	}
	events, err := m.resumeEvents(session.SessionID)
	if err != nil {
		return m, "Sessions\nerror: " + agentsessions.DisplayField(err.Error())
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

	// Keep main's notices and the foreign import disclosure.
	rows := m.withStartupNotices(initialTranscript())
	if importNote != "" {
		rows = appendRow(rows, rowSystem, importNote)
	}
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
	// Picked from its metadata a moment ago. Make sure it is still there, and
	// held, before resuming it: see sessions.HoldToContinue.
	if err := m.sessionStore.HoldToContinue(sessionID); err != nil {
		return nil, err
	}
	events, err := m.sessionStore.ReadRehydratedEvents(sessionID)
	if err == nil {
		return events, nil
	}
	if errors.Is(err, sessions.ErrPruning) {
		// Not a rehydration failure: the raw read would resume the session without
		// holding it while zero sessions prune may be removing it.
		return nil, err
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
		modelLine += "  (recorded: " + agentsessions.DisplayField(recorded) + ")"
	}
	providerLine := "provider: " + displayValue(m.providerName, "none")
	if recorded := strings.TrimSpace(session.Provider); recorded != "" && !strings.EqualFold(recorded, m.providerName) {
		providerLine += "  (recorded: " + recorded + ")"
	}
	lines := []string{
		"id: " + session.SessionID,
		"title: " + displayValue(agentsessions.DisplayField(session.Title), "untitled"),
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

func foreignSessionAge(updatedAt time.Time, now time.Time) string {
	if updatedAt.IsZero() {
		return ""
	}
	return relativeAge(updatedAt.Format(time.RFC3339), now)
}

// newSessionPicker builds the interactive /resume picker (mirrors /model & /provider):
// one row per resumable session — age + title (Label), session id (Value), and a
// project/model/size line (Detail). Returns nil when there are no resumable
// sessions so the caller falls back to the text path.
func (m model) newSessionPicker() *commandPicker {
	picker, _, _ := m.buildSessionPicker()
	return picker
}

// buildSessionPicker keeps the local and external sources independent. A local
// store failure removes only local rows; callers receive local and foreign
// problems separately so they can warn without hiding discoverable work from
// the sources that did succeed.
func (m model) buildSessionPicker() (*commandPicker, []error, error) {
	if m.sessionStore == nil {
		return nil, nil, nil
	}
	// A FAILED READ IS THE ONLY REASON TO GIVE UP HERE. An EMPTY local history is
	// not: foreign sessions are discovered independently of the store, and the
	// user with no Zero sessions at all is exactly the one this picker's import
	// path exists for — someone who has just installed Zero and wants to carry on
	// work another agent started. Returning early on len(metas) == 0 made the
	// feature invisible to precisely that user, and visible only once they had
	// already done the thing it was meant to save them.
	metas, err := m.sessionStore.ListResumable()
	if err != nil {
		metas = nil
	}
	now := m.now()
	items := make([]pickerItem, 0, len(metas))
	for _, meta := range metas {
		// Workspace-scoped: hide sessions from other project directories so /resume
		// lists this workspace's history, not every project's. Checked BEFORE the
		// per-session event read below, so a large global history doesn't pay 50
		// full file reads to build one workspace's list. Sessions with no recorded
		// Cwd (older runs) stay visible rather than vanishing.
		if !sessionMatchesWorkspace(sessions.OperationalCwd(meta), m.cwd) {
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
		label := displayValue(agentsessions.DisplayField(meta.Title), "untitled")
		if when := relativeAge(meta.UpdatedAt, now); when != "" {
			label = sessionPickerLabel(when, label)
		}
		agent := sessionAgentName(meta.Tag)
		items = append(items, pickerItem{
			Label:  label,
			Value:  meta.SessionID,
			Meta:   agent,
			Tab:    agent,
			Detail: m.sessionPickerDetail(meta),
		})
	}
	foreign, problems := m.foreignSessionItems(metas, now)
	return pickerFromParts(items, foreign), problems, err
}

// pickerFromParts assembles the picker from the two independent sources, and
// decides emptiness AFTER combining them rather than from either alone. Split
// out so that decision is testable without a session store on disk: it is the
// step that previously hid the whole import path from a user with no local
// history.
func pickerFromParts(local []pickerItem, foreign []pickerItem) *commandPicker {
	items := append(append([]pickerItem{}, local...), foreign...)
	if len(items) == 0 {
		return nil // nothing local worth resuming, and nothing foreign to import
	}
	return &commandPicker{
		kind:     pickerSession,
		title:    "Resume a session",
		items:    items,
		allItems: append([]pickerItem{}, items...),
		selected: 0,
		tabs:     sessionPickerTabs(items),
	}
}

// importForeignSession copies another agent's session into Zero and returns the
// new Zero session id, plus a note for the transcript saying what happened.
//
// Resuming a foreign session cannot be silent: it creates a durable Zero session
// the user did not explicitly ask for, and it may have run in a different
// directory, so the note names both.
func (m model) importForeignSessionCmd(ref string) tea.Cmd {
	store := m.sessionStore
	env := m.agentSessionsEnv
	originSession := m.activeSession.SessionID
	return func() tea.Msg {
		if store == nil {
			return foreignSessionImportedMsg{originSession: originSession, err: errors.New("no session store")}
		}
		adapter, id, err := agentsessions.ParseRef(env, ref)
		if err != nil {
			return foreignSessionImportedMsg{originSession: originSession, err: err}
		}
		// A resume imports the complete visible transcript by design. The total
		// event count is uncapped, but each source line remains bounded by the
		// agentsessions reader so one malformed record cannot exhaust memory.
		result, err := agentsessions.Import(store, adapter, id, agentsessions.ReadOptions{})
		return foreignSessionImportedMsg{result: result, originSession: originSession, err: err}
	}
}

// startForeignSessionImport is the picker's route into an import. It is a
// SEPARATE ENTRY POINT from startResumeCommand — choosePicker calls it directly
// for a foreign row — so the mid-run refusal has to be repeated here rather than
// inherited. Without it, choosing an un-imported row during a run ran the whole
// import for a session the model then refused to switch to. Reported by @jatmn.
func (m model) startForeignSessionImport(source agentsessions.ForeignSession) (model, string, tea.Cmd) {
	if m.pending {
		return m, resumeWhileRunningText, nil
	}
	if m.sessionImportInFlight {
		return m, "Sessions\na foreign session import is already in progress", nil
	}
	m.sessionImportInFlight = true
	return m, "", m.importForeignSessionSourceCmd(source)
}

func (m model) importForeignSessionSourceCmd(source agentsessions.ForeignSession) tea.Cmd {
	store := m.sessionStore
	env := m.agentSessionsEnv
	originSession := m.activeSession.SessionID
	return func() tea.Msg {
		if store == nil {
			return foreignSessionImportedMsg{originSession: originSession, err: errors.New("no session store")}
		}
		adapter, _, err := agentsessions.ParseRef(env, source.Agent+":"+source.ID)
		if err != nil {
			return foreignSessionImportedMsg{originSession: originSession, err: err}
		}
		result, err := agentsessions.ImportSource(store, adapter, source, agentsessions.ReadOptions{})
		return foreignSessionImportedMsg{result: result, originSession: originSession, err: err}
	}
}

func (m model) finishForeignSessionImport(msg foreignSessionImportedMsg) (model, string) {
	m.sessionImportInFlight = false
	if msg.err != nil {
		return m, "Sessions\n" + agentsessions.DisplayField(msg.err.Error())
	}
	// This session is no longer un-imported, so the memo that says otherwise
	// must go before the picker is rebuilt.
	agentsessions.InvalidateDiscovery()
	if m.pending || m.activeSession.SessionID != msg.originSession {
		note := importedSessionNote(msg.result, m.cwd)
		return m, note + "\nThe import completed, but Zero did not resume it because the active session changed or a run started."
	}
	return m.resumeZeroSession(msg.result.Session.SessionID, importedSessionNote(msg.result, m.cwd))
}

// importedSessionNote is the transcript row an import writes.
//
// IT IS A TRANSCRIPT ROW, drawn with the same trust as the picker row built
// below — which already sanitizes. The foreign id is the transcript's FILE NAME
// and the cwd a record inside it, and both reached appendRow raw, where an
// escape repaints the rows around it. DisplayField strips controls and then
// redacts, in that order. The agent name is this build's own adapter label and
// the Zero id is Zero's, so neither is foreign input.
//
// Split from importForeignSession so the cwd half can be proved: agentsessions
// .Import now sanitizes what it stores, so through the real entry point that
// call is unobservable and a test would pass with or without it. It still earns
// its place — a session imported by an earlier build holds whatever the foreign
// transcript said, and this is what draws it.
func importedSessionNote(result agentsessions.ImportResult, workspace string) string {
	note := fmt.Sprintf("Imported %s session %s into Zero as %s (%d events).",
		result.Source.Agent, agentsessions.DisplayField(result.Source.ID), result.Session.SessionID, result.Events)
	if recorded := strings.TrimSpace(result.Source.Cwd); recorded != "" && !sessionMatchesWorkspace(recorded, workspace) {
		note += "\nIt ran in " + agentsessions.DisplayField(recorded) + ", so paths it mentions refer to that tree."
	}
	return note
}

// foreignSessionItems lists sessions belonging to OTHER coding agents that have
// not been imported yet, so /resume shows the work that exists rather than only
// the part already copied into Zero. Choosing one imports it and then resumes,
// which is why its Value is an "<agent>:<id>" reference rather than a Zero id.
//
// Reading these is a bounded index of each transcript's head, never the whole
// file (see internal/agentsessions). A store that is missing or has changed
// shape contributes a caller-visible problem rather than failing the picker —
// /resume must still open on a machine where one vendor shipped a new format
// this morning, without pretending the partial index is complete.
// importedSourceRefs is the set of foreign sessions that have actually been
// imported, so the picker can skip offering them a second time — listing a
// session twice, once as itself and once as its copy, is worse than not offering
// it at all.
//
// A SESSION WITH NO EVENTS DOES NOT COUNT AS IMPORTED. Import creates the local
// session and appends its transcript as two steps, so an append that fails
// leaves a session carrying the import tag and nothing else. The tag alone used
// to be enough to suppress the foreign source here, while the loop that builds
// the local rows drops the same session for having EventCount == 0 — so the
// work disappeared from the picker entirely: not offered as the original, not
// listed as the copy, and a retry impossible because the source was hidden. The
// two filters have to agree on what a real session is.
//
// Current imports roll back a session whose event append fails. This filter is
// retained for empty import records left by older builds or interrupted
// cleanup, so upgrading restores the source to the picker and makes it
// retryable. Reported by @jatmn.
func importedSourceRefs(existing []sessions.Metadata) map[string]bool {
	imported := map[string]bool{}
	for _, meta := range existing {
		if meta.EventCount == 0 {
			continue
		}
		if ref, ok := agentsessions.ForeignSourceRefFromTag(meta.Tag); ok {
			imported[ref] = true
		}
	}
	return imported
}

func (m model) foreignSessionItems(existing []sessions.Metadata, now time.Time) ([]pickerItem, []error) {
	imported := importedSourceRefs(existing)

	found, problems := agentsessions.DiscoverAllCached(m.agentSessionsEnv, m.cwd)
	items := make([]pickerItem, 0, len(found))
	for _, session := range found {
		ref := session.Agent + ":" + session.ID
		if imported[ref] {
			continue
		}
		// Sanitized AND redacted: stripping controls stops the row being repainted,
		// but a title is often the user's first prompt and can carry a pasted key.
		// agentsessions.DisplayField does both, in the order the redaction tests
		// pin — controls first, so a secret split by an escape byte is reassembled
		// before the shape match runs.
		label := displayValue(agentsessions.DisplayField(session.Title), "untitled")
		if when := foreignSessionAge(session.UpdatedAt, now); when != "" {
			label = sessionPickerLabel(when, label)
		}
		source := session
		agent := displayAgentName(session.Agent, "unknown")
		items = append(items, pickerItem{
			Label:         label,
			Value:         ref,
			Meta:          agent,
			Tab:           agent,
			Detail:        m.foreignSessionPickerDetail(session),
			ForeignSource: &source,
		})
	}
	return items, problems
}

// sessionAgentName is the agent a session came from, for the picker's tab strip.
//
// Imported sessions carry "imported:<agent>" in their tag (see
// internal/agentsessions). Everything else is Zero's own work. Deriving this
// from the tag rather than storing a second field keeps one source of truth —
// two fields recording the same fact would drift (repo invariant #5).
func sessionAgentName(tag string) string {
	if agent := agentsessions.ImportedAgent(tag); agent != "" {
		return displayAgentName(agent, "zero")
	}
	return "zero"
}

func displayAgentName(agent, fallback string) string {
	return displayValue(agentsessions.DisplayField(agent), fallback)
}

// sessionPickerTabs builds the tab strip: "All" first, then one tab per agent
// actually present, most-populated first so the busiest source is nearest.
//
// Only agents with sessions get a tab. A strip advertising "codex" on a machine
// that has never run Codex is a dead end the user has to discover by pressing
// Tab twice.
func sessionPickerTabs(items []pickerItem) []string {
	counts := map[string]int{}
	order := []string{}
	for _, item := range items {
		if item.Tab == "" {
			continue
		}
		if _, seen := counts[item.Tab]; !seen {
			order = append(order, item.Tab)
		}
		counts[item.Tab]++
	}
	if len(order) < 2 {
		// One source only — the strip would say "All | zero" and mean nothing.
		return nil
	}
	sort.SliceStable(order, func(a, b int) bool {
		if counts[order[a]] != counts[order[b]] {
			return counts[order[a]] > counts[order[b]]
		}
		return order[a] < order[b]
	})
	return append([]string{pickerTabAll}, order...)
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

func (m model) foreignSessionPickerDetail(session agentsessions.ForeignSession) string {
	parts := make([]string, 0, 3)
	if project := displayPath(m.cwd, session.Cwd); project != "" {
		parts = append(parts, agentsessions.DisplayField(project))
	}
	if modelID := strings.TrimSpace(session.ModelID); modelID != "" {
		parts = append(parts, agentsessions.DisplayField(modelID))
	}
	if branch := strings.TrimSpace(session.GitBranch); branch != "" {
		parts = append(parts, agentsessions.DisplayField(branch))
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
		if !sessionMatchesWorkspace(sessions.OperationalCwd(metas[i]), m.cwd) {
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
//
// THE COMPARISON IS DELEGATED, NOT REIMPLEMENTED. sessionCwd is a value a
// foreign transcript wrote — passed raw by importedSessionNote, and as the
// persisted WorkspaceKey by the picker and latest-session filters — and this
// function used to EvalSymlinks it before the Windows branch was reached. On
// Windows that resolves "\\server\share\repo" by contacting the share, so a
// transcript could make /resume dial a host and stall the Update loop. Discovery
// already refuses to resolve Windows paths (agentsessions.normalizeDirWithFS);
// the comparison here now uses that same policy, so matching information never
// becomes authority to reach a filesystem endpoint. Reported by @jatmn.
func sessionMatchesWorkspace(sessionCwd, workspaceCwd string) bool {
	sessionCwd = strings.TrimSpace(sessionCwd)
	workspaceCwd = strings.TrimSpace(workspaceCwd)
	if sessionCwd == "" || workspaceCwd == "" {
		return true
	}
	return agentsessions.SameWorkspace(sessionCwd, workspaceCwd)
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
	importedBoundary := false
	for _, event := range events {
		switch event.Type {
		case sessions.EventToolCall, sessions.EventToolResult:
			return true
		case sessions.EventMessage:
			payload := sessionPayload(event)
			if agentsessions.NoteEventIsBoundary(payload) {
				importedBoundary = true
				continue
			}
			if strings.EqualFold(payloadString(payload, "role"), "user") {
				if importedBoundary && strings.TrimSpace(payloadString(payload, "content")) != "" {
					return true
				}
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

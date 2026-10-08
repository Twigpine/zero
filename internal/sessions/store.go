package sessions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Gitlawb/zero/internal/fsutil"
)

const (
	MetadataFile = "metadata.json"
	EventsFile   = "events.jsonl"
)

type EventType string

const (
	EventMessage            EventType = "message"
	EventToolCall           EventType = "tool_call"
	EventPermission         EventType = "permission"
	EventPermissionRequest  EventType = "permission_request"
	EventPermissionDecision EventType = "permission_decision"
	EventToolResult         EventType = "tool_result"
	EventProviderUsage      EventType = "provider_usage"
	EventUsage              EventType = EventProviderUsage
	EventError              EventType = "error"
	EventSessionCheckpoint  EventType = "session_checkpoint"
	EventSessionRewind      EventType = "session_rewind"
	EventCompaction         EventType = "session_compaction"
	EventSessionFork        EventType = "session_fork"
	EventSessionChild       EventType = "session_child"
	EventSpecialistStart    EventType = "specialist_start"
	EventSpecialistStop     EventType = "specialist_stop"
	EventSpecDraft          EventType = "spec_draft"
	EventSpecApproved       EventType = "spec_approved"
	EventSpecRejected       EventType = "spec_rejected"
	EventGoalCreated        EventType = "goal_created"
	EventGoalUpdated        EventType = "goal_updated"
	EventGoalCleared        EventType = "goal_cleared"
)

type SessionKind string

const (
	SessionKindFork      SessionKind = "fork"
	SessionKindChild     SessionKind = "child"
	SessionKindSide      SessionKind = "side"
	SessionKindSpecDraft SessionKind = "spec-draft"
	SessionKindSpecImpl  SessionKind = "spec-impl"
)

type SpecStatus string

const (
	SpecStatusDraft    SpecStatus = "draft"
	SpecStatusApproved SpecStatus = "approved"
	SpecStatusRejected SpecStatus = "rejected"
)

type GoalStatus string

const (
	GoalStatusActive        GoalStatus = "active"
	GoalStatusPaused        GoalStatus = "paused"
	GoalStatusBlocked       GoalStatus = "blocked"
	GoalStatusBudgetLimited GoalStatus = "budget_limited"
	GoalStatusUsageLimited  GoalStatus = "usage_limited"
	GoalStatusComplete      GoalStatus = "complete"
)

type Goal struct {
	Objective         string     `json:"objective"`
	Status            GoalStatus `json:"status"`
	StatusReason      string     `json:"statusReason,omitempty"`
	TokenBudget       int        `json:"tokenBudget,omitempty"`
	TokensUsed        int        `json:"tokensUsed,omitempty"`
	ContinuationCount int        `json:"continuationCount,omitempty"`
	ContinuationLimit int        `json:"continuationLimit,omitempty"`
	CreatedAt         string     `json:"createdAt"`
	UpdatedAt         string     `json:"updatedAt"`
}

type Metadata struct {
	SessionID           string      `json:"sessionId"`
	SessionKind         SessionKind `json:"sessionKind,omitempty"`
	Title               string      `json:"title,omitempty"`
	Cwd                 string      `json:"cwd,omitempty"`
	ModelID             string      `json:"modelId,omitempty"`
	Provider            string      `json:"provider,omitempty"`
	Tag                 string      `json:"tag,omitempty"`
	Depth               int         `json:"depth,omitempty"`
	ParentSessionID     string      `json:"parentSessionId,omitempty"`
	RootSessionID       string      `json:"rootSessionId,omitempty"`
	AgentName           string      `json:"agentName,omitempty"`
	TaskID              string      `json:"taskId,omitempty"`
	ForkedFromEventID   string      `json:"forkedFromEventId,omitempty"`
	ForkedFromSequence  int         `json:"forkedFromSequence,omitempty"`
	SpawnedFromEventID  string      `json:"spawnedFromEventId,omitempty"`
	SpawnedFromSequence int         `json:"spawnedFromSequence,omitempty"`
	SpecID              string      `json:"specId,omitempty"`
	SpecFilePath        string      `json:"specFilePath,omitempty"`
	SpecStatus          SpecStatus  `json:"specStatus,omitempty"`
	SpecDraftModelID    string      `json:"specDraftModelId,omitempty"`
	SpecDraftReasoning  string      `json:"specDraftReasoning,omitempty"`
	SpecUserComment     string      `json:"specUserComment,omitempty"`
	SpecRejectReason    string      `json:"specRejectReason,omitempty"`
	SpecSourceSessionID string      `json:"specSourceSessionId,omitempty"`
	SpecImplSessionID   string      `json:"specImplSessionId,omitempty"`
	Goal                *Goal       `json:"goal,omitempty"`
	CreatedAt           string      `json:"createdAt"`
	UpdatedAt           string      `json:"updatedAt"`
	EventCount          int         `json:"eventCount"`
	LastEventType       EventType   `json:"lastEventType,omitempty"`
}

type CreateInput struct {
	SessionID           string
	SessionKind         SessionKind
	Title               string
	Cwd                 string
	ModelID             string
	Provider            string
	Tag                 string
	Depth               int
	ParentSessionID     string
	RootSessionID       string
	AgentName           string
	TaskID              string
	ForkedFromEventID   string
	ForkedFromSequence  int
	SpawnedFromEventID  string
	SpawnedFromSequence int
	SpecID              string
	SpecFilePath        string
	SpecStatus          SpecStatus
	SpecDraftModelID    string
	SpecDraftReasoning  string
	SpecUserComment     string
	SpecRejectReason    string
	SpecSourceSessionID string
	SpecImplSessionID   string
	Goal                *Goal
}

type ForkInput struct {
	SessionID   string
	SessionKind SessionKind
	Title       string
	Cwd         string
	ModelID     string
	Provider    string
	Tag         string
}

type ChildInput struct {
	SessionID string
	Title     string
	Cwd       string
	ModelID   string
	Provider  string
	Tag       string
	Depth     int
	AgentName string
	TaskID    string
	Prompt    string
}

type TreeNode struct {
	Session  Metadata   `json:"session"`
	Children []TreeNode `json:"children"`
}

type AppendEventInput struct {
	Type    EventType
	Payload any
}

type preparedAppendEvent struct {
	Type    EventType
	Payload json.RawMessage
}

type RecordSpecInput struct {
	SpecID              string
	SpecFilePath        string
	SpecStatus          SpecStatus
	SpecDraftModelID    string
	SpecDraftReasoning  string
	SpecUserComment     string
	SpecRejectReason    string
	SpecSourceSessionID string
	SpecImplSessionID   string
}

type Event struct {
	ID        string          `json:"id"`
	SessionID string          `json:"sessionId"`
	Sequence  int             `json:"sequence"`
	Type      EventType       `json:"type"`
	CreatedAt string          `json:"createdAt"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type StoreOptions struct {
	RootDir string
	Now     func() time.Time
	Env     map[string]string
}

type Store struct {
	RootDir string
	now     func() time.Time
	locksMu sync.Mutex
	// sessionLocks holds one in-process mutex per session id. Entries are never
	// removed: doing so safely would require reference counting (a goroutine
	// blocked on a mutex must not have it deleted and recreated out from under
	// it, which would break mutual exclusion). The cost of leaving them is a
	// single *sync.Mutex per distinct session id touched by this Store's
	// lifetime, which is small and bounded in practice — the CLI process is
	// short-lived and the TUI works with a bounded set of sessions. There is no
	// session-close/delete lifecycle hook to prune against, so unbounded growth
	// is accepted deliberately rather than risk an unsafe eviction.
	sessionLocks map[string]*sync.Mutex
	idCounter    atomic.Uint64
}

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func NewStore(options StoreOptions) *Store {
	rootDir := strings.TrimSpace(options.RootDir)
	if rootDir == "" {
		rootDir = DefaultRoot(options.Env)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Store{RootDir: rootDir, now: now, sessionLocks: map[string]*sync.Mutex{}}
}

func DefaultRoot(env map[string]string) string {
	dataHome := strings.TrimSpace(envValue(env, "XDG_DATA_HOME"))
	home := strings.TrimSpace(envValue(env, "HOME"))
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = userHome
		}
	}
	base := dataHome
	if base == "" {
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "zero", "sessions")
}

func ValidSessionID(sessionID string) bool {
	return sessionIDPattern.MatchString(strings.TrimSpace(sessionID))
}

func (store *Store) Create(input CreateInput) (Metadata, error) {
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		sessionID = store.createID()
	}
	if !ValidSessionID(sessionID) {
		return Metadata{}, fmt.Errorf("invalid zero session id %q", input.SessionID)
	}
	if input.Depth < 0 {
		return Metadata{}, fmt.Errorf("invalid zero session depth %d", input.Depth)
	}

	timestamp := store.timestamp()
	session := Metadata{
		SessionID:           sessionID,
		SessionKind:         input.SessionKind,
		Title:               strings.TrimSpace(input.Title),
		Cwd:                 strings.TrimSpace(input.Cwd),
		ModelID:             strings.TrimSpace(input.ModelID),
		Provider:            strings.TrimSpace(input.Provider),
		Tag:                 strings.TrimSpace(input.Tag),
		Depth:               input.Depth,
		ParentSessionID:     strings.TrimSpace(input.ParentSessionID),
		RootSessionID:       strings.TrimSpace(input.RootSessionID),
		AgentName:           strings.TrimSpace(input.AgentName),
		TaskID:              strings.TrimSpace(input.TaskID),
		ForkedFromEventID:   strings.TrimSpace(input.ForkedFromEventID),
		ForkedFromSequence:  input.ForkedFromSequence,
		SpawnedFromEventID:  strings.TrimSpace(input.SpawnedFromEventID),
		SpawnedFromSequence: input.SpawnedFromSequence,
		SpecID:              strings.TrimSpace(input.SpecID),
		SpecFilePath:        strings.TrimSpace(input.SpecFilePath),
		SpecStatus:          normalizeSpecStatus(input.SpecStatus),
		SpecDraftModelID:    strings.TrimSpace(input.SpecDraftModelID),
		SpecDraftReasoning:  strings.TrimSpace(input.SpecDraftReasoning),
		SpecUserComment:     strings.TrimSpace(input.SpecUserComment),
		SpecRejectReason:    strings.TrimSpace(input.SpecRejectReason),
		SpecSourceSessionID: strings.TrimSpace(input.SpecSourceSessionID),
		SpecImplSessionID:   strings.TrimSpace(input.SpecImplSessionID),
		Goal:                cloneGoal(input.Goal),
		CreatedAt:           timestamp,
		UpdatedAt:           timestamp,
		EventCount:          0,
	}

	if err := os.MkdirAll(store.RootDir, 0o700); err != nil {
		return Metadata{}, fmt.Errorf("create zero session root: %w", err)
	}
	if err := os.Mkdir(store.sessionPath(sessionID), 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Metadata{}, fmt.Errorf("zero session already exists: %s", sessionID)
		}
		return Metadata{}, fmt.Errorf("create zero session directory: %w", err)
	}
	if err := store.writeMetadata(session); err != nil {
		return Metadata{}, err
	}
	file, err := os.OpenFile(store.eventsPath(sessionID), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Metadata{}, fmt.Errorf("create zero session events file: %w", err)
	}
	if err := file.Close(); err != nil {
		return Metadata{}, fmt.Errorf("close zero session events file: %w", err)
	}
	return session, nil
}

func (store *Store) Get(sessionID string) (*Metadata, error) {
	if !ValidSessionID(sessionID) {
		return nil, fmt.Errorf("invalid zero session id %q", sessionID)
	}
	session, err := store.readMetadata(sessionID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &session, nil
}

func (store *Store) List() ([]Metadata, error) {
	if err := os.MkdirAll(store.RootDir, 0o700); err != nil {
		return nil, fmt.Errorf("create zero session root: %w", err)
	}
	entries, err := os.ReadDir(store.RootDir)
	if err != nil {
		return nil, fmt.Errorf("read zero session root: %w", err)
	}
	sessions := []Metadata{}
	for _, entry := range entries {
		if !entry.IsDir() || !ValidSessionID(entry.Name()) {
			continue
		}
		session, err := store.Get(entry.Name())
		if err != nil || session == nil {
			continue
		}
		sessions = append(sessions, *session)
	}
	sort.SliceStable(sessions, func(left int, right int) bool {
		if sessions[left].UpdatedAt == sessions[right].UpdatedAt {
			return sessions[left].SessionID < sessions[right].SessionID
		}
		return sessions[left].UpdatedAt > sessions[right].UpdatedAt
	})
	return sessions, nil
}

func (store *Store) Latest() (*Metadata, error) {
	sessions, err := store.List()
	if err != nil || len(sessions) == 0 {
		return nil, err
	}
	return &sessions[0], nil
}

// IsResumableKind reports whether a session kind represents a standalone,
// user-resumable conversation rather than a transient or agent-owned sub-run.
// Regular ("") and user fork sessions are resumable; side, child, and spec
// sessions are not, so they do not flood the resume picker.
func IsResumableKind(kind SessionKind) bool {
	switch kind {
	case "", SessionKindFork:
		return true
	default:
		return false
	}
}

// ListResumable returns only the sessions a user can resume as standalone
// conversations (see IsResumableKind), newest-first like List.
func (store *Store) ListResumable() ([]Metadata, error) {
	all, err := store.List()
	if err != nil {
		return nil, err
	}
	resumable := make([]Metadata, 0, len(all))
	for _, session := range all {
		if IsResumableKind(session.SessionKind) {
			resumable = append(resumable, session)
		}
	}
	return resumable, nil
}

// LatestResumable returns the most-recently-updated resumable session, or nil
// when none exist. Used by `/resume latest` so it lands on a real conversation
// instead of the newest child/spec sub-run.
func (store *Store) LatestResumable() (*Metadata, error) {
	resumable, err := store.ListResumable()
	if err != nil || len(resumable) == 0 {
		return nil, err
	}
	return &resumable[0], nil
}

func (store *Store) Fork(parentSessionID string, input ForkInput) (Metadata, error) {
	if !ValidSessionID(parentSessionID) {
		return Metadata{}, fmt.Errorf("invalid zero session id %q", parentSessionID)
	}
	parent, err := store.Get(parentSessionID)
	if err != nil {
		return Metadata{}, err
	}
	if parent == nil {
		return Metadata{}, fmt.Errorf("zero session not found: %s", parentSessionID)
	}
	events, err := store.ReadEvents(parentSessionID)
	if err != nil {
		return Metadata{}, err
	}
	var last Event
	if len(events) > 0 {
		last = events[len(events)-1]
	}

	title := strings.TrimSpace(input.Title)
	if title == "" && parent.Title != "" {
		title = parent.Title + " (fork)"
	}
	kind := input.SessionKind
	if kind == "" {
		kind = SessionKindFork
	}
	if kind != SessionKindFork && kind != SessionKindSide {
		return Metadata{}, fmt.Errorf("invalid zero fork session kind %q", kind)
	}
	fork, err := store.Create(CreateInput{
		SessionID:          input.SessionID,
		SessionKind:        kind,
		Title:              title,
		Cwd:                firstNonEmpty(input.Cwd, parent.Cwd),
		ModelID:            firstNonEmpty(input.ModelID, parent.ModelID),
		Provider:           firstNonEmpty(input.Provider, parent.Provider),
		Tag:                input.Tag,
		ParentSessionID:    parent.SessionID,
		RootSessionID:      firstNonEmpty(parent.RootSessionID, parent.SessionID),
		ForkedFromEventID:  last.ID,
		ForkedFromSequence: last.Sequence,
	})
	if err != nil {
		return Metadata{}, err
	}
	copyInputs := []AppendEventInput{}
	for _, event := range events {
		// Do NOT copy usage accounting into the fork. It already counted against the
		// parent, and a usage report that aggregates the parent and the fork would
		// otherwise double-count it. The fork only needs the conversation history
		// (messages, tool calls, checkpoints) to continue; usage is not replayed.
		if event.Type == EventUsage {
			continue
		}
		copyInputs = append(copyInputs, AppendEventInput{Type: event.Type, Payload: event.Payload})
	}
	if _, err := store.AppendEvents(fork.SessionID, copyInputs); err != nil {
		return Metadata{}, err
	}
	copied := len(copyInputs)
	// Copy the parent's content-addressed checkpoint blobs into the fork so the
	// copied EventSessionCheckpoint events resolve to real blobs and a rewind on
	// the fork can restore file content (otherwise rewind reads missing blobs
	// and silently skips the files).
	if err := store.copyBlobs(parent.SessionID, fork.SessionID); err != nil {
		return Metadata{}, err
	}
	if _, err := store.AppendEvent(fork.SessionID, AppendEventInput{
		Type: EventSessionFork,
		Payload: map[string]any{
			"parentSessionId":    parent.SessionID,
			"parentEventCount":   parent.EventCount,
			"copiedEventCount":   copied,
			"forkedFromEventId":  last.ID,
			"forkedFromSequence": last.Sequence,
		},
	}); err != nil {
		return Metadata{}, err
	}
	loaded, err := store.readMetadata(fork.SessionID)
	if err != nil {
		return Metadata{}, err
	}
	return loaded, nil
}

func (store *Store) RecordSpec(sessionID string, input RecordSpecInput) (Metadata, Event, error) {
	if !ValidSessionID(sessionID) {
		return Metadata{}, Event{}, fmt.Errorf("invalid zero session id %q", sessionID)
	}
	status := normalizeSpecStatus(input.SpecStatus)
	if status == "" {
		return Metadata{}, Event{}, fmt.Errorf("zero spec status is required")
	}
	unlock, err := store.lockSession(sessionID)
	if err != nil {
		return Metadata{}, Event{}, err
	}
	defer unlock()

	session, err := store.readMetadata(sessionID)
	if err != nil {
		return Metadata{}, Event{}, err
	}
	applySpecRecord(&session, input, status)
	if err := store.writeMetadata(session); err != nil {
		return Metadata{}, Event{}, err
	}
	event, err := store.appendEventLocked(sessionID, AppendEventInput{
		Type: specEventType(status),
		Payload: map[string]any{
			"specId":              session.SpecID,
			"specFilePath":        session.SpecFilePath,
			"specStatus":          session.SpecStatus,
			"specDraftModelId":    session.SpecDraftModelID,
			"specDraftReasoning":  session.SpecDraftReasoning,
			"specUserComment":     session.SpecUserComment,
			"specRejectReason":    session.SpecRejectReason,
			"specSourceSessionId": session.SpecSourceSessionID,
			"specImplSessionId":   session.SpecImplSessionID,
		},
	})
	if err != nil {
		return Metadata{}, Event{}, err
	}
	loaded, err := store.readMetadata(sessionID)
	if err != nil {
		return Metadata{}, Event{}, err
	}
	return loaded, event, nil
}

func (store *Store) AppendEvent(sessionID string, input AppendEventInput) (Event, error) {
	events, err := store.AppendEvents(sessionID, []AppendEventInput{input})
	if err != nil {
		return Event{}, err
	}
	return events[0], nil
}

// AppendEvents appends a batch of events atomically under one session lock and
// durability pass. The returned events have contiguous sequences in input order.
// An empty batch is a no-op and does not rewrite metadata.
func (store *Store) AppendEvents(sessionID string, inputs []AppendEventInput) ([]Event, error) {
	if !ValidSessionID(sessionID) {
		return nil, fmt.Errorf("invalid zero session id %q", sessionID)
	}
	prepared, err := prepareAppendEventInputs(inputs)
	if err != nil {
		return nil, err
	}
	if len(prepared) == 0 {
		return []Event{}, nil
	}
	unlock, err := store.lockSession(sessionID)
	if err != nil {
		return nil, err
	}
	defer unlock()

	return store.appendPreparedEventsLocked(sessionID, prepared)
}

// AppendEventUnlessExists appends input only when exists(currentEvents) is false,
// performing the existence check and the append atomically under the session lock
// (in-process mutex + cross-process file lock). It is the safe primitive for
// "record once" accounting: a plain ReadEvents-then-AppendEvent has a
// check-then-act race where two callers — even in separate processes — both see
// the event absent and each append a duplicate. Returns appended=false when the
// predicate already matched. A nil predicate always appends.
func (store *Store) AppendEventUnlessExists(sessionID string, input AppendEventInput, exists func([]Event) bool) (Event, bool, error) {
	if !ValidSessionID(sessionID) {
		return Event{}, false, fmt.Errorf("invalid zero session id %q", sessionID)
	}
	if strings.TrimSpace(string(input.Type)) == "" {
		return Event{}, false, fmt.Errorf("zero session event type is required")
	}
	unlock, err := store.lockSession(sessionID)
	if err != nil {
		return Event{}, false, err
	}
	defer unlock()

	if exists != nil {
		// Safe under the held lock: ReadEvents only os.ReadFile's the log and never
		// re-acquires the session lock, so there is no deadlock.
		events, err := store.ReadEvents(sessionID)
		if err != nil {
			return Event{}, false, err
		}
		if exists(events) {
			return Event{}, false, nil
		}
	}
	event, err := store.appendEventLocked(sessionID, input)
	if err != nil {
		return Event{}, false, err
	}
	return event, true, nil
}

// appendEventLocked appends an event WITHOUT acquiring the session lock. The
// caller MUST already hold store.lockSession(sessionID). It exists so multi-step
// operations (e.g. ApplyRewind) can append the trailing marker atomically under
// the single lock they already hold, instead of re-locking (which would deadlock
// on the non-reentrant in-process mutex).
func (store *Store) appendEventLocked(sessionID string, input AppendEventInput) (Event, error) {
	prepared, err := prepareAppendEventInputs([]AppendEventInput{input})
	if err != nil {
		return Event{}, err
	}
	events, err := store.appendPreparedEventsLocked(sessionID, prepared)
	if err != nil {
		return Event{}, err
	}
	return events[0], nil
}

// appendPreparedEventsLocked appends pre-validated events WITHOUT acquiring the
// session lock. The caller MUST already hold store.lockSession(sessionID).
func (store *Store) appendPreparedEventsLocked(sessionID string, inputs []preparedAppendEvent) ([]Event, error) {
	if len(inputs) == 0 {
		return []Event{}, nil
	}
	session, err := store.readMetadata(sessionID)
	if err != nil {
		return nil, err
	}
	sequence := session.EventCount + 1
	// The events log is the source of truth for sequencing. metadata.EventCount is
	// persisted separately (after the event line below), so a crash between the two
	// can leave it behind the log; deriving from the log's last sequence then avoids
	// reusing a number (a duplicate that would mis-target /rewind). Best-effort: a
	// log read error falls back to the metadata count. Only triggers on a real
	// desync (log at/ahead of the metadata-derived sequence).
	if logSeq, err := store.lastEventSequence(sessionID); err == nil && logSeq >= sequence {
		sequence = logSeq + 1
	}
	events := make([]Event, 0, len(inputs))
	var batch bytes.Buffer
	for index, input := range inputs {
		timestamp := store.timestamp()
		event := Event{
			ID:        fmt.Sprintf("%s:%d", sessionID, sequence+index),
			SessionID: sessionID,
			Sequence:  sequence + index,
			Type:      input.Type,
			CreatedAt: timestamp,
			Payload:   input.Payload,
		}
		data, err := json.Marshal(event)
		if err != nil {
			return nil, fmt.Errorf("encode zero session event: %w", err)
		}
		batch.Write(data)
		batch.WriteByte('\n')
		events = append(events, event)
	}
	file, err := os.OpenFile(store.eventsPath(sessionID), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("append zero session event: %w", err)
	}
	if _, err := file.Write(batch.Bytes()); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("append zero session event: %w", err)
	}
	// fsync the events before reporting success: the derived metadata.json IS
	// fsync'd (writeMetadata), so without this a crash after the metadata flush
	// but before the events.jsonl page reaches disk leaves EventCount ahead of the
	// durable log — silently losing just-appended events (incl. checkpoints
	// that /rewind targets). Make the log at least as durable as its metadata. (AUDIT-M12)
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("sync zero session event: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close zero session event file: %w", err)
	}
	last := events[len(events)-1]
	session.UpdatedAt = last.CreatedAt
	session.EventCount = last.Sequence
	session.LastEventType = last.Type
	if err := store.writeMetadata(session); err != nil {
		return nil, err
	}
	return events, nil
}

// UpdateTitle replaces a session's Title and returns the updated metadata. It is
// serialized under the same per-session lock as AppendEvent and re-reads the
// latest metadata under that lock before rewriting, so a concurrent append can't
// clobber the new title (nor the title clobber a concurrent append's event
// count/timestamp). UpdatedAt is deliberately left untouched: a rename is not
// activity, so it must not reorder the session in the resumable list. A blank
// title is rejected so a failed model generation can never erase a useful
// first-message title, and an unchanged title is a no-op (no rewrite/fsync).
func (store *Store) UpdateTitle(sessionID string, title string) (Metadata, error) {
	if !ValidSessionID(sessionID) {
		return Metadata{}, fmt.Errorf("invalid zero session id %q", sessionID)
	}
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return Metadata{}, fmt.Errorf("zero session title is required")
	}
	unlock, err := store.lockSession(sessionID)
	if err != nil {
		return Metadata{}, err
	}
	defer unlock()

	session, err := store.readMetadata(sessionID)
	if err != nil {
		return Metadata{}, err
	}
	if session.Title == trimmed {
		return session, nil
	}
	session.Title = trimmed
	if err := store.writeMetadata(session); err != nil {
		return Metadata{}, err
	}
	return session, nil
}

// UpdateTitleIfCurrent replaces a session title only when it still matches
// expected. It lets background automatic naming avoid overwriting a newer manual
// rename while keeping the check and write under the same per-session lock.
func (store *Store) UpdateTitleIfCurrent(sessionID string, expected string, title string) (Metadata, bool, error) {
	if !ValidSessionID(sessionID) {
		return Metadata{}, false, fmt.Errorf("invalid zero session id %q", sessionID)
	}
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return Metadata{}, false, fmt.Errorf("zero session title is required")
	}
	unlock, err := store.lockSession(sessionID)
	if err != nil {
		return Metadata{}, false, err
	}
	defer unlock()

	session, err := store.readMetadata(sessionID)
	if err != nil {
		return Metadata{}, false, err
	}
	if session.Title != strings.TrimSpace(expected) {
		return session, false, nil
	}
	if session.Title == trimmed {
		return session, true, nil
	}
	session.Title = trimmed
	if err := store.writeMetadata(session); err != nil {
		return Metadata{}, false, err
	}
	return session, true, nil
}

// UpdateModel replaces a session's selected model without changing its activity
// timestamp or event counters. An empty model clears the session override.
func (store *Store) UpdateModel(sessionID string, modelID string) (Metadata, error) {
	if !ValidSessionID(sessionID) {
		return Metadata{}, fmt.Errorf("invalid zero session id %q", sessionID)
	}
	modelID = strings.TrimSpace(modelID)
	unlock, err := store.lockSession(sessionID)
	if err != nil {
		return Metadata{}, err
	}
	defer unlock()

	session, err := store.readMetadata(sessionID)
	if err != nil {
		return Metadata{}, err
	}
	if session.ModelID == modelID {
		return session, nil
	}
	session.ModelID = modelID
	if err := store.writeMetadata(session); err != nil {
		return Metadata{}, err
	}
	return session, nil
}

func (store *Store) ReadEvents(sessionID string) ([]Event, error) {
	events, _, err := store.ReadEventsWithPresence(sessionID)
	return events, err
}

// ReadEventsWithPresence preserves the distinction between an intentionally
// empty event log and a missing one. Most readers accept both as empty history,
// but activation protocols that promise to restore previously populated
// context need the stronger signal.
func (store *Store) ReadEventsWithPresence(sessionID string) ([]Event, bool, error) {
	if !ValidSessionID(sessionID) {
		return nil, false, fmt.Errorf("invalid zero session id %q", sessionID)
	}
	data, err := os.ReadFile(store.eventsPath(sessionID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Event{}, false, nil
		}
		return nil, false, fmt.Errorf("read zero session events: %w", err)
	}
	// A genuine torn tail is an INCOMPLETE final write — a crash mid-append leaves
	// the last line without its terminating newline. If the file ends with a
	// newline, every record was fully flushed, so a malformed final line is real
	// corruption and must still fail loudly.
	tornTailPossible := len(data) > 0 && data[len(data)-1] != '\n'
	lines := bytes.Split(data, []byte{'\n'})
	lastNonEmpty := -1
	for index, line := range lines {
		if len(bytes.TrimSpace(line)) > 0 {
			lastNonEmpty = index
		}
	}
	events := []Event{}
	for index, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			// Tolerate a torn tail: a crash mid-append leaves the final line
			// truncated (and thus without a trailing newline). Drop that partial
			// record so resume still recovers every complete event. A malformed
			// line anywhere earlier — or a complete-but-corrupt final line — is real
			// corruption and still fails loudly.
			if index == lastNonEmpty && tornTailPossible {
				break
			}
			return nil, true, fmt.Errorf("invalid json in zero session %s %s at line %d: %w", sessionID, EventsFile, index+1, err)
		}
		events = append(events, event)
	}
	return events, true, nil
}

func (store *Store) timestamp() string {
	return store.now().UTC().Format(time.RFC3339)
}

func (store *Store) createID() string {
	timestamp := store.now().UTC()
	return fmt.Sprintf("zero_%s_%d_%d", timestamp.Format("20060102150405"), timestamp.UnixNano(), store.idCounter.Add(1))
}

func (store *Store) sessionLock(sessionID string) *sync.Mutex {
	store.locksMu.Lock()
	defer store.locksMu.Unlock()
	if store.sessionLocks == nil {
		store.sessionLocks = map[string]*sync.Mutex{}
	}
	lock := store.sessionLocks[sessionID]
	if lock == nil {
		lock = &sync.Mutex{}
		store.sessionLocks[sessionID] = lock
	}
	return lock
}

// lockSession serializes mutations to a session both in-process (the existing
// per-Store mutex) and across processes (an OS file lock on a per-session
// .lock file), so a CLI rewind and a TUI sharing the same RootDir cannot
// interleave writes. It returns an unlock function that releases both locks in
// reverse order. The OS lock is best-effort: if it cannot be acquired (e.g. an
// unsupported platform) the in-memory mutex still applies.
func (store *Store) lockSession(sessionID string) (func(), error) {
	mu := store.sessionLock(sessionID)
	mu.Lock()
	release, err := store.acquireFileLock(sessionID)
	if err != nil {
		mu.Unlock()
		return nil, err
	}
	return func() {
		release()
		mu.Unlock()
	}, nil
}

func (store *Store) lockPath(sessionID string) string {
	return filepath.Join(store.sessionPath(sessionID), "session.lock")
}

func (store *Store) sessionPath(sessionID string) string {
	return filepath.Join(store.RootDir, sessionID)
}

func (store *Store) metadataPath(sessionID string) string {
	return filepath.Join(store.sessionPath(sessionID), MetadataFile)
}

func (store *Store) eventsPath(sessionID string) string {
	return filepath.Join(store.sessionPath(sessionID), EventsFile)
}

func (store *Store) readMetadata(sessionID string) (Metadata, error) {
	data, err := os.ReadFile(store.metadataPath(sessionID))
	if err != nil {
		return Metadata{}, err
	}
	var session Metadata
	if err := json.Unmarshal(data, &session); err != nil {
		return Metadata{}, fmt.Errorf("invalid zero session metadata %s: %w", sessionID, err)
	}
	return session, nil
}

func (store *Store) writeMetadata(session Metadata) error {
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("encode zero session metadata: %w", err)
	}
	path := store.metadataPath(session.SessionID)
	tmp := fmt.Sprintf("%s.tmp-%d", path, store.idCounter.Add(1))
	// fsync the temp file before renaming it into place. os.WriteFile does not
	// sync, so a crash right after the rename could surface a metadata file whose
	// contents were never flushed — a torn or empty file that corrupts the whole
	// session. Syncing the data before the atomic rename closes that window.
	if err := writeFileSync(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write zero session metadata: %w", err)
	}
	if err := fsutil.RenameWithRetry(tmp, path, nil); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace zero session metadata: %w", err)
	}
	// fsync the parent directory so the rename itself is durable: the temp file's
	// contents were synced above, but without syncing the directory a crash can
	// still lose the rename (the new directory entry), leaving the old/no file.
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync zero session dir: %w", err)
	}
	return nil
}

// syncDir fsyncs a directory so a rename/create within it is durable across a
// crash. A platform that cannot open a directory for sync (e.g. Windows) reports
// no error — the rename is best-effort durable there.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		// Windows does not support fsync on a directory handle; the rename is
		// best-effort durable there.
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return nil
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// writeFileSync writes data to path and fsyncs it before returning, so the bytes
// are durably on disk (unlike os.WriteFile, which leaves them in the page cache).
func writeFileSync(path string, data []byte, perm os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// writeFileAtomicSync durably writes content to path with the same crash-safe
// sequence writeMetadata uses: write a temp file and fsync it, atomically rename
// it into place, then fsync the parent directory. Checkpoint blobs and rewind
// restores go through this so they are as durable as session metadata (plain
// WriteFile+Rename leaves the bytes in the page cache, so a crash can surface a
// torn or empty file).
func (store *Store) writeFileAtomicSync(path string, content []byte, perm os.FileMode) error {
	tmp := fmt.Sprintf("%s.tmp-%d", path, store.idCounter.Add(1))
	if err := writeFileSync(tmp, content, perm); err != nil {
		return err
	}
	if err := fsutil.RenameWithRetry(tmp, path, nil); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Dir(path))
}

// lastEventSequence returns the sequence of the last COMPLETE (newline-
// terminated) event in the session log, or 0 if the log is empty/absent. A torn
// trailing partial line (an interrupted append) is ignored, matching ReadEvents.
// It reads only the file's tail, so it is O(1) regardless of log length. It lets
// the events log — not the separately-persisted metadata.EventCount — be the
// source of truth for the next sequence number, so a crash between the event
// append and the metadata write can never cause a reused (duplicate) sequence.
func (store *Store) lastEventSequence(sessionID string) (int, error) {
	file, err := os.Open(store.eventsPath(sessionID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()
	if size == 0 {
		return 0, nil
	}
	// Read the tail, growing the window until it contains the last complete line
	// (events are usually small; a very large last event grows it up to the file).
	window := int64(64 * 1024)
	for {
		if window > size {
			window = size
		}
		buf := make([]byte, window)
		if _, err := file.ReadAt(buf, size-window); err != nil {
			return 0, err
		}
		lastNL := bytes.LastIndexByte(buf, '\n')
		if lastNL < 0 {
			if window >= size {
				return 0, nil // no newline in the whole file -> no complete event
			}
			window *= 2
			continue
		}
		prevNL := bytes.LastIndexByte(buf[:lastNL], '\n')
		if prevNL < 0 && window < size {
			window *= 2 // the last complete line may start before the window
			continue
		}
		line := bytes.TrimSpace(buf[prevNL+1 : lastNL])
		if len(line) == 0 {
			return 0, nil
		}
		var probe struct {
			Sequence int `json:"sequence"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			// A complete-but-corrupt last line: defer to the full, recovery-aware
			// read so a single bad line never silently mis-sequences the next event.
			events, rerr := store.ReadEvents(sessionID)
			if rerr != nil {
				return 0, rerr
			}
			maxSeq := 0
			for _, event := range events {
				if event.Sequence > maxSeq {
					maxSeq = event.Sequence
				}
			}
			return maxSeq, nil
		}
		return probe.Sequence, nil
	}
}

func rawPayload(payload any) (json.RawMessage, error) {
	if payload == nil {
		return nil, nil
	}
	if raw, ok := payload.(json.RawMessage); ok {
		copied := append(json.RawMessage{}, bytes.TrimSpace(raw)...)
		if len(copied) == 0 {
			return nil, nil
		}
		if !json.Valid(copied) {
			return nil, fmt.Errorf("invalid raw JSON payload")
		}
		return copied, nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode zero session payload: %w", err)
	}
	return data, nil
}

func prepareAppendEventInputs(inputs []AppendEventInput) ([]preparedAppendEvent, error) {
	prepared := make([]preparedAppendEvent, 0, len(inputs))
	for _, input := range inputs {
		if strings.TrimSpace(string(input.Type)) == "" {
			return nil, fmt.Errorf("zero session event type is required")
		}
		payload, err := rawPayload(input.Payload)
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, preparedAppendEvent{
			Type:    input.Type,
			Payload: payload,
		})
	}
	return prepared, nil
}

func envValue(env map[string]string, key string) string {
	if env != nil {
		return env[key]
	}
	return os.Getenv(key)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func normalizeSpecStatus(status SpecStatus) SpecStatus {
	switch SpecStatus(strings.ToLower(strings.TrimSpace(string(status)))) {
	case SpecStatusDraft:
		return SpecStatusDraft
	case SpecStatusApproved:
		return SpecStatusApproved
	case SpecStatusRejected:
		return SpecStatusRejected
	default:
		return ""
	}
}

func specEventType(status SpecStatus) EventType {
	switch normalizeSpecStatus(status) {
	case SpecStatusApproved:
		return EventSpecApproved
	case SpecStatusRejected:
		return EventSpecRejected
	default:
		return EventSpecDraft
	}
}

func applySpecRecord(session *Metadata, input RecordSpecInput, status SpecStatus) {
	if specID := strings.TrimSpace(input.SpecID); specID != "" {
		session.SpecID = specID
	}
	if path := strings.TrimSpace(input.SpecFilePath); path != "" {
		session.SpecFilePath = path
	}
	session.SpecStatus = status
	if modelID := strings.TrimSpace(input.SpecDraftModelID); modelID != "" {
		session.SpecDraftModelID = modelID
	}
	if reasoning := strings.TrimSpace(input.SpecDraftReasoning); reasoning != "" {
		session.SpecDraftReasoning = reasoning
	}
	if comment := strings.TrimSpace(input.SpecUserComment); comment != "" {
		session.SpecUserComment = comment
	}
	if reason := strings.TrimSpace(input.SpecRejectReason); reason != "" {
		session.SpecRejectReason = reason
	}
	if sourceID := strings.TrimSpace(input.SpecSourceSessionID); sourceID != "" {
		session.SpecSourceSessionID = sourceID
	}
	if implID := strings.TrimSpace(input.SpecImplSessionID); implID != "" {
		session.SpecImplSessionID = implID
	}
}

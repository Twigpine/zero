package search

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Gitlawb/zero/internal/redaction"
	"github.com/Gitlawb/zero/internal/sessions"
)

const (
	IndexFileName  = "search-index.json"
	indexSchema    = 1
	defaultLimit   = 20
	defaultContext = 80
)

type Options struct {
	Store        *sessions.Store
	RootDir      string
	Limit        int
	ContextChars int
	SessionID    string
	Type         sessions.EventType
	Reindex      bool
	Now          func() time.Time
}

type EventSummary struct {
	ID        string             `json:"id"`
	Sequence  int                `json:"sequence"`
	Type      sessions.EventType `json:"type"`
	CreatedAt string             `json:"createdAt"`
}

type Hit struct {
	Session sessions.Metadata `json:"session"`
	Event   EventSummary      `json:"event"`
	Context string            `json:"context"`
	Match   Match             `json:"match"`
}

type Match struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

type Result struct {
	Query            string `json:"query"`
	NormalizedQuery  string `json:"normalizedQuery"`
	RootDir          string `json:"rootDir"`
	SearchedSessions int    `json:"searchedSessions"`
	// SkippedSessions counts sessions whose event log could not be read or
	// indexed; they are skipped so one corrupt session can't abort the search.
	SkippedSessions int   `json:"skippedSessions,omitempty"`
	TotalHits       int   `json:"totalHits"`
	Hits            []Hit `json:"hits"`
}

type Index struct {
	SchemaVersion     int          `json:"schemaVersion"`
	SessionID         string       `json:"sessionId"`
	SessionUpdatedAt  string       `json:"sessionUpdatedAt"`
	SessionEventCount int          `json:"sessionEventCount"`
	GeneratedAt       string       `json:"generatedAt"`
	Entries           []IndexEntry `json:"entries"`
}

type IndexEntry struct {
	SessionID string             `json:"sessionId"`
	EventID   string             `json:"eventId"`
	Sequence  int                `json:"sequence"`
	Type      sessions.EventType `json:"type"`
	CreatedAt string             `json:"createdAt"`
	Text      string             `json:"text"`
}

func Sessions(query string, options Options) (Result, error) {
	normalized := NormalizeQuery(query)
	store := options.Store
	if store == nil {
		store = sessions.NewStore(sessions.StoreOptions{RootDir: options.RootDir})
	}
	limit := normalizeLimit(options.Limit)
	contextChars := normalizeContext(options.ContextChars)
	if normalized == "" || limit == 0 {
		return Result{Query: query, NormalizedQuery: normalized, RootDir: store.RootDir, Hits: []Hit{}}, nil
	}
	sessionList, err := resolveSessions(store, strings.TrimSpace(options.SessionID))
	if err != nil {
		return Result{}, err
	}
	terms := splitTerms(normalized)
	now := options.Now
	if now == nil {
		now = time.Now
	}
	result := Result{
		Query:            query,
		NormalizedQuery:  normalized,
		RootDir:          store.RootDir,
		SearchedSessions: len(sessionList),
		Hits:             []Hit{},
	}
	for _, session := range sessionList {
		index, err := LoadIndex(store, session, LoadOptions{Reindex: options.Reindex, Now: now})
		if err != nil {
			// A single corrupt or unreadable session must not abort the whole
			// search surface; skip it and report the count.
			result.SkippedSessions++
			continue
		}
		for _, entry := range index.Entries {
			if options.Type != "" && entry.Type != options.Type {
				continue
			}
			match, ok := findMatch(entry.Text, normalized, terms)
			if !ok {
				continue
			}
			result.Hits = append(result.Hits, Hit{
				Session: session,
				Event: EventSummary{
					ID:        entry.EventID,
					Sequence:  entry.Sequence,
					Type:      entry.Type,
					CreatedAt: entry.CreatedAt,
				},
				Context: buildContext(entry.Text, match.Start, match.End, contextChars),
				Match:   match,
			})
			if len(result.Hits) >= limit {
				result.TotalHits = len(result.Hits)
				return result, nil
			}
		}
	}
	result.TotalHits = len(result.Hits)
	return result, nil
}

type LoadOptions struct {
	Reindex bool
	Now     func() time.Time
}

func LoadIndex(store *sessions.Store, session sessions.Metadata, options LoadOptions) (Index, error) {
	if !options.Reindex {
		index, ok := readIndex(store, session)
		if ok && current(index, session) {
			return index, nil
		}
	}
	return RebuildIndex(store, session, options.Now)
}

func RebuildIndex(store *sessions.Store, session sessions.Metadata, now func() time.Time) (Index, error) {
	if now == nil {
		now = time.Now
	}
	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		return Index{}, err
	}
	index := Index{
		SchemaVersion:     indexSchema,
		SessionID:         session.SessionID,
		SessionUpdatedAt:  session.UpdatedAt,
		SessionEventCount: session.EventCount,
		GeneratedAt:       now().UTC().Format(time.RFC3339),
		Entries:           make([]IndexEntry, 0, len(events)),
	}
	for _, event := range events {
		index.Entries = append(index.Entries, IndexEntry{
			SessionID: session.SessionID,
			EventID:   event.ID,
			Sequence:  event.Sequence,
			Type:      event.Type,
			CreatedAt: event.CreatedAt,
			Text:      ExtractText(redactedPayload(event.Payload)),
		})
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return Index{}, fmt.Errorf("encode Zero search index: %w", err)
	}
	if err := os.WriteFile(indexPath(store, session.SessionID), append(data, '\n'), 0o600); err != nil {
		return Index{}, fmt.Errorf("write Zero search index: %w", err)
	}
	return index, nil
}

func FormatResult(result Result) string {
	query := redaction.RedactString(strings.TrimSpace(result.Query), redaction.Options{})
	if len(result.Hits) == 0 {
		return fmt.Sprintf("No local session events matched %q. Searched %s.", query, count(result.SearchedSessions, "session"))
	}
	lines := []string{fmt.Sprintf("Found %s for %q:", count(result.TotalHits, "local session event"), query)}
	for index, hit := range result.Hits {
		title := ""
		if hit.Session.Title != "" {
			title = " - " + redaction.RedactString(hit.Session.Title, redaction.Options{})
		}
		lines = append(lines, fmt.Sprintf("%d. %s #%d %s%s", index+1, redaction.RedactString(hit.Session.SessionID, redaction.Options{}), hit.Event.Sequence, redaction.RedactString(string(hit.Event.Type), redaction.Options{}), title))
		context := strings.Join(strings.Fields(redaction.RedactString(hit.Context, redaction.Options{})), " ")
		if context != "" {
			lines = append(lines, "   "+context)
		}
		details := []string{}
		if hit.Session.Cwd != "" {
			details = append(details, "cwd: "+redaction.RedactString(hit.Session.Cwd, redaction.Options{}))
		}
		if hit.Session.ModelID != "" {
			details = append(details, "model: "+redaction.RedactString(hit.Session.ModelID, redaction.Options{}))
		}
		if hit.Session.Provider != "" {
			details = append(details, "provider: "+redaction.RedactString(hit.Session.Provider, redaction.Options{}))
		}
		details = append(details, "updated: "+redaction.RedactString(hit.Session.UpdatedAt, redaction.Options{}))
		lines = append(lines, "   "+strings.Join(details, " | "))
	}
	return strings.Join(lines, "\n")
}

func RedactResult(result Result) Result {
	options := redaction.Options{}
	redacted := result
	redacted.Query = redaction.RedactString(redacted.Query, options)
	redacted.NormalizedQuery = redaction.RedactString(redacted.NormalizedQuery, options)
	redacted.RootDir = redaction.RedactString(redacted.RootDir, options)
	redacted.Hits = make([]Hit, len(result.Hits))
	for index, hit := range result.Hits {
		redacted.Hits[index] = Hit{
			Session: redactMetadata(hit.Session, options),
			Event:   redactEventSummary(hit.Event, options),
			Context: redaction.RedactString(hit.Context, options),
			Match:   hit.Match,
		}
	}
	return redacted
}

// redactMetadata redacts EVERY text field of the metadata, not the subset a
// reader happens to think is user-facing. `zero search --json` prints this
// straight to stdout, and the fields carrying the most free text are the ones
// least likely to be on a hand-kept list: SpecUserComment and SpecRejectReason
// are review text the user typed, SpecDraftReasoning is the model's own prose,
// and Goal.Objective and Goal.StatusReason are whatever the goal was phrased
// as. A field added to sessions.Metadata later is covered by
// TestRedactMetadataCoversEveryTextField, which walks the struct rather than
// repeating this list, so the two cannot drift apart silently.
func redactMetadata(session sessions.Metadata, options redaction.Options) sessions.Metadata {
	session.SessionID = redaction.RedactString(session.SessionID, options)
	session.SessionKind = sessions.SessionKind(redaction.RedactString(string(session.SessionKind), options))
	session.Title = redaction.RedactString(session.Title, options)
	session.Cwd = redaction.RedactString(session.Cwd, options)
	session.ModelID = redaction.RedactString(session.ModelID, options)
	session.Provider = redaction.RedactString(session.Provider, options)
	session.Tag = redaction.RedactString(session.Tag, options)
	session.ParentSessionID = redaction.RedactString(session.ParentSessionID, options)
	session.RootSessionID = redaction.RedactString(session.RootSessionID, options)
	session.AgentName = redaction.RedactString(session.AgentName, options)
	session.TaskID = redaction.RedactString(session.TaskID, options)
	session.ForkedFromEventID = redaction.RedactString(session.ForkedFromEventID, options)
	session.SpawnedFromEventID = redaction.RedactString(session.SpawnedFromEventID, options)
	session.SpecID = redaction.RedactString(session.SpecID, options)
	session.SpecFilePath = redaction.RedactString(session.SpecFilePath, options)
	session.SpecStatus = sessions.SpecStatus(redaction.RedactString(string(session.SpecStatus), options))
	session.SpecDraftModelID = redaction.RedactString(session.SpecDraftModelID, options)
	session.SpecDraftReasoning = redaction.RedactString(session.SpecDraftReasoning, options)
	session.SpecUserComment = redaction.RedactString(session.SpecUserComment, options)
	session.SpecRejectReason = redaction.RedactString(session.SpecRejectReason, options)
	session.SpecSourceSessionID = redaction.RedactString(session.SpecSourceSessionID, options)
	session.SpecImplSessionID = redaction.RedactString(session.SpecImplSessionID, options)
	session.CreatedAt = redaction.RedactString(session.CreatedAt, options)
	session.UpdatedAt = redaction.RedactString(session.UpdatedAt, options)
	session.LastEventType = sessions.EventType(redaction.RedactString(string(session.LastEventType), options))
	session.Goal = redactGoal(session.Goal, options)
	return session
}

// redactGoal returns a redacted COPY. Metadata is passed by value, but Goal is
// a pointer inside it, so redacting through it would rewrite the goal the
// caller still holds: a search would quietly edit the live session record it
// was only supposed to report on.
func redactGoal(goal *sessions.Goal, options redaction.Options) *sessions.Goal {
	if goal == nil {
		return nil
	}
	redacted := *goal
	redacted.Objective = redaction.RedactString(redacted.Objective, options)
	redacted.Status = sessions.GoalStatus(redaction.RedactString(string(redacted.Status), options))
	redacted.StatusReason = redaction.RedactString(redacted.StatusReason, options)
	redacted.CreatedAt = redaction.RedactString(redacted.CreatedAt, options)
	redacted.UpdatedAt = redaction.RedactString(redacted.UpdatedAt, options)
	return &redacted
}

func redactEventSummary(event EventSummary, options redaction.Options) EventSummary {
	event.ID = redaction.RedactString(event.ID, options)
	event.Type = sessions.EventType(redaction.RedactString(string(event.Type), options))
	event.CreatedAt = redaction.RedactString(event.CreatedAt, options)
	return event
}

func NormalizeQuery(query string) string {
	return strings.ToLower(strings.Join(strings.Fields(query), " "))
}

func ExtractText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case float64, float32, int, int64, bool:
		return fmt.Sprint(typed)
	case []any:
		parts := []string{}
		for _, item := range typed {
			if text := ExtractText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " ")
	case map[string]any:
		parts := []string{}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			parts = append(parts, key)
			item := typed[key]
			if text := ExtractText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, " ")
	default:
		return fmt.Sprint(typed)
	}
}

func resolveSessions(store *sessions.Store, sessionID string) ([]sessions.Metadata, error) {
	if sessionID == "" {
		return store.List()
	}
	session, err := store.Get(sessionID)
	if err != nil {
		return []sessions.Metadata{}, err
	}
	if session == nil {
		// Surfacing the miss beats silently "succeeding" with zero results
		// against a session that doesn't exist.
		return []sessions.Metadata{}, fmt.Errorf("zero session not found: %s", sessionID)
	}
	return []sessions.Metadata{*session}, nil
}

func readIndex(store *sessions.Store, session sessions.Metadata) (Index, bool) {
	data, err := os.ReadFile(indexPath(store, session.SessionID))
	if err != nil {
		return Index{}, false
	}
	var index Index
	if err := json.Unmarshal(data, &index); err != nil {
		return Index{}, false
	}
	return index, true
}

func current(index Index, session sessions.Metadata) bool {
	return index.SchemaVersion == indexSchema &&
		index.SessionID == session.SessionID &&
		index.SessionUpdatedAt == session.UpdatedAt &&
		index.SessionEventCount == session.EventCount &&
		len(index.Entries) == session.EventCount
}

func redactedPayload(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return redaction.RedactString(string(raw), redaction.Options{})
	}
	return redaction.RedactValue(payload, redaction.Options{})
}

// findMatch locates the query (or all terms) case-insensitively and returns
// byte offsets INTO THE ORIGINAL text. Matching happens on a lowered copy, but
// Unicode lowering can change byte lengths (İ → i̇, K → k), so lowered offsets
// are mapped back through a per-byte offset table — applying them to the
// original directly mis-slices runes and could even run past the end of the
// string (a panic in buildContext).
func findMatch(text string, query string, terms []string) (Match, bool) {
	lowered, offsets := lowerWithOffsets(text)
	mapBack := func(start, end int) Match {
		return Match{Start: offsets[start], End: offsets[end]}
	}
	if index := strings.Index(lowered, query); index >= 0 {
		return mapBack(index, index+len(query)), true
	}
	first := -1
	last := -1
	for _, term := range terms {
		index := strings.Index(lowered, term)
		if index < 0 {
			return Match{}, false
		}
		if first < 0 || index < first {
			first = index
		}
		if end := index + len(term); end > last {
			last = end
		}
	}
	if first < 0 {
		return Match{}, false
	}
	return mapBack(first, last), true
}

// lowerWithOffsets lowers text rune-by-rune and returns, for every byte offset
// of the lowered string (inclusive of the end boundary), the corresponding
// byte offset in the original.
func lowerWithOffsets(text string) (string, []int) {
	var lowered strings.Builder
	lowered.Grow(len(text))
	offsets := make([]int, 0, len(text)+1)
	for index, glyph := range text {
		lower := strings.ToLower(string(glyph))
		for range len(lower) {
			offsets = append(offsets, index)
		}
		lowered.WriteString(lower)
	}
	offsets = append(offsets, len(text))
	return lowered.String(), offsets
}

func buildContext(text string, start int, end int, contextChars int) string {
	left := snapRuneStart(text, start-contextChars)
	right := snapRuneStart(text, end+contextChars)
	return strings.TrimSpace(text[left:right])
}

// snapRuneStart clamps index into [0, len(text)] and walks it back to the
// nearest rune boundary so context slicing never emits invalid UTF-8.
func snapRuneStart(text string, index int) int {
	if index <= 0 {
		return 0
	}
	if index >= len(text) {
		return len(text)
	}
	for index > 0 && !utf8.RuneStart(text[index]) {
		index--
	}
	return index
}

func splitTerms(query string) []string {
	if query == "" {
		return nil
	}
	return strings.Fields(query)
}

func normalizeLimit(limit int) int {
	if limit == 0 {
		return defaultLimit
	}
	if limit < 0 {
		return 0
	}
	return limit
}

func normalizeContext(contextChars int) int {
	if contextChars == 0 {
		return defaultContext
	}
	if contextChars < 0 {
		return 0
	}
	return contextChars
}

func indexPath(store *sessions.Store, sessionID string) string {
	return filepath.Join(store.RootDir, sessionID, IndexFileName)
}

func count(value int, label string) string {
	suffix := "s"
	if value == 1 {
		suffix = ""
	}
	return fmt.Sprintf("%d %s%s", value, label, suffix)
}

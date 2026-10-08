package sessions

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Gitlawb/zero/internal/redaction"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

type EventRef struct {
	ID        string    `json:"id"`
	Sequence  int       `json:"sequence"`
	Type      EventType `json:"type"`
	CreatedAt string    `json:"createdAt,omitempty"`
}

type RewindOptions struct {
	TargetSequence int
	TargetEventID  string
	KeepTarget     bool
}

type RewindPlan struct {
	SessionID       string     `json:"sessionId"`
	TargetSequence  int        `json:"targetSequence"`
	TargetEventID   string     `json:"targetEventId"`
	KeepTarget      bool       `json:"keepTarget"`
	KeptCount       int        `json:"keptCount"`
	DroppedCount    int        `json:"droppedCount"`
	LastKeptEventID string     `json:"lastKeptEventId,omitempty"`
	KeptEvents      []EventRef `json:"keptEvents"`
	DroppedEvents   []EventRef `json:"droppedEvents"`
}

type CompactionOptions struct {
	PreserveLast   int
	MaxPromptChars int
	SkipPrompt     bool
}

type CompactionPlan struct {
	SessionID         string     `json:"sessionId"`
	PreserveLast      int        `json:"preserveLast"`
	CompactableCount  int        `json:"compactableCount"`
	PreservedCount    int        `json:"preservedCount"`
	CompactableEvents []EventRef `json:"compactableEvents"`
	PreservedEvents   []EventRef `json:"preservedEvents"`
	SummaryPrompt     string     `json:"summaryPrompt"`
	// PromptChars counts prompt text content and excludes provider protocol framing.
	PromptChars int  `json:"promptChars"`
	Truncated   bool `json:"truncated,omitempty"`
}

type RecordCompactionInput struct {
	Plan    CompactionPlan
	Summary string
}

// CompactionPayload is the payload of an EventCompaction event. It records the
// summary that should replace compacted-away events during replay.
type CompactionPayload struct {
	Summary                  string     `json:"summary"`
	PreserveLast             int        `json:"preserveLast"`
	CompactableCount         int        `json:"compactableCount"`
	PreservedCount           int        `json:"preservedCount"`
	CompactedThroughEventID  string     `json:"compactedThroughEventId,omitempty"`
	CompactedThroughSequence int        `json:"compactedThroughSequence,omitempty"`
	CompactableEvents        []EventRef `json:"compactableEvents,omitempty"`
	PreservedEvents          []EventRef `json:"preservedEvents,omitempty"`
	PromptChars              int        `json:"promptChars,omitempty"`
	Truncated                bool       `json:"truncated,omitempty"`
}

const defaultCompactionPreserveLast = 6
const defaultCompactionMaxPromptChars = 8000

func (store *Store) PlanRewind(sessionID string, options RewindOptions) (RewindPlan, error) {
	if !ValidSessionID(sessionID) {
		return RewindPlan{}, fmt.Errorf("invalid zero session id %q", sessionID)
	}
	events, err := store.ReadEvents(sessionID)
	if err != nil {
		return RewindPlan{}, err
	}
	if len(events) == 0 {
		return RewindPlan{}, fmt.Errorf("zero session %s has no events to rewind", sessionID)
	}
	targetIndex, err := findRewindTarget(events, options)
	if err != nil {
		return RewindPlan{}, err
	}
	target := events[targetIndex]
	cutoff := targetIndex
	if options.KeepTarget {
		cutoff = targetIndex + 1
	}
	kept := events[:cutoff]
	dropped := events[cutoff:]
	plan := RewindPlan{
		SessionID:      sessionID,
		TargetSequence: target.Sequence,
		TargetEventID:  target.ID,
		KeepTarget:     options.KeepTarget,
		KeptCount:      len(kept),
		DroppedCount:   len(dropped),
		KeptEvents:     eventRefs(kept),
		DroppedEvents:  eventRefs(dropped),
	}
	if len(kept) > 0 {
		plan.LastKeptEventID = kept[len(kept)-1].ID
	}
	return plan, nil
}

func findRewindTarget(events []Event, options RewindOptions) (int, error) {
	targetEventID := strings.TrimSpace(options.TargetEventID)
	if targetEventID == "" && options.TargetSequence <= 0 {
		return -1, fmt.Errorf("rewind target event id or sequence is required")
	}
	if targetEventID != "" && options.TargetSequence > 0 {
		for index, event := range events {
			if event.ID == targetEventID {
				if event.Sequence != options.TargetSequence {
					return -1, fmt.Errorf("conflicting rewind target selectors: event %s has sequence %d, not %d", targetEventID, event.Sequence, options.TargetSequence)
				}
				return index, nil
			}
		}
		return -1, fmt.Errorf("rewind target event %s was not found", targetEventID)
	}
	for index, event := range events {
		if targetEventID != "" && event.ID == targetEventID {
			return index, nil
		}
		if options.TargetSequence > 0 && event.Sequence == options.TargetSequence {
			return index, nil
		}
	}
	if targetEventID != "" {
		return -1, fmt.Errorf("rewind target event %s was not found", targetEventID)
	}
	return -1, fmt.Errorf("rewind target sequence %d was not found", options.TargetSequence)
}

func (store *Store) PlanCompaction(sessionID string, options CompactionOptions) (CompactionPlan, error) {
	if !ValidSessionID(sessionID) {
		return CompactionPlan{}, fmt.Errorf("invalid zero session id %q", sessionID)
	}
	events, err := store.ReadEvents(sessionID)
	if err != nil {
		return CompactionPlan{}, err
	}
	preserveLast := options.PreserveLast
	if preserveLast <= 0 {
		preserveLast = defaultCompactionPreserveLast
	}
	maxPromptChars := options.MaxPromptChars
	if maxPromptChars <= 0 {
		maxPromptChars = defaultCompactionMaxPromptChars
	}
	split := len(events) - preserveLast
	if split < 0 {
		split = 0
	}
	compactable := events[:split]
	preserved := events[split:]
	prompt := ""
	truncated := false
	if !options.SkipPrompt {
		prompt, truncated = buildCompactionPrompt(compactable, maxPromptChars)
	}
	return CompactionPlan{
		SessionID:         sessionID,
		PreserveLast:      preserveLast,
		CompactableCount:  len(compactable),
		PreservedCount:    len(preserved),
		CompactableEvents: eventRefs(compactable),
		PreservedEvents:   eventRefs(preserved),
		SummaryPrompt:     prompt,
		PromptChars:       len(prompt),
		Truncated:         truncated,
	}, nil
}

func (store *Store) RecordCompaction(sessionID string, input RecordCompactionInput) (Event, error) {
	if !ValidSessionID(sessionID) {
		return Event{}, fmt.Errorf("invalid zero session id %q", sessionID)
	}
	if strings.TrimSpace(input.Plan.SessionID) == "" {
		return Event{}, fmt.Errorf("compaction plan session id is required")
	}
	if input.Plan.SessionID != sessionID {
		return Event{}, fmt.Errorf("compaction plan session %s does not match %s", input.Plan.SessionID, sessionID)
	}
	payload, err := CompactionPayloadFromPlan(input.Summary, input.Plan)
	if err != nil {
		return Event{}, err
	}
	return store.AppendEvent(sessionID, AppendEventInput{Type: EventCompaction, Payload: payload})
}

func CompactionPayloadFromPlan(summary string, plan CompactionPlan) (CompactionPayload, error) {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return CompactionPayload{}, fmt.Errorf("compaction summary is required")
	}
	if len(plan.CompactableEvents) == 0 {
		return CompactionPayload{}, fmt.Errorf("compaction plan has no compactable events")
	}
	lastCompactable := plan.CompactableEvents[len(plan.CompactableEvents)-1]
	return CompactionPayload{
		Summary:                  summary,
		PreserveLast:             plan.PreserveLast,
		CompactableCount:         plan.CompactableCount,
		PreservedCount:           plan.PreservedCount,
		CompactedThroughEventID:  lastCompactable.ID,
		CompactedThroughSequence: lastCompactable.Sequence,
		CompactableEvents:        cloneEventRefs(plan.CompactableEvents),
		PreservedEvents:          cloneEventRefs(plan.PreservedEvents),
		PromptChars:              plan.PromptChars,
		Truncated:                plan.Truncated,
	}, nil
}

func (store *Store) ReadRehydratedEvents(sessionID string) ([]Event, error) {
	events, _, err := store.ReadRehydratedEventsWithPresence(sessionID)
	return events, err
}

// ReadRehydratedEventsWithPresence carries the underlying event-log presence
// through compaction projection without changing ReadRehydratedEvents' existing
// empty-on-missing contract.
func (store *Store) ReadRehydratedEventsWithPresence(sessionID string) ([]Event, bool, error) {
	events, present, err := store.ReadEventsWithPresence(sessionID)
	if err != nil {
		return nil, present, err
	}
	rehydrated, err := RehydrateEvents(events)
	return rehydrated, present, err
}

func (store *Store) ReadReplayEvents(sessionID string) ([]Event, error) {
	return store.ReadRehydratedEvents(sessionID)
}

func RehydrateEvents(events []Event) ([]Event, error) {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type != EventCompaction {
			continue
		}
		payload, err := decodeCompactionPayload(events[i])
		if err != nil {
			return nil, err
		}
		return rehydrateEventsWithCompaction(events, events[i], payload), nil
	}
	return cloneEvents(events), nil
}

func decodeCompactionPayload(event Event) (CompactionPayload, error) {
	var payload CompactionPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return CompactionPayload{}, fmt.Errorf("decode compaction payload seq %d: %w", event.Sequence, err)
	}
	if strings.TrimSpace(payload.Summary) == "" {
		return CompactionPayload{}, fmt.Errorf("decode compaction payload seq %d: summary is required", event.Sequence)
	}
	return payload, nil
}

func rehydrateEventsWithCompaction(events []Event, compaction Event, payload CompactionPayload) []Event {
	skipIDs := map[string]bool{}
	for _, ref := range payload.CompactableEvents {
		if ref.ID != "" {
			skipIDs[ref.ID] = true
		}
	}
	useSequenceCutoff := len(skipIDs) == 0 && payload.CompactedThroughSequence > 0
	shouldSkip := func(event Event) bool {
		if skipIDs[event.ID] {
			return true
		}
		return useSequenceCutoff && event.Sequence <= payload.CompactedThroughSequence
	}

	rehydrated := make([]Event, 0, len(events))
	insertedSummary := false
	for _, event := range events {
		if event.ID == compaction.ID {
			continue
		}
		if shouldSkip(event) {
			if !insertedSummary {
				rehydrated = append(rehydrated, compaction)
				insertedSummary = true
			}
			continue
		}
		rehydrated = append(rehydrated, event)
	}
	if !insertedSummary {
		return append([]Event{compaction}, rehydrated...)
	}
	return rehydrated
}

func buildCompactionPrompt(events []Event, maxChars int) (string, bool) {
	if len(events) == 0 {
		return "No compactable Zero session events.", false
	}
	lines := []string{
		"Summarize these Zero session events for future context.",
		"Preserve user intent, tool outcomes, important files, blockers, and follow-up state.",
	}
	for _, event := range events {
		lines = append(lines, fmt.Sprintf("%d %s %s", event.Sequence, event.Type, shapedPayloadPreview(event)))
	}
	prompt := strings.Join(lines, "\n")
	if maxChars > 0 && len(prompt) > maxChars {
		if maxChars <= len("\n[truncated]") {
			return cutPromptRuneBoundary(prompt, maxChars), true
		}
		return cutPromptRuneBoundary(prompt, maxChars-len("\n[truncated]")) + "\n[truncated]", true
	}
	return prompt, false
}

// CompactionMessages converts durable session events into the same normalized
// message shape used by the running agent's compaction pipeline. Provider usage
// and checkpoint blobs are omitted. Message/tool fields rely on the session
// writer's invariant that tool output is scrubbed before persistence;
// ask_user answers and generic event previews are redacted here.
func CompactionMessages(events []Event) []zeroruntime.Message {
	messages := make([]zeroruntime.Message, 0, len(events))
	for _, event := range events {
		var payload map[string]any
		_ = json.Unmarshal(event.Payload, &payload)
		stringField := func(key string) string {
			value, _ := payload[key].(string)
			return strings.TrimSpace(value)
		}
		switch event.Type {
		case EventMessage:
			role := strings.ToLower(stringField("role"))
			content := stringField("content")
			switch role {
			case "user":
				messages = append(messages, zeroruntime.Message{Role: zeroruntime.MessageRoleUser, Content: content})
			case "assistant":
				messages = append(messages, zeroruntime.Message{Role: zeroruntime.MessageRoleAssistant, Content: content})
			case "ask_user_answers":
				if answers, ok := payload["answers"]; ok {
					encoded, _ := json.Marshal(answers)
					messages = append(messages, zeroruntime.Message{Role: zeroruntime.MessageRoleUser, Content: "User answers: " + redaction.RedactString(string(encoded), redaction.Options{})})
				}
			default:
				if content != "" {
					messages = append(messages, zeroruntime.Message{Role: zeroruntime.MessageRoleUser, Content: content})
				}
			}
		case EventToolCall:
			messages = append(messages, zeroruntime.Message{Role: zeroruntime.MessageRoleAssistant, ToolCalls: []zeroruntime.ToolCall{{
				ID: firstSessionString(stringField("toolCallId"), stringField("id")), Name: firstSessionString(stringField("name"), stringField("toolName")), Arguments: stringField("arguments"),
			}}})
		case EventToolResult:
			status := strings.ToLower(stringField("status"))
			messages = append(messages, zeroruntime.Message{
				Role: zeroruntime.MessageRoleTool, ToolCallID: firstSessionString(stringField("toolCallId"), stringField("id")),
				Content: stringField("output"), IsError: status != "" && status != "ok", ChangedFiles: sessionStringSlice(payload["changedFiles"]),
			})
		case EventCompaction:
			if summary := stringField("summary"); summary != "" {
				messages = append(messages, zeroruntime.Message{Role: zeroruntime.MessageRoleUser, Content: "[Summary of earlier conversation]\n" + summary})
			}
		case EventProviderUsage, EventSessionCheckpoint:
			continue
		default:
			preview := shapedPayloadPreview(event)
			if strings.TrimSpace(preview) != "" && preview != "{}" {
				messages = append(messages, zeroruntime.Message{Role: zeroruntime.MessageRoleUser, Content: fmt.Sprintf("[%s] %s", event.Type, preview)})
			}
		}
	}
	return messages
}

func firstSessionString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func sessionStringSlice(value any) []string {
	raw, ok := value.([]any)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(raw))
	for _, item := range raw {
		if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
			values = append(values, strings.TrimSpace(text))
		}
	}
	return values
}

func shapedPayloadPreview(event Event) string {
	switch event.Type {
	case EventPermission, EventPermissionRequest, EventPermissionDecision:
		return permissionPayloadPreview(event.Payload)
	case EventToolCall:
		return toolPayloadPreview(event.Payload, []string{"id", "name", "toolName"})
	case EventToolResult:
		return toolPayloadPreview(event.Payload, []string{"id", "name", "toolName", "status"})
	default:
		return payloadPreview(event.Payload)
	}
}

func permissionPayloadPreview(payload json.RawMessage) string {
	var value map[string]any
	if err := json.Unmarshal(payload, &value); err != nil {
		return payloadPreview(payload)
	}
	shaped := map[string]any{}
	for _, key := range []string{"action", "name", "toolName", "permission", "permissionMode", "sideEffect", "grantMatched"} {
		if field, ok := value[key]; ok {
			shaped[key] = field
		}
	}
	if risk, ok := value["risk"].(map[string]any); ok {
		if level, ok := risk["level"]; ok {
			shaped["riskLevel"] = level
		}
	}
	return marshalPreview(shaped)
}

func toolPayloadPreview(payload json.RawMessage, allowedKeys []string) string {
	var value map[string]any
	if err := json.Unmarshal(payload, &value); err != nil {
		return payloadPreview(payload)
	}
	shaped := map[string]any{}
	for _, key := range allowedKeys {
		if field, ok := value[key]; ok {
			shaped[key] = field
		}
	}
	if len(shaped) == 0 {
		shaped["payload"] = "redacted"
	}
	return marshalPreview(shaped)
}

func payloadPreview(payload json.RawMessage) string {
	if len(payload) == 0 {
		return "{}"
	}
	value := strings.Join(strings.Fields(string(payload)), " ")
	value = redaction.RedactString(value, redaction.Options{})
	if len(value) > 240 {
		return cutPromptRuneBoundary(value, 240) + "..."
	}
	return value
}

// cutPromptRuneBoundary truncates to at most n bytes on a rune boundary so
// prompt and preview truncation can't emit invalid UTF-8.
func cutPromptRuneBoundary(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func marshalPreview(value map[string]any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return payloadPreview(data)
}

func eventRefs(events []Event) []EventRef {
	refs := make([]EventRef, 0, len(events))
	for _, event := range events {
		refs = append(refs, EventRef{
			ID:        event.ID,
			Sequence:  event.Sequence,
			Type:      event.Type,
			CreatedAt: event.CreatedAt,
		})
	}
	return refs
}

func cloneEventRefs(refs []EventRef) []EventRef {
	if len(refs) == 0 {
		return nil
	}
	cloned := make([]EventRef, len(refs))
	copy(cloned, refs)
	return cloned
}

func cloneEvents(events []Event) []Event {
	if len(events) == 0 {
		return []Event{}
	}
	cloned := make([]Event, len(events))
	copy(cloned, events)
	return cloned
}

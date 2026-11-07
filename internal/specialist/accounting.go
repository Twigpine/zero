package specialist

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/Twigpine/zero/internal/background"
	"github.com/Twigpine/zero/internal/sessions"
)

const specialistAccountingSource = "specialist"

// accountingMu serializes the stop/usage accounting paths within this process as
// a cheap fast path. The real dedup guarantee, however, comes from
// appendSpecialistEventOnce → Store.AppendEventUnlessExists, which performs the
// existence check and the append atomically under the session lock (in-process
// mutex + cross-process file lock). That closes the check-then-append race even
// across processes — e.g. a foreground onExit racing a TaskOutput poll or a
// background reaper in a separate process — which accountingMu alone (process-
// local) could not. Executor is used by value, so a struct field would not be
// shared across copies; a single process-wide lock is sufficient here.
var accountingMu sync.Mutex

type specialistAccountingInput struct {
	ParentSessionID string
	ChildSessionID  string
	SpecialistName  string
	Description     string
	ToolCallID      string
	Mode            string
	Background      bool
	PID             int
}

func (executor Executor) recordSpecialistStart(input specialistAccountingInput) {
	payload := baseSpecialistPayload(input)
	_, _ = appendSpecialistSessionEvent(executor.SessionStore, input.ParentSessionID, sessions.EventSpecialistStart, payload)
}

func (executor Executor) recordSpecialistStop(input specialistAccountingInput, summary StreamResult, status string, exitCode int, runErr error, usageRolledUp bool) {
	store := accountingStore(executor.SessionStore)
	accountingMu.Lock()
	defer accountingMu.Unlock()
	payload := baseSpecialistPayload(input)
	if summary.RunID != "" {
		payload["runId"] = summary.RunID
	}
	payload["status"] = specialistStopStatus(status, exitCode, runErr)
	payload["exitCode"] = exitCode
	payload["usageRolledUp"] = usageRolledUp
	if runErr != nil {
		payload["error"] = runErr.Error()
	}
	if len(summary.Errors) > 0 {
		payload["errors"] = append([]string(nil), summary.Errors...)
	}
	// Atomic check+append under the session lock so a concurrent stop path cannot
	// also pass the existence check and write a duplicate stop event.
	_, _ = appendSpecialistEventOnce(store, input.ParentSessionID, sessions.EventSpecialistStop, payload, input.ChildSessionID, summary.RunID)
}

func (executor Executor) rollUpSpecialistUsage(input specialistAccountingInput, summary StreamResult) bool {
	rolledUp, _ := appendSpecialistUsageRollup(executor.SessionStore, input, summary)
	return rolledUp
}

func (executor Executor) recordBackgroundTaskAccounting(task background.Task, summary StreamResult) {
	if task.Status == background.StatusRunning {
		return
	}
	input := specialistAccountingInput{
		ParentSessionID: strings.TrimSpace(task.ParentID),
		ChildSessionID:  strings.TrimSpace(task.ID),
		SpecialistName:  strings.TrimSpace(task.SpecialistName),
		Description:     strings.TrimSpace(task.Description),
		Mode:            "background",
		Background:      true,
		PID:             task.PID,
	}
	rolledUp := executor.rollUpSpecialistUsage(input, summary)
	executor.recordSpecialistStop(input, summary, string(task.Status), task.ExitCode, nil, rolledUp)
}

func appendSpecialistUsageRollup(store *sessions.Store, input specialistAccountingInput, summary StreamResult) (bool, error) {
	if !summary.Usage.HasUsage() {
		return false, nil
	}
	if strings.TrimSpace(input.ParentSessionID) == "" || strings.TrimSpace(input.ChildSessionID) == "" || !sessions.ValidSessionID(input.ParentSessionID) {
		return false, nil
	}
	store = accountingStore(store)
	accountingMu.Lock()
	defer accountingMu.Unlock()
	payload := baseSpecialistPayload(input)
	if summary.RunID != "" {
		payload["runId"] = summary.RunID
	}
	payload["promptTokens"] = summary.Usage.PromptTokens
	payload["completionTokens"] = summary.Usage.CompletionTokens
	payload["totalTokens"] = summary.Usage.EffectiveTotalTokens()
	payload["usageEvents"] = summary.Usage.Events
	// Atomic check+append under the session lock so the TaskOutput poll and the
	// onExit path cannot both pass the existence check and double-count usage.
	return appendSpecialistEventOnce(store, input.ParentSessionID, sessions.EventUsage, payload, input.ChildSessionID, summary.RunID)
}

func appendSpecialistSessionEvent(store *sessions.Store, parentSessionID string, eventType sessions.EventType, payload map[string]any) (sessions.Event, error) {
	parentSessionID = strings.TrimSpace(parentSessionID)
	if parentSessionID == "" || !sessions.ValidSessionID(parentSessionID) {
		return sessions.Event{}, nil
	}
	store = accountingStore(store)
	return store.AppendEvent(parentSessionID, sessions.AppendEventInput{Type: eventType, Payload: payload})
}

// specialistEventMatcher returns a predicate reporting whether a "record once"
// specialist event of eventType has already been written for this child/run. The
// catch-all runId rules mirror the dedup the accounting paths depend on.
func specialistEventMatcher(eventType sessions.EventType, childSessionID, runID string) func([]sessions.Event) bool {
	childSessionID = strings.TrimSpace(childSessionID)
	runID = strings.TrimSpace(runID)
	return func(events []sessions.Event) bool {
		if childSessionID == "" {
			return false
		}
		for _, event := range events {
			if event.Type != eventType {
				continue
			}
			payload := map[string]any{}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				continue
			}
			if specialistPayloadString(payload, "source") != specialistAccountingSource {
				continue
			}
			if specialistPayloadString(payload, "childSessionId") != childSessionID && specialistPayloadString(payload, "taskId") != childSessionID {
				continue
			}
			// An already-recorded event with NO runId is a catch-all for this child
			// (e.g. the immediate stop written when a PID couldn't be registered) and
			// must match a later event that does carry a runId — otherwise the same
			// child gets two stop/usage events. We match when: querying without a runId,
			// the existing event has none, or the runIds are equal.
			existingRunID := specialistPayloadString(payload, "runId")
			if runID == "" || existingRunID == "" || existingRunID == runID {
				return true
			}
		}
		return false
	}
}

// appendSpecialistEventOnce atomically records a "record once" specialist event:
// it appends payload only if no matching event already exists, under the session
// store's lock so concurrent stop/usage callers (even cross-process) cannot both
// pass an existence check and each write a duplicate. Returns appended=false when
// an equivalent event was already present.
func appendSpecialistEventOnce(store *sessions.Store, parentSessionID string, eventType sessions.EventType, payload map[string]any, childSessionID, runID string) (bool, error) {
	parentSessionID = strings.TrimSpace(parentSessionID)
	if parentSessionID == "" || !sessions.ValidSessionID(parentSessionID) {
		return false, nil
	}
	store = accountingStore(store)
	_, appended, err := store.AppendEventUnlessExists(
		parentSessionID,
		sessions.AppendEventInput{Type: eventType, Payload: payload},
		specialistEventMatcher(eventType, childSessionID, runID),
	)
	return appended, err
}

func baseSpecialistPayload(input specialistAccountingInput) map[string]any {
	childSessionID := strings.TrimSpace(input.ChildSessionID)
	payload := map[string]any{
		"source":         specialistAccountingSource,
		"childSessionId": childSessionID,
		"taskId":         childSessionID,
		"mode":           strings.TrimSpace(input.Mode),
		"background":     input.Background,
	}
	if specialistName := strings.TrimSpace(input.SpecialistName); specialistName != "" {
		payload["specialist"] = specialistName
	}
	if description := strings.TrimSpace(input.Description); description != "" {
		payload["description"] = description
	}
	if toolCallID := strings.TrimSpace(input.ToolCallID); toolCallID != "" {
		payload["toolCallId"] = toolCallID
	}
	if input.PID > 0 {
		payload["pid"] = input.PID
	}
	return payload
}

func specialistStopStatus(status string, exitCode int, runErr error) string {
	status = strings.TrimSpace(status)
	if status != "" {
		return status
	}
	if runErr != nil || exitCode != 0 {
		return "error"
	}
	return "success"
}

func specialistPayloadString(payload map[string]any, key string) string {
	value, ok := payload[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return ""
	}
}

func accountingStore(store *sessions.Store) *sessions.Store {
	if store != nil {
		return store
	}
	return sessions.NewStore(sessions.StoreOptions{})
}

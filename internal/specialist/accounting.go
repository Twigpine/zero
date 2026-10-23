package specialist

import (
	"context"
	"encoding/json"
	"os"
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
	// LaunchFailed marks a child that never started (e.g. the command failed to
	// exec). The internal start/stop accounting still records the attempt, but no
	// public specialistStart/specialistStop hook fires for it: there was no
	// sub-agent lifecycle to observe. The zero value is "started", so a caller
	// that forgets to set it never silently suppresses hooks.
	LaunchFailed bool
}

// recordSpecialistStart records the internal attempt accounting. It deliberately
// does NOT dispatch the public specialistStart hook: the hook fires only once
// the child has actually started (dispatchSpecialistStartHook), so a failed
// launch cannot announce a sub-agent that never existed.
func (executor Executor) recordSpecialistStart(input specialistAccountingInput) {
	payload := baseSpecialistPayload(input)
	_, _ = appendSpecialistSessionEvent(executor.SessionStore, input.ParentSessionID, sessions.EventSpecialistStart, payload)
}

// dispatchSpecialistStartHook publishes the specialistStart hook for a child
// whose startup was confirmed. Callers pass the same accounting input used for
// the attempt record so payload and matcher subject stay identical.
func (executor Executor) dispatchSpecialistStartHook(input specialistAccountingInput) {
	if input.LaunchFailed || executor.LifecycleHooks == nil || executor.LifecycleHooks.Dispatch == nil {
		return
	}
	executor.dispatchLifecycleHook("specialistStart", input, baseSpecialistPayload(input))
}

func (executor Executor) recordSpecialistStop(input specialistAccountingInput, summary StreamResult, status string, exitCode int, runErr error, usageRolledUp bool) {
	store := accountingStore(executor.SessionStore)
	payload := baseSpecialistPayload(input)
	// childStarted is the durable eligibility flag for the public stop hook: a
	// later reaper (TaskOutput poll, recovered TaskStop, onExit in another
	// process) derives whether the terminal transition deserves a hook from the
	// recorded stop event, since it cannot know whether the child ever started.
	payload["childStarted"] = !input.LaunchFailed
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
	// Hold accountingMu only for the deduped append so a slow lifecycle hook cannot
	// stall concurrent specialist stop/usage accounting.
	accountingMu.Lock()
	_, _ = appendSpecialistEventOnce(store, input.ParentSessionID, sessions.EventSpecialistStop, payload, input.ChildSessionID, summary.RunID)
	accountingMu.Unlock()
	executor.dispatchSpecialistStopHook(store, input, payload, summary.RunID)
}

// specialistStopHookEvent durably records that the specialistStop hook dispatch
// decision for one child/run was settled. It lives in the parent session's event
// log - the same cross-process, lock-guarded ledger the accounting dedup uses -
// so a later Zero process reaping the same task (fresh LifecycleHooks bridge)
// does not re-run the hook. This is internal bookkeeping, not a lifecycle event.
const specialistStopHookEvent sessions.EventType = "specialist_stop_hook"

// dispatchSpecialistStopHook runs the specialistStop hook at most once per
// child/run. The claim follows the accounting equivalence rules (an empty runId
// is a catch-all for the same child), so concurrent or sequential finishers -
// Task.onExit, a TaskOutput poll in another process, a recovered TaskStop - share
// one durable claim. Swarm members have no parent session (nothing durable to
// key on), so they fall back to this bridge's in-process claim, which mirrors
// the same run-ID equivalence.
//
// Hooks are external side effects, so on a store error the durable claim cannot
// be established; the hook then falls back to the in-process claim rather than
// running unchecked, keeping at-most-once within this process.
func (executor Executor) dispatchSpecialistStopHook(store *sessions.Store, input specialistAccountingInput, payload map[string]any, runID string) {
	hooks := executor.LifecycleHooks
	if hooks == nil || hooks.Dispatch == nil {
		return
	}
	started := !input.LaunchFailed
	parentSessionID := strings.TrimSpace(input.ParentSessionID)
	if parentSessionID == "" || !sessions.ValidSessionID(parentSessionID) {
		if started && hooks.claimInProcessStopDispatch(input.ChildSessionID, runID) {
			executor.dispatchLifecycleHook("specialistStop", input, payload)
		}
		return
	}
	if recorded, ok := recordedStopChildStarted(store, parentSessionID, input.ChildSessionID, runID); ok {
		started = recorded
	}
	claimed, err := appendSpecialistEventOnce(store, parentSessionID, specialistStopHookEvent, stopHookClaimPayload(input, runID, started), input.ChildSessionID, runID)
	if err != nil {
		// The durable ledger is unavailable. Keep the hook advisory but bounded to
		// a single dispatch in this process rather than dropping it or risking an
		// unbounded duplicate.
		if started && hooks.claimInProcessStopDispatch(input.ChildSessionID, runID) {
			executor.dispatchLifecycleHook("specialistStop", input, payload)
		}
		return
	}
	if claimed && started {
		executor.dispatchLifecycleHook("specialistStop", input, payload)
	}
}

// recordedStopChildStarted reads the first recorded stop event matching the
// child/run equivalence rules and reports its childStarted eligibility flag.
// ok is false when no stop event exists yet or it predates the flag, in which
// case the caller keeps its locally known eligibility.
func recordedStopChildStarted(store *sessions.Store, parentSessionID, childSessionID, runID string) (started bool, ok bool) {
	store = accountingStore(store)
	childSessionID = strings.TrimSpace(childSessionID)
	if childSessionID == "" {
		return false, false
	}
	runID = strings.TrimSpace(runID)
	events, err := store.ReadEvents(parentSessionID)
	if err != nil {
		return false, false
	}
	for _, event := range events {
		if event.Type != sessions.EventSpecialistStop {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		if !specialistPayloadMatches(payload, childSessionID, runID) {
			continue
		}
		if started, ok := payload["childStarted"].(bool); ok {
			return started, true
		}
		return false, false
	}
	return false, false
}

func stopHookClaimPayload(input specialistAccountingInput, runID string, started bool) map[string]any {
	payload := baseSpecialistPayload(input)
	payload["hook"] = "specialistStop"
	// childStarted is the resolved eligibility (locally known or read back from
	// the durable stop event); dispatched records that a hook was run for it.
	payload["childStarted"] = started
	payload["dispatched"] = started
	if runID = strings.TrimSpace(runID); runID != "" {
		payload["runId"] = runID
	}
	return payload
}

func (executor Executor) dispatchLifecycleHook(event string, input specialistAccountingInput, payload map[string]any) {
	if executor.LifecycleHooks == nil || executor.LifecycleHooks.Dispatch == nil {
		return
	}
	hookPayload := make(map[string]any, len(payload)+1)
	for key, value := range payload {
		hookPayload[key] = value
	}
	hookPayload["event"] = event
	executor.LifecycleHooks.Dispatch(context.Background(), event, strings.TrimSpace(input.SpecialistName), hookPayload)
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

// taskStreamSummary builds the stream summary for a finished background task
// from its output file. Every reaper (Task.onExit, TaskOutput, TaskStop) derives
// its runId this way, so the durable stop event and stop-hook claim agree on the
// run identity regardless of which path observed the task first.
func taskStreamSummary(task background.Task) StreamResult {
	summary := StreamResult{ExitCode: task.ExitCode}
	if data, err := os.ReadFile(task.OutputFile); err == nil {
		summary, _ = summarizeTaskData(string(data), task.ExitCode)
	}
	return summary
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
			if specialistPayloadMatches(payload, childSessionID, runID) {
				return true
			}
		}
		return false
	}
}

// specialistPayloadMatches applies the shared "same specialist stop" identity to
// one event payload. The accounting dedup (specialistEventMatcher) and the
// durable stop-hook claim both use it, so the audit event and the hook side
// effect can never disagree about what counts as a repeat. An already-recorded
// event with NO runId is a catch-all for this child (e.g. the immediate stop
// written when a PID couldn't be registered) and matches a later event that
// carries a runId - otherwise the same child gets two stop/usage events. A query
// without a runId likewise matches any recorded run.
func specialistPayloadMatches(payload map[string]any, childSessionID, runID string) bool {
	if specialistPayloadString(payload, "source") != specialistAccountingSource {
		return false
	}
	if specialistPayloadString(payload, "childSessionId") != childSessionID && specialistPayloadString(payload, "taskId") != childSessionID {
		return false
	}
	existingRunID := specialistPayloadString(payload, "runId")
	return runID == "" || existingRunID == "" || existingRunID == runID
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

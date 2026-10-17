package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/trace"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

type taskStatus string

const (
	taskStatusActive     taskStatus = "active"
	taskStatusComplete   taskStatus = "complete"
	taskStatusIncomplete taskStatus = "incomplete"
)

type taskPlanParity string

const (
	taskPlanParityUnknown  taskPlanParity = "unknown"
	taskPlanParityMatch    taskPlanParity = "match"
	taskPlanParityMismatch taskPlanParity = "mismatch"
)

type taskPlanItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
	Notes   string `json:"notes,omitempty"`
}

type taskPlanState struct {
	Items      []taskPlanItem `json:"items,omitempty"`
	Pending    int            `json:"pending"`
	InProgress int            `json:"in_progress"`
	Completed  int            `json:"completed"`
	Failed     int            `json:"failed"`
}

type taskToolState struct {
	Succeeded int `json:"succeeded"`
	Failed    int `json:"failed"`
}

type taskVerificationState struct {
	Passed      int     `json:"passed"`
	Failed      int     `json:"failed"`
	LastOutcome Outcome `json:"last_outcome,omitempty"`
}

type taskFailureState struct {
	Key     string `json:"key,omitempty"`
	Tool    string `json:"tool"`
	Command string `json:"command,omitempty"`
	Summary string `json:"summary"`
}

type taskApprovalState struct {
	Tool     string                   `json:"tool"`
	Decision PermissionDecisionAction `json:"decision,omitempty"`
	Scope    string                   `json:"scope,omitempty"`
	Reason   string                   `json:"reason,omitempty"`
}

type taskArtifactState struct {
	Tool string `json:"tool"`
	Path string `json:"path"`
}

type taskStateSnapshot struct {
	Revision            int                   `json:"revision"`
	Objective           string                `json:"objective"`
	Status              taskStatus            `json:"status"`
	Plan                taskPlanState         `json:"plan"`
	Tools               taskToolState         `json:"tools"`
	Verification        taskVerificationState `json:"verification"`
	ChangedFiles        []string              `json:"changed_files,omitempty"`
	Constraints         []string              `json:"constraints,omitempty"`
	UnresolvedFailures  []taskFailureState    `json:"unresolved_failures,omitempty"`
	Approvals           []taskApprovalState   `json:"approvals,omitempty"`
	Artifacts           []taskArtifactState   `json:"artifacts,omitempty"`
	ResolvedFailureKeys []string              `json:"-"`
	CompletionDecision  CompletionDecision    `json:"completion_decision,omitempty"`
	CompletionReason    string                `json:"completion_reason,omitempty"`
	PlanParity          taskPlanParity        `json:"plan_parity"`
}

type taskStateEventKind int

const (
	taskStateEventPlan taskStateEventKind = iota + 1
	taskStateEventToolResult
	taskStateEventVerification
	taskStateEventCompletion
	taskStateEventPermission
)

type taskStateEvent struct {
	kind         taskStateEventKind
	arguments    string
	toolResult   ToolResult
	verification Outcome
	completion   completionEvaluation
	permission   PermissionEvent
}

// taskState is a deterministic projection of facts the loop already observes.
// It does not initiate work or replace the transcript; consumers must check
// transcript parity before using its compact representation.
type taskState struct {
	snapshotValue taskStateSnapshot
	changedFiles  map[string]struct{}
	recorder      *trace.Recorder
	planObserved  bool
}

func newTaskState(objective string, recorder *trace.Recorder) *taskState {
	state := &taskState{
		snapshotValue: taskStateSnapshot{
			Objective:  strings.TrimSpace(objective),
			Status:     taskStatusActive,
			PlanParity: taskPlanParityUnknown,
		},
		changedFiles: map[string]struct{}{},
	}
	state.snapshotValue.Constraints = extractExplicitConstraints(objective)
	state.recorder = recorder
	return state
}

func (state *taskState) observe(event taskStateEvent) bool {
	if state == nil {
		return false
	}
	changed := false
	switch event.kind {
	case taskStateEventPlan:
		plan, ok := parseTaskPlan(event.arguments)
		if !ok {
			return false
		}
		state.snapshotValue.Plan = summarizeTaskPlan(plan)
		state.planObserved = true
		state.markActive()
		changed = true
	case taskStateEventToolResult:
		state.markActive()
		if event.toolResult.Status == tools.StatusOK {
			state.snapshotValue.Tools.Succeeded++
		} else {
			state.snapshotValue.Tools.Failed++
		}
		for _, path := range event.toolResult.ChangedFiles {
			path = strings.TrimSpace(path)
			if path != "" {
				state.changedFiles[path] = struct{}{}
			}
		}
		state.snapshotValue.ChangedFiles = sortedKeys(state.changedFiles)
		state.observeToolEvidence(event.arguments, event.toolResult)
		changed = true
	case taskStateEventVerification:
		if event.verification == OutcomeDisabled {
			return false
		}
		state.markActive()
		state.snapshotValue.Verification.LastOutcome = event.verification
		switch event.verification {
		case OutcomePassed:
			state.snapshotValue.Verification.Passed++
		case OutcomeCorrecting, OutcomeReported, OutcomeAborted:
			state.snapshotValue.Verification.Failed++
		}
		changed = true
	case taskStateEventCompletion:
		state.snapshotValue.CompletionDecision = event.completion.Decision
		state.snapshotValue.CompletionReason = event.completion.Reason
		switch event.completion.Decision {
		case CompletionComplete:
			state.snapshotValue.Status = taskStatusComplete
		case CompletionIncomplete:
			state.snapshotValue.Status = taskStatusIncomplete
		default:
			state.snapshotValue.Status = taskStatusActive
		}
		changed = true
	case taskStateEventPermission:
		state.markActive()
		state.observePermission(event.permission)
		changed = true
	}
	if changed {
		state.snapshotValue.Revision++
		if event.kind != taskStateEventToolResult {
			state.emit()
		}
	}
	return changed
}

func (state *taskState) markActive() {
	state.snapshotValue.Status = taskStatusActive
	state.snapshotValue.CompletionDecision = ""
	state.snapshotValue.CompletionReason = ""
}

func (state *taskState) snapshot() taskStateSnapshot {
	if state == nil {
		return taskStateSnapshot{}
	}
	snapshot := state.snapshotValue
	snapshot.Plan.Items = append([]taskPlanItem(nil), state.snapshotValue.Plan.Items...)
	snapshot.ChangedFiles = append([]string(nil), state.snapshotValue.ChangedFiles...)
	snapshot.Constraints = append([]string(nil), state.snapshotValue.Constraints...)
	snapshot.UnresolvedFailures = append([]taskFailureState(nil), state.snapshotValue.UnresolvedFailures...)
	snapshot.Approvals = append([]taskApprovalState(nil), state.snapshotValue.Approvals...)
	snapshot.Artifacts = append([]taskArtifactState(nil), state.snapshotValue.Artifacts...)
	snapshot.ResolvedFailureKeys = append([]string(nil), state.snapshotValue.ResolvedFailureKeys...)
	return snapshot
}

const (
	maxTaskEvidenceEntries = 8
	maxTaskEvidenceBytes   = 320
	maxTaskConstraints     = 10
)

func (state *taskState) observeToolEvidence(arguments string, result ToolResult) {
	command := capTaskEvidence(commandFromArguments(arguments))
	key := taskFailureKey(result.Name, command, arguments)
	if result.Status == tools.StatusOK {
		if hasTaskFailure(state.snapshotValue.UnresolvedFailures, key) {
			state.snapshotValue.UnresolvedFailures = removeTaskFailure(state.snapshotValue.UnresolvedFailures, key)
			state.snapshotValue.ResolvedFailureKeys = appendBoundedUnique(state.snapshotValue.ResolvedFailureKeys, key, maxTaskEvidenceEntries)
		}
	} else {
		state.snapshotValue.ResolvedFailureKeys = removeString(state.snapshotValue.ResolvedFailureKeys, key)
		state.snapshotValue.UnresolvedFailures = removeTaskFailure(state.snapshotValue.UnresolvedFailures, key)
		state.snapshotValue.UnresolvedFailures = appendBounded(state.snapshotValue.UnresolvedFailures, taskFailureState{
			Key: key, Tool: result.Name, Command: command, Summary: capTaskEvidence(firstLine(result.Output)),
		}, maxTaskEvidenceEntries)
	}
	for _, metaKey := range []string{"spill_path", "artifact_path"} {
		if path := strings.TrimSpace(result.Meta[metaKey]); path != "" {
			state.snapshotValue.Artifacts = appendBoundedUnique(state.snapshotValue.Artifacts,
				taskArtifactState{Tool: result.Name, Path: capTaskEvidence(path)}, maxTaskEvidenceEntries)
		}
	}
}

func taskFailureKey(toolName, command, arguments string) string {
	identity := command
	if identity == "" {
		canonical := []byte(strings.TrimSpace(arguments))
		var value any
		if json.Unmarshal(canonical, &value) == nil {
			if encoded, err := json.Marshal(value); err == nil {
				canonical = encoded
			}
		}
		digest := sha256.Sum256(canonical)
		identity = fmt.Sprintf("args:%x", digest[:8])
	}
	return strings.TrimSpace(toolName) + "\x00" + identity
}

func (state *taskState) observePermission(event PermissionEvent) {
	decision := event.DecisionAction
	if decision == "" {
		decision = PermissionDecisionAction(event.Action)
	}
	state.snapshotValue.Approvals = appendBoundedUnique(state.snapshotValue.Approvals, taskApprovalState{
		Tool: event.ToolName, Decision: decision, Scope: capTaskEvidence(event.Scope),
		Reason: capTaskEvidence(firstNonEmptyString(event.DecisionReason, event.Reason)),
	}, maxTaskEvidenceEntries)
}

func commandFromArguments(arguments string) string {
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(arguments)), &object) != nil {
		return ""
	}
	for _, key := range []string{"cmd", "command", "script", "shell"} {
		var command string
		if raw, ok := object[key]; ok && json.Unmarshal(raw, &command) == nil {
			return strings.TrimSpace(command)
		}
	}
	return ""
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		value = value[:index]
	}
	return value
}

func capTaskEvidence(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxTaskEvidenceBytes {
		return value
	}
	limit := maxTaskEvidenceBytes - 3
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return strings.TrimSpace(value[:limit]) + "..."
}

var explicitConstraintPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bprefer(?:s|red|ring)?\s+\w`),
	regexp.MustCompile(`(?i)\bdon'?t want\b`),
	regexp.MustCompile(`(?i)\balways (?:use|do|run|prefer|keep|make|format|write|add|set|put|prefix|start|include|append)\b`),
	regexp.MustCompile(`(?i)\bnever (?:use|do|run|push|commit|write|ignore|add|set|put|remove|delete|include|deploy)\b`),
	regexp.MustCompile(`(?i)\bplease (?:use|avoid|keep|make|don'?t|do not|format|write)\b`),
	regexp.MustCompile(`(?i)\b(?:style|format|language|naming)\s*[:=]\s*\S`),
}

func extractExplicitConstraints(text string) []string {
	var constraints []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 5 || len(line) > 200 || strings.HasSuffix(line, "?") || strings.Contains(line, "?...") {
			continue
		}
		for _, pattern := range explicitConstraintPatterns {
			if pattern.MatchString(line) {
				constraints = appendBoundedUnique(constraints, line, maxTaskConstraints)
				break
			}
		}
		if len(constraints) == maxTaskConstraints {
			break
		}
	}
	return constraints
}

func appendBounded[T any](values []T, value T, limit int) []T {
	values = append(values, value)
	if len(values) > limit {
		values = values[len(values)-limit:]
	}
	return values
}

func appendBoundedUnique[T comparable](values []T, value T, limit int) []T {
	filtered := values[:0]
	for _, existing := range values {
		if existing != value {
			filtered = append(filtered, existing)
		}
	}
	return appendBounded(filtered, value, limit)
}

func hasTaskFailure(values []taskFailureState, key string) bool {
	for _, value := range values {
		if value.Key == key {
			return true
		}
	}
	return false
}

func removeTaskFailure(values []taskFailureState, key string) []taskFailureState {
	out := values[:0]
	for _, value := range values {
		if value.Key != key {
			out = append(out, value)
		}
	}
	return out
}

func removeString(values []string, target string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != target {
			out = append(out, value)
		}
	}
	return out
}

// observePlanParity compares only the latest plan projection with the plan tool
// calls still present in messages. It mutates the snapshot and emits when the
// parity value changes; objective, tool, and verification fields are not part of
// this comparison.
func (state *taskState) observePlanParity(messages []zeroruntime.Message) taskPlanParity {
	if state == nil {
		return taskPlanParityUnknown
	}
	transcriptPlan, found, valid := latestTaskPlan(messages)
	tracked := state.snapshotValue.Plan.Items
	parity := taskPlanParityMatch
	if !valid || found != state.planObserved || (found && !reflect.DeepEqual(transcriptPlan, tracked)) {
		parity = taskPlanParityMismatch
	}
	if state.snapshotValue.PlanParity != parity {
		state.snapshotValue.PlanParity = parity
		state.snapshotValue.Revision++
		state.emit()
	}
	return parity
}

type completionContext struct {
	Objective             string
	PlanPending           bool
	PlanMatchesTranscript bool
}

func (state *taskState) completionContext(messages []zeroruntime.Message, transcriptPlanPending bool) completionContext {
	context := completionContext{PlanPending: transcriptPlanPending}
	if state == nil {
		return context
	}
	context.Objective = state.snapshotValue.Objective
	context.PlanMatchesTranscript = state.observePlanParity(messages) == taskPlanParityMatch
	if context.PlanMatchesTranscript {
		context.PlanPending = state.snapshotValue.Plan.Pending+state.snapshotValue.Plan.InProgress > 0
	}
	return context
}

func (state *taskState) snapshotForCompaction(messages []zeroruntime.Message) *taskStateSnapshot {
	if state == nil {
		return nil
	}
	state.observePlanParity(messages)
	snapshot := state.snapshot()
	return &snapshot
}

func (state *taskState) emit() {
	if state == nil || state.recorder == nil {
		return
	}
	snapshot := state.snapshotValue
	state.recorder.EmitTaskState(trace.TaskStateEvent{
		Revision:            snapshot.Revision,
		Status:              string(snapshot.Status),
		PlanPending:         snapshot.Plan.Pending,
		PlanInProgress:      snapshot.Plan.InProgress,
		PlanCompleted:       snapshot.Plan.Completed,
		PlanFailed:          snapshot.Plan.Failed,
		ToolsSucceeded:      snapshot.Tools.Succeeded,
		ToolsFailed:         snapshot.Tools.Failed,
		VerificationPassed:  snapshot.Verification.Passed,
		VerificationFailed:  snapshot.Verification.Failed,
		VerificationOutcome: string(snapshot.Verification.LastOutcome),
		ChangedFileCount:    len(snapshot.ChangedFiles),
		CompletionDecision:  string(snapshot.CompletionDecision),
		PlanParity:          string(snapshot.PlanParity),
	})
}

func parseTaskPlan(arguments string) ([]taskPlanItem, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(arguments)), &object) != nil {
		return nil, false
	}
	rawPlan, ok := object["plan"]
	if !ok || string(rawPlan) == "null" {
		return nil, false
	}
	var parsed []struct {
		ID      *string `json:"id"`
		Content string  `json:"content"`
		Status  string  `json:"status"`
		Notes   string  `json:"notes"`
	}
	if json.Unmarshal(rawPlan, &parsed) != nil {
		return nil, false
	}
	plan := make([]taskPlanItem, 0, len(parsed))
	for _, raw := range parsed {
		if raw.Content == "" {
			return nil, false
		}
		plan = append(plan, taskPlanItem{
			Content: raw.Content,
			Status:  normalizeTaskPlanStatus(raw.Status),
			Notes:   raw.Notes,
		})
	}
	lastInProgress := -1
	for index := range plan {
		if plan[index].Status == "in_progress" {
			if lastInProgress >= 0 {
				plan[lastInProgress].Status = "completed"
			}
			lastInProgress = index
		}
	}
	if len(plan) == 0 {
		return nil, true
	}
	return plan, true
}

func normalizeTaskPlanStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "complete", "done", "finished", "resolved", "✓", "x", "[x]":
		return "completed"
	case "in_progress", "in-progress", "inprogress", "in progress", "active", "doing", "started", "current", "wip", "ongoing", "running":
		return "in_progress"
	case "failed", "fail", "error", "errored", "blocked", "cancelled", "canceled", "abandoned", "skipped":
		return "failed"
	default:
		return "pending"
	}
}

func summarizeTaskPlan(items []taskPlanItem) taskPlanState {
	plan := taskPlanState{Items: append([]taskPlanItem(nil), items...)}
	for _, item := range items {
		switch item.Status {
		case "completed":
			plan.Completed++
		case "in_progress":
			plan.InProgress++
		case "failed":
			plan.Failed++
		default:
			plan.Pending++
		}
	}
	return plan
}

func latestTaskPlan(messages []zeroruntime.Message) ([]taskPlanItem, bool, bool) {
	for i := len(messages) - 1; i >= 0; i-- {
		for j := len(messages[i].ToolCalls) - 1; j >= 0; j-- {
			call := messages[i].ToolCalls[j]
			if call.Name != planToolName {
				continue
			}
			plan, ok := parseTaskPlan(call.Arguments)
			return plan, true, ok
		}
	}
	return nil, false, true
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	// Paths are evidence, not chronology. Stable ordering makes replay snapshots
	// deterministic even if events originate from parallel read batches later.
	sort.Strings(keys)
	return keys
}

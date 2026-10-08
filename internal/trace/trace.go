// Package trace records per-turn timing for a Zero agent run.
//
// Tracing is opt-in. A *Recorder is attached to agent.Options and threaded
// through the run via context (see FromContext / WithContext). When the
// recorder is nil, every stamp is a no-op and the agent loop, providers, and
// tools are byte-identical to an untraced run.
//
// The emitted NDJSON is compatible with internal/agenteval's trace contract:
// one JSON object per line carrying a "type" (and usually "name") field, so
// agenteval.ParseTraceEventKeys / MissingTraceEvents can validate a run.
//
// Attribution model. Each stamp records a wall interval [start, end]. Spans
// that run concurrently or are nested (e.g. provider_connect inside generation,
// permission_wait inside tool_execution) are NOT summed into each other. The
// recorder derives a parent for each span by interval containment and computes
// per-span exclusive time (duration minus the union of its children's
// intervals). AttributedDuration is the sum of top-level (non-nested) spans —
// the time accounted for without double-counting. Coverage is the fraction of
// wall covered by the union of all span intervals; it is the honest "≥95% of
// wall accounted for" metric and never exceeds 1.
package trace

import "time"

// Span names. These are the "name" keys emitted in the NDJSON event stream
// (e.g. {"type":"span","name":"generation","duration_ms":123.4}) and the
// vocabulary a run's wall time is attributed to. Only names that are actually
// stamped appear here: phases without a real stamp are omitted so readers do
// not trust empty categories. RequiredEventKeys lists the subset guaranteed in
// any successful model turn; OptionalEventKeys lists the rest.
const (
	SpanToolPartition   = "tool_partition"   // partitioning the tool set for the prompt
	SpanProviderConnect = "provider_connect" // client.Do in the provider seam
	SpanProviderQueue   = "provider_queue"   // pre-send OAuth/token resolve
	SpanProviderPrewarm = "provider_prewarm" // best-effort turn-session prewarm probe
	SpanGeneration      = "generation"       // streaming a model completion
	SpanToolExecution   = "tool_execution"   // executing tool calls
	SpanPermissionWait  = "permission_wait"  // waiting on a permission prompt
	SpanVerification    = "verification"     // self-correct verify pass
	SpanCompaction      = "compaction"       // context compaction
)

// Counter names. Emitted as {"type":"counter","name":"tool_calls","value":7}.
const (
	CounterModelRequests     = "model_requests"
	CounterToolCalls         = "tool_calls"
	CounterRetryCount        = "retry_count"
	CounterReconnectCount    = "reconnect_count"
	CounterCompactionCount   = "compaction_count"
	CounterCompletionNudges  = "completion_nudges"
	CounterAcceptanceChecks  = "acceptance_checks"
	CounterPollingTurn       = "polling_turn"
	CounterModelSwitches     = "model_switches"
	CounterInputTokens       = "input_tokens"
	CounterCachedInputTokens = "cached_input_tokens"
	CounterCacheWriteTokens  = "cache_write_tokens"
	CounterOutputTokens      = "output_tokens"
	CounterPrewarmAttempts   = "prewarm_attempts"
	CounterPrefixStable      = "prefix_stable"
	CounterPrefixDrift       = "prefix_drift"
	// CounterPostureEscalations counts one-shot in-run posture escalations by
	// the execution-profile controller. Deliberately distinct from
	// model_switches: a posture escalation changes loop policy knobs, never the
	// model or session.
	CounterPostureEscalations = "posture_escalations"
)

const (
	CounterResponseChainReused   = "response_chain_reused"
	CounterResponseChainReset    = "response_chain_reset"
	CounterResponsesHTTPFallback = "responses_http_fallback"
)

// Span is one named wall interval attributed to part of a run. Each stamp is
// its own entry (spans are not merged by name): a run that streams two model
// completions has two generation entries, which lets the recorder derive
// parent/child nesting by interval containment and compute exclusive time.
//
// Duration is the inclusive wall time (End - Start). Exclusive is Duration
// minus the union of this span's children's intervals — the time uniquely
// attributable to this phase rather than to a nested sub-phase. Parent is the
// name of the tightest containing span, or "" for a top-level span.
type Span struct {
	Name      string        `json:"name"`
	Start     time.Time     `json:"start,omitempty"`
	End       time.Time     `json:"end,omitempty"`
	Duration  time.Duration `json:"duration"`
	Parent    string        `json:"parent,omitempty"`
	Exclusive time.Duration `json:"exclusive,omitempty"`
}

// Counter is a named integer accumulated during a run (counts and token totals).
type Counter struct {
	Name  string `json:"name"`
	Value int64  `json:"value"`
}

// OutputBudgetEvent is compact, content-free metadata for one tool result's
// output budgeting decision. It deliberately carries no output text, paths, or
// arguments so tracing cannot become a secret-bearing side channel.
type OutputBudgetEvent struct {
	Tool                    string `json:"tool"`
	Category                string `json:"category"`
	OriginalBytes           int    `json:"original_bytes"`
	RetainedBytes           int    `json:"retained_bytes"`
	EstimatedOriginalTokens int    `json:"estimated_original_tokens"`
	EstimatedRetainedTokens int    `json:"estimated_retained_tokens"`
	Truncated               bool   `json:"truncated"`
	Reason                  string `json:"reason,omitempty"`
	SpillCreated            bool   `json:"spill_created"`
}

// TaskStateEvent is a content-free snapshot of the run's structured task
// projection. Objective text, plan content, tool output, file paths, and
// confirmable content fingerprints are deliberately excluded so tracing cannot
// become a secret-bearing side channel.
type TaskStateEvent struct {
	Revision            int    `json:"revision"`
	Status              string `json:"status"`
	PlanPending         int    `json:"plan_pending"`
	PlanInProgress      int    `json:"plan_in_progress"`
	PlanCompleted       int    `json:"plan_completed"`
	PlanFailed          int    `json:"plan_failed"`
	ToolsSucceeded      int    `json:"tools_succeeded"`
	ToolsFailed         int    `json:"tools_failed"`
	VerificationPassed  int    `json:"verification_passed"`
	VerificationFailed  int    `json:"verification_failed"`
	VerificationOutcome string `json:"verification_outcome,omitempty"`
	ChangedFileCount    int    `json:"changed_file_count"`
	CompletionDecision  string `json:"completion_decision,omitempty"`
	PlanParity          string `json:"plan_parity"`
}

// TurnTrace is the finished record for one agent.Run. It is the value
// emitters serialize; it is not mutated after Finish returns a snapshot.
type TurnTrace struct {
	SessionID           string              `json:"session_id"`
	RunID               string              `json:"run_id"`
	Profile             string              `json:"profile,omitempty"`
	StartedAt           time.Time           `json:"started_at"`
	FirstVisibleEventAt time.Time           `json:"first_visible_event_at,omitempty"`
	FirstUsefulActionAt time.Time           `json:"first_useful_action_at,omitempty"`
	FirstTokenAt        time.Time           `json:"first_token_at,omitempty"`
	CompletedAt         time.Time           `json:"completed_at"`
	Spans               []Span              `json:"spans"`
	Counters            []Counter           `json:"counters"`
	PrefixHashes        []PrefixHash        `json:"prefix_hashes,omitempty"`
	OutputBudgets       []OutputBudgetEvent `json:"output_budgets,omitempty"`
	TaskStates          []TaskStateEvent    `json:"task_states,omitempty"`
}

// PrefixHash is one fingerprint of the prompt prefix emitted by an agent run.
// SystemPromptHash covers the exact joined system-message content, including
// every dynamic section. ToolsHash covers tool names and descriptions in their
// emitted order; SchemaHash covers their canonical parameter schemas in that
// same order. CompletePrefixHash combines those three provider-visible parts.
// The remaining hashes retain narrower diagnostic boundaries so a trace can
// identify common sources of system-prompt drift. All hashes are hex-encoded
// SHA-256 and emitted as a "prefix_hash" NDJSON event.
type PrefixHash struct {
	SystemPromptHash       string `json:"system_prompt"`
	BaseInstructionsHash   string `json:"base_instructions"`
	ConfirmationPolicyHash string `json:"confirmation_policy"`
	ProjectContextHash     string `json:"project_context"`
	SkillsHash             string `json:"skills"`
	ToolsHash              string `json:"tools"`
	SchemaHash             string `json:"schema"`
	CompletePrefixHash     string `json:"complete_prefix"`
	InvalidationReason     string `json:"invalidation_reason,omitempty"`
}

// WallDuration is the total traced wall time of the run.
func (t *TurnTrace) WallDuration() time.Duration {
	if t == nil || t.CompletedAt.IsZero() || t.StartedAt.IsZero() {
		return 0
	}
	return t.CompletedAt.Sub(t.StartedAt)
}

// AttributedDuration is the sum of top-level (non-nested) span durations — the
// wall time accounted for without double-counting nested or concurrent sub-
// phases. For a well-instrumented sequential run this is close to WallDuration
// (gaps are uninstrumented regions); it does not inflate from nested children.
func (t *TurnTrace) AttributedDuration() time.Duration {
	if t == nil {
		return 0
	}
	var total time.Duration
	for _, span := range t.Spans {
		if span.Parent == "" {
			total += span.Duration
		}
	}
	return total
}

// Coverage is the fraction of WallDuration covered by the union of all span
// intervals, capped at 1.0. This is the honest "what fraction of wall time did
// we account for" metric: overlapping or nested spans do not push it above 1.
// Returns 0 when wall is zero or no span has a usable interval.
func (t *TurnTrace) Coverage() float64 {
	if t == nil {
		return 0
	}
	wall := t.WallDuration()
	if wall <= 0 {
		return 0
	}
	union := unionIntervalDuration(t.Spans)
	if union <= 0 {
		return 0
	}
	ratio := float64(union) / float64(wall)
	if ratio > 1 {
		ratio = 1
	}
	return ratio
}

// AttributionRatio is Coverage — the fraction of wall covered by span
// intervals. A run is considered well-attributed when this is >= 0.95. It
// never exceeds 1, so it is a sound denominator for "share of attributed time".
func (t *TurnTrace) AttributionRatio() float64 {
	return t.Coverage()
}

// Span returns the total inclusive duration recorded for name across all its
// occurrences, or zero if absent.
func (t *TurnTrace) Span(name string) time.Duration {
	if t == nil {
		return 0
	}
	var total time.Duration
	for _, span := range t.Spans {
		if span.Name == name {
			total += span.Duration
		}
	}
	return total
}

// Exclusive returns the total exclusive duration recorded for name across all
// its occurrences (each occurrence's Duration minus its children), or zero.
func (t *TurnTrace) Exclusive(name string) time.Duration {
	if t == nil {
		return 0
	}
	var total time.Duration
	for _, span := range t.Spans {
		if span.Name == name {
			total += span.Exclusive
		}
	}
	return total
}

// Counter returns the value recorded for name, or zero if absent.
func (t *TurnTrace) Counter(name string) int64 {
	if t == nil {
		return 0
	}
	for _, c := range t.Counters {
		if c.Name == name {
			return c.Value
		}
	}
	return 0
}

// RequiredEventKeys is the set of span/counter event keys guaranteed in any
// successful single-turn traced run, regardless of provider or auth path.
// Tests assert via agenteval.MissingTraceEvents that a run produces all of
// these. Phases that are conditional (tools, permission, compaction,
// verification) are in OptionalEventKeys, not here, so a healthy short trace
// does not false-fail.
func RequiredEventKeys() []string {
	return []string{
		"span:" + SpanToolPartition,
		"span:" + SpanGeneration,
		"span:" + SpanProviderConnect,
		"counter:" + CounterModelRequests,
		"counter:" + CounterInputTokens,
		"counter:" + CounterOutputTokens,
		"trace:run",
	}
}

// OptionalEventKeys are event keys a traced run MAY emit depending on what the
// run does (it may call no tools, need no permission, never compact, etc.).
// Callers must not treat their absence as a failure.
func OptionalEventKeys() []string {
	return []string{
		"span:" + SpanProviderQueue,
		"span:" + SpanToolExecution,
		"span:" + SpanPermissionWait,
		"span:" + SpanCompaction,
		"span:" + SpanVerification,
		"counter:" + CounterToolCalls,
		"counter:" + CounterCachedInputTokens,
		"counter:" + CounterCacheWriteTokens,
		"counter:" + CounterRetryCount,
		"counter:" + CounterReconnectCount,
		"counter:" + CounterCompactionCount,
		"counter:" + CounterCompletionNudges,
		"counter:" + CounterAcceptanceChecks,
		"counter:" + CounterPollingTurn,
		"counter:" + CounterModelSwitches,
		"span:" + SpanProviderPrewarm,
		"counter:" + CounterPrewarmAttempts,
		"counter:" + CounterPrefixStable,
		"counter:" + CounterPrefixDrift,
		"counter:" + CounterResponseChainReused,
		"counter:" + CounterResponseChainReset,
		"counter:" + CounterResponsesHTTPFallback,
		"counter:" + CounterPostureEscalations,
		"event:prefix_hash",
		"task_state",
	}
}

package agent

import (
	"context"

	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/hooks"
	"github.com/Gitlawb/zero/internal/sandbox"
	"github.com/Gitlawb/zero/internal/streamjson"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/trace"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

type Message = zeroruntime.Message
type Provider = zeroruntime.Provider
type ToolCall = zeroruntime.ToolCall
type Usage = zeroruntime.Usage

type PermissionMode string
type PermissionAction string
type PermissionDecisionAction string

const (
	PermissionModeAuto      PermissionMode = "auto"
	PermissionModeAsk       PermissionMode = "ask"
	PermissionModeUnsafe    PermissionMode = "unsafe"
	PermissionModeSpecDraft PermissionMode = "spec-draft"
	// PermissionModePlan is an interactive, read-only planning mode. It applies
	// to the CURRENT session (unlike spec-draft, which drafts in a separate
	// session): the agent may inspect the workspace and shape the plan with
	// update_plan/ask_user, but no mutating tool is advertised, so it cannot
	// write files, run shell, or implement while planning. Entry points:
	// the TUI's /plan on (exit with /plan off, which restores whatever mode
	// was active before), `zero exec --plan`, and the ACP session mode
	// selector ("plan").
	PermissionModePlan PermissionMode = "plan"
	// PermissionModeMemberAuto is a headless mode for swarm/specialist MEMBERS: it
	// advertises the in-workspace mutators a member needs to build (write/edit +
	// shell) on top of the Auto set, while the sandbox engine still gates them at
	// call time — in-workspace writes and sandbox-backed shell auto-allow, but
	// out-of-workspace writes, network, and destructive commands still prompt (and
	// a headless member has no approver, so they are denied). It normalizes to Auto
	// everywhere except ToolAdvertised, so authority is never widened beyond what an
	// interactive auto agent already has inside the sandbox.
	PermissionModeMemberAuto PermissionMode = "member-auto"
)

type StopReason string

const (
	StopReasonSpecReviewRequired StopReason = "spec_review_required"
)

const (
	PermissionActionAllow  PermissionAction = "allow"
	PermissionActionPrompt PermissionAction = "prompt"
	PermissionActionDeny   PermissionAction = "deny"
	PermissionActionCancel PermissionAction = "cancel"
)

const (
	PermissionDecisionAllow             PermissionDecisionAction = "allow"
	PermissionDecisionAllowStrict       PermissionDecisionAction = "allow_with_strict_auto_review"
	PermissionDecisionAllowForSession   PermissionDecisionAction = "allow_for_session"
	PermissionDecisionAllowPrefix       PermissionDecisionAction = "allow_prefix_for_session"
	PermissionDecisionAlwaysAllowPrefix PermissionDecisionAction = "always_allow_prefix"
	PermissionDecisionDeny              PermissionDecisionAction = "deny"
	PermissionDecisionAlwaysAllow       PermissionDecisionAction = "always_allow"
	PermissionDecisionCancel            PermissionDecisionAction = "cancel"
)

type ToolResult struct {
	ToolCallID string
	Name       string
	Status     tools.Status
	Output     string
	// Truncated reports that the tool's model-visible output omitted content.
	// The full result may be recoverable through Meta["spill_path"].
	Truncated bool
	Meta      map[string]string
	// Images the tool produced, delivered to the model as a following user
	// message rather than on this result. See tools.Result.Images.
	Images       []zeroruntime.ImageBlock
	Redacted     bool
	ChangedFiles []string
	FileDiffs    []tools.FileDiff
	// ChangeSummaries are non-selectable generated-tree summaries emitted by
	// command execution; callers must not schedule per-file work from them.
	ChangeSummaries []execution.Change
	Display         tools.Display
	Outcome         tools.ToolOutcome
	// DenialReason categorizes why a tool call was blocked (empty when it ran).
	// It lets a surface distinguish the cause precisely instead of parsing Output.
	DenialReason DenialCategory
	// Risk is the sandbox risk classification of this call, stamped for EXECUTED
	// results so run-policy observers (the execution-profile controller) can see
	// the risk level of an allowed mutation. It mirrors the classification the
	// permission path already computes; denied or canceled results keep the zero
	// value. Pure observation: nothing about permissions or sandboxing changes.
	Risk sandbox.Risk
	// LoadedTools carries the deferred-tool names a tool_search call asked the
	// loop to expose next turn (lifted from Meta["load_tools"]). nil for every
	// ordinary tool result; only tool_search populates it.
	LoadedTools []string
	// RequestedModel is the model id a tool asked the loop to switch to for the
	// rest of the run (lifted from the tool's Meta["escalate_to_model"]). Empty
	// for every normal tool result; the Run loop performs the switch when it is
	// set and Options.ModelSwitcher is wired.
	RequestedModel string
}

// ModelOutput returns the bounded provider-facing result while preserving
// compatibility with synthetic and restored results created before outcomes
// were finalized.
func (result ToolResult) ModelOutput() string {
	if result.Outcome.Finalized() {
		return result.Outcome.ModelView
	}
	return result.Output
}

// HumanDisplay returns the presentation intended for interactive surfaces.
func (result ToolResult) HumanDisplay() tools.Display {
	if result.Outcome.Finalized() {
		return result.Outcome.HumanView
	}
	return result.Display
}

// DenialCategory classifies why a tool call was blocked before it executed.
type DenialCategory string

const (
	DenialNone             DenialCategory = ""
	DenialFiltered         DenialCategory = "filtered"          // tool not enabled for this run
	DenialPermissionDenied DenialCategory = "permission_denied" // approval declined
	DenialApprovalCanceled DenialCategory = "approval_canceled" // approval canceled and run aborted
	DenialSandboxBlock     DenialCategory = "sandbox_block"     // blocked by the sandbox
	DenialHookBlocked      DenialCategory = "hook_blocked"      // vetoed by a beforeTool hook
)

// ProfilePolicy is the loop-facing slice of a selected execution profile.
// The surface (exec flag, TUI command) resolves a named profile into this
// policy; the loop only ever sees the policy, never the catalog.
type ProfilePolicy struct {
	// Name labels trace counters and diagnostics (e.g. "fast"). The trace
	// recorder's profile label is set by the caller at recorder construction.
	Name string
	// Escalate, when non-nil, arms one-shot in-run posture escalation.
	Escalate *PostureEscalation
}

// PostureEscalation describes a one-shot escalation to stricter knob values,
// applied mid-run when an armed trigger fires. Targets are the values the
// selected profile DISPLACED at run start (i.e. "restore the balanced
// posture"), so escalation can never introduce a value that was not already
// valid for this run and model. Zero-valued targets leave that knob untouched.
type PostureEscalation struct {
	// MaxTurns raises the turn ceiling to this value when greater than the
	// ceiling in effect. 0 leaves the ceiling untouched.
	MaxTurns int
	// ReasoningEffort replaces the run's effort when non-empty.
	ReasoningEffort string
	// RestoreDefaultEffort clears the run's effort override (back to the
	// provider/model default) when true. It exists because the displaced value
	// of a profile-filled effort is "" — which as a ReasoningEffort target
	// means "leave untouched" — so restoring the default needs its own signal.
	// Ignored when ReasoningEffort is non-empty.
	RestoreDefaultEffort bool
	// RestoreCompletionGate re-enables RequireCompletionSignal (headless
	// completion semantics) when true.
	RestoreCompletionGate bool

	// Triggers. A zero value disables that signal entirely.
	// OnToolFailureStreak fires when the repeated-failure guard observes a
	// same-tool retriable-failure streak of at least this length.
	OnToolFailureStreak int
	// OnCompletionUncertain fires on the Nth uncertain completion evaluation
	// (continue nudge or semantic check). Headless only: the completion gate
	// never runs interactively.
	OnCompletionUncertain int
	// OnSelfCorrectFailure fires when a post-edit verification cycle reports a
	// failing outcome (correcting, reported, or aborted).
	OnSelfCorrectFailure bool
	// OnRiskyMutation fires when an EXECUTED tool result carries a sandbox risk
	// level at or above this threshold. Empty disables the signal.
	OnRiskyMutation sandbox.RiskLevel
}

type PermissionRequest struct {
	ToolCallID         string                     `json:"toolCallId"`
	ToolName           string                     `json:"name"`
	Action             PermissionAction           `json:"action"`
	Permission         string                     `json:"permission"`
	PermissionMode     PermissionMode             `json:"permissionMode"`
	Autonomy           string                     `json:"autonomy,omitempty"`
	SideEffect         string                     `json:"sideEffect"`
	Reason             string                     `json:"reason,omitempty"`
	Scope              string                     `json:"scope,omitempty"`
	Risk               sandbox.Risk               `json:"risk"`
	Args               map[string]any             `json:"args,omitempty"`
	Block              *sandbox.Block             `json:"block,omitempty"`
	GrantMatched       bool                       `json:"grantMatched,omitempty"`
	Grant              *sandbox.Grant             `json:"grant,omitempty"`
	CommandPrefix      []string                   `json:"commandPrefix,omitempty"`
	AvailableDecisions []PermissionDecisionAction `json:"availableDecisions,omitempty"`
	// PrefixApprovalEscalates reports that approving a command prefix will also
	// run the command OUTSIDE the sandbox, not merely stop asking about it.
	//
	// It exists because that consequence was real but invisible. Approving a
	// prefix rewrites the call to sandbox_permissions: require_escalated, which
	// resolves to a nil engine and genuinely unsandboxed execution, and the
	// engine's own escalation prompt is then satisfied by the approval just
	// given for the sandboxed form. So the operator authorized one thing and got
	// a wider one, having been shown only "allow command prefix for session".
	//
	// The escalation itself is deliberate and guarded: proposedCommandPrefix
	// refuses to offer a prefix while any other segment of the command is not
	// known-safe, precisely so an unreviewed segment cannot ride out of the
	// sandbox on it. What was missing was telling the person deciding, so this
	// surfaces it rather than removing it.
	PrefixApprovalEscalates bool `json:"prefixApprovalEscalates,omitempty"`
}

type PermissionDecision struct {
	Action PermissionDecisionAction `json:"action"`
	Reason string                   `json:"reason,omitempty"`
}

type PermissionEvent struct {
	ToolCallID        string                   `json:"toolCallId"`
	ToolName          string                   `json:"name"`
	Action            PermissionAction         `json:"action"`
	DecisionAction    PermissionDecisionAction `json:"decisionAction,omitempty"`
	Permission        string                   `json:"permission"`
	PermissionGranted bool                     `json:"permissionGranted,omitempty"`
	PermissionMode    PermissionMode           `json:"permissionMode"`
	Autonomy          string                   `json:"autonomy,omitempty"`
	SideEffect        string                   `json:"sideEffect"`
	Reason            string                   `json:"reason,omitempty"`
	Scope             string                   `json:"scope,omitempty"`
	DecisionReason    string                   `json:"decisionReason,omitempty"`
	Risk              sandbox.Risk             `json:"risk"`
	Block             *sandbox.Block           `json:"block,omitempty"`
	GrantMatched      bool                     `json:"grantMatched,omitempty"`
	Grant             *sandbox.Grant           `json:"grant,omitempty"`
	CommandPrefix     []string                 `json:"commandPrefix,omitempty"`
}

// AskUserQuestion is one clarifying question the agent wants answered. Options are
// optional suggested answers an interactive front-end can render as a picker;
// Recommended (when set) is the suggested default — it should match one of Options.
// Header is an optional short tab title for a multi-question prompt (falls back to
// the question text). OptionDescriptions, when present, holds a one-line description
// per option aligned by index to Options (empty string = no description).
type AskUserQuestion struct {
	Question           string   `json:"question"`
	Header             string   `json:"header,omitempty"`
	Options            []string `json:"options,omitempty"`
	OptionDescriptions []string `json:"optionDescriptions,omitempty"`
	Recommended        string   `json:"recommended,omitempty"`
	MultiSelect        bool     `json:"multiSelect,omitempty"`
}

// AskUserRequest is handed to OnAskUser when the model invokes the ask_user tool.
type AskUserRequest struct {
	ToolCallID string            `json:"toolCallId"`
	Header     string            `json:"header,omitempty"`
	Questions  []AskUserQuestion `json:"questions"`
}

// AskUserResponse carries the user's answers back to the loop, one per question.
type AskUserResponse struct {
	Answers []string `json:"answers"`
}

// SpecialistInfo is a one-line summary of a delegatable sub-agent (its name and
// when-to-use description) surfaced to the orchestrator's system prompt so it can
// route work to the right specialist. It is plain data so the agent package needs
// no dependency on internal/specialist.
type SpecialistInfo struct {
	Name      string
	WhenToUse string
}

// SkillInfo is a one-line summary of a reusable, on-demand skill (its name and
// frontmatter description) surfaced to the system prompt so the model can invoke
// the right skill with the skill tool on the first try instead of guessing a name
// and reading the failure. Like SpecialistInfo it is plain data, so the agent
// package needs no dependency on internal/skills.
type SkillInfo struct {
	Name        string
	Description string
}

type Options struct {
	MaxTurns int
	// DeferThreshold activates deferred MCP-tool loading: when the number of
	// deferred-eligible visible tools is >= this value (and it is > 0), their
	// full schemas are withheld and advertised as compact lines via tool_search.
	// 0 (or below the eligible count) keeps every tool eager — byte-identical to
	// the pre-deferral behavior.
	DeferThreshold int
	// Specialists lists the sub-agents the orchestrator may delegate to via the
	// Task tool; when non-empty the system prompt gains a delegation section that
	// names them and nudges the model to offload read-heavy work (search,
	// exploration) so verbose tool output stays out of the main context. It is
	// populated only where the Task tool is actually registered, so an empty slice
	// (the default) reproduces the previous prompt byte-for-byte.
	Specialists []SpecialistInfo
	// Skills lists the reusable skills installed for this run (the default skills
	// dir merged with any plugin skill roots). When non-empty the system prompt
	// gains an <available_skills> block naming them so the model loads the right one
	// via the skill tool on the first try. Empty (the default) reproduces the
	// previous prompt byte-for-byte.
	Skills []SkillInfo
	// Specialist/sub-agent metadata is carried through exec now and consumed by
	// the specialist runtime in later slices.
	SessionID        string
	CallingSessionID string
	CallingToolUseID string
	Tag              string
	Depth            int
	SessionTitle     string
	ProviderName     string
	Model            string
	ReasoningEffort  string
	ServiceTier      string
	Cwd              string
	SystemPrompt     string
	// TransientSystemPrompt adds trusted runtime guidance for this run only.
	// Empty preserves the ordinary system prompt byte-for-byte.
	TransientSystemPrompt string
	// ResponseStyle is the operator-selected reply style from the TUI /style
	// command (e.g. "concise", "explanatory", "review"). It is rendered into the
	// system prompt as a short directive. Empty or "balanced" adds nothing — the
	// prompt is then byte-identical to the pre-style behavior.
	ResponseStyle string
	// Images are optional image attachments to seed onto the initial user turn.
	// nil for text-only runs (the seeded message then carries no images, exactly
	// as before).
	Images []zeroruntime.ImageBlock
	// ContextWindow is the model's maximum input token budget. When > 0 the agent
	// loop compacts long conversations once the estimated size crosses a fraction
	// of this window. 0 DISABLES compaction entirely (every existing caller/test
	// behaves identically).
	ContextWindow int
	// CompactionPreserveLast is how many trailing messages compaction keeps
	// verbatim. <= 0 falls back to defaultCompactionPreserveLast.
	CompactionPreserveLast int
	// Summarizer, when set, lazily builds the provider used for compaction
	// summarization calls — typically a cheap/fast model, since summaries at
	// main-model prices are the single most expensive recurring event in a
	// long run. It receives the main model currently in force and may return
	// (nil, nil) when no dedicated summarizer applies to it (the main model is
	// already the cheap one). Built on the first paid compaction and again
	// after a mid-run model switch. Any failure (build or call) falls back to
	// the run's main provider until the next switch, so a misconfigured
	// summarizer can never break compaction. nil keeps today's behavior.
	Summarizer func(ctx context.Context, mainModelID string) (Provider, error)
	// ContextWindowFor, when set, resolves a model ID to its context window so
	// the compactor can re-derive its threshold after a mid-run model switch
	// (escalate_model). Without it a switch keeps compacting against the
	// original model's window — overflowing a smaller target or over-compacting
	// a larger one. Return <= 0 when the model is unknown (window unchanged).
	ContextWindowFor func(modelID string) int
	Registry         *tools.Registry
	PermissionMode   PermissionMode
	Autonomy         string
	Sandbox          *sandbox.Engine
	// FileTracker records per-session file read/write versions so the write tools
	// can detect a file changed on disk outside Zero since it was last read. nil
	// disables the check. Created once per session and threaded into every tool run.
	FileTracker *tools.FileTracker
	// Hooks, when set, runs configured beforeTool (blocking) and afterTool
	// (advisory) commands around each tool call. nil disables hooks entirely; a
	// dispatcher built from an empty config is also a safe no-op.
	Hooks         *hooks.Dispatcher
	EnabledTools  []string
	DisabledTools []string
	OnText        func(string)
	OnReasoning   func(string)
	OnToolCall    func(ToolCall)
	// OnToolCallStart / OnToolCallDelta stream a tool call's arguments LIVE as the
	// model generates them — OnToolCallStart on open (id, tool name), then
	// OnToolCallDelta for each argument fragment. A surface can render the
	// in-progress call (e.g. a file being written) instead of waiting for
	// OnToolCall, which only fires once the whole call has accumulated. nil no-ops.
	OnToolCallStart     func(id, name string)
	OnToolCallDelta     func(id, fragment string)
	OnPermissionRequest func(context.Context, PermissionRequest) (PermissionDecision, error)
	OnPermission        func(PermissionEvent)
	OnAskUser           func(context.Context, AskUserRequest) (AskUserResponse, error)
	OnToolResult        func(ToolResult)
	OnUsage             func(Usage)
	// OnToolProgress, when set, is called with each stream-json event a
	// specialist child process emits while running. The toolCallID identifies
	// which Task tool call the progress belongs to. nil is a no-op.
	OnToolProgress func(toolCallID string, event streamjson.Event)
	// OnContext, when set, is called for each main agent request with its
	// per-category context budget, including a replacement request after
	// compaction or a stall retry. Internal summarizer requests are excluded so
	// surfaces keep showing the active conversation budget. Opt-in like the other
	// callbacks; nil is a no-op.
	OnContext func(ContextBreakdown)
	// ModelSwitcher, when set, lets a tool escalate the run to a stronger model
	// mid-run: the loop calls it with the requested model id and, on success,
	// swaps the active provider and updates Options.Model for the rest of the
	// run. nil DISABLES escalation entirely (the loop ignores any switch
	// request), so every existing caller is unaffected. A returned error is
	// non-fatal: the run continues on the current model.
	ModelSwitcher func(ctx context.Context, modelID string) (Provider, error)
	// TurnSessionProvider, when set, supplies the turn session the run streams
	// through — the seam an optimized provider session (connection reuse,
	// prewarm, native compaction) plugs into without touching the loop. nil
	// keeps the default: the loop wraps the passed provider in a no-op session
	// whose Stream IS provider.StreamCompletion, so behavior is byte-identical
	// and every existing caller is unaffected.
	TurnSessionProvider zeroruntime.TurnSessionProvider
	// ModelSessionSwitcher, when set, is the target-aware escalation hook: the
	// loop prefers it over ModelSwitcher, and its TurnSessionProvider keeps an
	// optimized session (and its capabilities) across a mid-run model switch.
	// nil falls back to ModelSwitcher, whose bare Provider is wrapped in the
	// default no-op session — today's behavior, unchanged. Same non-fatal error
	// contract as ModelSwitcher: a returned error records a note and the run
	// continues on the current model and session.
	ModelSessionSwitcher func(ctx context.Context, modelID string) (zeroruntime.TurnSessionProvider, error)
	// Profile, when set, arms the execution-profile posture controller for this
	// run (auto-escalation to stricter knob values on failure/uncertainty/risky
	// mutation signals). nil — the default everywhere today — leaves the loop
	// byte-identical: no observation, no escalation, no counters. Same opt-in
	// convention as Trace and SelfCorrect.
	Profile *ProfilePolicy
	// Trace, when set, records per-turn timing for the run: the loop stamps
	// spans (prompt build, generation, tool execution, permission wait,
	// compaction, provider connect) and counters (model requests, tool calls,
	// retries, tokens) into it. nil DISABLES tracing entirely — every stamp is
	// nil-safe and the loop is byte-identical to an untraced run. The caller
	// owns the recorder: Run stamps into it but does not Finish or emit it.
	// A fresh Recorder is required per Run — reusing one across runs merges
	// their spans, counters, and first-event timestamps, and Finish freezes a
	// recorder so no further stamps take.
	Trace *trace.Recorder
	// SelfCorrect, when set, runs a post-edit verify-and-correct cycle after a
	// mutating tool call: it verifies the changed files (LSP diagnostics + project
	// tests) and feeds failures back to the model to fix, bounded by an attempt
	// ceiling and the autonomy gate. nil disables it entirely (the loop is
	// byte-identical to before). One instance per run — it holds attempt state.
	SelfCorrect *SelfCorrector
	// FileDiagnostics, when set, checks files changed by mutating tools for
	// error-severity language diagnostics IN THE BACKGROUND and appends any
	// errors as a nudge before the model's next request — the model still sees
	// an error it introduced at its next decision point, but no tool call ever
	// blocks on the language server (the old inline path stalled every edit on
	// a ≥300ms debounce, 10s cap). Build one with NewFileDiagnostics. nil
	// disables post-edit diagnostics.
	FileDiagnostics func(ctx context.Context, absPath string) string

	// RequireCompletionSignal gates run completion for HEADLESS exec. Without it,
	// any assistant turn that produces text but no tool call is accepted as the
	// final answer. With it, a no-tool-call turn is NOT treated as "done" while
	// work clearly remains — pending update_plan items, or a message that ends on a
	// continuation cue ("…Let me check the config:"). The loop then nudges the
	// model to continue instead, bounded by maxContinueNudges (and still by
	// MaxTurns and the run deadline); if the model keeps stalling, the run
	// finalizes as INCOMPLETE (Result.Incomplete) rather than success. When the
	// run's profile also enables SelfCorrect, an otherwise-complete turn gets one
	// task-grounded semantic check before success; profiles without SelfCorrect
	// add no model call. Default false leaves the loop byte-identical, so the
	// interactive TUI is unaffected.
	RequireCompletionSignal bool

	runPermissions *permissionRunState
}

type Result struct {
	FinalAnswer string
	Turns       int
	Messages    []Message
	StopReason  StopReason
	// FinishReason is the provider's normalized terminal stop reason for the turn
	// that produced FinalAnswer: zeroruntime.FinishReasonLength when the output
	// hit the token cap, FinishReasonContentFilter when it was filtered. Empty for
	// a normal completion.
	FinishReason string
	// Incomplete reports that a headless run (RequireCompletionSignal) stopped with
	// work clearly unfinished: the model ended a turn with no tool call while plan
	// items were pending or the message ended mid-step, the model admitted it
	// guessed / could not meet the objective, and/or it failed a task-grounded
	// acceptance check. Callers map it to a non-success terminal status / exit
	// code. False for every normal completion.
	Incomplete bool
	// IncompleteReason is a short, model-derived explanation of why the run was
	// marked Incomplete (e.g. "pending plan items remain"). Empty when Incomplete
	// is false. Surfaced in logs / run_end so an abandoned run is debuggable.
	IncompleteReason string
}

// TruncationNotice returns a user-facing warning when the final response was
// truncated, or "" for a normal completion. Shared by the CLI and TUI so the
// wording stays consistent.
func (result Result) TruncationNotice() string {
	switch result.FinishReason {
	case zeroruntime.FinishReasonLength:
		return "Response was cut off at the output token limit and may be incomplete. " +
			"Raise the model's max output tokens or ask zero to continue."
	case zeroruntime.FinishReasonContentFilter:
		return "Response was withheld or cut off by the provider's content filter and may be incomplete."
	case "":
		return ""
	default:
		return "Response ended early (" + result.FinishReason + ") and may be incomplete."
	}
}

package tools

import (
	"context"
	"strings"

	"github.com/Twigpine/zero/internal/execution"
	"github.com/Twigpine/zero/internal/sandbox"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

type SideEffect string
type Permission string
type Status string
type SandboxPermissionOverride string

const (
	// SideEffectNone marks a control-only tool that neither reads nor mutates
	// state — no read/write/shell/network/out-of-workspace effect. Examples:
	// tool_search (only reports already-registered tool schemas) and
	// escalate_model (only requests a loop-level model switch).
	SideEffectNone           SideEffect = "none"
	SideEffectRead           SideEffect = "read"
	SideEffectWrite          SideEffect = "write"
	SideEffectShell          SideEffect = "shell"
	SideEffectNetwork        SideEffect = "network"
	SideEffectLocalControl   SideEffect = "local_control"
	SideEffectLocalBrowser   SideEffect = "local_browser"
	SideEffectLocalDesktop   SideEffect = "local_desktop"
	SideEffectLocalTerminal  SideEffect = "local_terminal"
	SideEffectOutOfWorkspace SideEffect = "out_of_workspace"
)

const (
	PermissionAllow  Permission = "allow"
	PermissionPrompt Permission = "prompt"
	PermissionDeny   Permission = "deny"
)

const (
	StatusOK    Status = "ok"
	StatusError Status = "error"
)

const (
	SandboxPermissionsUseDefault                SandboxPermissionOverride = "use_default"
	SandboxPermissionsRequireEscalated          SandboxPermissionOverride = "require_escalated"
	SandboxPermissionsWithAdditionalPermissions SandboxPermissionOverride = "with_additional_permissions"
)

const (
	SandboxLikelyDeniedMeta  = "sandbox_likely_denied"
	SandboxDenialKindMeta    = "sandbox_denial_kind"
	SandboxDenialReasonMeta  = "sandbox_denial_reason"
	SandboxDenialKeywordMeta = "sandbox_denial_keyword"
	// sandboxNoticesMeta transports the plan notices from addSandboxMeta to
	// finalizeToolOutcome, which promotes them onto Result.EnforcementNotices.
	// Kept as metadata as well, because integrations reading the result JSON have
	// no other way to see them.
	sandboxNoticesMeta = "sandbox_notices"
)

const (
	SandboxDenialKindSandbox = "sandbox"
	SandboxDenialKindNetwork = "network"
)

type Safety struct {
	SideEffect SideEffect
	Permission Permission
	Reason     string
	// AdvertiseInAuto allows selected non-allow tools to be visible in auto mode
	// while still requiring the normal permission flow before execution.
	AdvertiseInAuto bool
}

type Schema struct {
	Type                 string                    `json:"type"`
	Properties           map[string]PropertySchema `json:"properties,omitempty"`
	Required             []string                  `json:"required,omitempty"`
	AdditionalProperties bool                      `json:"additionalProperties"`
}

type PropertySchema struct {
	Type        string          `json:"type"`
	Description string          `json:"description,omitempty"`
	Enum        []string        `json:"enum,omitempty"`
	Default     any             `json:"default,omitempty"`
	Items       *PropertySchema `json:"items,omitempty"`
	Minimum     *int            `json:"minimum,omitempty"`
	Maximum     *int            `json:"maximum,omitempty"`
	MinLength   *int            `json:"minLength,omitempty"`
	MaxLength   *int            `json:"maxLength,omitempty"`
	MinItems    *int            `json:"minItems,omitempty"`
	// Properties/Required describe nested object fields (for Type "object" or an
	// object-typed Items).
	Properties map[string]PropertySchema `json:"properties,omitempty"`
	Required   []string                  `json:"required,omitempty"`
}

type Result struct {
	Status          Status
	Output          string
	Truncated       bool
	Meta            map[string]string
	SandboxDecision *sandbox.Decision `json:"-"`
	// ExecutionRequest and ExecutionOutcome are the typed command protocol used
	// by command-oriented tools. Legacy Output and Meta fields remain populated
	// while callers migrate away from parsing text and string metadata.
	ExecutionRequest *execution.Request `json:"-"`
	ExecutionOutcome *execution.Outcome `json:"-"`
	// Images are pictures the tool is handing the model — a screenshot it just
	// captured, a file it was asked to look at. Text-only tools leave it nil.
	//
	// They are NOT delivered on this result's own tool-result message. Every
	// provider drops images there: Anthropic maps a tool result to a tool_result
	// block whose content is a string, Gemini to a functionResponse, and OpenAI
	// guards its image content-parts to the user role. The agent loop therefore
	// emits them as a following user message, which is also the only shape that
	// keeps one tool result per tool call.
	Images []zeroruntime.ImageBlock `json:"-"`
	// EnforcementNotices are least-privilege disclosures about the enforcement
	// actually applied to this command, and they are USER AND MODEL VISIBLE.
	//
	// A separate field rather than text baked into Output, so one canonical
	// result carries it and every surface reads it through the accessors below.
	// The first attempt put this in Meta alongside the sandbox metadata, which
	// looked like the established channel and is not one: nothing in production
	// reads those keys, ModelOutput and HumanDisplay never consult Meta, and the
	// durable history drops it. The disclosure reached nobody.
	EnforcementNotices []string `json:"enforcementNotices,omitempty"`
	// Redacted is set when secret scrubbing altered Output before it left the
	// tool-execution boundary.
	Redacted bool
	// ChangedFiles lists workspace-relative paths a mutating tool wrote;
	// entries under a granted extra write root are absolute, since
	// workspace-relative would be ambiguous there.
	ChangedFiles []string
	// FileDiffs carries exact, bounded before/after text for built-in file
	// mutations. A missing entry means the client must fall back to ChangedFiles
	// rather than inventing a partial diff.
	FileDiffs []FileDiff
	// ChangeSummaries contains bounded generated-tree changes. These are shown
	// in session evidence and the Files panel but are never treated as files to
	// open or diagnose individually.
	ChangeSummaries []execution.Change
	// Display carries a short, structured summary for the TUI / stream.
	Display Display
	// Outcome is the finalized, typed representation produced at the registry
	// seam. ModelView, HumanView, and Artifact deliberately serve different
	// consumers; Output, Display, and spill metadata remain synchronized for
	// compatibility with direct tool callers and persisted sessions.
	Outcome ToolOutcome
	// pendingFileObservation is proposed by read_file and committed only after
	// the final model-visible output boundary confirms the exact content survived.
	pendingFileObservation *pendingFileObservation
}

// ToolOutcome is the canonical post-execution representation of one tool
// result. It separates the bounded provider payload from the human-facing
// presentation and the recoverable output retained outside model context.
type ToolOutcome struct {
	ModelView   string
	HumanView   Display
	Artifact    *ToolArtifact
	Diagnostics OutcomeDiagnostics
	finalized   bool
}

// Finalized reports whether the outcome crossed the registry boundary. Direct
// Tool.Run results deliberately return false and use their legacy fields.
func (outcome ToolOutcome) Finalized() bool {
	return outcome.finalized
}

// ToolArtifact identifies recoverable output saved by the tool boundary.
// CompleteAtBoundary means the artifact contains every redacted byte received
// by that boundary; an underlying process may already have applied its own
// capture limit before producing the result.
type ToolArtifact struct {
	Path               string
	CompleteAtBoundary bool
}

// OutcomeDiagnostics describes how the model-facing representation differs
// from the redacted output received by the registry boundary.
type OutcomeDiagnostics struct {
	Category                string
	OriginalBytes           int
	ModelBytes              int
	EstimatedOriginalTokens int
	EstimatedModelTokens    int
	Truncated               bool
	Redacted                bool
	Reason                  string
}

// BaseModelOutput returns the UNDECORATED provider-facing text: the finalized
// model view, falling back to the legacy field for direct Tool.Run callers that
// have not crossed the registry. It carries no enforcement notices.
//
// Callers that PROJECT a result into another carrier (the agent loop building an
// agent.ToolResult) must copy this, not ModelOutput, and copy the typed notice
// slice alongside it. Storing already-rendered text next to the same notices is
// two representations of one fact with no contract between them, and whichever
// side of the projection loses its finalized outcome renders the disclosure
// twice.
func (result Result) BaseModelOutput() string {
	if result.Outcome.finalized {
		return result.Outcome.ModelView
	}
	return result.Output
}

// BaseDisplay is BaseModelOutput for the presentation half, and carries no
// enforcement notices for the same reason.
func (result Result) BaseDisplay() Display {
	if result.Outcome.finalized {
		return result.Outcome.HumanView
	}
	return result.Display
}

// ModelOutput returns the finalized provider-facing text with the enforcement
// disclosure rendered in front of it. This is the only place the model view is
// decorated.
func (result Result) ModelOutput() string {
	return WithEnforcementNotices(result.BaseModelOutput(), result.EnforcementNotices)
}

// HumanDisplay returns the finalized presentation with the enforcement
// disclosure rendered in front of the summary.
func (result Result) HumanDisplay() Display {
	display := result.BaseDisplay()
	display.Summary = WithEnforcementNotices(display.Summary, result.EnforcementNotices)
	return display
}

// WithEnforcementNotices puts the enforcement disclosure IN FRONT of the text.
//
// PREPENDED, not appended, because the output budget trims from the end: a
// notice at the tail is the first thing a long result loses, and a disclosure
// that survives only on short outputs is not a disclosure. It is also why this
// lives on the accessors rather than at the call sites that build results.
// The previous version wrote it into Result.Meta, and neither ModelOutput nor
// HumanDisplay nor the durable history reads Meta, so it reached nobody at all.
func WithEnforcementNotices(text string, notices []string) string {
	if len(notices) == 0 {
		return text
	}
	joined := strings.TrimSpace(strings.Join(notices, "\n"))
	if joined == "" {
		return text
	}
	if strings.TrimSpace(text) == "" {
		return joined
	}
	return joined + "\n\n" + text
}

// WithoutEnforcementNotices is the inverse, for a reader of STORED text.
//
// A persisted tool result carries its output with the disclosure already
// composed in, and the same notices again as a typed field. A reader that
// rebuilds a result from both has to take the composed copy back off, or the
// accessors above compose it a second time and the disclosure is drawn twice.
// It lives beside WithEnforcementNotices so the two agree on the format by
// construction rather than by a second spelling of it somewhere else.
//
// Text that was never decorated is returned unchanged, so a record whose writer
// stored the undecorated output still round-trips to exactly one disclosure.
func WithoutEnforcementNotices(text string, notices []string) string {
	if len(notices) == 0 {
		return text
	}
	joined := strings.TrimSpace(strings.Join(notices, "\n"))
	if joined == "" {
		return text
	}
	if rest, ok := strings.CutPrefix(text, joined+"\n\n"); ok {
		return rest
	}
	if strings.TrimSpace(text) == joined {
		return ""
	}
	return text
}

// Display carries a short, structured summary of a tool result for the TUI/stream.
type Display struct {
	Summary string
	Kind    string // e.g. file, diff, search, shell
	// Preview is a multi-line, card-only body (e.g. a unified diff or file head)
	// for the TUI. It is NEVER sent to the model — Output stays the short summary
	// the model sees — so a rich code preview costs zero model tokens.
	Preview string
}

type Tool interface {
	Name() string
	Description() string
	Parameters() Schema
	Safety() Safety
	Run(ctx context.Context, args map[string]any) Result
}

// IsBuiltInApplyPatch reports whether tool is Zero's scoped patch tool. The
// marker method is package-private so external and MCP tools cannot claim this
// identity merely by sharing the public name "apply_patch".
func IsBuiltInApplyPatch(tool Tool) bool {
	_, ok := tool.(interface{ isBuiltInApplyPatch() })
	return ok
}

// ArgsPermissioner is an optional interface a Tool can implement to refine its
// permission for a SPECIFIC call based on its arguments. When a tool implements
// it, the agent loop consults PermissionForArgs(args) instead of the static
// Safety().Permission when deciding whether the call needs approval. It exists to
// safely RELAX a prompt to allow for arguments the tool can prove are harmless
// (e.g. delegating to a read-only sub-agent); a tool must return its static,
// stricter permission whenever it cannot prove the call is safe.
type ArgsPermissioner interface {
	PermissionForArgs(args map[string]any) Permission
}

// PrePermissionRejecter lets a tool reject a call that cannot safely or validly
// run before any permission prompt is shown. Implementations must be purely
// local and deterministic: no filesystem, process, DNS, or network access.
type PrePermissionRejecter interface {
	RejectBeforePermission(args map[string]any) (Result, bool)
}

type baseTool struct {
	name         string
	description  string
	parameters   Schema
	safety       Safety
	capabilities ToolCapabilities // zero value = EffectUnknown, not thread-safe
	deferred     bool
}

func (tool baseTool) Name() string {
	return tool.name
}

func (tool baseTool) Description() string {
	return tool.description
}

func (tool baseTool) Parameters() Schema {
	return tool.parameters
}

func (tool baseTool) Safety() Safety {
	return tool.safety
}

// Deferred reports whether this built-in is discoverable on demand.
func (tool baseTool) Deferred() bool {
	return tool.deferred
}

func okResult(output string) Result {
	return Result{Status: StatusOK, Output: output}
}

func errorResult(output string) Result {
	return Result{Status: StatusError, Output: output}
}

func readOnlySafety(reason string) Safety {
	return Safety{
		SideEffect: SideEffectRead,
		Permission: PermissionAllow,
		Reason:     reason,
	}
}

func promptSafety(sideEffect SideEffect, reason string) Safety {
	return Safety{
		SideEffect: sideEffect,
		Permission: PermissionPrompt,
		Reason:     reason,
	}
}

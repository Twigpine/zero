package zeroruntime

import (
	"context"
	"strings"
)

// MessageRole identifies the origin of a conversation message.
type MessageRole string

// StreamEventType identifies one event in a provider completion stream.
type StreamEventType string

// AgentEventType is the stable PRD-level event stream shared by TUI,
// headless output, sessions, and future editor integrations.
type AgentEventType string

const (
	MessageRoleSystem    MessageRole = "system"
	MessageRoleUser      MessageRole = "user"
	MessageRoleAssistant MessageRole = "assistant"
	MessageRoleTool      MessageRole = "tool"
)

const (
	AgentEventText       AgentEventType = "text"
	AgentEventToolCall   AgentEventType = "tool_call"
	AgentEventToolResult AgentEventType = "tool_result"
	AgentEventThinking   AgentEventType = "thinking"
	AgentEventUsage      AgentEventType = "usage"
	AgentEventPlanUpdate AgentEventType = "plan_update"
	AgentEventError      AgentEventType = "error"
	AgentEventTurnEnd    AgentEventType = "turn_end"
)

const (
	StreamEventText StreamEventType = "text"
	// StreamEventReasoning carries live reasoning deltas that must never be
	// folded into answer text or persisted as assistant content.
	StreamEventReasoning     StreamEventType = "reasoning"
	StreamEventToolCallStart StreamEventType = "tool-call-start"
	StreamEventToolCallDelta StreamEventType = "tool-call-delta"
	StreamEventToolCallEnd   StreamEventType = "tool-call-end"
	// StreamEventToolCallDropped signals the model attempted a tool call that
	// was malformed (no usable name/id) and could not be dispatched. The agent
	// uses this to ask the model to retry instead of silently ending the turn.
	StreamEventToolCallDropped StreamEventType = "tool-call-dropped"
	StreamEventUsage           StreamEventType = "usage"
	StreamEventDone            StreamEventType = "done"
	StreamEventError           StreamEventType = "error"
)

// Normalized terminal finish reasons for responses that did not end normally.
// Providers map their native stop reasons onto these so consumers can detect a
// truncated or filtered response regardless of which provider produced it. A
// normal completion leaves the finish reason empty.
const (
	// FinishReasonLength means the response was truncated at the output token
	// cap (OpenAI finish_reason=="length", Anthropic stop_reason=="max_tokens",
	// Gemini finishReason=="MAX_TOKENS").
	FinishReasonLength = "length"
	// FinishReasonContentFilter means the response was withheld or cut off by a
	// content/safety filter (OpenAI finish_reason=="content_filter",
	// Gemini finishReason=="SAFETY").
	FinishReasonContentFilter = "content_filter"
)

// ToolCall is a normalized assistant request to run a tool.
type ToolCall struct {
	ID             string
	ProviderCallID string
	Name           string
	Arguments      string
	// Freeform reports that Arguments is raw tool input rather than a JSON
	// object. Only providers that advertise a matching freeform definition set
	// it; ordinary function calls remain unchanged.
	Freeform bool
	// Signature carries a provider's opaque reasoning signature bound to this call
	// (Gemini attaches a thoughtSignature to a functionCall part). It must be
	// echoed back with the call on later turns or the provider may reject the
	// multi-turn function-calling conversation. Empty for providers that do not
	// use it. Only the originating adapter interprets it.
	Signature string
}

// ReasoningBlock is a provider-emitted reasoning artifact that must be replayed
// verbatim on later turns or the provider rejects the (tool-using) conversation:
// Anthropic requires thinking / redacted_thinking blocks be passed back with
// their signatures. Only the adapter named by Provider interprets a block; other
// adapters ignore foreign blocks, so a mid-run provider switch is safe.
type ReasoningBlock struct {
	Provider  string // adapter that produced and can replay this block ("anthropic", "gemini")
	Type      string // provider block type ("thinking", "redacted_thinking")
	Text      string // human-readable reasoning text (empty for redacted/opaque blocks)
	Signature string // cryptographic signature the provider requires on replay
	Data      string // opaque provider payload (e.g. Anthropic redacted_thinking data)
}

// Message is a normalized conversation turn passed to providers.
type Message struct {
	Role               MessageRole
	Content            string
	ToolCalls          []ToolCall
	ToolCallID         string
	ToolCallProviderID string
	IsError            bool             // tool-result status; ignored for non-tool messages
	ChangedFiles       []string         // durable tool-result mutation targets; ignored by providers
	Images             []ImageBlock     // optional; nil for text-only messages
	Reasoning          []ReasoningBlock // optional; preserved thinking blocks to replay
}

type ToolDefinitionType string

const (
	ToolDefinitionFunction ToolDefinitionType = "function"
	ToolDefinitionFreeform ToolDefinitionType = "custom"
)

type ToolDefinitionFormat struct {
	Type       string `json:"type"`
	Syntax     string `json:"syntax"`
	Definition string `json:"definition"`
}

// ToolDefinition describes a model-visible function or freeform tool. Type is
// empty for the legacy function shape; Format is used only by freeform tools.
type ToolDefinition struct {
	Name        string
	Description string
	Parameters  map[string]any
	Type        ToolDefinitionType
	Format      *ToolDefinitionFormat
}

// TokenUsage accepts provider-specific token aliases before normalization.
type TokenUsage struct {
	InputTokens       int
	PromptTokens      int
	CachedInputTokens int
	// CacheWriteTokens is the cache-creation (cache-write) portion of the input,
	// billed at a premium rate by providers that support it (e.g. Anthropic).
	// Like CachedInputTokens it is a SUBSET of InputTokens, not additive.
	CacheWriteTokens int
	OutputTokens     int
	CompletionTokens int
	ReasoningTokens  int
}

// Usage records normalized token accounting reported by a provider.
//
// Token accounting is consistent across providers: InputTokens is the TOTAL
// prompt size (uncached + cache-read + cache-write); CachedInputTokens (cache
// read, discounted) and CacheWriteTokens (cache creation, premium) are subsets
// of it, so uncached input = InputTokens - CachedInputTokens - CacheWriteTokens.
// OutputTokens is the TOTAL output size, including hidden reasoning tokens when
// a provider reports them separately; ReasoningTokens is a subset of OutputTokens,
// not an additive count.
type Usage struct {
	InputTokens       int
	OutputTokens      int
	PromptTokens      int
	CompletionTokens  int
	CachedInputTokens int
	CacheWriteTokens  int
	ReasoningTokens   int
}

// TotalTokens returns prompt plus completion tokens.
func (usage Usage) TotalTokens() int {
	return usage.EffectiveInputTokens() + usage.EffectiveOutputTokens()
}

func (usage Usage) EffectiveInputTokens() int {
	if usage.InputTokens != 0 {
		return usage.InputTokens
	}
	return usage.PromptTokens
}

func (usage Usage) EffectiveOutputTokens() int {
	if usage.OutputTokens != 0 {
		return usage.OutputTokens
	}
	return usage.CompletionTokens
}

func (usage Usage) BillableOutputTokens() int {
	return usage.EffectiveOutputTokens()
}

func (usage Usage) VisibleOutputTokens() int {
	visible := usage.EffectiveOutputTokens() - usage.ReasoningTokens
	if visible < 0 {
		return 0
	}
	return visible
}

// StreamEvent is one normalized event emitted by a streaming provider.
type StreamEvent struct {
	Type               StreamEventType
	Content            string
	ToolCallID         string
	ToolCallProviderID string
	ToolName           string
	ToolCallSignature  string // opaque reasoning signature bound to this call (Gemini thoughtSignature)
	ToolCallFreeform   bool
	ArgumentsFragment  string
	Usage              Usage
	Error              string
	// ResponseID is the provider's completed response identifier when one is
	// available. Stateful turn sessions use it to chain a later compatible
	// request; ordinary providers and consumers leave it empty.
	ResponseID string
	// StatusCode is the HTTP status code behind Error, when the failure came from
	// an HTTP response (0 for transport-level/context errors that never received
	// one). Providers set it alongside Error on a StreamEventError so the status
	// survives past the flattened error string (#674).
	StatusCode int
	// Cause is the raw, secret-redacted upstream detail behind Error — the
	// provider's own error message or response body — kept separate from Error's
	// curated/classified wording so both are preserved rather than one
	// overwriting the other. Always passed through the same redaction already
	// applied to Error (see provider.redact); never stored unredacted.
	Cause string
	// FinishReason carries the provider's normalized terminal stop reason when a
	// response did not end normally (e.g. FinishReasonLength when the output hit
	// the token cap, or FinishReasonContentFilter when it was filtered). It is
	// empty for a normal completion. Providers set it on the terminal/done event.
	FinishReason string
	// ReasoningBlocks carries completed reasoning artifacts (Anthropic thinking /
	// redacted_thinking blocks) that must be preserved for replay. Providers attach
	// them to the terminal/done event; the collector accumulates them.
	ReasoningBlocks []ReasoningBlock
}

// StreamError is the terminal error returned for a failed provider stream. It
// implements error via Message, so existing `err.Error()` call sites are
// unaffected by this type; callers that need the underlying HTTP status code or
// upstream cause recover them with errors.As instead of re-parsing the message
// string. This is what lets a session's "error" event persist real diagnostic
// detail instead of only the flattened message (#674).
type StreamError struct {
	Message    string
	StatusCode int
	Cause      string
}

func (e *StreamError) Error() string { return e.Message }

// CompletionRequest groups provider input messages and available tools.
type CompletionRequest struct {
	Messages []Message
	Tools    []ToolDefinition
	// ReasoningEffort, when non-empty, asks a reasoning-capable model to spend the
	// given level of thinking effort ("minimal"/"low"/"medium"/"high"). Each
	// provider adapter maps it to its own API shape (OpenAI reasoning_effort,
	// Anthropic/Gemini thinking budgets) and ignores it for models that do not
	// support reasoning. Empty means "let the provider decide".
	ReasoningEffort string
	// ServiceTier selects an optional provider-advertised request tier. Empty
	// means the provider default; "priority" is the wire value used by fast mode.
	// Callers must only set a tier advertised for the active provider/model.
	ServiceTier string
	// PromptCacheKey, when non-empty, is an opaque stable identifier for the
	// conversation (the session ID). Providers with server-side prefix-cache
	// routing forward it — OpenAI `prompt_cache_key` — so consecutive requests
	// land on a replica that already holds the cached prompt prefix instead of
	// re-billing the full prefix each turn. Providers without an equivalent
	// ignore it.
	PromptCacheKey string
}

// Provider streams normalized completion events for one request.
type Provider interface {
	StreamCompletion(ctx context.Context, request CompletionRequest) (<-chan StreamEvent, error)
}

// ImageBlock is a normalized image attachment carried on a Message. Data is the
// RAW decoded image bytes (no base64, no data: prefix); each provider encodes it
// into its own wire format. MediaType is a normalized MIME, e.g. "image/png".
type ImageBlock struct {
	MediaType string
	Data      []byte
}

// NormalizeImageMediaType canonicalizes a caller-supplied image type to one of
// the allow-listed MIME strings (image/png, image/jpeg, image/gif, image/webp)
// or "" when the input is outside the allow-list (the caller then rejects it).
//
// It lowercases and trims the input, strips a leading "data:<m>;base64," prefix
// (using <m>), maps a bare png|jpeg|jpg|gif|webp to image/<x> (jpg->jpeg), and
// passes through an already-image/<x> value in the allow-list (image/jpg is
// also folded to image/jpeg). It is pure and has no dependencies.
func NormalizeImageMediaType(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	// Strip a data: URI wrapper, keeping the media type between "data:" and ";".
	if rest, ok := strings.CutPrefix(s, "data:"); ok {
		if i := strings.IndexByte(rest, ';'); i >= 0 {
			s = rest[:i]
		} else {
			s = rest
		}
		s = strings.TrimSpace(s)
	}
	switch s {
	case "png", "image/png":
		return "image/png"
	case "jpeg", "jpg", "image/jpeg", "image/jpg":
		return "image/jpeg"
	case "gif", "image/gif":
		return "image/gif"
	case "webp", "image/webp":
		return "image/webp"
	default:
		return ""
	}
}

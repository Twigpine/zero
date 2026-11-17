package anthropic

type messagesRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []anthropicMessage `json:"messages"`
	// System is either a plain string or a []systemBlock when prompt caching is
	// enabled (Anthropic accepts both). The block form lets us attach a
	// cache_control breakpoint to the stable system prompt.
	System any `json:"system,omitempty"`
	// Thinking enables extended thinking when a reasoning effort was requested.
	// Omitted (nil) for normal requests so behavior is unchanged.
	Thinking     *thinkingConfig `json:"thinking,omitempty"`
	OutputConfig *outputConfig   `json:"output_config,omitempty"`
	Tools        []anthropicTool `json:"tools,omitempty"`
	Stream       bool            `json:"stream"`
}

// thinkingConfig requests extended thinking. Anthropic uses "enabled" with a
// token budget; MiniMax-M3.1-Flash uses "adaptive" with output_config.effort.
type thinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type outputConfig struct {
	Effort string `json:"effort"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// systemBlock is a single text block of the system prompt. A cache_control
// breakpoint on a block caches everything up to and including it.
type systemBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// cacheControl marks a prompt-cache breakpoint. Only "ephemeral" is supported.
type cacheControl struct {
	Type string `json:"type"`
}

const cacheEphemeral = "ephemeral"

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
	// CacheControl on the LAST tool caches the whole (stable) tool-definition
	// block so it is not re-billed every turn.
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type streamPayload struct {
	Type         string         `json:"type"`
	Index        int            `json:"index"`
	Message      *streamMessage `json:"message"`
	ContentBlock *contentBlock  `json:"content_block"`
	Delta        *streamDelta   `json:"delta"`
	Usage        *usage         `json:"usage"`
	Error        *apiError      `json:"error"`
}

type streamMessage struct {
	Usage usage `json:"usage"`
}

type contentBlock struct {
	Type  string         `json:"type"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
	// Data is the opaque payload of a redacted_thinking block (delivered whole in
	// content_block_start, with no deltas).
	Data string `json:"data"`
}

type streamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
	// Thinking/Signature carry extended-thinking deltas: thinking_delta streams the
	// reasoning text, signature_delta delivers the block's verification signature.
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

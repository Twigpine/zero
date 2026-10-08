package openai

type chatCompletionRequest struct {
	Model               string           `json:"model"`
	Messages            []chatMessage    `json:"messages"`
	Tools               []toolDefinition `json:"tools,omitempty"`
	MaxCompletionTokens int              `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string           `json:"reasoning_effort,omitempty"`
	ServiceTier         string           `json:"service_tier,omitempty"`
	Stream              bool             `json:"stream"`
	StreamOptions       *streamOptions   `json:"stream_options,omitempty"`
	// PromptCacheKey asks the backend to route the request to a replica that
	// already holds this conversation's prefix in its prompt cache (the OpenAI
	// `prompt_cache_key` parameter). Omitted when the caller carries no session
	// identity, when the provider was constructed with DisablePromptCacheKey
	// (openai-compatible gateways that reject unknown fields), or when
	// ZERO_DISABLE_PROMPT_CACHE_KEY is set.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
}

// streamOptions requests the final usage chunk on a streaming response. Without
// include_usage the OpenAI streaming API never sends the usage object, so token
// accounting is silently zero for real OpenAI streams.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role string `json:"role"`
	// Content has no omitempty: strict OpenAI-compatible servers (e.g. some
	// Ollama-cloud models like glm-*) reject a message whose `content` is absent
	// or null with "invalid message content type: <nil>". mapMessage always sets
	// this (to "" when there's no text), so a contentless message serializes as
	// `"content":""` rather than being dropped.
	Content    any               `json:"content"`
	ToolCalls  []requestToolCall `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
}

// contentPart is one element of an OpenAI multimodal `content` array. A part is
// either text (Type "text") or an inline image data URI (Type "image_url").
type contentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *imageURLPart `json:"image_url,omitempty"`
}

// imageURLPart carries an inline image as a `data:<media>;base64,<b64>` URI.
type imageURLPart struct {
	URL string `json:"url"`
}

type requestToolCall struct {
	ID       string                  `json:"id"`
	Type     string                  `json:"type"`
	Function requestToolCallFunction `json:"function"`
}

type requestToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolDefinition struct {
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type streamChunk struct {
	Choices []streamChoice `json:"choices"`
	Usage   *usage         `json:"usage"`
	Error   *apiError      `json:"error"`
}

type streamChoice struct {
	Delta        streamDelta `json:"delta"`
	FinishReason string      `json:"finish_reason"`
}

type streamDelta struct {
	Content          string                `json:"content"`
	ReasoningContent string                `json:"reasoning_content"`
	Reasoning        string                `json:"reasoning"`
	ToolCalls        []streamToolCallDelta `json:"tool_calls"`
}

type streamToolCallDelta struct {
	Index    int                 `json:"index"`
	ID       string              `json:"id"`
	Function streamFunctionDelta `json:"function"`
}

type streamFunctionDelta struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type usage struct {
	PromptTokens            int                    `json:"prompt_tokens"`
	CompletionTokens        int                    `json:"completion_tokens"`
	PromptTokensDetails     promptTokenDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails completionTokenDetails `json:"completion_tokens_details"`
}

type promptTokenDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

type completionTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

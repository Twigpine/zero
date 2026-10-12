package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/providers/providerio"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

const defaultBaseURL = "https://api.openai.com/v1"

// Options configures an OpenAI-compatible chat completions provider.
type Options struct {
	APIKey  string
	BaseURL string
	// Endpoint, when non-empty, overrides the default `{BaseURL}/chat/completions`
	// request URL. Used by flavors (notably the Codex provider) that speak a
	// different path on the same host — e.g. `/responses` on the ChatGPT Codex
	// backend. When empty, the openai provider falls back to its default
	// chat-completions path so every existing caller is unchanged.
	Endpoint        string
	Model           string
	AuthHeader      string
	AuthScheme      string
	AuthHeaderValue string
	CustomHeaders   map[string]string
	HTTPClient      *http.Client
	UserAgent       string
	// OAuthResolver, when set, supplies an OAuth bearer credential per request and
	// is preferred over APIKey; a nil resolver (or one that yields ok=false) uses
	// the API key. See providerio.SendWithAuthRetry.
	OAuthResolver providerio.TokenResolver
	// MaxTokens caps the model's output tokens. Zero omits the cap (the model's
	// own default applies). Resolved from the model registry by the factory.
	MaxTokens int
	// StreamIdleTimeout aborts the stream if no data arrives for this long.
	// When unset, Zero uses providerio.ResolveStreamIdleTimeout — the
	// ZERO_STREAM_IDLE_TIMEOUT override or providerio.DefaultStreamIdleTimeout.
	StreamIdleTimeout time.Duration
	// ParseThinkTags converts streamed <think>...</think> content into reasoning
	// events for OpenAI-compatible models known to emit that legacy format.
	ParseThinkTags bool
	// SetRequestExtra, when non-nil, is invoked on every outgoing request after
	// the provider's built-in headers (Content-Type, User-Agent) are set, so a
	// wrapper (notably the Codex provider) can inject request-specific headers
	// — e.g. an account id resolved from the OAuth token, or a hard-coded
	// "originator" value. It is also called on the 401-refresh retry, so any
	// per-request state must be re-derivable from the live request.
	SetRequestExtra func(*http.Request)
	// DisablePromptCacheKey omits OpenAI's prompt_cache_key even when the
	// caller supplies a session identity. Used for openai-compatible gateways
	// (NVIDIA NIM, strict local proxies, …) that validate and reject unknown
	// request fields instead of ignoring them. Official OpenAI keeps the field
	// enabled; the ZERO_DISABLE_PROMPT_CACHE_KEY env kill switch still applies
	// on top for any endpoint.
	DisablePromptCacheKey bool
	SessionHeader         string
}

// Provider streams completions from an OpenAI-compatible chat completions API.
type Provider struct {
	apiKey                string
	baseURL               string
	endpoint              string
	model                 string
	authHeader            string
	authScheme            string
	authHeaderValue       string
	customHeaders         map[string]string
	oauthResolver         providerio.TokenResolver
	maxTokens             int
	httpClient            *http.Client
	userAgent             string
	streamIdleTimeout     time.Duration
	parseThinkTags        bool
	setRequestExtra       func(*http.Request)
	disablePromptCacheKey bool
	sessionHeader         providerio.SessionHeader
}

// New creates an OpenAI-compatible provider.
func New(options Options) (*Provider, error) {
	model := strings.TrimSpace(options.Model)
	if model == "" {
		return nil, errors.New("openai provider requires a model")
	}

	baseURL := strings.TrimSpace(options.BaseURL)
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if _, err := url.ParseRequestURI(baseURL); err != nil {
		return nil, fmt.Errorf("invalid OpenAI base URL: %w", err)
	}

	// Endpoint overrides the default chat-completions path. When empty we
	// append `/chat/completions` to the (trimmed) baseURL. When non-empty we
	// trust the caller's value verbatim so a flavor (Codex) can target a
	// sibling path on the same host without re-implementing the transport.
	endpoint := strings.TrimSpace(options.Endpoint)
	if endpoint == "" {
		endpoint = baseURL + "/chat/completions"
	}

	// Route through providerio.HTTPClient so a missing client gets the shared,
	// stall-hardened transport (bounded response-header wait + shorter idle-conn
	// reuse) rather than the raw http.DefaultClient, which hangs on a reused-dead
	// pooled connection (the macOS-only "both providers stall" bug).
	httpClient := providerio.HTTPClient(options.HTTPClient)

	maxTokens := options.MaxTokens
	if maxTokens < 0 {
		maxTokens = 0
	}

	return &Provider{
		apiKey:                options.APIKey,
		baseURL:               baseURL,
		endpoint:              endpoint,
		model:                 model,
		authHeader:            strings.TrimSpace(options.AuthHeader),
		authScheme:            strings.TrimSpace(options.AuthScheme),
		authHeaderValue:       strings.TrimSpace(options.AuthHeaderValue),
		customHeaders:         providerio.CopyHeaders(options.CustomHeaders),
		oauthResolver:         options.OAuthResolver,
		maxTokens:             maxTokens,
		httpClient:            httpClient,
		userAgent:             options.UserAgent,
		streamIdleTimeout:     providerio.ResolveStreamIdleTimeout(options.StreamIdleTimeout),
		parseThinkTags:        options.ParseThinkTags,
		setRequestExtra:       options.SetRequestExtra,
		disablePromptCacheKey: options.DisablePromptCacheKey,
		sessionHeader:         providerio.NewSessionHeader(options.SessionHeader),
	}, nil
}

// StreamCompletion sends one streaming chat completion request.
func (provider *Provider) StreamCompletion(
	ctx context.Context,
	request zeroruntime.CompletionRequest,
) (<-chan zeroruntime.StreamEvent, error) {
	body, err := json.Marshal(provider.openAIRequest(request))
	if err != nil {
		return nil, fmt.Errorf("encode OpenAI request: %w", err)
	}

	events := make(chan zeroruntime.StreamEvent, 16)
	go func() {
		defer close(events)
		provider.stream(ctx, body, request.PromptCacheKey, events)
	}()

	return events, nil
}

func (provider *Provider) stream(ctx context.Context, body []byte, sessionID string, events chan<- zeroruntime.StreamEvent) {
	endpoint := provider.endpoint

	// streamCtx lets the idle watchdog abort an in-flight body read by cancelling
	// the request, rather than closing response.Body directly (which would race
	// with the deferred Close below). Cancelling unblocks the reader goroutine.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	// Retry transient failures (network errors, 429, and 5xx) before surfacing
	// them — hosted gateways return intermittent 500s and rate limits that
	// succeed on a quick retry. Shared with the Anthropic/Gemini providers.
	response, err := providerio.SendWithAuthRetry(streamCtx, provider.httpClient, http.MethodPost, endpoint, body,
		providerio.AuthHeaders{
			APIKey:            provider.apiKey,
			DefaultAuthHeader: "Authorization",
			DefaultAuthScheme: "Bearer",
			AuthHeader:        provider.authHeader,
			AuthScheme:        provider.authScheme,
			AuthHeaderValue:   provider.authHeaderValue,
			CustomHeaders:     provider.customHeaders,
		},
		provider.oauthResolver,
		func(request *http.Request) {
			provider.sessionHeader.Apply(request, sessionID)
			request.Header.Set("Content-Type", "application/json")
			if provider.userAgent != "" {
				request.Header.Set("User-Agent", provider.userAgent)
			}
			if provider.setRequestExtra != nil {
				provider.setRequestExtra(request)
			}
		}, 0)
	if err != nil {
		// A caller-driven cancel/deadline (parent ctx done) surfaces here as a
		// transport error whose string can contain "context deadline exceeded" plus
		// the request host — which UpstreamUnreachable would mislabel as an upstream
		// outage. Check the parent context first and surface its error verbatim, so
		// only genuine connect failures (ctx still live) get humanized.
		if ctxErr := ctx.Err(); ctxErr != nil {
			sendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventError, Error: provider.redact("provider stream error: " + ctxErr.Error())})
			return
		}
		// A direct connection that never completes (e.g. a hosted endpoint blocked
		// by the local network) surfaces as a transport error; humanize it the same
		// way as a proxy's gateway error so the user sees a clear connectivity cause.
		if humanized, ok := providerio.UpstreamUnreachable(err.Error()); ok {
			sendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventError, Error: provider.redact(humanized)})
			return
		}
		sendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventError, Error: provider.redact("provider stream error: " + err.Error())})
		return
	}
	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		provider.emitHTTPError(ctx, response, events)
		return
	}

	state := newToolState(provider.parseThinkTags)
	// Use the shared SSE reader (also used by the Anthropic/Gemini providers) so
	// multi-line "data:" continuation fields are joined into one payload, and the
	// idle watchdog / context cancellation are handled uniformly.
	err = providerio.ScanSSEDataWithContext(streamCtx, cancelStream, response.Body, provider.streamIdleTimeout, func(data string) bool {
		return provider.emitPayload(ctx, data, state, events)
	})
	if errors.Is(err, providerio.ErrStreamIdle) || errors.Is(err, providerio.ErrStreamStalled) {
		state.flushBufferedContent(events)
		state.closeBufferedOpen(events)
		sendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: provider.redact("provider stream error: " + providerio.StreamTimeoutMessage(err, provider.streamIdleTimeout)),
		})
		return
	}
	if err != nil {
		state.flushBufferedContent(events)
		state.closeBufferedOpen(events)
		sendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventError, Error: provider.redact("provider stream error: " + err.Error())})
		return
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		state.flushBufferedContent(events)
		state.closeBufferedOpen(events)
		sendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventError, Error: provider.redact("provider stream error: " + ctxErr.Error())})
		return
	}
	if !state.done {
		state.flushContent(ctx, events)
		state.closeOpen(ctx, events)
		sendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone, FinishReason: state.finishReason})
	}
}

// openAIStreamErrorStatusByCode maps a streamed error payload's "code" field
// to the HTTP status classifiedError expects, covering both the numeric-string
// codes some providers send ("429") and the semantic string codes OpenAI-
// compatible providers commonly send instead (rate_limit_exceeded). Both
// forms of the same condition must classify identically, or retry/backoff
// logic downstream would only kick in for whichever form a given provider
// happens to use. insufficient_quota maps to 429 (Too Many Requests) to match
// OpenAI's own API, which returns that code with a 429 status when a caller
// has exceeded their billing quota.
var openAIStreamErrorStatusByCode = map[string]int{
	"429":                 http.StatusTooManyRequests,
	"401":                 http.StatusUnauthorized,
	"403":                 http.StatusForbidden,
	"rate_limit_exceeded": http.StatusTooManyRequests,
	"insufficient_quota":  http.StatusTooManyRequests,
	"invalid_api_key":     http.StatusUnauthorized,
}

// emitPayload handles one accumulated SSE data payload ([DONE]/blank lines are
// already filtered by the shared reader). It returns false to abort the stream
// after emitting a terminal error.
func (provider *Provider) emitPayload(ctx context.Context, data string, state *toolState, events chan<- zeroruntime.StreamEvent) bool {
	var chunk streamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		state.flushContent(ctx, events)
		state.closeOpen(ctx, events)
		sendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: provider.redact("provider stream error: malformed JSON: " + err.Error()),
		})
		state.done = true
		return false
	}
	if chunk.Error != nil {
		state.flushContent(ctx, events)
		state.closeOpen(ctx, events)
		statusCode := http.StatusInternalServerError
		if chunk.Error.Code != nil {
			switch c := chunk.Error.Code.(type) {
			case string:
				if code, ok := openAIStreamErrorStatusByCode[c]; ok {
					statusCode = code
				}
			case float64:
				if code, ok := openAIStreamErrorStatusByCode[strconv.Itoa(int(c))]; ok {
					statusCode = code
				}
			case int:
				if code, ok := openAIStreamErrorStatusByCode[strconv.Itoa(c)]; ok {
					statusCode = code
				}
			}
		}
		sendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: provider.classifiedError(statusCode, chunk.Error.Message),
		})
		state.done = true
		return false
	}
	provider.emitChunk(ctx, chunk, state, events)
	return true
}

func (provider *Provider) emitChunk(
	ctx context.Context,
	chunk streamChunk,
	state *toolState,
	events chan<- zeroruntime.StreamEvent,
) {
	for _, choice := range chunk.Choices {
		if reasoning := choice.Delta.reasoningText(); reasoning != "" {
			sendEvent(ctx, events, zeroruntime.StreamEvent{
				Type:    zeroruntime.StreamEventReasoning,
				Content: reasoning,
			})
		}
		if choice.Delta.Content != "" {
			state.emitContent(ctx, events, choice.Delta.Content)
		}
		for _, toolCall := range choice.Delta.ToolCalls {
			state.applyDelta(ctx, toolCall, events)
		}
		if choice.FinishReason == "tool_calls" {
			state.flushContent(ctx, events)
			state.closeOpen(ctx, events)
		}
		if reason := mapFinishReason(choice.FinishReason); reason != "" {
			state.finishReason = reason
		}
	}

	if chunk.Usage != nil {
		sendEvent(ctx, events, zeroruntime.StreamEvent{
			Type: zeroruntime.StreamEventUsage,
			Usage: zeroruntime.Usage{
				PromptTokens:      chunk.Usage.PromptTokens,
				CompletionTokens:  chunk.Usage.CompletionTokens,
				CachedInputTokens: chunk.Usage.PromptTokensDetails.CachedTokens,
				CacheWriteTokens:  chunk.Usage.PromptTokensDetails.CacheWriteTokens,
				ReasoningTokens:   chunk.Usage.CompletionTokensDetails.ReasoningTokens,
			},
		})
	}
}

func (delta streamDelta) reasoningText() string {
	if delta.ReasoningContent != "" {
		return delta.ReasoningContent
	}
	return delta.Reasoning
}

// mapFinishReason maps OpenAI's finish_reason onto the runtime's normalized
// terminal reasons. A normal finish ("stop"/"tool_calls"/"") returns "".
func mapFinishReason(reason string) string {
	switch reason {
	case "length":
		return zeroruntime.FinishReasonLength
	case "content_filter":
		return zeroruntime.FinishReasonContentFilter
	default:
		return ""
	}
}

func (provider *Provider) emitHTTPError(ctx context.Context, response *http.Response, events chan<- zeroruntime.StreamEvent) {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	message := strings.TrimSpace(string(body))
	var parsed struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error.Message != "" {
		message = parsed.Error.Message
	}
	if message == "" {
		message = response.Status
	}
	// A local proxy (e.g. an Ollama daemon serving a "-cloud" model) answers on
	// localhost but returns a gateway error when it cannot reach its own backend.
	// Surface that as a clear connectivity message instead of the raw proxied body.
	if humanized, ok := providerio.UpstreamUnreachable(message); ok {
		sendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventError, Error: provider.redact(humanized)})
		return
	}
	sendEvent(ctx, events, zeroruntime.StreamEvent{
		Type:  zeroruntime.StreamEventError,
		Error: provider.classifiedError(response.StatusCode, message),
	})
}

func (provider *Provider) classifiedError(statusCode int, message string) string {
	return providerio.ClassifiedError(statusCode, message, provider.apiKey, provider.authHeaderValue)
}

func (provider *Provider) redact(message string) string {
	return providerio.Redact(message, provider.apiKey, provider.authHeaderValue)
}

func sendEvent(ctx context.Context, events chan<- zeroruntime.StreamEvent, event zeroruntime.StreamEvent) {
	select {
	case <-ctx.Done():
		if event.Type == zeroruntime.StreamEventError {
			select {
			case events <- event:
			default:
			}
		}
	case events <- event:
	}
}

func sendBufferedEvent(events chan<- zeroruntime.StreamEvent, event zeroruntime.StreamEvent) {
	select {
	case events <- event:
	default:
	}
}

func (provider *Provider) openAIRequest(request zeroruntime.CompletionRequest) chatCompletionRequest {
	messages := make([]chatMessage, 0, len(request.Messages))
	for _, message := range request.Messages {
		// Drop a degenerate assistant turn that carries neither text nor tool calls
		// (e.g. a sub-agent that failed with no output). The Anthropic/Gemini mappers
		// already skip empty turns; without this, the contentless message reaches
		// strict OpenAI-compatible servers and is rejected.
		if message.Role == zeroruntime.MessageRoleAssistant &&
			strings.TrimSpace(message.Content) == "" && len(message.ToolCalls) == 0 {
			continue
		}
		messages = append(messages, mapMessage(message))
	}

	mapped := chatCompletionRequest{
		Model:    provider.model,
		Messages: messages,
		Stream:   true,
		// Request the terminal usage chunk; OpenAI omits it on streams otherwise,
		// which silently zeroes token accounting.
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	if provider.maxTokens > 0 {
		mapped.MaxCompletionTokens = provider.maxTokens
	}
	// reasoning_effort is only valid for reasoning models; callers gate it against
	// the model's capabilities, so an empty value (the default for non-reasoning
	// models) is simply omitted. Only forward the values the API accepts.
	if effort := openAIReasoningEffort(request.ReasoningEffort); effort != "" {
		mapped.ReasoningEffort = effort
	}
	if tier := openAIServiceTier(request.ServiceTier); tier != "" {
		mapped.ServiceTier = tier
	}
	// prompt_cache_key is a documented OpenAI parameter for server-side prefix
	// cache routing. Official OpenAI accepts it; many openai-compatible
	// gateways (NVIDIA NIM, strict local proxies) reject unknown fields with a
	// 400. Those providers are constructed with DisablePromptCacheKey, and any
	// endpoint can still force-omit via ZERO_DISABLE_PROMPT_CACHE_KEY=1.
	if key := strings.TrimSpace(request.PromptCacheKey); key != "" && !provider.disablePromptCacheKey && !promptCacheKeyDisabled() {
		mapped.PromptCacheKey = key
	}
	if len(request.Tools) > 0 {
		mapped.Tools = make([]toolDefinition, 0, len(request.Tools))
		for _, tool := range request.Tools {
			mapped.Tools = append(mapped.Tools, toolDefinition{
				Type: "function",
				Function: toolFunction{
					Name:        tool.Name,
					Description: tool.Description,
					Parameters:  normalizeToolParameters(tool.Parameters),
				},
			})
		}
	}
	return mapped
}

// normalizeToolParameters keeps schemas accepted by strict OpenAI-compatible
// servers such as LM Studio. No-argument tools still need an object-valued
// parameters.properties field instead of an omitted field.
func normalizeToolParameters(parameters map[string]any) map[string]any {
	normalized := make(map[string]any, len(parameters)+1)
	for key, value := range parameters {
		normalized[key] = value
	}
	properties, ok := normalized["properties"].(map[string]any)
	if !ok || properties == nil {
		normalized["properties"] = map[string]any{}
	}
	return normalized
}

// promptCacheKeyDisabled reports whether the ZERO_DISABLE_PROMPT_CACHE_KEY
// kill switch is set to a truthy value. "0" and "false" (any case) are
// no-ops, matching how ZERO_FORMAT_ON_WRITE parses boolean flags, so an
// explicitly-disabled toggle never flips the behavior it names.
func promptCacheKeyDisabled() bool {
	value := strings.TrimSpace(os.Getenv("ZERO_DISABLE_PROMPT_CACHE_KEY"))
	return value != "" && value != "0" && !strings.EqualFold(value, "false")
}

// openAIReasoningEffort normalizes a requested effort to a value the OpenAI chat
// completions API accepts, or "" to omit the field. "none" (and anything else)
// is dropped rather than risking a 400 on an unrecognized enum.
func openAIReasoningEffort(requested string) string {
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return strings.ToLower(strings.TrimSpace(requested))
	default:
		return ""
	}
}

func openAIServiceTier(requested string) string {
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "priority", "flex":
		return strings.ToLower(strings.TrimSpace(requested))
	default:
		return ""
	}
}

func mapMessage(message zeroruntime.Message) chatMessage {
	mapped := chatMessage{
		Role:       string(message.Role),
		ToolCallID: message.ToolCallID,
	}

	// Image content-parts are only valid on a user turn. Anthropic/Gemini emit
	// images solely from their user branches; OpenAI funnels every role through
	// this one mapper, so guard the parts path to the user role. A non-user
	// message that happens to carry Images keeps the plain string/nil content
	// path (its images are simply not serialized).
	if len(message.Images) == 0 || message.Role != zeroruntime.MessageRoleUser {
		// Always set content (to "" when empty) so it serializes as `"content":""`
		// rather than being dropped. Strict OpenAI-compatible servers reject a
		// message with no content field; tool results and assistant-with-tool-calls
		// turns must still carry an (empty) content. Truly empty assistant turns are
		// dropped upstream in openAIRequest.
		mapped.Content = message.Content
	} else {
		parts := make([]contentPart, 0, len(message.Images)+1)
		if message.Content != "" {
			parts = append(parts, contentPart{Type: "text", Text: message.Content})
		}
		for _, image := range message.Images {
			parts = append(parts, contentPart{
				Type: "image_url",
				ImageURL: &imageURLPart{
					URL: "data:" + image.MediaType + ";base64," +
						base64.StdEncoding.EncodeToString(image.Data),
				},
			})
		}
		mapped.Content = parts
	}

	if len(message.ToolCalls) > 0 {
		mapped.ToolCalls = make([]requestToolCall, 0, len(message.ToolCalls))
		for _, toolCall := range message.ToolCalls {
			mapped.ToolCalls = append(mapped.ToolCalls, requestToolCall{
				ID:   toolCall.ID,
				Type: "function",
				Function: requestToolCallFunction{
					Name:      toolCall.Name,
					Arguments: toolCall.Arguments,
				},
			})
		}
	}
	return mapped
}

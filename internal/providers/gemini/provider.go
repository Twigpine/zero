package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/providers/providerio"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

const defaultBaseURL = "https://generativelanguage.googleapis.com"
const defaultMaxTokens = 8192

// thinkingBudgetForEffort maps a requested reasoning effort to a Gemini thinking
// token budget, capped at 24576 (the lowest per-model ceiling among 2.5 models).
// 0 means "no thinking config" (leave the request unchanged).
func thinkingBudgetForEffort(effort string) int {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal":
		return 1024
	case "low":
		return 4096
	case "medium":
		return 8192
	case "high":
		return 24576
	default:
		return 0
	}
}

// Options configures a Gemini streamGenerateContent provider.
type Options struct {
	APIKey          string
	BaseURL         string
	Model           string
	MaxTokens       int
	AuthHeader      string
	AuthScheme      string
	AuthHeaderValue string
	CustomHeaders   map[string]string
	HTTPClient      *http.Client
	UserAgent       string
	// OAuthResolver, when set, supplies an OAuth bearer credential per request and
	// is retried once with a forced token refresh after an upstream 401 (matching
	// the OpenAI and Anthropic providers). Nil falls back to plain API-key auth.
	OAuthResolver providerio.TokenResolver
	// StreamIdleTimeout aborts the stream if no data arrives for this long.
	// When unset, Zero uses providerio.ResolveStreamIdleTimeout — the
	// ZERO_STREAM_IDLE_TIMEOUT override or providerio.DefaultStreamIdleTimeout.
	StreamIdleTimeout time.Duration
}

// Provider streams completions from the Gemini API.
type Provider struct {
	apiKey            string
	baseURL           string
	model             string
	maxTokens         int
	authHeader        string
	authScheme        string
	authHeaderValue   string
	customHeaders     map[string]string
	httpClient        *http.Client
	userAgent         string
	oauthResolver     providerio.TokenResolver
	streamIdleTimeout time.Duration
}

// New creates a Gemini provider.
func New(options Options) (*Provider, error) {
	model := normalizeModel(options.Model)
	if model == "" {
		return nil, errors.New("gemini provider requires a model")
	}
	maxTokens, err := providerio.PositiveOrDefault(options.MaxTokens, defaultMaxTokens, "zero Gemini provider maxTokens")
	if err != nil {
		return nil, err
	}
	baseURL, err := providerio.NormalizeBaseURL(options.BaseURL, defaultBaseURL, "Gemini")
	if err != nil {
		return nil, err
	}
	return &Provider{
		apiKey:            options.APIKey,
		baseURL:           baseURL,
		model:             model,
		maxTokens:         maxTokens,
		authHeader:        strings.TrimSpace(options.AuthHeader),
		authScheme:        strings.TrimSpace(options.AuthScheme),
		authHeaderValue:   strings.TrimSpace(options.AuthHeaderValue),
		customHeaders:     providerio.CopyHeaders(options.CustomHeaders),
		httpClient:        providerio.HTTPClient(options.HTTPClient),
		userAgent:         options.UserAgent,
		oauthResolver:     options.OAuthResolver,
		streamIdleTimeout: providerio.ResolveStreamIdleTimeout(options.StreamIdleTimeout),
	}, nil
}

// StreamCompletion sends one streaming Gemini GenerateContent request.
func (provider *Provider) StreamCompletion(
	ctx context.Context,
	request zeroruntime.CompletionRequest,
) (<-chan zeroruntime.StreamEvent, error) {
	mapped, err := provider.geminiRequest(request)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(mapped)
	if err != nil {
		return nil, fmt.Errorf("encode Gemini request: %w", err)
	}

	events := make(chan zeroruntime.StreamEvent, 16)
	go func() {
		defer close(events)
		provider.stream(ctx, body, events)
	}()
	return events, nil
}

func (provider *Provider) stream(ctx context.Context, body []byte, events chan<- zeroruntime.StreamEvent) {
	// streamCtx lets the idle watchdog abort an in-flight body read by cancelling
	// the request, which unblocks the SSE reader goroutine.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	response, err := providerio.SendWithAuthRetry(streamCtx, provider.httpClient, http.MethodPost, provider.streamURL(), body,
		providerio.AuthHeaders{
			APIKey:            provider.apiKey,
			DefaultAuthHeader: "x-goog-api-key",
			AuthHeader:        provider.authHeader,
			AuthScheme:        provider.authScheme,
			AuthHeaderValue:   provider.authHeaderValue,
			CustomHeaders:     provider.customHeaders,
		},
		provider.oauthResolver,
		func(request *http.Request) {
			request.Header.Set("Content-Type", "application/json")
			if provider.userAgent != "" {
				request.Header.Set("User-Agent", provider.userAgent)
			}
		}, 0)
	if err != nil {
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventError, Error: provider.redact("provider stream error: " + err.Error())})
		return
	}
	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		provider.emitHTTPError(ctx, response, events)
		return
	}

	state := streamState{}
	err = providerio.ScanSSEDataWithContext(streamCtx, cancelStream, response.Body, provider.streamIdleTimeout, func(data string) bool {
		return provider.emitPayload(ctx, data, &state, events)
	})
	if errors.Is(err, providerio.ErrStreamIdle) || errors.Is(err, providerio.ErrStreamStalled) {
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: provider.redact("provider stream error: " + providerio.StreamTimeoutMessage(err, provider.streamIdleTimeout)),
		})
		return
	}
	if err != nil {
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventError, Error: provider.redact("provider stream error: " + err.Error())})
		return
	}
	if err := ctx.Err(); err != nil {
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventError, Error: provider.redact("provider stream error: " + err.Error())})
		return
	}
	if !state.done {
		provider.emitDone(ctx, &state, events)
	}
}

func (provider *Provider) emitPayload(ctx context.Context, data string, state *streamState, events chan<- zeroruntime.StreamEvent) bool {
	var payload streamPayload
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: provider.redact("provider stream error: malformed JSON: " + err.Error()),
		})
		state.done = true
		return false
	}
	if payload.Error != nil {
		message := firstNonEmpty(payload.Error.Message, payload.Error.Status, "Gemini stream error")
		status := payload.Error.Code
		if status == 0 {
			status = http.StatusInternalServerError
		}
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: provider.classifiedError(status, message),
		})
		state.done = true
		return false
	}
	if payload.PromptFeedback != nil && payload.PromptFeedback.BlockReason != "" {
		message := firstNonEmpty(payload.PromptFeedback.BlockReasonMessage, payload.PromptFeedback.BlockReason)
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: provider.redact("provider error: Content blocked: " + message),
		})
		state.done = true
		return false
	}
	if payload.UsageMetadata != nil {
		state.inputTokens = payload.UsageMetadata.PromptTokenCount
		state.reasoningTokens = payload.UsageMetadata.ThoughtsTokenCount
		state.outputTokens = payload.UsageMetadata.CandidatesTokenCount + state.reasoningTokens
		state.cachedTokens = payload.UsageMetadata.CachedContentTokenCount
		state.hasUsage = true
	}
	for _, candidate := range payload.Candidates {
		if reason := mapFinishReason(candidate.FinishReason); reason != "" {
			state.finishReason = reason
		}
		if candidate.Content == nil {
			continue
		}
		for _, part := range candidate.Content.Parts {
			// Skip thought summary parts: their text is reasoning, not the answer.
			if part.Thought {
				continue
			}
			if part.Text != "" {
				providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: part.Text})
			}
			if part.FunctionCall != nil {
				if part.FunctionCall.Name == "" {
					// A functionCall without a usable name can't be dispatched.
					// Signal a drop once so the agent can ask the model to retry
					// instead of silently ending the turn (mirrors OpenAI/Anthropic).
					providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDropped})
					continue
				}
				state.syntheticToolIndex++
				// The thought signature rides on the part beside the functionCall;
				// preserve it for replay.
				if !provider.emitToolCall(ctx, *part.FunctionCall, part.ThoughtSignature, state.syntheticToolIndex, events) {
					state.done = true
					return false
				}
			}
		}
	}
	for _, functionCall := range payload.FunctionCalls {
		if functionCall.Name == "" {
			// Nameless top-level functionCall: signal a drop once rather than
			// silently skipping it.
			providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDropped})
			continue
		}
		state.syntheticToolIndex++
		if !provider.emitToolCall(ctx, functionCall, "", state.syntheticToolIndex, events) {
			state.done = true
			return false
		}
	}
	return true
}

func (provider *Provider) emitDone(ctx context.Context, state *streamState, events chan<- zeroruntime.StreamEvent) {
	if state.hasUsage {
		usage, err := zeroruntime.NormalizeUsage(zeroruntime.TokenUsage{
			InputTokens:       state.inputTokens,
			OutputTokens:      state.outputTokens,
			ReasoningTokens:   state.reasoningTokens,
			CachedInputTokens: state.cachedTokens,
		})
		if err == nil {
			providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventUsage, Usage: usage})
		}
	}
	providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone, FinishReason: state.finishReason})
	state.done = true
}

func (provider *Provider) emitToolCall(ctx context.Context, functionCall functionCall, signature string, syntheticIndex int, events chan<- zeroruntime.StreamEvent) bool {
	id := functionCall.ID
	if id == "" {
		id = fmt.Sprintf("gemini_tool_%d", syntheticIndex)
	}
	args, err := normalizeFunctionCallArgs(firstNonNil(functionCall.Args, functionCall.Arguments), functionCall.Name)
	if err != nil {
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventError, Error: provider.redact("provider error: " + err.Error())})
		return false
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventError, Error: provider.redact("provider error: " + err.Error())})
		return false
	}
	providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: id, ToolName: functionCall.Name, ToolCallSignature: signature})
	providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: id, ArgumentsFragment: string(encoded)})
	providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: id})
	return true
}

func (provider *Provider) emitHTTPError(ctx context.Context, response *http.Response, events chan<- zeroruntime.StreamEvent) {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	message := response.Status
	if parsed := parseErrorMessage(body); parsed != "" {
		message = parsed
	} else if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
		message = trimmed
	}
	providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
		Type:  zeroruntime.StreamEventError,
		Error: provider.classifiedError(response.StatusCode, message),
	})
}

func (provider *Provider) geminiRequest(request zeroruntime.CompletionRequest) (generateContentRequest, error) {
	systemInstruction, contents, err := mapMessages(request.Messages)
	if err != nil {
		return generateContentRequest{}, err
	}
	if len(contents) == 0 {
		return generateContentRequest{}, errors.New("zero Gemini provider requires at least one non-system message")
	}

	mapped := generateContentRequest{
		SystemInstruction: systemInstruction,
		Contents:          contents,
		GenerationConfig:  generationConfig{MaxOutputTokens: provider.maxTokens},
	}
	// Thinking: enable a budget when a reasoning effort was requested. Omitted
	// otherwise so default requests are unchanged.
	if budget := thinkingBudgetForEffort(request.ReasoningEffort); budget > 0 {
		mapped.GenerationConfig.ThinkingConfig = &thinkingConfig{ThinkingBudget: budget}
	}
	if len(request.Tools) > 0 {
		declarations := make([]geminiFunctionDeclaration, 0, len(request.Tools))
		for _, tool := range request.Tools {
			declarations = append(declarations, geminiFunctionDeclaration{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  sanitizeGeminiSchema(tool.Parameters),
			})
		}
		mapped.Tools = []geminiToolGroup{{FunctionDeclarations: declarations}}
	}
	return mapped, nil
}

// geminiSchemaFields is the subset of JSON Schema keywords Google's
// functionDeclarations[].parameters accepts (the Generative AI Schema type).
// Anything outside it — most notably OpenAI's `additionalProperties`, which Zero
// emits on every tool's parameters and which Gemini 400s on ("Unknown name
// \"additionalProperties\" … Cannot find field") — must be dropped before the
// request goes out. Kept as an allowlist rather than a denylist so a schema
// keyword Zero (or an MCP server) adds later can't silently leak an unsupported
// field into the Gemini payload.
var geminiSchemaFields = map[string]bool{
	"type": true, "format": true, "title": true, "description": true,
	"nullable": true, "enum": true, "items": true, "properties": true,
	"required": true, "anyOf": true, "propertyOrdering": true, "default": true,
	"minimum": true, "maximum": true, "minItems": true, "maxItems": true,
	"minLength": true, "maxLength": true, "minProperties": true,
	"maxProperties": true, "pattern": true, "example": true,
}

// sanitizeGeminiSchema returns a copy of a JSON-Schema map keeping only the
// keywords Gemini supports (geminiSchemaFields), recursing through the nested
// schemas under `properties`, `items`, and `anyOf`. Returns nil for a nil input
// so a parameterless tool stays parameterless.
func sanitizeGeminiSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	out := make(map[string]any, len(schema))
	for key, value := range schema {
		if !geminiSchemaFields[key] {
			continue
		}
		switch key {
		case "properties":
			if props, ok := value.(map[string]any); ok {
				cleaned := make(map[string]any, len(props))
				for name, sub := range props {
					if subMap, ok := sub.(map[string]any); ok {
						cleaned[name] = sanitizeGeminiSchema(subMap)
					} else {
						cleaned[name] = sub
					}
				}
				value = cleaned
			}
		case "items":
			if subMap, ok := value.(map[string]any); ok {
				value = sanitizeGeminiSchema(subMap)
			}
		case "anyOf":
			if variants, ok := value.([]any); ok {
				cleaned := make([]any, len(variants))
				for i, variant := range variants {
					if subMap, ok := variant.(map[string]any); ok {
						cleaned[i] = sanitizeGeminiSchema(subMap)
					} else {
						cleaned[i] = variant
					}
				}
				value = cleaned
			}
		}
		out[key] = value
	}
	return out
}

func mapMessages(messages []zeroruntime.Message) (*geminiContent, []geminiContent, error) {
	systemParts := []geminiPart{}
	contents := []geminiContent{}
	toolNamesByID := make(map[string]string)

	for _, message := range messages {
		content := message.Content
		hasContent := strings.TrimSpace(content) != ""
		switch message.Role {
		case zeroruntime.MessageRoleSystem:
			if hasContent {
				systemParts = append(systemParts, geminiPart{Text: content})
			}
		case zeroruntime.MessageRoleTool:
			if message.ToolCallID == "" {
				return nil, nil, errors.New("zero Gemini provider requires toolCallId on tool result messages")
			}
			name := toolNamesByID[message.ToolCallID]
			if name == "" {
				name = message.ToolCallID
			}
			appendUserParts(&contents, []geminiPart{{
				FunctionResponse: &geminiFunctionResponse{
					ID:       message.ToolCallID,
					Name:     name,
					Response: map[string]interface{}{"result": content},
				},
			}})
		case zeroruntime.MessageRoleAssistant:
			parts := []geminiPart{}
			if hasContent {
				parts = append(parts, geminiPart{Text: content})
			}
			for _, toolCall := range message.ToolCalls {
				args, err := parseToolArguments(toolCall.Arguments, toolCall.Name)
				if err != nil {
					return nil, nil, err
				}
				toolNamesByID[toolCall.ID] = toolCall.Name
				parts = append(parts, geminiPart{
					FunctionCall: &geminiFunctionCall{
						ID:   toolCall.ID,
						Name: toolCall.Name,
						Args: args,
					},
					// Replay the thought signature so multi-turn function calling with
					// thinking is not rejected. Empty for non-thinking runs.
					ThoughtSignature: toolCall.Signature,
				})
			}
			if len(parts) > 0 {
				contents = append(contents, geminiContent{Role: "model", Parts: parts})
			}
		default:
			parts := []geminiPart{}
			if hasContent {
				parts = append(parts, geminiPart{Text: content})
			}
			for _, image := range message.Images {
				parts = append(parts, geminiPart{InlineData: &geminiInlineData{
					MimeType: image.MediaType,
					Data:     base64.StdEncoding.EncodeToString(image.Data),
				}})
			}
			if len(parts) > 0 {
				appendUserParts(&contents, parts)
			}
		}
	}
	var systemInstruction *geminiContent
	if len(systemParts) > 0 {
		systemInstruction = &geminiContent{Parts: systemParts}
	}
	return systemInstruction, contents, nil
}

func appendUserParts(contents *[]geminiContent, parts []geminiPart) {
	if len(*contents) > 0 && (*contents)[len(*contents)-1].Role == "user" {
		(*contents)[len(*contents)-1].Parts = append((*contents)[len(*contents)-1].Parts, parts...)
		return
	}
	*contents = append(*contents, geminiContent{Role: "user", Parts: parts})
}

func parseToolArguments(argumentsJSON string, toolName string) (map[string]any, error) {
	if strings.TrimSpace(argumentsJSON) == "" {
		return map[string]any{}, nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(argumentsJSON), &parsed); err != nil {
		return nil, fmt.Errorf("zero Gemini provider could not parse tool arguments for %s as JSON", toolName)
	}
	object, ok := parsed.(map[string]any)
	if !ok || object == nil {
		return nil, fmt.Errorf("zero Gemini provider requires tool arguments for %s to be a JSON object", toolName)
	}
	return object, nil
}

func normalizeFunctionCallArgs(value any, toolName string) (map[string]any, error) {
	if value == nil {
		return map[string]any{}, nil
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, fmt.Errorf("zero Gemini provider requires streamed tool arguments for %s to be a JSON object", toolName)
	}
	return object, nil
}

func parseErrorMessage(body []byte) string {
	var parsed struct {
		Error   apiError `json:"error"`
		Message string   `json:"message"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	return firstNonEmpty(parsed.Error.Message, parsed.Message)
}

func (provider *Provider) streamURL() string {
	return provider.baseURL + "/v1beta/models/" + url.PathEscape(provider.model) + ":streamGenerateContent?alt=sse"
}

func (provider *Provider) classifiedError(statusCode int, message string) string {
	return providerio.ClassifiedError(statusCode, message, provider.apiKey, provider.authHeaderValue)
}

func (provider *Provider) redact(message string) string {
	return providerio.Redact(message, provider.apiKey, provider.authHeaderValue)
}

func normalizeModel(model string) string {
	model = strings.TrimSpace(model)
	model = strings.TrimPrefix(model, "models/")
	return strings.TrimSpace(model)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

type streamState struct {
	inputTokens        int
	outputTokens       int
	reasoningTokens    int
	cachedTokens       int
	hasUsage           bool
	syntheticToolIndex int
	finishReason       string // normalized terminal stop reason (empty for normal stop)
	done               bool
}

// mapFinishReason maps a Gemini candidate finishReason onto the runtime's
// normalized terminal reasons. A normal stop ("STOP"/"") returns "".
func mapFinishReason(reason string) string {
	switch reason {
	case "", "STOP", "FINISH_REASON_UNSPECIFIED":
		return "" // normal completion
	case "MAX_TOKENS":
		return zeroruntime.FinishReasonLength
	case "SAFETY", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII", "RECITATION", "IMAGE_SAFETY":
		return zeroruntime.FinishReasonContentFilter
	default:
		// MALFORMED_FUNCTION_CALL, OTHER, LANGUAGE, UNEXPECTED_TOOL_CALL, and any
		// future non-STOP reason: surface the raw reason so a truncated/aborted turn
		// isn't mistaken for a clean completion (M3). TruncationNotice renders it as
		// "Response ended early (<reason>)".
		return reason
	}
}

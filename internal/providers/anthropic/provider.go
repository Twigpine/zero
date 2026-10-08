package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/providers/providerio"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

const defaultBaseURL = "https://api.anthropic.com"
const defaultVersion = "2023-06-01"
const defaultMaxTokens = 4096

// providerName tags reasoning blocks this adapter produces so only it replays
// them (a mid-run switch to another provider ignores foreign blocks).
const providerName = "anthropic"

// Extended-thinking budget bounds. Anthropic counts thinking tokens against
// max_tokens and requires a budget of at least 1024, so we reserve room for the
// actual response on top of the budget.
const (
	minThinkingBudget = 1024
	minResponseTokens = 4096
)

// thinkingBudgetForEffort maps a requested reasoning effort to a thinking token
// budget. 0 means "no extended thinking".
func thinkingBudgetForEffort(effort string) int {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal":
		return minThinkingBudget
	case "low":
		return 4096
	case "medium":
		return 10000
	case "high":
		return 24000
	default:
		return 0
	}
}

// resolveThinking returns the thinking budget and the max_tokens to send. When a
// budget is requested, max_tokens is raised if needed so the budget plus a
// minimum response both fit (Anthropic rejects budget >= max_tokens). enabled is
// false when no thinking was requested, leaving the request unchanged.
func resolveThinking(effort string, maxTokens int) (budget int, effectiveMax int, enabled bool) {
	budget = thinkingBudgetForEffort(effort)
	if budget <= 0 {
		return 0, maxTokens, false
	}
	effectiveMax = maxTokens
	if effectiveMax <= 0 {
		effectiveMax = defaultMaxTokens
	}
	if effectiveMax < budget+minResponseTokens {
		effectiveMax = budget + minResponseTokens
	}
	return budget, effectiveMax, true
}

// Options configures an Anthropic Messages API provider.
type Options struct {
	APIKey          string
	BaseURL         string
	Model           string
	MaxTokens       int
	Version         string
	Beta            string
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
	// StreamIdleTimeout aborts the stream if no data arrives for this long.
	// When unset, Zero uses providerio.ResolveStreamIdleTimeout — the
	// ZERO_STREAM_IDLE_TIMEOUT override or providerio.DefaultStreamIdleTimeout.
	StreamIdleTimeout time.Duration
	SessionHeader     string
}

// Provider streams completions from Anthropic's Messages API.
type Provider struct {
	apiKey            string
	baseURL           string
	model             string
	maxTokens         int
	version           string
	beta              string
	authHeader        string
	authScheme        string
	authHeaderValue   string
	customHeaders     map[string]string
	oauthResolver     providerio.TokenResolver
	httpClient        *http.Client
	userAgent         string
	streamIdleTimeout time.Duration
	sessionHeader     providerio.SessionHeader
}

// New creates an Anthropic provider.
func New(options Options) (*Provider, error) {
	model := strings.TrimSpace(options.Model)
	if model == "" {
		return nil, errors.New("anthropic provider requires a model")
	}
	maxTokens, err := providerio.PositiveOrDefault(options.MaxTokens, defaultMaxTokens, "zero Anthropic provider maxTokens")
	if err != nil {
		return nil, err
	}
	baseURL, err := providerio.NormalizeBaseURL(options.BaseURL, defaultBaseURL, "Anthropic")
	if err != nil {
		return nil, err
	}
	version := strings.TrimSpace(options.Version)
	if version == "" {
		version = defaultVersion
	}
	return &Provider{
		apiKey:            options.APIKey,
		baseURL:           baseURL,
		model:             model,
		maxTokens:         maxTokens,
		version:           version,
		beta:              strings.TrimSpace(options.Beta),
		authHeader:        strings.TrimSpace(options.AuthHeader),
		authScheme:        strings.TrimSpace(options.AuthScheme),
		authHeaderValue:   strings.TrimSpace(options.AuthHeaderValue),
		customHeaders:     providerio.CopyHeaders(options.CustomHeaders),
		oauthResolver:     options.OAuthResolver,
		httpClient:        providerio.HTTPClient(options.HTTPClient),
		userAgent:         options.UserAgent,
		streamIdleTimeout: providerio.ResolveStreamIdleTimeout(options.StreamIdleTimeout),
		sessionHeader:     providerio.NewSessionHeader(options.SessionHeader),
	}, nil
}

// StreamCompletion sends one streaming Anthropic Messages request.
func (provider *Provider) StreamCompletion(
	ctx context.Context,
	request zeroruntime.CompletionRequest,
) (<-chan zeroruntime.StreamEvent, error) {
	mapped, err := provider.anthropicRequest(request)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(mapped)
	if err != nil {
		return nil, fmt.Errorf("encode Anthropic request: %w", err)
	}

	events := make(chan zeroruntime.StreamEvent, 16)
	go func() {
		defer close(events)
		provider.stream(ctx, body, request.PromptCacheKey, events)
	}()
	return events, nil
}

func (provider *Provider) stream(ctx context.Context, body []byte, sessionID string, events chan<- zeroruntime.StreamEvent) {
	// streamCtx lets the idle watchdog abort an in-flight body read by cancelling
	// the request, which unblocks the SSE reader goroutine.
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	response, err := providerio.SendWithAuthRetry(streamCtx, provider.httpClient, http.MethodPost, provider.baseURL+"/v1/messages", body,
		providerio.AuthHeaders{
			APIKey:            provider.apiKey,
			DefaultAuthHeader: "x-api-key",
			AuthHeader:        provider.authHeader,
			AuthScheme:        provider.authScheme,
			AuthHeaderValue:   provider.authHeaderValue,
			CustomHeaders:     provider.customHeaders,
		},
		provider.oauthResolver,
		func(request *http.Request) {
			provider.sessionHeader.Apply(request, sessionID)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("anthropic-version", provider.version)
			if provider.beta != "" {
				request.Header.Set("anthropic-beta", provider.beta)
			}
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

	state := newStreamState()
	err = providerio.ScanSSEDataWithContext(streamCtx, cancelStream, response.Body, provider.streamIdleTimeout, func(data string) bool {
		return provider.emitPayload(ctx, data, state, events)
	})
	if errors.Is(err, providerio.ErrStreamIdle) || errors.Is(err, providerio.ErrStreamStalled) {
		state.closeOpen(ctx, events)
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: provider.redact("provider stream error: " + providerio.StreamTimeoutMessage(err, provider.streamIdleTimeout)),
		})
		return
	}
	if err != nil {
		state.closeOpen(ctx, events)
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventError, Error: provider.redact("provider stream error: " + err.Error())})
		return
	}
	if err := ctx.Err(); err != nil {
		state.closeOpen(ctx, events)
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventError, Error: provider.redact("provider stream error: " + err.Error())})
		return
	}
	if !state.done {
		provider.emitDone(ctx, state, events)
	}
}

func (provider *Provider) emitPayload(ctx context.Context, data string, state *streamState, events chan<- zeroruntime.StreamEvent) bool {
	var payload streamPayload
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		state.closeOpen(ctx, events)
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: provider.redact("provider stream error: malformed JSON: " + err.Error()),
		})
		state.done = true
		return false
	}

	switch payload.Type {
	case "message_start":
		if payload.Message != nil {
			state.recordUsage(payload.Message.Usage)
		}
	case "content_block_start":
		if payload.ContentBlock != nil && (payload.ContentBlock.Type == "thinking" || payload.ContentBlock.Type == "redacted_thinking") {
			state.startThinking(payload.Index, payload.ContentBlock)
			return true
		}
		if payload.ContentBlock != nil && payload.ContentBlock.Type == "tool_use" {
			if payload.ContentBlock.ID == "" || payload.ContentBlock.Name == "" {
				// A tool_use block without a usable id/name can't be dispatched.
				// Signal a drop once so the agent can ask the model to retry
				// instead of silently ending the turn (mirrors OpenAI).
				providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDropped})
				return true
			}
			state.startTool(ctx, payload.Index, payload.ContentBlock.ID, payload.ContentBlock.Name, events)
			if len(payload.ContentBlock.Input) > 0 {
				encoded, err := json.Marshal(payload.ContentBlock.Input)
				if err == nil {
					state.deltaTool(ctx, payload.Index, string(encoded), events)
				}
			}
		}
	case "content_block_delta":
		if payload.Delta == nil {
			return true
		}
		switch payload.Delta.Type {
		case "text_delta":
			if payload.Delta.Text != "" {
				providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: payload.Delta.Text})
			}
		case "input_json_delta":
			if payload.Delta.PartialJSON != "" {
				state.deltaTool(ctx, payload.Index, payload.Delta.PartialJSON, events)
			}
		case "thinking_delta", "signature_delta":
			state.deltaThinking(payload.Index, payload.Delta)
		}
	case "content_block_stop":
		state.stopThinking(payload.Index)
		state.stopTool(ctx, payload.Index, events)
	case "message_delta":
		if payload.Usage != nil {
			state.recordUsage(*payload.Usage)
		}
		if payload.Delta != nil {
			if reason := mapStopReason(payload.Delta.StopReason); reason != "" {
				state.finishReason = reason
			}
		}
	case "message_stop":
		provider.emitDone(ctx, state, events)
	case "error":
		message := "Anthropic stream error"
		if payload.Error != nil {
			message = firstNonEmpty(payload.Error.Message, payload.Error.Type, message)
		}
		state.closeOpen(ctx, events)
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: provider.classifiedError(http.StatusInternalServerError, message),
		})
		state.done = true
		return false
	}
	return true
}

func (provider *Provider) emitDone(ctx context.Context, state *streamState, events chan<- zeroruntime.StreamEvent) {
	state.closeOpen(ctx, events)
	if state.hasInputUsage || state.hasOutputUsage {
		usage, err := zeroruntime.NormalizeUsage(zeroruntime.TokenUsage{
			// Anthropic reports input_tokens (uncached), cache_read, and
			// cache_creation SEPARATELY. The runtime models cache-read and
			// cache-write as disjoint SUBSETS of total input, so report the full
			// prompt size as InputTokens, cache hits as CachedInputTokens, and
			// cache creation as CacheWriteTokens (priced at the premium rate).
			InputTokens:       state.inputTokens + state.cacheReadTokens + state.cacheCreationTokens,
			CachedInputTokens: state.cacheReadTokens,
			CacheWriteTokens:  state.cacheCreationTokens,
			OutputTokens:      state.outputTokens,
		})
		if err == nil {
			providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventUsage, Usage: usage})
		}
	}
	providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
		Type:            zeroruntime.StreamEventDone,
		FinishReason:    state.finishReason,
		ReasoningBlocks: state.reasoningBlocks,
	})
	state.done = true
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

func (provider *Provider) anthropicRequest(request zeroruntime.CompletionRequest) (messagesRequest, error) {
	system, messages, err := mapMessages(request.Messages)
	if err != nil {
		return messagesRequest{}, err
	}
	if len(messages) == 0 {
		return messagesRequest{}, errors.New("zero Anthropic provider requires at least one non-system message")
	}

	mapped := messagesRequest{
		Model:     provider.model,
		MaxTokens: provider.maxTokens,
		Messages:  messages,
		Stream:    true,
	}
	// Extended thinking: enable when a reasoning effort was requested. The budget
	// is counted against max_tokens, so raise max_tokens to leave room for the
	// response. Temperature is intentionally left unset (Anthropic requires the
	// default when thinking is on).
	if budget, effectiveMax, enabled := resolveThinking(request.ReasoningEffort, provider.maxTokens); enabled {
		mapped.MaxTokens = effectiveMax
		mapped.Thinking = &thinkingConfig{Type: "enabled", BudgetTokens: budget}
	}
	// Prompt caching: send the (stable, per-run) system prompt as a cacheable text
	// block so the system instructions + tool definitions are not re-billed on
	// every turn. The cache_control breakpoint on the last system block covers the
	// whole system prompt; the breakpoint on the last tool covers all tool defs.
	// Cache hits show up as cache_read_input_tokens in the usage. Non-caching
	// providers ignore the field, and Anthropic accepts an empty/omitted system.
	if strings.TrimSpace(system) != "" {
		mapped.System = []systemBlock{{
			Type:         "text",
			Text:         system,
			CacheControl: &cacheControl{Type: cacheEphemeral},
		}}
	}
	if len(request.Tools) > 0 {
		mapped.Tools = make([]anthropicTool, 0, len(request.Tools))
		for _, tool := range request.Tools {
			mapped.Tools = append(mapped.Tools, anthropicTool{
				Name:        tool.Name,
				Description: tool.Description,
				InputSchema: tool.Parameters,
			})
		}
		mapped.Tools[len(mapped.Tools)-1].CacheControl = &cacheControl{Type: cacheEphemeral}
	}
	applyMessageCacheBreakpoints(mapped.Messages)
	return mapped, nil
}

// applyMessageCacheBreakpoints marks the last content block of the final two
// messages with cache_control so the conversation transcript is cached
// turn-over-turn, not just the system prompt and tool definitions. Two message
// breakpoints (not one) keep the previous turn's prefix a cache hit while the
// newest suffix is being written. Anthropic allows at most 4 breakpoints per
// request: system + tools use two above, these use the remaining two. Thinking
// blocks cannot carry cache_control, so the marker goes on the last block that
// can.
func applyMessageCacheBreakpoints(messages []anthropicMessage) {
	marked := 0
	for i := len(messages) - 1; i >= 0 && marked < 2; i-- {
		blocks := contentBlocks(messages[i].Content)
		for j := len(blocks) - 1; j >= 0; j-- {
			blockType, _ := blocks[j]["type"].(string)
			if blockType == "thinking" || blockType == "redacted_thinking" {
				continue
			}
			blocks[j]["cache_control"] = map[string]any{"type": cacheEphemeral}
			messages[i].Content = blocks
			marked++
			break
		}
	}
}

func mapMessages(messages []zeroruntime.Message) (string, []anthropicMessage, error) {
	systemParts := []string{}
	mapped := []anthropicMessage{}
	for _, message := range messages {
		content := message.Content
		hasContent := strings.TrimSpace(content) != ""
		switch message.Role {
		case zeroruntime.MessageRoleSystem:
			if hasContent {
				systemParts = append(systemParts, content)
			}
		case zeroruntime.MessageRoleTool:
			if message.ToolCallID == "" {
				return "", nil, errors.New("zero Anthropic provider requires toolCallId on tool result messages")
			}
			appendUserBlocks(&mapped, []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": message.ToolCallID,
				"content":     content,
			}})
		case zeroruntime.MessageRoleAssistant:
			blocks := []map[string]any{}
			// Replay preserved thinking blocks first: Anthropic requires them at the
			// start of the assistant turn and rejects tool conversations that drop
			// them. Blocks from other providers are ignored.
			for _, block := range message.Reasoning {
				if block.Provider != providerName {
					continue
				}
				switch block.Type {
				case "thinking":
					blocks = append(blocks, map[string]any{"type": "thinking", "thinking": block.Text, "signature": block.Signature})
				case "redacted_thinking":
					blocks = append(blocks, map[string]any{"type": "redacted_thinking", "data": block.Data})
				}
			}
			if hasContent {
				blocks = append(blocks, map[string]any{"type": "text", "text": content})
			}
			for _, toolCall := range message.ToolCalls {
				input, err := parseToolArguments(toolCall.Arguments, toolCall.Name)
				if err != nil {
					return "", nil, err
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    toolCall.ID,
					"name":  toolCall.Name,
					"input": input,
				})
			}
			if len(blocks) == 0 {
				continue
			}
			var messageContent any = blocks
			if len(blocks) == 1 && blocks[0]["type"] == "text" {
				messageContent = blocks[0]["text"]
			}
			mapped = append(mapped, anthropicMessage{Role: "assistant", Content: messageContent})
		default:
			blocks := []map[string]any{}
			if hasContent {
				blocks = append(blocks, map[string]any{"type": "text", "text": content})
			}
			for _, image := range message.Images {
				blocks = append(blocks, map[string]any{
					"type": "image",
					"source": map[string]any{
						"type":       "base64",
						"media_type": image.MediaType,
						"data":       base64.StdEncoding.EncodeToString(image.Data),
					},
				})
			}
			if len(blocks) > 0 {
				appendUserBlocks(&mapped, blocks)
			}
		}
	}
	return strings.Join(systemParts, "\n\n"), mapped, nil
}

func appendUserBlocks(messages *[]anthropicMessage, blocks []map[string]any) {
	if len(*messages) > 0 && (*messages)[len(*messages)-1].Role == "user" {
		last := &(*messages)[len(*messages)-1]
		last.Content = append(contentBlocks(last.Content), blocks...)
		return
	}
	*messages = append(*messages, anthropicMessage{Role: "user", Content: blocks})
}

func contentBlocks(content any) []map[string]any {
	if content == nil {
		return nil
	}
	if text, ok := content.(string); ok {
		return []map[string]any{{"type": "text", "text": text}}
	}
	if blocks, ok := content.([]map[string]any); ok {
		return blocks
	}
	return nil
}

func parseToolArguments(argumentsJSON string, toolName string) (map[string]any, error) {
	if strings.TrimSpace(argumentsJSON) == "" {
		return map[string]any{}, nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(argumentsJSON), &parsed); err != nil {
		return nil, fmt.Errorf("zero Anthropic provider could not parse tool arguments for %s as JSON", toolName)
	}
	object, ok := parsed.(map[string]any)
	if !ok || object == nil {
		return nil, fmt.Errorf("zero Anthropic provider requires tool arguments for %s to be a JSON object", toolName)
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

func (provider *Provider) classifiedError(statusCode int, message string) string {
	return providerio.ClassifiedError(statusCode, message, provider.apiKey, provider.authHeaderValue)
}

func (provider *Provider) redact(message string) string {
	return providerio.Redact(message, provider.apiKey, provider.authHeaderValue)
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

type toolBlock struct {
	id   string
	name string
}

type streamState struct {
	tools               map[int]toolBlock
	thinking            map[int]*thinkingBuf // open thinking/redacted_thinking blocks by index
	reasoningBlocks     []zeroruntime.ReasoningBlock
	inputTokens         int
	outputTokens        int
	cacheReadTokens     int // prompt-cache hits (cheap, re-billed reads)
	cacheCreationTokens int // tokens written to the cache this turn
	hasInputUsage       bool
	hasOutputUsage      bool
	finishReason        string // normalized terminal stop reason (empty for normal stop)
	done                bool
}

// thinkingBuf accumulates one extended-thinking content block as it streams.
type thinkingBuf struct {
	kind      string // "thinking" or "redacted_thinking"
	text      strings.Builder
	signature string
	data      string
}

// mapStopReason maps Anthropic's message_delta stop_reason onto the runtime's
// normalized terminal reasons. A normal stop ("end_turn"/"tool_use"/"stop_sequence"/"")
// returns "".
func mapStopReason(reason string) string {
	switch reason {
	case "max_tokens":
		return zeroruntime.FinishReasonLength
	case "refusal":
		// The model declined to respond — surface it as content-filtered so the
		// empty/partial turn isn't mistaken for a normal completion (M4).
		return zeroruntime.FinishReasonContentFilter
	default:
		// end_turn / tool_use / stop_sequence (and "") are normal completions.
		// pause_turn is also normal here: it is Anthropic's long-running-turn pause
		// (used with server-side tools), where the turn is NOT truncated or refused —
		// the client is expected to resume it by sending the response back. Treating
		// it as a non-normal early stop would fire a spurious truncation notice, so it
		// maps to "" like the other clean stops.
		return ""
	}
}

func newStreamState() *streamState {
	return &streamState{tools: make(map[int]toolBlock), thinking: make(map[int]*thinkingBuf)}
}

// startThinking opens a thinking/redacted_thinking block at the given index.
// redacted_thinking arrives whole (its opaque data is on content_block_start).
func (state *streamState) startThinking(index int, block *contentBlock) {
	buf := &thinkingBuf{kind: block.Type}
	if block.Type == "redacted_thinking" {
		buf.data = block.Data
	}
	state.thinking[index] = buf
}

// deltaThinking applies a thinking_delta (reasoning text) or signature_delta to
// the open block at the index.
func (state *streamState) deltaThinking(index int, delta *streamDelta) {
	buf, ok := state.thinking[index]
	if !ok {
		return
	}
	if delta.Thinking != "" {
		buf.text.WriteString(delta.Thinking)
	}
	if delta.Signature != "" {
		buf.signature = delta.Signature
	}
}

// stopThinking finalizes the block at the index into a preserved ReasoningBlock.
func (state *streamState) stopThinking(index int) {
	buf, ok := state.thinking[index]
	if !ok {
		return
	}
	state.reasoningBlocks = append(state.reasoningBlocks, zeroruntime.ReasoningBlock{
		Provider:  providerName,
		Type:      buf.kind,
		Text:      buf.text.String(),
		Signature: buf.signature,
		Data:      buf.data,
	})
	delete(state.thinking, index)
}

func (state *streamState) recordUsage(usage usage) {
	if usage.InputTokens != 0 {
		state.inputTokens = usage.InputTokens
		state.hasInputUsage = true
	}
	if usage.CacheReadInputTokens != 0 {
		state.cacheReadTokens = usage.CacheReadInputTokens
		state.hasInputUsage = true
	}
	if usage.CacheCreationInputTokens != 0 {
		state.cacheCreationTokens = usage.CacheCreationInputTokens
		state.hasInputUsage = true
	}
	if usage.OutputTokens != 0 {
		state.outputTokens = usage.OutputTokens
		state.hasOutputUsage = true
	}
}

func (state *streamState) startTool(ctx context.Context, index int, id string, name string, events chan<- zeroruntime.StreamEvent) {
	state.tools[index] = toolBlock{id: id, name: name}
	providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
		Type:       zeroruntime.StreamEventToolCallStart,
		ToolCallID: id,
		ToolName:   name,
	})
}

func (state *streamState) deltaTool(ctx context.Context, index int, fragment string, events chan<- zeroruntime.StreamEvent) {
	tool, ok := state.tools[index]
	if !ok || fragment == "" {
		return
	}
	providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
		Type:              zeroruntime.StreamEventToolCallDelta,
		ToolCallID:        tool.id,
		ArgumentsFragment: fragment,
	})
}

func (state *streamState) stopTool(ctx context.Context, index int, events chan<- zeroruntime.StreamEvent) {
	tool, ok := state.tools[index]
	if !ok {
		return
	}
	providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
		Type:       zeroruntime.StreamEventToolCallEnd,
		ToolCallID: tool.id,
	})
	delete(state.tools, index)
}

func (state *streamState) closeOpen(ctx context.Context, events chan<- zeroruntime.StreamEvent) {
	for index := range state.tools {
		state.stopTool(ctx, index, events)
	}
	// Finalize any thinking blocks still open at stream end — the SSE ended after
	// thinking_delta/signature_delta but before content_block_stop. Without this
	// they never reach state.reasoningBlocks and the synthetic `done` event drops
	// them, making the next Anthropic replay inconsistent. Finalize in index order
	// so multiple blocks are preserved in the order Anthropic emitted them.
	if len(state.thinking) > 0 {
		indices := make([]int, 0, len(state.thinking))
		for index := range state.thinking {
			indices = append(indices, index)
		}
		sort.Ints(indices)
		for _, index := range indices {
			state.stopThinking(index)
		}
	}
}

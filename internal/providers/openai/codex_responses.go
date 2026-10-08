package openai

// Codex Responses API adapter.
//
// The ChatGPT Codex backend (chatgpt.com/backend-api/codex/responses) serves
// the OpenAI Responses API, NOT the chat-completions API. The two schemas
// differ in both request and stream shape:
//
//	Chat-completions (what the openai provider speaks):
//	  request:  { model, messages, tools, stream, stream_options, max_completion_tokens }
//	  stream:   data: {"choices":[{"delta":{"content","tool_calls",...}}]}
//	            data: [DONE]
//
//	Responses (what the Codex backend speaks):
//	  request:  { model, input, tools, stream, max_output_tokens }
//	  input item types: "message" (role: user/assistant/system, content parts),
//	                    "function_call" (id, call_id, name, arguments),
//	                    "function_call_output" (call_id, output)
//	  stream:   event: response.created | response.in_progress
//	            event: response.output_item.added
//	            event: response.content_part.added
//	            event: response.output_text.delta / response.output_text.done
//	            event: response.function_call_arguments.delta
//	            event: response.output_item.done
//	            event: response.completed
//	            event: response.error | response.failed
//
// The previous Codex implementation reused the openai chat-completions transport
// verbatim and only changed the URL — that sent a chat-completions body to
// /responses, which the Codex backend rejects. This file replaces the transport
// with a Responses-shaped request builder, a typed SSE event dispatcher, and a
// tool-call accumulator that emits normalized StreamEventToolCallStart /
// StreamEventToolCallDelta / StreamEventToolCallEnd / StreamEventUsage /
// StreamEventDone events for the runtime.
//
// The Codex provider still uses providerio for the HTTP/auth/retry/idle-timeout
// plumbing so the OAuth 401-refresh and bearer rotation paths stay identical
// to the rest of the openai-compatible surface.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Gitlawb/zero/internal/providers/providerio"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// Codex Responses API event type names. Only the ones the Codex backend
// actually emits are listed; unknown event types are ignored.
const (
	responsesEventCreated           = "response.created"
	responsesEventInProgress        = "response.in_progress"
	responsesEventOutputItemAdded   = "response.output_item.added"
	responsesEventContentPartAdded  = "response.content_part.added"
	responsesEventOutputTextDelta   = "response.output_text.delta"
	responsesEventReasoningDelta    = "response.reasoning_summary_text.delta"
	responsesEventOutputTextDone    = "response.output_text.done"
	responsesEventFunctionArgsDelta = "response.function_call_arguments.delta"
	responsesEventCustomInputDelta  = "response.custom_tool_call_input.delta"
	responsesEventContentPartDone   = "response.content_part.done"
	responsesEventOutputItemDone    = "response.output_item.done"
	responsesEventCompleted         = "response.completed"
	responsesEventFailed            = "response.failed"
	responsesEventError             = "response.error"
	responsesEventIncomplete        = "response.incomplete"
)

const applyPatchLarkGrammar = `start: begin_patch hunk+ end_patch
begin_patch: "*** Begin Patch" LF
end_patch: "*** End Patch" LF?
hunk: add_hunk | delete_hunk | update_hunk
add_hunk: "*** Add File: " filename LF add_line+
delete_hunk: "*** Delete File: " filename LF
update_hunk: "*** Update File: " filename LF change_move? change?
filename: /(.+)/
add_line: "+" /(.*)/ LF -> line
change_move: "*** Move to: " filename LF
change: (change_context | change_line)+ eof_line?
change_context: ("@@" | "@@ " /(.+)/) LF
change_line: ("+" | "-" | " ") /(.*)/ LF
eof_line: "*** End of File" LF
%import common.LF`

// responsesRequest is the wire shape POSTed to {baseURL}/responses.
type responsesRequest struct {
	Type               string              `json:"type,omitempty"`
	Model              string              `json:"model"`
	Instructions       string              `json:"instructions"`
	PreviousResponseID string              `json:"previous_response_id,omitempty"`
	Input              []inputItem         `json:"input"`
	Stream             bool                `json:"stream"`
	Store              bool                `json:"store"`
	ParallelToolCalls  bool                `json:"parallel_tool_calls"`
	MaxOutputTokens    int                 `json:"max_output_tokens,omitempty"`
	Tools              []responsesTool     `json:"tools,omitempty"`
	Reasoning          *responsesReasoning `json:"reasoning,omitempty"`
	ServiceTier        string              `json:"service_tier,omitempty"`
	PromptCacheKey     string              `json:"prompt_cache_key,omitempty"`
}

// responsesReasoning carries the reasoning controls for the Responses API. The
// chat-completions `reasoning_effort` field is nested under `reasoning.effort`
// here; omitted entirely when the caller requests no (or an unsupported) effort.
type responsesReasoning struct {
	Effort string `json:"effort,omitempty"`
	// Summary requests a streamed reasoning summary ("auto" lets the API pick a
	// level). Without it the backend emits no reasoning events, so a long thinking
	// phase produces zero visible output and reads as a hang in the UI.
	Summary string `json:"summary,omitempty"`
}

// inputItem is one element of the Responses `input` array. The Type field
// dispatches between message / function_call / function_call_output.
type inputItem struct {
	Type    string        `json:"type"`
	Role    string        `json:"role,omitempty"`
	Content []contentItem `json:"content,omitempty"`
	// function_call fields
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Input     string `json:"input,omitempty"`
	// function_call_output fields
	Output any `json:"output,omitempty"`
}

// contentItem is one element of a message's `content` array. Type dispatches
// between input_text / output_text / input_image.
type contentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type responsesTool struct {
	Type        string                            `json:"type"`
	Name        string                            `json:"name"`
	Description string                            `json:"description,omitempty"`
	Parameters  map[string]any                    `json:"parameters,omitempty"`
	Format      *zeroruntime.ToolDefinitionFormat `json:"format,omitempty"`
}

// responsesEvent is the decoded form of one Responses SSE data payload.
// Only the fields each event type actually carries are populated.
type responsesEvent struct {
	Type string `json:"type"`
	// delta payloads (response.output_text.delta / function_call_arguments.delta)
	Delta string `json:"delta,omitempty"`
	// item payloads (response.output_item.added / done)
	ItemID string `json:"item_id,omitempty"`
	CallID string `json:"call_id,omitempty"`
	// OutputIndex is a *int so a real 0 (the first output) is distinguishable from
	// "absent" — a plain int defaulting to 0 dropped a no-item_id call's args (M1).
	OutputIndex *int         `json:"output_index,omitempty"`
	Item        *itemPayload `json:"item,omitempty"`
	// completed / failed / incomplete
	Response *responsePayload `json:"response,omitempty"`
	// error
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Param   string `json:"param,omitempty"`
}

type itemPayload struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Input     string `json:"input,omitempty"`
	Status    string `json:"status,omitempty"`
}

type responsePayload struct {
	ID     string        `json:"id"`
	Status string        `json:"status"`
	Usage  *usagePayload `json:"usage,omitempty"`
	Error  *errorPayload `json:"error,omitempty"`
}

type usagePayload struct {
	InputTokens         int            `json:"input_tokens"`
	OutputTokens        int            `json:"output_tokens"`
	TotalTokens         int            `json:"total_tokens"`
	OutputTokensDetails *outputDetails `json:"output_tokens_details,omitempty"`
	InputTokensDetails  *inputDetails  `json:"input_tokens_details,omitempty"`
}

type outputDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type inputDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

type errorPayload struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// toolCallBuilder accumulates one in-flight function_call across its
// response.output_item.added, the argument deltas, and the final
// response.output_item.done.
type toolCallBuilder struct {
	ID        string
	CallID    string
	Name      string
	Arguments strings.Builder
	Freeform  bool
	started   bool
	ended     bool
}

// responsesState tracks in-flight tool calls and whether the terminal
// event has been emitted. It is the Responses-API equivalent of the
// chat-completions toolState in provider.go.
type responsesState struct {
	toolCalls  map[string]*toolCallBuilder
	toolOrder  []string
	text       strings.Builder
	responseID string
	usage      *usagePayload
	emitted    bool
	done       bool
}

func newResponsesState() *responsesState {
	return &responsesState{toolCalls: map[string]*toolCallBuilder{}}
}

func (state *responsesState) outputInputItems() []inputItem {
	items := make([]inputItem, 0, len(state.toolOrder)+1)
	if state.text.Len() > 0 {
		items = append(items, inputItem{
			Type:    "message",
			Role:    "assistant",
			Content: []contentItem{{Type: "output_text", Text: state.text.String()}},
		})
	}
	for _, key := range state.toolOrder {
		builder := state.toolCalls[key]
		if builder == nil || builder.ID == "" || builder.Name == "" {
			continue
		}
		callID := builder.CallID
		if callID == "" {
			callID = builder.ID
		}
		itemType := "function_call"
		if builder.Freeform {
			itemType = "custom_tool_call"
		}
		item := inputItem{
			Type:   itemType,
			ID:     builder.ID,
			CallID: callID,
			Name:   builder.Name,
		}
		if builder.Freeform {
			item.Input = builder.Arguments.String()
		} else {
			item.Arguments = builder.Arguments.String()
		}
		items = append(items, item)
	}
	return items
}

// buildResponsesRequest converts a runtime CompletionRequest into the
// Responses API wire format. System messages fold into a top-level
// `instructions` field; everything else (user/assistant turns, tool
// calls, tool results) becomes items in the `input` array.
func (p *CodexProvider) buildResponsesRequest(request zeroruntime.CompletionRequest) (*responsesRequest, error) {
	if p.inner == nil || strings.TrimSpace(p.inner.model) == "" {
		return nil, errors.New("codex provider: model is required")
	}
	req := &responsesRequest{
		Model:             p.inner.model,
		Stream:            true,
		ParallelToolCalls: true,
		MaxOutputTokens:   p.inner.maxTokens,
		PromptCacheKey:    request.PromptCacheKey,
	}
	instructions := []string{}
	customCallIDs := map[string]string{}
	for _, msg := range request.Messages {
		switch msg.Role {
		case zeroruntime.MessageRoleSystem:
			if content := strings.TrimSpace(msg.Content); content != "" {
				instructions = append(instructions, content)
			}
		case zeroruntime.MessageRoleUser:
			req.Input = append(req.Input, p.userInputItem(msg))
		case zeroruntime.MessageRoleAssistant:
			items, err := p.assistantInputItems(msg)
			if err != nil {
				return nil, err
			}
			req.Input = append(req.Input, items...)
			for _, call := range msg.ToolCalls {
				if call.Freeform && call.ID != "" {
					callID := call.ProviderCallID
					if callID == "" {
						callID = call.ID
					}
					customCallIDs[call.ID] = callID
				}
			}
		case zeroruntime.MessageRoleTool:
			if msg.ToolCallID == "" {
				return nil, fmt.Errorf("codex provider: tool message missing tool call id")
			}
			outputType := "function_call_output"
			callID := msg.ToolCallProviderID
			if callID == "" {
				callID = msg.ToolCallID
			}
			if customID, custom := customCallIDs[msg.ToolCallID]; custom {
				outputType = "custom_tool_call_output"
				callID = customID
			}
			req.Input = append(req.Input, inputItem{
				Type:   outputType,
				CallID: callID,
				Output: msg.Content,
			})
		default:
			return nil, fmt.Errorf("codex provider: unsupported message role %q", msg.Role)
		}
	}
	req.Instructions = strings.Join(instructions, "\n\n")
	for _, tool := range request.Tools {
		definition := tool
		if definition.Name == "apply_patch" && definition.Type == zeroruntime.ToolDefinitionFreeform {
			definition.Description = "The apply_patch tool edits files from a raw structured patch. Use absolute file paths in patch headers to target a granted extra write root. Do not wrap the patch in JSON."
			definition.Format = &zeroruntime.ToolDefinitionFormat{Type: "grammar", Syntax: "lark", Definition: applyPatchLarkGrammar}
		}
		wireTool := responsesTool{
			Type:        string(definition.Type),
			Name:        definition.Name,
			Description: definition.Description,
			Parameters:  definition.Parameters,
			Format:      definition.Format,
		}
		if wireTool.Type == "" {
			wireTool.Type = string(zeroruntime.ToolDefinitionFunction)
		}
		if wireTool.Type == string(zeroruntime.ToolDefinitionFreeform) {
			wireTool.Parameters = nil
		}
		req.Tools = append(req.Tools, wireTool)
	}
	// Codex / o-series reasoning models take the effort tier nested under
	// `reasoning` on the Responses API (the chat-completions `reasoning_effort`
	// moved here). Reuse the chat normalizer so only API-accepted values are sent
	// and an empty or unsupported effort simply omits the field — without this the
	// caller's chosen effort was silently dropped for every Codex model.
	if effort := openAIReasoningEffort(request.ReasoningEffort); effort != "" {
		// Summary "auto" makes the backend stream reasoning_summary_text deltas so a
		// long thinking phase shows live progress instead of looking hung.
		req.Reasoning = &responsesReasoning{Effort: effort, Summary: "auto"}
	}
	if tier := openAIServiceTier(request.ServiceTier); tier != "" {
		req.ServiceTier = tier
	}
	return req, nil
}

// userInputItem renders a user message as a Responses `message` item with
// one input_text part (and one input_image part per attached image, if
// any). The image bytes are base64-encoded into a data: URI exactly like
// the chat-completions transport does.
func (p *CodexProvider) userInputItem(msg zeroruntime.Message) inputItem {
	parts := []contentItem{}
	if msg.Content != "" {
		parts = append(parts, contentItem{Type: "input_text", Text: msg.Content})
	}
	for _, image := range msg.Images {
		encoded := base64.StdEncoding.EncodeToString(image.Data)
		parts = append(parts, contentItem{
			Type:     "input_image",
			ImageURL: "data:" + image.MediaType + ";base64," + encoded,
		})
	}
	return inputItem{Type: "message", Role: "user", Content: parts}
}

// assistantInputItems renders an assistant turn as either a single
// `message` (text-only) or a `message` followed by one `function_call`
// per tool call. Tool-call IDs use ToolCall.ID directly — Responses
// accepts `id` and `call_id` interchangeably, and the runtime already
// guarantees non-empty IDs at construction time.
func (p *CodexProvider) assistantInputItems(msg zeroruntime.Message) ([]inputItem, error) {
	items := []inputItem{}
	if msg.Content != "" {
		items = append(items, inputItem{
			Type:    "message",
			Role:    "assistant",
			Content: []contentItem{{Type: "output_text", Text: msg.Content}},
		})
	}
	for _, tc := range msg.ToolCalls {
		if tc.ID == "" {
			return nil, errors.New("codex provider: assistant tool call missing id")
		}
		if tc.Name == "" {
			return nil, errors.New("codex provider: assistant tool call missing name")
		}
		callID := tc.ProviderCallID
		if callID == "" {
			callID = tc.ID
		}
		itemType := "function_call"
		if tc.Freeform {
			itemType = "custom_tool_call"
		}
		item := inputItem{
			Type:   itemType,
			ID:     tc.ID,
			CallID: callID,
			Name:   tc.Name,
		}
		if tc.Freeform {
			item.Input = tc.Arguments
		} else {
			item.Arguments = tc.Arguments
		}
		items = append(items, item)
	}
	return items, nil
}

// streamResponses sends the prebuilt Responses body, parses the typed SSE
// event stream from the Codex backend, and emits runtime events. It
// uses the shared providerio plumbing for auth (including the 401
// OAuth-refresh retry), the SSE scanner (with the idle watchdog), and
// the upstream-unreachable humanizer. Returns when the stream is
// exhausted, an error event is emitted, or ctx is cancelled.
func (p *CodexProvider) streamResponses(
	ctx context.Context,
	body []byte,
	events chan<- zeroruntime.StreamEvent,
) {
	streamCtx, cancelStream := context.WithCancel(ctx)
	defer cancelStream()

	inner := p.inner
	response, err := providerio.SendWithAuthRetry(streamCtx, inner.httpClient, http.MethodPost, inner.endpoint, body,
		providerio.AuthHeaders{
			APIKey:            inner.apiKey,
			DefaultAuthHeader: "Authorization",
			DefaultAuthScheme: "Bearer",
			AuthHeader:        inner.authHeader,
			AuthScheme:        inner.authScheme,
			AuthHeaderValue:   inner.authHeaderValue,
			CustomHeaders:     inner.customHeaders,
		},
		inner.oauthResolver,
		func(request *http.Request) {
			request.Header.Set("Content-Type", "application/json")
			// injectCodexHeaders sets originator, chatgpt-account-id, and a
			// branded User-Agent. It also runs on the 401-refresh retry, so
			// per-request state (account id, fresh token) is re-derivable.
			p.injectCodexHeaders(request)
		}, 0)
	if err != nil {
		// Surface ctx errors verbatim so caller-driven cancels are not
		// mislabeled as upstream outages (same posture as the openai
		// provider's stream()).
		if ctxErr := ctx.Err(); ctxErr != nil {
			providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
				Type:  zeroruntime.StreamEventError,
				Error: p.redact("provider stream error: " + ctxErr.Error()),
			})
			return
		}
		if humanized, ok := providerio.UpstreamUnreachable(err.Error()); ok {
			providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
				Type:  zeroruntime.StreamEventError,
				Error: p.redact(humanized),
			})
			return
		}
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: p.redact("provider stream error: " + err.Error()),
		})
		return
	}
	defer func() {
		_ = response.Body.Close()
	}()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		p.emitResponsesHTTPError(ctx, response, events)
		return
	}

	state := newResponsesState()
	err = providerio.ScanSSEDataWithContext(streamCtx, cancelStream, response.Body, inner.streamIdleTimeout, func(data string) bool {
		return p.emitResponsesEvent(ctx, data, state, events)
	})
	if errors.Is(err, providerio.ErrStreamIdle) || errors.Is(err, providerio.ErrStreamStalled) {
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: p.redact("provider stream error: " + providerio.StreamTimeoutMessage(err, inner.streamIdleTimeout)),
		})
		return
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: p.redact("provider stream error: " + err.Error()),
		})
		return
	}
	if !state.done {
		// The Codex backend closed the stream without emitting response.completed
		// (e.g. it ran out of output tokens or hit an internal limit). Surface a
		// length-truncation finish so the runtime can react.
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:         zeroruntime.StreamEventDone,
			FinishReason: zeroruntime.FinishReasonLength,
		})
	}
}

// emitResponsesHTTPError maps a non-2xx Codex response into a single
// StreamEventError, redacting API keys / bearer tokens that may appear
// in the body. Mirrors the openai provider's emitHTTPError so callers
// see the same error shape regardless of which provider produced it.
func (p *CodexProvider) emitResponsesHTTPError(
	ctx context.Context,
	response *http.Response,
	events chan<- zeroruntime.StreamEvent,
) {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	// The Codex backend may return either the chat-completions error shape
	// (`{"error":{"message":...}}`) or a Responses-API error
	// (`{"message":...}` directly). Try both so we surface a useful
	// message either way.
	message := strings.TrimSpace(string(body))
	var chatError struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &chatError); err == nil && chatError.Error.Message != "" {
		message = chatError.Error.Message
	}
	var responsesError responsesEvent
	if err := json.Unmarshal(body, &responsesError); err == nil && responsesError.Message != "" {
		message = responsesError.Message
	}
	if message == "" {
		message = response.Status
	}
	if humanized, ok := providerio.UpstreamUnreachable(message); ok {
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: p.redact(humanized),
		})
		return
	}
	providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
		Type:  zeroruntime.StreamEventError,
		Error: p.classifiedError(response.StatusCode, message),
	})
}

// emitResponsesEvent decodes one Responses SSE data payload and converts
// it into zero or more runtime events. Returns false to stop the scan
// after emitting a terminal event (error / completion-with-error).
func (p *CodexProvider) emitResponsesEvent(
	ctx context.Context,
	data string,
	state *responsesState,
	events chan<- zeroruntime.StreamEvent,
) bool {
	var event responsesEvent
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: p.redact("provider stream error: malformed Responses event: " + err.Error()),
		})
		state.done = true
		return false
	}
	return p.emitParsedResponsesEvent(ctx, &event, state, events)
}

// emitParsedResponsesEvent converts one already-decoded Responses event into
// zero or more runtime events. WebSocket sessions use this path to avoid
// decoding each frame twice; the SSE path retains emitResponsesEvent above.
func (p *CodexProvider) emitParsedResponsesEvent(
	ctx context.Context,
	event *responsesEvent,
	state *responsesState,
	events chan<- zeroruntime.StreamEvent,
) bool {
	if event.Type == "" {
		// A payload that parses as JSON but carries no `type` field is a
		// malformed Responses event — the type is the discriminator that
		// tells us how to interpret the rest of the payload. Surface it as
		// a stream error so the runtime doesn't hang on a phantom
		// completion that will never come.
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: p.redact("provider stream error: Responses event missing required `type` field"),
		})
		state.done = true
		return false
	}
	switch event.Type {
	case responsesEventCreated, responsesEventInProgress,
		responsesEventContentPartAdded, responsesEventContentPartDone,
		responsesEventOutputTextDone, responsesEventIncomplete:
		// Informational or terminal-without-error events we don't surface to
		// the runtime. response.incomplete is folded into a length finish at
		// scan-end (state.done stays false so the wrapper emits the
		// StreamEventDone with FinishReasonLength).
		if event.Response != nil && event.Response.ID != "" {
			state.responseID = event.Response.ID
		}
		return true
	case responsesEventOutputItemAdded:
		p.handleOutputItemAdded(ctx, event, state, events)
		return true
	case responsesEventOutputTextDelta:
		if event.Delta != "" {
			state.emitted = true
			state.text.WriteString(event.Delta)
			providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
				Type:    zeroruntime.StreamEventText,
				Content: event.Delta,
			})
		}
		return true
	case responsesEventReasoningDelta:
		// Reasoning summary deltas: surface as live "thinking" so a long reasoning
		// phase shows progress (and keeps the activity clock fresh) instead of
		// looking like a hang. Requested via reasoning.summary="auto".
		if event.Delta != "" {
			state.emitted = true
			providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
				Type:    zeroruntime.StreamEventReasoning,
				Content: event.Delta,
			})
		}
		return true
	case responsesEventFunctionArgsDelta:
		p.handleToolInputDelta(ctx, event, state, events, false)
		return true
	case responsesEventCustomInputDelta:
		p.handleToolInputDelta(ctx, event, state, events, true)
		return true
	case responsesEventOutputItemDone:
		p.handleOutputItemDone(ctx, event, state, events)
		return true
	case responsesEventCompleted, responsesEventFailed:
		return p.handleTerminalResponse(ctx, event, state, events)
	case responsesEventError:
		message := event.Message
		if event.Code != "" {
			message = strings.TrimSpace(event.Code + " " + message)
		}
		if message == "" {
			message = "provider error"
		}
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: p.redact(message),
		})
		state.done = true
		return false
	default:
		// Unknown event types are ignored (not errors). The Codex backend
		// may emit Responses events the openai Codex provider doesn't
		// recognize — surfacing them as errors would block every other
		// field in the same payload.
		return true
	}
}

// handleOutputItemAdded registers a new function_call item so subsequent
// argument deltas can be attributed, and emits StreamEventToolCallStart
// (with the function name once it is known). A `message` item is just
// tracked in case the backend ever ties text deltas to it — currently
// text deltas carry their own item_id and the message itself is a
// no-op for the runtime.
func (p *CodexProvider) handleOutputItemAdded(
	ctx context.Context,
	event *responsesEvent,
	state *responsesState,
	events chan<- zeroruntime.StreamEvent,
) {
	if event.Item == nil {
		return
	}
	switch event.Item.Type {
	case "function_call", "custom_tool_call":
		key := p.toolCallKey(event)
		if key == "" {
			return
		}
		builder, exists := state.toolCalls[key]
		if !exists {
			builder = &toolCallBuilder{ID: key, Freeform: event.Item.Type == "custom_tool_call"}
			state.toolCalls[key] = builder
			state.toolOrder = append(state.toolOrder, key)
		}
		if event.Item.CallID != "" {
			builder.CallID = event.Item.CallID
		}
		if event.Item.Name != "" {
			builder.Name = event.Item.Name
		}
		builder.Freeform = builder.Freeform || event.Item.Type == "custom_tool_call"
		if builder.Freeform && event.Item.Input != "" && builder.Arguments.Len() == 0 {
			builder.Arguments.WriteString(event.Item.Input)
		}
		state.emitted = true
		if !builder.started {
			providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
				Type:               zeroruntime.StreamEventToolCallStart,
				ToolCallID:         key,
				ToolCallProviderID: builder.CallID,
				ToolName:           builder.Name,
				ToolCallFreeform:   builder.Freeform,
			})
			builder.started = true
		}
	}
}

// handleFunctionArgsDelta appends one function-call argument fragment
// to the in-flight builder and emits StreamEventToolCallDelta. The
// Codex backend uses either `item_id` or `output_index` to attribute
// the delta; we honor both.
func (p *CodexProvider) handleToolInputDelta(
	ctx context.Context,
	event *responsesEvent,
	state *responsesState,
	events chan<- zeroruntime.StreamEvent,
	freeform bool,
) {
	key := p.toolCallKey(event)
	if key == "" {
		return
	}
	builder, ok := state.toolCalls[key]
	if !ok {
		// The Codex backend started streaming arguments before the
		// matching output_item.added (or the added event was dropped by
		// an intermediate proxy). Treat the delta as the start of a new
		// tool call with no known name yet — the eventual
		// response.output_item.done carries the final name.
		builder = &toolCallBuilder{ID: key, Freeform: freeform}
		state.toolCalls[key] = builder
		state.toolOrder = append(state.toolOrder, key)
	}
	builder.Freeform = builder.Freeform || freeform
	if event.CallID != "" && builder.CallID == "" {
		builder.CallID = event.CallID
	}
	builder.Arguments.WriteString(event.Delta)
	state.emitted = true
	providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
		Type:              zeroruntime.StreamEventToolCallDelta,
		ToolCallID:        key,
		ArgumentsFragment: event.Delta,
	})
}

// handleOutputItemDone finalizes a tool call when the item type is
// function_call. The accumulated arguments and the (possibly late)
// function name are written to the builder before StreamEventToolCallEnd
// is emitted. A non-function_call done event (typically the assistant
// `message` that wraps the text) is a no-op for the runtime.
func (p *CodexProvider) handleOutputItemDone(
	ctx context.Context,
	event *responsesEvent,
	state *responsesState,
	events chan<- zeroruntime.StreamEvent,
) {
	if event.Item == nil || (event.Item.Type != "function_call" && event.Item.Type != "custom_tool_call") {
		return
	}
	key := p.toolCallKey(event)
	if key == "" {
		return
	}
	builder, ok := state.toolCalls[key]
	if !ok {
		builder = &toolCallBuilder{ID: key, Freeform: event.Item.Type == "custom_tool_call"}
		state.toolCalls[key] = builder
		state.toolOrder = append(state.toolOrder, key)
	}
	if event.Item.Name != "" {
		builder.Name = event.Item.Name
	}
	if event.Item.CallID != "" {
		builder.CallID = event.Item.CallID
	}
	builder.Freeform = builder.Freeform || event.Item.Type == "custom_tool_call"
	finalInput := event.Item.Arguments
	if builder.Freeform {
		finalInput = event.Item.Input
	}
	if finalInput != "" && builder.Arguments.Len() == 0 {
		builder.Arguments.WriteString(finalInput)
	}
	if !builder.started {
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:               zeroruntime.StreamEventToolCallStart,
			ToolCallID:         key,
			ToolCallProviderID: builder.CallID,
			ToolName:           builder.Name,
			ToolCallFreeform:   builder.Freeform,
		})
		builder.started = true
	}
	providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
		Type:       zeroruntime.StreamEventToolCallEnd,
		ToolCallID: key,
		ToolName:   builder.Name,
	})
	builder.ended = true
	state.emitted = true
}

// handleTerminalResponse emits the final usage + done event for a
// response.completed / response.failed. response.failed carries an
// error payload that we surface as a runtime error after the usage
// chunk, so token accounting is preserved even on a failure path.
func (p *CodexProvider) handleTerminalResponse(
	ctx context.Context,
	event *responsesEvent,
	state *responsesState,
	events chan<- zeroruntime.StreamEvent,
) bool {
	if event.Response == nil {
		// A terminal event with no Response payload must still emit a terminal
		// stream event, or the collector returns a clean-looking empty completion
		// that masks a real failure (M2). A response.failed without a payload is an
		// error; a response.completed without one is just an empty (but clean) turn.
		if event.Type == responsesEventFailed {
			providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
				Type:  zeroruntime.StreamEventError,
				Error: "codex: response.failed with no error detail",
			})
		} else {
			providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone})
		}
		state.done = true
		return false
	}
	state.usage = event.Response.Usage
	state.responseID = event.Response.ID
	if event.Response.Usage != nil {
		usage := zeroruntime.Usage{
			InputTokens:  event.Response.Usage.InputTokens,
			OutputTokens: event.Response.Usage.OutputTokens,
		}
		if event.Response.Usage.InputTokensDetails != nil {
			usage.CachedInputTokens = event.Response.Usage.InputTokensDetails.CachedTokens
			usage.CacheWriteTokens = event.Response.Usage.InputTokensDetails.CacheWriteTokens
		}
		if event.Response.Usage.OutputTokensDetails != nil {
			usage.ReasoningTokens = event.Response.Usage.OutputTokensDetails.ReasoningTokens
		}
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventUsage,
			Usage: usage,
		})
	}
	if event.Response.Error != nil {
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: p.redact(event.Response.Error.Message),
		})
		state.done = true
		return false
	}
	// A response.failed carrying a payload whose `error` object is null/omitted (the
	// backend can emit a failed terminal with the reason only in `status`) must still
	// surface as an error — not fall through to a clean Done. Otherwise a real failure
	// is reported as a normal successful turn, the same silent-failure class the
	// nil-payload branch above guards (M2). A response.failed with a `status` carries
	// it in the message for context.
	if event.Type == responsesEventFailed || event.Response.Status == "failed" {
		msg := "codex: response.failed with no error detail"
		if status := strings.TrimSpace(event.Response.Status); status != "" {
			msg = fmt.Sprintf("codex: response.failed (status %q) with no error detail", status)
		}
		providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:  zeroruntime.StreamEventError,
			Error: msg,
		})
		state.done = true
		return false
	}
	providerio.SendEvent(ctx, events, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone, ResponseID: event.Response.ID})
	state.done = true
	return false
}

// toolCallKey returns the canonical id used to track an in-flight
// function call across multiple SSE events. The Codex backend may emit
// `item_id` (the call id) or rely on `output_index` (the position in
// the response.output array); we accept both for robustness.
func (p *CodexProvider) toolCallKey(event *responsesEvent) string {
	if event.ItemID != "" {
		return event.ItemID
	}
	if event.Item != nil && event.Item.ID != "" {
		return event.Item.ID
	}
	if event.Item != nil && event.Item.CallID != "" {
		return event.Item.CallID
	}
	if event.CallID != "" {
		return event.CallID
	}
	if event.OutputIndex != nil {
		// output_index is present (a *int distinguishes a real 0 — the first output
		// in the response — from "absent"), so key on it even when 0 instead of
		// dropping the call's args when no item_id is present (M1).
		return fmt.Sprintf("output-%d", *event.OutputIndex)
	}
	return ""
}

// redact is the same secret-stripping helper the openai provider uses
// for its own error messages — provided as a method on CodexProvider so
// the Responses stream path can call it without reaching across the
// inner Provider.
func (p *CodexProvider) redact(message string) string {
	if p.inner == nil {
		return providerio.Redact(message)
	}
	return providerio.Redact(message, p.inner.apiKey, p.inner.authHeaderValue)
}

// classifiedError wraps a non-2xx body with the same status-code-aware
// prefix the openai provider uses ("auth error: ", "rate limit error: ",
// "provider error: ", "provider request error: ").
func (p *CodexProvider) classifiedError(statusCode int, message string) string {
	if p.inner == nil {
		return providerio.ClassifiedError(statusCode, message)
	}
	return providerio.ClassifiedError(statusCode, message, p.inner.apiKey, p.inner.authHeaderValue)
}

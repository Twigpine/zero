package zeroruntime

import (
	"context"
	"fmt"
	"strings"
)

// CollectedStream is the non-streaming summary of provider events.
type CollectedStream struct {
	Text      string
	ToolCalls []ToolCall
	Usage     Usage
	Error     string
	// ErrorStatusCode and ErrorCause mirror StreamEvent.StatusCode/Cause for a
	// StreamEventError — see those fields for what each carries. Both are zero
	// value when Error came from a context cancellation or other non-HTTP source.
	ErrorStatusCode  int
	ErrorCause       string
	DroppedToolCalls int // malformed tool calls the provider could not dispatch
	// FinishReason is the provider's normalized terminal stop reason when the
	// response did not end normally (FinishReasonLength / FinishReasonContentFilter).
	// It is empty for a normal completion. Truncated reports whether it is set.
	FinishReason string
	// ReasoningBlocks are the response's preserved reasoning artifacts (Anthropic
	// thinking blocks) that must be replayed on the next turn. Empty for providers
	// or runs without extended thinking.
	ReasoningBlocks []ReasoningBlock
	// HasReasoning records whether the provider streamed reasoning deltas. The
	// deltas remain non-answer content, but they still prove the turn was live.
	HasReasoning bool
}

// CollectOptions provides callbacks for consumers that need live stream updates.
type CollectOptions struct {
	OnText      func(string)
	OnReasoning func(string)
	OnUsage     func(Usage)
	// OnToolCallStart fires when a tool call opens (id + tool name), and
	// OnToolCallDelta fires for each streamed argument fragment. Together they let
	// a surface render a tool call's arguments LIVE (e.g. a file being written)
	// instead of waiting for the whole call to accumulate. nil is a no-op.
	OnToolCallStart func(id, name string)
	OnToolCallDelta func(id, fragment string)
}

// SeedMessages creates the initial system and user turns for a request. It is a
// text-only convenience that delegates to SeedMessagesWithImages with no images
// (the user turn's Images stays nil, byte-identical to the prior behavior).
func SeedMessages(systemPrompt string, userPrompt string) []Message {
	return SeedMessagesWithImages(systemPrompt, userPrompt, nil)
}

// SeedMessagesWithImages creates the initial system and user turns and attaches
// any image attachments to the user turn. images may be nil (text-only). The
// images are deep-copied so the seeded message never aliases the caller's slice
// or the underlying Data bytes (a later mutation of the caller's bytes can never
// reach into the conversation history).
func SeedMessagesWithImages(systemPrompt string, userPrompt string, images []ImageBlock) []Message {
	return []Message{
		{Role: MessageRoleSystem, Content: systemPrompt},
		{Role: MessageRoleUser, Content: userPrompt, Images: CloneImageBlocks(images)},
	}
}

// CloneImageBlocks deep-copies a slice of ImageBlock, including each Data byte
// slice, so the returned blocks share no backing array with the input. It
// returns nil for a nil or empty input (preserving the text-only "no images"
// representation). Use it wherever image-carrying messages are seeded or copied
// so raw image bytes are never aliased across history/request/result copies.
func CloneImageBlocks(in []ImageBlock) []ImageBlock {
	if len(in) == 0 {
		return nil
	}
	out := make([]ImageBlock, len(in))
	for index, block := range in {
		out[index] = ImageBlock{
			MediaType: block.MediaType,
			Data:      append([]byte(nil), block.Data...),
		}
	}
	return out
}

// CollectStream drains provider events into text, tool calls, usage, and error state.
func CollectStream(ctx context.Context, events <-chan StreamEvent) CollectedStream {
	return CollectStreamWithOptions(ctx, events, CollectOptions{})
}

// CollectStreamWithOptions drains provider events and emits optional live callbacks.
func CollectStreamWithOptions(ctx context.Context, events <-chan StreamEvent, options CollectOptions) CollectedStream {
	collected := CollectedStream{}
	collector := newToolCallCollector()
	usageSeen := false
	finish := func() CollectedStream {
		collector.flush(&collected)
		if usageSeen && options.OnUsage != nil {
			options.OnUsage(collected.Usage)
		}
		return collected
	}

	for {
		select {
		case <-ctx.Done():
			collected.Error = ctx.Err().Error()
			return finish()
		case event, ok := <-events:
			if !ok {
				return finish()
			}

			// A non-normal terminal stop reason can ride on any event (providers
			// attach it to their done/terminal event). Record it regardless of
			// type so a truncated/filtered response is never mistaken for a
			// normal completion.
			if event.FinishReason != "" {
				collected.FinishReason = event.FinishReason
			}
			// Reasoning blocks (Anthropic thinking) can ride on any terminal event;
			// accumulate them regardless of type so they survive for replay.
			if len(event.ReasoningBlocks) > 0 {
				collected.ReasoningBlocks = append(collected.ReasoningBlocks, event.ReasoningBlocks...)
			}

			switch event.Type {
			case StreamEventText:
				// Accumulate in a Builder rather than `collected.Text +=`, which
				// reallocated the whole string on every chunk (O(n^2) over a long
				// streamed response). flush materializes it into collected.Text.
				collector.text.WriteString(event.Content)
				if options.OnText != nil {
					options.OnText(event.Content)
				}
			case StreamEventReasoning:
				if strings.TrimSpace(event.Content) != "" {
					collected.HasReasoning = true
				}
				if options.OnReasoning != nil {
					options.OnReasoning(event.Content)
				}
			case StreamEventToolCallStart:
				collector.start(event.ToolCallID, event.ToolCallProviderID, event.ToolName, event.ToolCallSignature, event.ToolCallFreeform)
				if options.OnToolCallStart != nil {
					options.OnToolCallStart(event.ToolCallID, event.ToolName)
				}
			case StreamEventToolCallDelta:
				collector.delta(event.ToolCallID, event.ArgumentsFragment)
				if options.OnToolCallDelta != nil {
					options.OnToolCallDelta(event.ToolCallID, event.ArgumentsFragment)
				}
			case StreamEventToolCallEnd:
				collector.end(event.ToolCallID)
			case StreamEventToolCallDropped:
				collected.DroppedToolCalls++
			case StreamEventUsage:
				collected.Usage = mergeUsageSnapshot(collected.Usage, event.Usage)
				usageSeen = true
			case StreamEventError:
				collected.Error = event.Error
				collected.ErrorStatusCode = event.StatusCode
				collected.ErrorCause = event.Cause
				return finish()
			case StreamEventDone:
				return finish()
			}
		}
	}
}

func mergeUsageSnapshot(left Usage, right Usage) Usage {
	inputTokens := left.EffectiveInputTokens()
	if value := right.EffectiveInputTokens(); value != 0 {
		inputTokens = value
	}

	outputTokens := left.EffectiveOutputTokens()
	if value := right.EffectiveOutputTokens(); value != 0 {
		outputTokens = value
	}

	cachedInputTokens := left.CachedInputTokens
	if right.CachedInputTokens != 0 {
		cachedInputTokens = right.CachedInputTokens
	}

	cacheWriteTokens := left.CacheWriteTokens
	if right.CacheWriteTokens != 0 {
		cacheWriteTokens = right.CacheWriteTokens
	}

	reasoningTokens := left.ReasoningTokens
	if right.ReasoningTokens != 0 {
		reasoningTokens = right.ReasoningTokens
	}

	usage, err := NormalizeUsage(TokenUsage{
		InputTokens:       inputTokens,
		OutputTokens:      outputTokens,
		CachedInputTokens: cachedInputTokens,
		CacheWriteTokens:  cacheWriteTokens,
		ReasoningTokens:   reasoningTokens,
	})
	if err != nil {
		return right
	}
	return usage
}

// toolCallCollector accumulates streamed tool calls in start order. Calls are
// keyed by an internal key (the ToolCallID when non-empty, or a synthetic
// per-stream key for empty IDs) so distinct simultaneous calls that share an
// empty/duplicate ID never merge. Completed calls are NOT emitted at end time;
// flush emits every collected call in one ordered pass so output always follows
// model/start order regardless of the order calls finished.
type toolCallCollector struct {
	calls       map[string]*ToolCall
	order       []string
	openEmptyID []string // stack of synthetic keys for in-flight empty-id calls
	synthetic   int
	text        strings.Builder // accumulated assistant text, materialized in flush
	// pendingEmptyDelta is the synthetic key of an empty-id call that was opened
	// by a delta arriving before any start (so its buffered arguments aren't
	// orphaned). The next empty-id start adopts it instead of opening a new call.
	pendingEmptyDelta string
}

func newToolCallCollector() *toolCallCollector {
	return &toolCallCollector{calls: make(map[string]*ToolCall)}
}

// start begins a tool call. A non-empty ID reuses any open call with that ID
// (some backends re-emit the same start); an empty ID always begins a fresh
// synthetic call so concurrent empty-id calls stay distinct.
func (collector *toolCallCollector) start(id string, providerCallID string, name string, signature string, freeform bool) {
	key := id
	if id == "" {
		// Adopt an empty-id call that a delta opened before this start, so its
		// already-buffered arguments and this start's name land on one call.
		if collector.pendingEmptyDelta != "" {
			key = collector.pendingEmptyDelta
			collector.pendingEmptyDelta = ""
		} else {
			collector.synthetic++
			key = fmt.Sprintf("\x00synthetic-%d", collector.synthetic)
			collector.openEmptyID = append(collector.openEmptyID, key)
		}
	}
	call := collector.ensure(key, id)
	if providerCallID != "" && call.ProviderCallID == "" {
		call.ProviderCallID = providerCallID
	}
	// Only set the name when non-empty and still unset, so a duplicate or
	// nameless follow-up start cannot clobber an already-resolved name.
	if name != "" && call.Name == "" {
		call.Name = name
	}
	// Preserve the reasoning signature (Gemini thoughtSignature) so it can be
	// replayed with the call on later turns.
	if signature != "" && call.Signature == "" {
		call.Signature = signature
	}
	call.Freeform = call.Freeform || freeform
}

func (collector *toolCallCollector) delta(id string, fragment string) {
	key, ok := collector.resolveKey(id)
	if !ok {
		if id == "" {
			// An empty-id delta with no in-flight empty-id call: open one and
			// remember it so a following empty-id start adopts it instead of
			// orphaning these buffered arguments under a nameless call.
			collector.synthetic++
			key = fmt.Sprintf("\x00synthetic-%d", collector.synthetic)
			collector.openEmptyID = append(collector.openEmptyID, key)
			collector.pendingEmptyDelta = key
		} else {
			key = id
		}
		collector.ensure(key, id)
	}
	collector.calls[key].Arguments += fragment
}

// end closes an in-flight call. It does not emit anything; flush does, in start
// order. For empty IDs it pops the in-flight empty-id call off the stack so a
// following empty-id delta/end can't attach to an already-closed call.
func (collector *toolCallCollector) end(id string) {
	if id == "" {
		if len(collector.openEmptyID) > 0 {
			closed := collector.openEmptyID[len(collector.openEmptyID)-1]
			collector.openEmptyID = collector.openEmptyID[:len(collector.openEmptyID)-1]
			// If a delta-opened call is closed before any start adopts it, drop
			// the pending pointer so a later start can't attach to a closed call.
			if closed == collector.pendingEmptyDelta {
				collector.pendingEmptyDelta = ""
			}
		}
	}
}

// resolveKey maps an event ID to its internal key. Empty IDs route to the most
// recently started, not-yet-ended empty-id call.
func (collector *toolCallCollector) resolveKey(id string) (string, bool) {
	if id == "" {
		if len(collector.openEmptyID) == 0 {
			return "", false
		}
		return collector.openEmptyID[len(collector.openEmptyID)-1], true
	}
	if _, ok := collector.calls[id]; ok {
		return id, true
	}
	return "", false
}

func (collector *toolCallCollector) ensure(key string, id string) *ToolCall {
	if call, ok := collector.calls[key]; ok {
		return call
	}
	call := &ToolCall{ID: id}
	collector.calls[key] = call
	collector.order = append(collector.order, key)
	return call
}

// flush emits every collected call once, in start order. Malformed (nameless)
// calls are dropped so the agent never dispatches an empty tool name.
func (collector *toolCallCollector) flush(collected *CollectedStream) {
	for _, key := range collector.order {
		call, ok := collector.calls[key]
		if !ok {
			continue
		}
		delete(collector.calls, key)
		if call.Name != "" {
			collected.ToolCalls = append(collected.ToolCalls, *call)
		} else {
			collected.DroppedToolCalls++
		}
	}
	collector.order = collector.order[:0]
	collector.openEmptyID = collector.openEmptyID[:0]
	collector.pendingEmptyDelta = ""
	// flush is the single finalization point hit before every return, so
	// materialize the accumulated text here exactly once.
	collected.Text = collector.text.String()
}

package openai

import (
	"context"
	"sort"
	"strings"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

const (
	thinkOpenTag  = "<think>"
	thinkCloseTag = "</think>"
)

type toolState struct {
	calls          map[int]*pendingToolCall
	think          thinkTagSplitter
	parseThinkTags bool
	// finishReason holds the normalized terminal stop reason (zeroruntime
	// FinishReason*) when the response ended abnormally (length/content_filter),
	// so the provider can attach it to the done event. Empty for a normal finish.
	finishReason string
	// done is set once a terminal event (error) has been emitted so the post-scan
	// path does not emit a second done after the stream already ended.
	done bool
}

type pendingToolCall struct {
	id        string
	name      string
	arguments string
	started   bool
	ended     bool
}

func newToolState(parseThinkTags bool) *toolState {
	return &toolState{calls: make(map[int]*pendingToolCall), parseThinkTags: parseThinkTags}
}

type streamEventEmitter func(zeroruntime.StreamEvent)

type thinkTagSplitter struct {
	pending    string
	inThinking bool
}

func (state *toolState) emitContent(ctx context.Context, events chan<- zeroruntime.StreamEvent, content string) {
	state.emitContentWith(content, func(event zeroruntime.StreamEvent) {
		sendEvent(ctx, events, event)
	})
}

func (state *toolState) flushContent(ctx context.Context, events chan<- zeroruntime.StreamEvent) {
	state.flushContentWith(func(event zeroruntime.StreamEvent) {
		sendEvent(ctx, events, event)
	})
}

func (state *toolState) flushBufferedContent(events chan<- zeroruntime.StreamEvent) {
	state.flushContentWith(func(event zeroruntime.StreamEvent) {
		sendBufferedEvent(events, event)
	})
}

func (state *toolState) emitContentWith(content string, emit streamEventEmitter) {
	if !state.parseThinkTags {
		emit(zeroruntime.StreamEvent{Type: zeroruntime.StreamEventText, Content: content})
		return
	}
	state.think.push(content, func(eventType zeroruntime.StreamEventType, text string) {
		emit(zeroruntime.StreamEvent{Type: eventType, Content: text})
	})
}

func (state *toolState) flushContentWith(emit streamEventEmitter) {
	if !state.parseThinkTags {
		return
	}
	state.think.flush(func(eventType zeroruntime.StreamEventType, text string) {
		emit(zeroruntime.StreamEvent{Type: eventType, Content: text})
	})
}

func (splitter *thinkTagSplitter) push(content string, emit func(zeroruntime.StreamEventType, string)) {
	if content == "" {
		return
	}
	splitter.pending += content
	splitter.drain(false, emit)
}

func (splitter *thinkTagSplitter) flush(emit func(zeroruntime.StreamEventType, string)) {
	splitter.drain(true, emit)
}

func (splitter *thinkTagSplitter) drain(final bool, emit func(zeroruntime.StreamEventType, string)) {
	for {
		tag := thinkOpenTag
		if splitter.inThinking {
			tag = thinkCloseTag
		}
		if index := indexFold(splitter.pending, tag); index >= 0 {
			splitter.emitCurrent(emit, splitter.pending[:index])
			splitter.pending = splitter.pending[index+len(tag):]
			splitter.inThinking = !splitter.inThinking
			continue
		}
		if final {
			splitter.emitCurrent(emit, splitter.pending)
			splitter.pending = ""
			return
		}
		keep := trailingTagPrefixLen(splitter.pending, tag)
		if keep == len(splitter.pending) {
			return
		}
		emitText := splitter.pending[:len(splitter.pending)-keep]
		splitter.emitCurrent(emit, emitText)
		splitter.pending = splitter.pending[len(splitter.pending)-keep:]
		return
	}
}

func (splitter *thinkTagSplitter) emitCurrent(emit func(zeroruntime.StreamEventType, string), text string) {
	if text == "" {
		return
	}
	eventType := zeroruntime.StreamEventText
	if splitter.inThinking {
		eventType = zeroruntime.StreamEventReasoning
	}
	emit(eventType, text)
}

func trailingTagPrefixLen(text string, tag string) int {
	maxLen := min(len(text), len(tag)-1)
	for length := maxLen; length > 0; length-- {
		if strings.EqualFold(text[len(text)-length:], tag[:length]) {
			return length
		}
	}
	return 0
}

func indexFold(text string, needle string) int {
	if needle == "" {
		return 0
	}
	if len(text) < len(needle) {
		return -1
	}
	for index := 0; index <= len(text)-len(needle); index++ {
		if strings.EqualFold(text[index:index+len(needle)], needle) {
			return index
		}
	}
	return -1
}

func (state *toolState) applyDelta(
	ctx context.Context,
	delta streamToolCallDelta,
	events chan<- zeroruntime.StreamEvent,
) {
	call := state.calls[delta.Index]
	if call == nil {
		call = &pendingToolCall{}
		state.calls[delta.Index] = call
	}

	// Set id and name once. Some OpenAI-compatible backends (e.g. minimax via
	// Ollama) occasionally stream a second tool_calls entry at the same index;
	// overwriting id/name there corrupts the in-flight call and leaks a phantom
	// nameless call into the collector ("Unknown tool \"\""). Keep the first.
	if delta.ID != "" && call.id == "" {
		call.id = delta.ID
	}
	if delta.Function.Name != "" && call.name == "" {
		call.name = delta.Function.Name
	}
	if delta.Function.Arguments != "" {
		call.arguments += delta.Function.Arguments
	}

	if call.id == "" || call.name == "" || call.ended {
		return
	}

	if !call.started {
		call.started = true
		sendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:       zeroruntime.StreamEventToolCallStart,
			ToolCallID: call.id,
			ToolName:   call.name,
		})
	}
	if call.arguments != "" {
		sendEvent(ctx, events, zeroruntime.StreamEvent{
			Type:              zeroruntime.StreamEventToolCallDelta,
			ToolCallID:        call.id,
			ArgumentsFragment: call.arguments,
		})
		call.arguments = ""
	}
}

func (state *toolState) closeOpen(ctx context.Context, events chan<- zeroruntime.StreamEvent) {
	state.closeOpenWith(func(event zeroruntime.StreamEvent) {
		sendEvent(ctx, events, event)
	})
}

func (state *toolState) closeBufferedOpen(events chan<- zeroruntime.StreamEvent) {
	state.closeOpenWith(func(event zeroruntime.StreamEvent) {
		sendBufferedEvent(events, event)
	})
}

func (state *toolState) closeOpenWith(emit streamEventEmitter) {
	indexes := make([]int, 0, len(state.calls))
	for index := range state.calls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	for _, index := range indexes {
		call := state.calls[index]
		if call == nil || call.ended {
			continue
		}
		// A call that lacks a usable name/id can't be dispatched. If the model
		// nonetheless attempted one (it streamed an id or arguments), signal a
		// drop once so the agent can ask it to retry instead of silently ending.
		if call.id == "" || call.name == "" {
			if call.id != "" || call.name != "" || call.arguments != "" {
				call.ended = true
				emit(zeroruntime.StreamEvent{Type: zeroruntime.StreamEventToolCallDropped})
			}
			continue
		}
		if !call.started {
			call.started = true
			emit(zeroruntime.StreamEvent{
				Type:       zeroruntime.StreamEventToolCallStart,
				ToolCallID: call.id,
				ToolName:   call.name,
			})
		}
		if call.arguments != "" {
			emit(zeroruntime.StreamEvent{
				Type:              zeroruntime.StreamEventToolCallDelta,
				ToolCallID:        call.id,
				ArgumentsFragment: call.arguments,
			})
			call.arguments = ""
		}
		call.ended = true
		emit(zeroruntime.StreamEvent{
			Type:       zeroruntime.StreamEventToolCallEnd,
			ToolCallID: call.id,
		})
	}
}

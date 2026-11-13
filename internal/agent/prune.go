package agent

import (
	"fmt"
	"strings"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

// Stale tool-output pruning reclaims context at ZERO token/latency cost before
// the loop falls back to the paid LLM summarizer. A long, dump-heavy session
// accumulates large read_file/grep/glob/bash tool results that the model has
// long since acted on; their bodies are dead weight. Pruning replaces those
// older bodies with a compact placeholder (keeping the tool message + its
// ToolCallID so provider replay stays valid and the model knows what was
// there), preserving recent turns exactly — no paraphrase loss.
const (
	// pruneProtectRecentTokens is the trailing window of tool output kept
	// verbatim — the model is most likely still using it.
	pruneProtectRecentTokens = 40000
	// pruneMinReclaimTokens gates the whole pass: only prune when the reclaimable
	// (older, large) tool output exceeds this, so a short session is untouched.
	pruneMinReclaimTokens = 20000
	// pruneMinBodyTokens skips small tool results — replacing a tiny body saves
	// nothing and just loses information.
	pruneMinBodyTokens = 200
)

// prunedPlaceholder is the body a pruned tool result is replaced with. It names
// the tool and the original size so the model can re-fetch if it needs to.
func prunedPlaceholder(toolName string, originalTokens int) string {
	if toolName == "" {
		toolName = "tool"
	}
	return fmt.Sprintf("[pruned %s output (~%d tokens) to reclaim context — re-run the tool if you need it again]", toolName, originalTokens)
}

// pruneStaleToolOutput walks messages newest-first, protects the last
// preserveLast messages and a trailing pruneProtectRecentTokens of tool output,
// then replaces the body of older large tool results with a placeholder. It
// returns the (possibly) rewritten slice and the number of tokens reclaimed.
// When nothing meets the bar it returns the input unchanged with reclaimed=0.
//
// It never drops a message, never touches non-tool messages, and never prunes
// an already-pruned body — so it is idempotent and safe to run every turn.
func pruneStaleToolOutput(messages []zeroruntime.Message, preserveLast int) ([]zeroruntime.Message, int) {
	if len(messages) == 0 {
		return messages, 0
	}
	if preserveLast < 0 {
		preserveLast = 0
	}

	// Index → original token cost of each prunable tool body, scanning newest
	// first and skipping the protected suffix.
	type candidate struct {
		index  int
		tokens int
	}
	var candidates []candidate
	recentToolTokens := 0
	reclaimable := 0
	protectUntil := len(messages) - preserveLast // indices >= this are protected

	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != zeroruntime.MessageRoleTool {
			continue
		}
		bodyTokens := ApproxTextTokens(msg.Content)
		if i >= protectUntil {
			recentToolTokens += bodyTokens
			continue
		}
		// Still within the trailing-output protection window?
		if recentToolTokens < pruneProtectRecentTokens {
			recentToolTokens += bodyTokens
			continue
		}
		if bodyTokens < pruneMinBodyTokens || isPrunedPlaceholder(msg.Content) {
			continue
		}
		candidates = append(candidates, candidate{index: i, tokens: bodyTokens})
		reclaimable += bodyTokens
	}

	if reclaimable < pruneMinReclaimTokens || len(candidates) == 0 {
		return messages, 0
	}

	// Copy-on-write so we never mutate the caller's slice in place.
	out := make([]zeroruntime.Message, len(messages))
	copy(out, messages)
	reclaimed := 0
	for _, c := range candidates {
		toolName := toolNameForResult(out, c.index)
		newBody := prunedPlaceholder(toolName, c.tokens)
		reclaimed += c.tokens - ApproxTextTokens(newBody)
		out[c.index].Content = newBody
	}
	return out, reclaimed
}

// toolNameForResult finds the tool name for the tool-result message at index by
// matching its ToolCallID against the assistant tool_call that produced it.
func toolNameForResult(messages []zeroruntime.Message, index int) string {
	id := messages[index].ToolCallID
	if id == "" {
		return ""
	}
	for j := index - 1; j >= 0; j-- {
		for _, call := range messages[j].ToolCalls {
			if call.ID == id {
				return call.Name
			}
		}
	}
	return ""
}

func isPrunedPlaceholder(content string) bool {
	return strings.HasPrefix(strings.TrimSpace(content), "[pruned ")
}

var supersedableReadTools = map[string]bool{
	"read_file":          true,
	"read_minified_file": true,
	"grep":               true,
	"glob":               true,
	"list_directory":     true,
}

// pruneSupersededReadResults removes only older byte-identical results of the
// same read call. It runs when context pressure already requires a rewrite, so
// it does not trade a warm provider-cache prefix for a small apparent saving.
func pruneSupersededReadResults(messages []zeroruntime.Message) ([]zeroruntime.Message, int) {
	type seenResultKey struct {
		call    string
		content string
	}
	seen := map[seenResultKey]struct{}{}
	out := messages
	reclaimed := 0
	copied := false

	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message.Role != zeroruntime.MessageRoleTool || isPrunedPlaceholder(message.Content) {
			continue
		}
		call, ok := toolCallForResult(messages, index)
		if !ok || !supersedableReadTools[call.Name] {
			continue
		}
		key := seenResultKey{
			call:    call.Name + "\x00" + strings.TrimSpace(call.Arguments),
			content: message.Content,
		}
		if _, exists := seen[key]; exists {
			placeholder := fmt.Sprintf("[superseded identical %s result — the newer result remains in context]", call.Name)
			if !copied {
				out = append([]zeroruntime.Message(nil), messages...)
				copied = true
			}
			out[index].Content = placeholder
			reclaimed += max(0, ApproxTextTokens(message.Content)-ApproxTextTokens(placeholder))
			continue
		}
		seen[key] = struct{}{}
	}
	return out, reclaimed
}

func toolCallForResult(messages []zeroruntime.Message, resultIndex int) (zeroruntime.ToolCall, bool) {
	id := messages[resultIndex].ToolCallID
	if id == "" {
		return zeroruntime.ToolCall{}, false
	}
	for index := resultIndex - 1; index >= 0; index-- {
		for _, call := range messages[index].ToolCalls {
			if call.ID == id {
				return call, true
			}
		}
	}
	return zeroruntime.ToolCall{}, false
}

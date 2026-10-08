package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// The final two messages must carry a cache_control breakpoint on their last
// cacheable block so the conversation transcript — not just system prompt and
// tools — is a prompt-cache hit on the next turn. Earlier messages must stay
// unmarked (Anthropic caps breakpoints at 4 per request: system, tools, and
// these two).
func TestAnthropicRequestMarksLastTwoMessagesForCaching(t *testing.T) {
	t.Parallel()
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		writeSSEEvent(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer server.Close()

	provider, err := New(Options{BaseURL: server.URL + "/", Model: "claude-test"})
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	stream, err := provider.StreamCompletion(context.Background(), zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{
			{Role: zeroruntime.MessageRoleUser, Content: "first"},
			{
				Role:      zeroruntime.MessageRoleAssistant,
				Content:   "calling a tool",
				ToolCalls: []zeroruntime.ToolCall{{ID: "toolu_1", Name: "grep", Arguments: `{}`}},
			},
			{Role: zeroruntime.MessageRoleTool, Content: "result", ToolCallID: "toolu_1"},
		},
	})
	if err != nil {
		t.Fatalf("StreamCompletion returned error: %v", err)
	}
	drain(stream)

	messages := gotBody["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("expected 3 wire messages, got %d: %#v", len(messages), messages)
	}
	lastBlockHasCache := func(message any) bool {
		blocks, ok := message.(map[string]any)["content"].([]any)
		if !ok || len(blocks) == 0 {
			return false
		}
		last := blocks[len(blocks)-1].(map[string]any)
		control, ok := last["cache_control"].(map[string]any)
		return ok && control["type"] == "ephemeral"
	}
	if lastBlockHasCache(messages[0]) {
		t.Fatalf("first message must not carry a breakpoint: %#v", messages[0])
	}
	if !lastBlockHasCache(messages[1]) {
		t.Fatalf("second-to-last message must carry a breakpoint: %#v", messages[1])
	}
	if !lastBlockHasCache(messages[2]) {
		t.Fatalf("last message must carry a breakpoint: %#v", messages[2])
	}
}

// A string-content message must be converted to a block array so it can carry
// the breakpoint, and thinking blocks must never carry cache_control (the API
// rejects them) — the marker goes on the last cacheable block instead.
func TestApplyMessageCacheBreakpointsSkipsThinkingBlocks(t *testing.T) {
	t.Parallel()
	messages := []anthropicMessage{
		{Role: "user", Content: "plain string"},
		{Role: "assistant", Content: []map[string]any{
			{"type": "text", "text": "answer"},
			{"type": "thinking", "thinking": "hmm", "signature": "sig"},
		}},
	}
	applyMessageCacheBreakpoints(messages)

	userBlocks := contentBlocks(messages[0].Content)
	if len(userBlocks) != 1 || userBlocks[0]["cache_control"] == nil {
		t.Fatalf("string content must become a marked block: %#v", messages[0].Content)
	}
	assistantBlocks := contentBlocks(messages[1].Content)
	if assistantBlocks[1]["cache_control"] != nil {
		t.Fatalf("thinking block must not be marked: %#v", assistantBlocks[1])
	}
	if assistantBlocks[0]["cache_control"] == nil {
		t.Fatalf("text block must carry the breakpoint instead: %#v", assistantBlocks[0])
	}
}

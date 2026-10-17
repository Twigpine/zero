package agent

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

// bigToolResult builds a tool-result message with approximately tokens worth of
// body, paired to a tool call id. ApproxTextTokens counts non-space chars / 4,
// so 4*tokens non-space chars yields ~tokens.
func bigToolResult(id string, tokens int) zeroruntime.Message {
	return zeroruntime.Message{
		Role:       zeroruntime.MessageRoleTool,
		ToolCallID: id,
		Content:    strings.Repeat("x", tokens*4),
	}
}

func toolCallMsg(id, name string) zeroruntime.Message {
	return zeroruntime.Message{
		Role:      zeroruntime.MessageRoleAssistant,
		ToolCalls: []zeroruntime.ToolCall{{ID: id, Name: name}},
	}
}

func toolCallWithArgs(id, name, args string) zeroruntime.Message {
	return zeroruntime.Message{
		Role:      zeroruntime.MessageRoleAssistant,
		ToolCalls: []zeroruntime.ToolCall{{ID: id, Name: name, Arguments: args}},
	}
}

func TestPruneSkipsSmallSessions(t *testing.T) {
	// Total reclaimable is under the gate → no pruning.
	msgs := []zeroruntime.Message{
		toolCallMsg("c1", "read_file"),
		bigToolResult("c1", 5000),
	}
	out, reclaimed := pruneStaleToolOutput(msgs, 4)
	if reclaimed != 0 {
		t.Fatalf("small session should reclaim nothing, got %d", reclaimed)
	}
	if out[1].Content != msgs[1].Content {
		t.Fatal("content must be untouched when under the gate")
	}
}

func TestPrunePreservesRecentAndPrunesOld(t *testing.T) {
	// One huge OLD result (beyond the protect window) + recent small ones.
	msgs := []zeroruntime.Message{
		toolCallMsg("old", "grep"),
		bigToolResult("old", 60000), // old, big → prunable
		// Filler to push "old" past the recent-protection window:
		toolCallMsg("mid", "read_file"),
		bigToolResult("mid", 45000), // protected by the 40k recent window
		zeroruntime.Message{Role: zeroruntime.MessageRoleAssistant, Content: "thinking"},
		zeroruntime.Message{Role: zeroruntime.MessageRoleUser, Content: "next"},
	}
	out, reclaimed := pruneStaleToolOutput(msgs, 2)
	if reclaimed <= 0 {
		t.Fatalf("expected the old 60k tool result to be pruned, reclaimed=%d", reclaimed)
	}
	if !isPrunedPlaceholder(out[1].Content) {
		t.Fatalf("old result should be replaced with a placeholder, got %q", out[1].Content[:40])
	}
	// The placeholder names the tool, and the message + id survive (provider replay).
	if !strings.Contains(out[1].Content, "grep") {
		t.Fatalf("placeholder should name the tool, got %q", out[1].Content)
	}
	if out[1].ToolCallID != "old" || out[1].Role != zeroruntime.MessageRoleTool {
		t.Fatal("pruned message must keep its role and ToolCallID for replay")
	}
	// The recent big result stays verbatim.
	if isPrunedPlaceholder(out[3].Content) {
		t.Fatal("a result inside the recent-protection window must not be pruned")
	}
}

func TestPruneIsIdempotent(t *testing.T) {
	msgs := []zeroruntime.Message{
		toolCallMsg("old", "bash"),
		bigToolResult("old", 60000),
		toolCallMsg("mid", "read_file"),
		bigToolResult("mid", 45000),
		zeroruntime.Message{Role: zeroruntime.MessageRoleUser, Content: "go"},
	}
	once, r1 := pruneStaleToolOutput(msgs, 1)
	if r1 == 0 {
		t.Fatal("first pass should prune")
	}
	_, r2 := pruneStaleToolOutput(once, 1)
	if r2 != 0 {
		t.Fatalf("second pass should be a no-op (already pruned), reclaimed=%d", r2)
	}
}

func TestPruneNeverTouchesNonToolMessages(t *testing.T) {
	msgs := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleUser, Content: strings.Repeat("u ", 60000)},
		{Role: zeroruntime.MessageRoleAssistant, Content: strings.Repeat("a ", 60000)},
	}
	out, reclaimed := pruneStaleToolOutput(msgs, 0)
	if reclaimed != 0 {
		t.Fatalf("non-tool messages must never be pruned, reclaimed=%d", reclaimed)
	}
	if out[0].Content != msgs[0].Content || out[1].Content != msgs[1].Content {
		t.Fatal("non-tool content changed")
	}
}

func TestPruneDoesNotMutateInput(t *testing.T) {
	msgs := []zeroruntime.Message{
		toolCallMsg("old", "grep"),
		bigToolResult("old", 60000),
		toolCallMsg("mid", "read_file"),
		bigToolResult("mid", 45000),
		zeroruntime.Message{Role: zeroruntime.MessageRoleUser, Content: "go"},
	}
	original := msgs[1].Content
	_, reclaimed := pruneStaleToolOutput(msgs, 1)
	if reclaimed == 0 {
		t.Fatal("expected a prune")
	}
	if msgs[1].Content != original {
		t.Fatal("pruneStaleToolOutput must not mutate the caller's slice")
	}
}

func TestPruneSupersededReadResultsOnlyDropsIdenticalOlderReads(t *testing.T) {
	body := strings.Repeat("same source line\n", 1000)
	messages := []zeroruntime.Message{
		toolCallWithArgs("old", "read_file", `{"path":"a.go"}`),
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "old", Content: body},
		toolCallWithArgs("different", "read_file", `{"path":"b.go"}`),
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "different", Content: body},
		toolCallWithArgs("new", "read_file", `{"path":"a.go"}`),
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "new", Content: body},
	}
	out, reclaimed := pruneSupersededReadResults(messages)
	if reclaimed <= 0 || !strings.Contains(out[1].Content, "superseded identical") {
		t.Fatalf("older duplicate was not pruned: reclaimed=%d output=%q", reclaimed, out[1].Content)
	}
	if out[3].Content != body || out[5].Content != body {
		t.Fatal("different call and newest result must remain exact")
	}
	if messages[1].Content != body {
		t.Fatal("input slice was mutated")
	}
}

func TestPruneSupersededReadResultsKeepsChangedAndMutatingResults(t *testing.T) {
	messages := []zeroruntime.Message{
		toolCallWithArgs("r1", "read_file", `{"path":"a.go"}`),
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "r1", Content: "version one"},
		toolCallWithArgs("r2", "read_file", `{"path":"a.go"}`),
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "r2", Content: "version two"},
		toolCallWithArgs("e1", "edit_file", `{"path":"a.go"}`),
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "e1", Content: "edited"},
		toolCallWithArgs("e2", "edit_file", `{"path":"a.go"}`),
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "e2", Content: "edited"},
	}
	out, reclaimed := pruneSupersededReadResults(messages)
	if reclaimed != 0 {
		t.Fatalf("unsafe results were pruned: %d", reclaimed)
	}
	for index := range messages {
		if out[index].Content != messages[index].Content {
			t.Fatalf("message %d changed unexpectedly", index)
		}
	}
}

func TestPruneSupersededReadResultsFindsMatchingBodyAcrossChangedRead(t *testing.T) {
	bodyA := strings.Repeat("version a\n", 100)
	bodyB := strings.Repeat("version b\n", 100)
	messages := []zeroruntime.Message{
		toolCallWithArgs("old-a", "read_file", `{"path":"a.go"}`),
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "old-a", Content: bodyA},
		toolCallWithArgs("middle-b", "read_file", `{"path":"a.go"}`),
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "middle-b", Content: bodyB},
		toolCallWithArgs("new-a", "read_file", `{"path":"a.go"}`),
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "new-a", Content: bodyA},
	}

	out, reclaimed := pruneSupersededReadResults(messages)

	if reclaimed <= 0 || !strings.Contains(out[1].Content, "superseded identical") {
		t.Fatalf("older matching body was not pruned across a changed result: reclaimed=%d output=%q", reclaimed, out[1].Content)
	}
	if out[3].Content != bodyB || out[5].Content != bodyA {
		t.Fatal("changed middle result and newest matching result must remain exact")
	}
}

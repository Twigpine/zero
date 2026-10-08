package agent

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestClipBytesHandlesTinyLimitsOnRuneBoundaries(t *testing.T) {
	for _, limit := range []int{0, 1, 2, 3} {
		if clipped := clipBytes("世界", limit); len(clipped) > limit || !utf8.ValidString(clipped) {
			t.Fatalf("clipBytes limit %d returned invalid result %q", limit, clipped)
		}
	}
}

func TestBoundaryClippersHandleNonPositiveLimits(t *testing.T) {
	for _, clip := range []func(string, int) string{clipPrefixAtBoundary, clipSuffixAtBoundary} {
		for _, limit := range []int{-1, 0} {
			if got := clip("世界", limit); got != "" {
				t.Fatalf("clipper limit %d = %q, want empty", limit, got)
			}
		}
	}
}

func TestProjectCompactionInputDropsRecoverableToolBodies(t *testing.T) {
	largeBody := strings.Repeat("recoverable source line\n", 1000)
	messages := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleUser, Content: "Inspect the parser and preserve its current behavior."},
		{Role: zeroruntime.MessageRoleAssistant, Content: "I will inspect before editing.", ToolCalls: []zeroruntime.ToolCall{{ID: "read", Name: "read_file", Arguments: `{"path":"internal/parser.go"}`}}},
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "read", Content: largeBody},
		{Role: zeroruntime.MessageRoleAssistant, ToolCalls: []zeroruntime.ToolCall{{ID: "test", Name: "exec_command", Arguments: `{"cmd":"go test ./internal/parser"}`}}},
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "test", Content: "Error: parser regression\nlong diagnostic details"},
	}

	projection := projectCompactionInput(messages)
	projected := projection.messages
	if len(projected) != 1 {
		t.Fatalf("projection = %#v, want one compact message", projected)
	}
	content := projected[0].Content
	for _, want := range []string{"Inspect the parser", `read_file "internal/parser.go"`, `exec_command "go test ./internal/parser"`, "Error: parser regression"} {
		if !strings.Contains(content, want) {
			t.Fatalf("projection missing %q: %q", want, content)
		}
	}
	if strings.Contains(content, "recoverable source line") || strings.Contains(content, "long diagnostic details") {
		t.Fatalf("projection retained reconstructible or overlong tool output: %q", content)
	}
	if estimateTokens(projected)*20 >= estimateTokens(messages) {
		t.Fatalf("projection did not materially reduce tokens: before=%d after=%d", estimateTokens(messages), estimateTokens(projected))
	}
}

func TestProjectCompactionInputRetainsStructurallyMarkedToolErrors(t *testing.T) {
	messages := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleAssistant, ToolCalls: []zeroruntime.ToolCall{{ID: "git", Name: "exec_command", Arguments: `{"cmd":"git status"}`}}},
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "git", Content: "fatal: not a git repository", IsError: true},
	}

	projected := projectCompactionInput(messages).messages
	if len(projected) != 1 || !strings.Contains(projected[0].Content, "fatal: not a git repository") {
		t.Fatalf("structured tool error was dropped from projection: %#v", projected)
	}
}

func TestProjectCompactionInputRetainsAskUserExchangeAndSemanticArguments(t *testing.T) {
	messages := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleAssistant, ToolCalls: []zeroruntime.ToolCall{{ID: "ask", Name: "ask_user", Arguments: `{"questions":[{"question":"Which database should remain?"}]}`}}},
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "ask", Content: "Which database should remain?: Postgres only; do not touch MySQL."},
		{Role: zeroruntime.MessageRoleAssistant, ToolCalls: []zeroruntime.ToolCall{
			{ID: "patch", Name: "apply_patch", Arguments: `{"patch":"*** Begin Patch\n*** Update File: internal/db.go\n*** End Patch"}`},
			{ID: "raw-patch", Name: "apply_patch", Freeform: true, Arguments: "*** Begin Patch\n*** Update File: internal/api.go\n*** Add File: internal/config.go\n*** End Patch\n"},
			{ID: "fetch", Name: "web_fetch", Arguments: `{"url":"https://example.com/spec"}`},
			{ID: "task", Name: "task", Arguments: `{"prompt":"Inspect the Windows implementation"}`},
		}},
		{Role: zeroruntime.MessageRoleTool, ToolCallID: "patch", Content: "Done!", ChangedFiles: []string{"internal/db.go"}},
	}

	content := projectCompactionInput(messages).messages[0].Content
	for _, want := range []string{"Which database should remain?", "Postgres only; do not touch MySQL", "internal/db.go", "internal/api.go", "internal/config.go", "https://example.com/spec", "Inspect the Windows implementation"} {
		if !strings.Contains(content, want) {
			t.Fatalf("projection missing %q: %q", want, content)
		}
	}
}

func TestProjectCompactionInputCapsToolCallsPerTurn(t *testing.T) {
	calls := make([]zeroruntime.ToolCall, 12)
	for index := range calls {
		calls[index] = zeroruntime.ToolCall{Name: "read_file", Arguments: `{"path":"file.go"}`}
	}
	projected := projectCompactionInput([]zeroruntime.Message{{Role: zeroruntime.MessageRoleAssistant, ToolCalls: calls}}).messages
	content := projected[0].Content
	if strings.Count(content, "* read_file") != compactionToolCallsPerTurn || !strings.Contains(content, "4 earlier tool calls omitted") {
		t.Fatalf("tool-call tail was not bounded: %q", content)
	}
}

func TestProjectCompactionInputCarriesPreviousSummaryOutsideBriefCap(t *testing.T) {
	previous := "keep the earlier architecture decision\n" + strings.Repeat("prior detail ", 2000) + "\nkeep the newest prior decision"
	messages := []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: summaryLabel + "\n" + previous + "\n\n" + preservedStateLabel + "\n{}"}}
	for index := range 500 {
		messages = append(messages, zeroruntime.Message{Role: zeroruntime.MessageRoleAssistant, Content: strings.Repeat("later transcript detail ", 20)})
		messages = append(messages, zeroruntime.Message{Role: zeroruntime.MessageRoleUser, Content: "later request " + strings.Repeat("context ", 20) + string(rune('a'+index%26))})
	}

	projection := projectCompactionInput(messages)
	content := projection.messages[0].Content
	if !strings.Contains(content, "[previous summary]\nkeep the earlier architecture decision") || !strings.Contains(content, "keep the newest prior decision") {
		t.Fatalf("previous summary was lost when the brief tail was capped: %q", content)
	}
	if !strings.Contains(content, "middle transcript omitted to fit compaction budget") {
		t.Fatalf("oversized brief was not head/tail capped: %q", content)
	}
	if !projection.truncated {
		t.Fatal("projection did not structurally report truncation")
	}
}

func TestProjectCompactionInputDoesNotInferTruncationFromUserText(t *testing.T) {
	projection := projectCompactionInput([]zeroruntime.Message{{
		Role: zeroruntime.MessageRoleUser, Content: "Keep this literal marker: [middle omitted]",
	}})
	if projection.truncated {
		t.Fatal("literal user text produced a false truncation signal")
	}
}

package agent

import (
	"testing"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestMeasureContextSplitsByCategory(t *testing.T) {
	messages := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleSystem, Content: filler(2000)},
		{Role: zeroruntime.MessageRoleUser, Content: filler(400)},
		{Role: zeroruntime.MessageRoleAssistant, Content: filler(400)},
	}
	tools := []zeroruntime.ToolDefinition{
		{Name: "read_file", Description: filler(200), Parameters: map[string]any{"type": "object"}},
	}

	breakdown := MeasureContext(messages, tools, 100_000)

	if breakdown.SystemTokens <= 0 || breakdown.MessageTokens <= 0 || breakdown.ToolTokens <= 0 {
		t.Fatalf("expected all categories non-zero, got %#v", breakdown)
	}
	if got := breakdown.SystemTokens + breakdown.ToolTokens + breakdown.MessageTokens; got != breakdown.TotalTokens {
		t.Fatalf("TotalTokens %d != sum of categories %d", breakdown.TotalTokens, got)
	}
	// System content (2000 chars) dominates the conversation (800 chars total).
	if breakdown.SystemTokens <= breakdown.MessageTokens {
		t.Fatalf("expected system to dominate messages, got system=%d messages=%d", breakdown.SystemTokens, breakdown.MessageTokens)
	}
	wantFraction := float64(breakdown.TotalTokens) / 100_000
	if breakdown.UsedFraction != wantFraction {
		t.Fatalf("UsedFraction = %v, want %v", breakdown.UsedFraction, wantFraction)
	}
	if len(breakdown.Blocks) != 3 {
		t.Fatalf("Blocks = %#v, want system/tools/conversation", breakdown.Blocks)
	}
}

func TestMeasureContextUnknownWindowHasZeroFraction(t *testing.T) {
	breakdown := MeasureContext([]zeroruntime.Message{
		{Role: zeroruntime.MessageRoleUser, Content: "hi"},
	}, nil, 0)
	if breakdown.UsedFraction != 0 {
		t.Fatalf("UsedFraction = %v, want 0 when context window unknown", breakdown.UsedFraction)
	}
	if breakdown.ToolTokens != 0 {
		t.Fatalf("ToolTokens = %d, want 0 with no tools", breakdown.ToolTokens)
	}
}

func TestMeasureContextNoSystemMessage(t *testing.T) {
	breakdown := MeasureContext([]zeroruntime.Message{
		{Role: zeroruntime.MessageRoleUser, Content: filler(400)},
	}, nil, 1000)
	if breakdown.SystemTokens != 0 {
		t.Fatalf("SystemTokens = %d, want 0 when there is no system message", breakdown.SystemTokens)
	}
	if breakdown.MessageTokens <= 0 {
		t.Fatalf("MessageTokens = %d, want > 0", breakdown.MessageTokens)
	}
}

// filler returns a string of n characters for sizing assertions.
func filler(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

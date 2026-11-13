package agent

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestCompactMessagesReturnsMetadataForManualCompaction(t *testing.T) {
	messages := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleSystem, Content: "system prompt"},
		{Role: zeroruntime.MessageRoleUser, Content: "first question"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "first answer"},
		{Role: zeroruntime.MessageRoleUser, Content: "second question"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "recent answer"},
		{Role: zeroruntime.MessageRoleUser, Content: "latest question"},
	}

	var captured []zeroruntime.Message
	result, err := CompactMessages(messages, CompactionOptions{
		PreserveLast: 2,
		Summarize: func(toSummarize []zeroruntime.Message) (string, error) {
			captured = append([]zeroruntime.Message(nil), toSummarize...)
			return "  manual summary  ", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !result.Compacted {
		t.Fatal("expected compaction to be reported")
	}
	if result.RemovedCount != 3 {
		t.Fatalf("RemovedCount = %d, want 3", result.RemovedCount)
	}
	if result.PreservedCount != 3 {
		t.Fatalf("PreservedCount = %d, want 3", result.PreservedCount)
	}
	if result.SummaryText != "manual summary" {
		t.Fatalf("SummaryText = %q, want trimmed summary", result.SummaryText)
	}
	if result.ProjectedChars != compactionMessageChars(captured) || result.ProjectedChars == 0 {
		t.Fatalf("ProjectedChars = %d, want %d", result.ProjectedChars, compactionMessageChars(captured))
	}
	if result.Truncated {
		t.Fatal("Truncated = true for a projection within its limits")
	}
	if len(captured) != 1 || !strings.Contains(captured[0].Content, "first question") || !strings.Contains(captured[0].Content, "second question") {
		t.Fatalf("summarized projection = %#v, want intent from the non-preserved middle", captured)
	}
	if len(result.Messages) != 4 {
		t.Fatalf("compacted message count = %d, want 4", len(result.Messages))
	}
	if result.Messages[0].Content != "system prompt" {
		t.Fatalf("system message was not preserved at head: %#v", result.Messages)
	}
	if result.Messages[1].Role != zeroruntime.MessageRoleUser {
		t.Fatalf("summary message role = %s, want user", result.Messages[1].Role)
	}
	if !strings.Contains(result.Messages[1].Content, summaryLabel) || !strings.Contains(result.Messages[1].Content, "manual summary") {
		t.Fatalf("summary message did not include label and body: %q", result.Messages[1].Content)
	}
	if result.Messages[2].Content != "recent answer" || result.Messages[3].Content != "latest question" {
		t.Fatalf("preserved suffix changed: %#v", result.Messages[2:])
	}
}

func TestCompactMessagesNoopReturnsUncompactedMetadata(t *testing.T) {
	messages := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleSystem, Content: "system"},
		{Role: zeroruntime.MessageRoleUser, Content: "hi"},
	}
	called := false

	result, err := CompactMessages(messages, CompactionOptions{
		PreserveLast: 8,
		Summarize: func([]zeroruntime.Message) (string, error) {
			called = true
			return "summary", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if called {
		t.Fatal("Summarize should not be called for a no-op compaction")
	}
	if result.Compacted {
		t.Fatal("Compacted = true for a no-op")
	}
	if result.RemovedCount != 0 {
		t.Fatalf("RemovedCount = %d, want 0", result.RemovedCount)
	}
	if result.PreservedCount != len(messages) {
		t.Fatalf("PreservedCount = %d, want %d", result.PreservedCount, len(messages))
	}
	if result.SummaryText != "" {
		t.Fatalf("SummaryText = %q, want empty", result.SummaryText)
	}
	if result.ProjectedChars != 0 || result.Truncated {
		t.Fatalf("no-op projection metadata = (%d, %t), want zero values", result.ProjectedChars, result.Truncated)
	}
	if !reflect.DeepEqual(result.Messages, messages) {
		t.Fatalf("Messages changed on no-op: %#v", result.Messages)
	}
}

func TestCompactMessagesReturnsTruncatedProjectionMetadata(t *testing.T) {
	messages := []zeroruntime.Message{{Role: zeroruntime.MessageRoleSystem, Content: "system"}}
	for index := range 20 {
		messages = append(messages, zeroruntime.Message{
			Role:    zeroruntime.MessageRoleUser,
			Content: strings.Repeat("contextword ", 256) + string(rune('a'+index)),
		})
	}
	messages = append(messages,
		zeroruntime.Message{Role: zeroruntime.MessageRoleAssistant, Content: "recent answer"},
		zeroruntime.Message{Role: zeroruntime.MessageRoleUser, Content: "latest question"},
	)

	var captured []zeroruntime.Message
	result, err := CompactMessages(messages, CompactionOptions{
		PreserveLast: 2,
		Summarize: func(toSummarize []zeroruntime.Message) (string, error) {
			captured = append([]zeroruntime.Message(nil), toSummarize...)
			return "bounded summary", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Compacted || !result.Truncated {
		t.Fatalf("compaction metadata = compacted %t, truncated %t; want both true", result.Compacted, result.Truncated)
	}
	if result.ProjectedChars != compactionMessageChars(captured) || result.ProjectedChars == 0 {
		t.Fatalf("ProjectedChars = %d, want %d", result.ProjectedChars, compactionMessageChars(captured))
	}
}

func TestCompactMessagesPropagatesSummarizeError(t *testing.T) {
	messages := []zeroruntime.Message{
		{Role: zeroruntime.MessageRoleSystem, Content: "system"},
		{Role: zeroruntime.MessageRoleUser, Content: "first question"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "first answer"},
		{Role: zeroruntime.MessageRoleUser, Content: "second question"},
		{Role: zeroruntime.MessageRoleAssistant, Content: "recent answer"},
		{Role: zeroruntime.MessageRoleUser, Content: "latest question"},
	}

	_, err := CompactMessages(messages, CompactionOptions{
		PreserveLast: 2,
		Summarize: func([]zeroruntime.Message) (string, error) {
			return "", errors.New("summarizer unavailable")
		},
	})
	if err == nil {
		t.Fatal("expected summarizer error")
	}
	if !strings.Contains(err.Error(), "summarizer unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

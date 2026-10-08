package zeroruntime

import (
	"context"
	"testing"
)

func TestToolCallCollectorPreservesProviderMetadataAndFreeformState(t *testing.T) {
	collector := newToolCallCollector()
	collector.start("public-1", "provider-1", "apply_patch", "sig-1", true)
	collector.delta("public-1", "raw patch")
	collected := &CollectedStream{}
	collector.flush(collected)

	if len(collected.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v, want one", collected.ToolCalls)
	}
	call := collected.ToolCalls[0]
	if call.ID != "public-1" || call.ProviderCallID != "provider-1" || call.Name != "apply_patch" ||
		call.Signature != "sig-1" || call.Arguments != "raw patch" || !call.Freeform {
		t.Fatalf("collected tool call lost provider metadata: %#v", call)
	}
}

func TestToolCallCollectorPreservesEmptyPublicID(t *testing.T) {
	collector := newToolCallCollector()
	collector.start("", "provider-1", "read_file", "", false)
	collector.delta("", `{"path":"README.md"}`)
	collector.end("")
	collected := &CollectedStream{}
	collector.flush(collected)

	if len(collected.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v, want one", collected.ToolCalls)
	}
	call := collected.ToolCalls[0]
	if call.ID != "" || call.ProviderCallID != "provider-1" || call.Name != "read_file" {
		t.Fatalf("empty-public-id tool call = %#v", call)
	}
}

func TestCollectStreamErrorPreservesIncompleteToolCallMetadata(t *testing.T) {
	events := make(chan StreamEvent, 2)
	events <- StreamEvent{
		Type:               StreamEventToolCallStart,
		ToolCallID:         "public-1",
		ToolCallProviderID: "provider-1",
		ToolName:           "apply_patch",
		ToolCallFreeform:   true,
	}
	events <- StreamEvent{Type: StreamEventError, Error: "stream failed"}
	close(events)

	collected := CollectStreamWithOptions(context.Background(), events, CollectOptions{})
	if collected.Error != "stream failed" {
		t.Fatalf("collected error = %q, want stream failed", collected.Error)
	}
	if len(collected.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v, want one incomplete call", collected.ToolCalls)
	}
	call := collected.ToolCalls[0]
	if call.ProviderCallID != "provider-1" || !call.Freeform {
		t.Fatalf("incomplete tool call lost provider metadata: %#v", call)
	}
}

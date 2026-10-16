package openai

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

func TestToolCallKeyOutputIndexZero(t *testing.T) {
	t.Parallel()
	p := &CodexProvider{}
	zero, two := 0, 2
	// output_index 0 with no item_id must produce a key (it was dropped before M1).
	if got := p.toolCallKey(&responsesEvent{OutputIndex: &zero}); got != "output-0" {
		t.Errorf("OutputIndex 0 → %q, want output-0", got)
	}
	if got := p.toolCallKey(&responsesEvent{OutputIndex: &two}); got != "output-2" {
		t.Errorf("OutputIndex 2 → %q, want output-2", got)
	}
	if got := p.toolCallKey(&responsesEvent{}); got != "" {
		t.Errorf("absent output_index + no item_id → %q, want empty", got)
	}
	if got := p.toolCallKey(&responsesEvent{ItemID: "call_x", OutputIndex: &zero}); got != "call_x" {
		t.Errorf("item_id should take precedence → %q", got)
	}
}

func TestResponsesStatePreservesDistinctCallID(t *testing.T) {
	provider := &CodexProvider{}
	state := newResponsesState()
	events := make(chan zeroruntime.StreamEvent, 4)
	provider.handleOutputItemAdded(context.Background(), &responsesEvent{
		ItemID: "item-1",
		Item: &itemPayload{
			Type:   "function_call",
			ID:     "item-1",
			CallID: "call-1",
			Name:   "read_file",
		},
	}, state, events)
	items := state.outputInputItems()
	if len(items) != 1 || items[0].ID != "item-1" || items[0].CallID != "call-1" {
		t.Fatalf("captured function call = %#v, want item id item-1 and call id call-1", items)
	}
}

func TestCodexCustomApplyPatchStreamProducesFreeformCall(t *testing.T) {
	provider := &CodexProvider{}
	state := newResponsesState()
	events := make(chan zeroruntime.StreamEvent, 16)
	patch := "*** Begin Patch\n*** Add File: hello.txt\n+hello\n*** End Patch\n"
	for _, payload := range []string{
		`{"type":"response.output_item.added","item_id":"item-1","item":{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch"}}`,
		`{"type":"response.custom_tool_call_input.delta","item_id":"item-1","call_id":"call-1","delta":"*** Begin Patch\n*** Add File: hello.txt\n"}`,
		`{"type":"response.custom_tool_call_input.delta","item_id":"item-1","call_id":"call-1","delta":"+hello\n*** End Patch\n"}`,
		`{"type":"response.output_item.done","item_id":"item-1","item":{"type":"custom_tool_call","call_id":"call-1","name":"apply_patch","input":"*** Begin Patch\n*** Add File: hello.txt\n+hello\n*** End Patch\n"}}`,
		`{"type":"response.completed","response":{"id":"resp-1","status":"completed"}}`,
	} {
		if keepReading := provider.emitResponsesEvent(context.Background(), payload, state, events); !keepReading && !state.done {
			t.Fatalf("stream stopped before terminal event for %s", payload)
		}
	}
	close(events)
	collected := zeroruntime.CollectStream(context.Background(), events)
	if len(collected.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v, want one", collected.ToolCalls)
	}
	call := collected.ToolCalls[0]
	if call.ID != "item-1" || call.ProviderCallID != "call-1" || call.Name != "apply_patch" || !call.Freeform || call.Arguments != patch {
		t.Fatalf("custom apply_patch call = %#v", call)
	}
}

func TestCodexApplyPatchUsesFreeformWireContract(t *testing.T) {
	provider, err := NewCodexProvider(CodexOptions{Options: Options{
		APIKey:  "test-token",
		BaseURL: "https://chatgpt.example/backend-api/codex",
		Model:   "gpt-test",
	}})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	patch := "*** Begin Patch\n*** Add File: hello.txt\n+hello\n*** End Patch\n"
	request, err := provider.buildResponsesRequest(zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{
			{Role: zeroruntime.MessageRoleUser, Content: "Add the file."},
			{Role: zeroruntime.MessageRoleAssistant, ToolCalls: []zeroruntime.ToolCall{{
				ID: "item-1", ProviderCallID: "call-1", Name: "apply_patch", Arguments: patch, Freeform: true,
			}}},
			{Role: zeroruntime.MessageRoleTool, ToolCallID: "item-1", ToolCallProviderID: "call-1", Content: "Done!"},
		},
		Tools: []zeroruntime.ToolDefinition{{
			Name: "apply_patch", Description: "Apply a patch.", Type: zeroruntime.ToolDefinitionFreeform,
		}},
	})
	if err != nil {
		t.Fatalf("buildResponsesRequest: %v", err)
	}
	if len(request.Tools) != 1 || request.Tools[0].Type != string(zeroruntime.ToolDefinitionFreeform) ||
		request.Tools[0].Format == nil || request.Tools[0].Format.Syntax != "lark" || request.Tools[0].Parameters != nil {
		t.Fatalf("apply_patch tool wire shape = %#v", request.Tools)
	}
	if !strings.Contains(request.Tools[0].Format.Definition, "*** Begin Patch") {
		t.Fatalf("apply_patch grammar = %q", request.Tools[0].Format.Definition)
	}
	if !strings.Contains(request.Tools[0].Description, "absolute file paths") || !strings.Contains(request.Tools[0].Description, "granted extra write root") {
		t.Fatalf("apply_patch description does not explain extra-root paths: %q", request.Tools[0].Description)
	}
	if len(request.Input) != 3 || request.Input[1].Type != "custom_tool_call" || request.Input[1].ID != "item-1" ||
		request.Input[1].CallID != "call-1" || request.Input[1].Input != patch ||
		request.Input[2].Type != "custom_tool_call_output" || request.Input[2].CallID != "call-1" {
		t.Fatalf("custom apply_patch replay input = %#v", request.Input)
	}
}

func TestCodexPreservesCustomApplyPatchFunction(t *testing.T) {
	provider, err := NewCodexProvider(CodexOptions{Options: Options{
		APIKey:  "test-token",
		BaseURL: "https://chatgpt.example/backend-api/codex",
		Model:   "gpt-test",
	}})
	if err != nil {
		t.Fatalf("NewCodexProvider: %v", err)
	}
	parameters := map[string]any{
		"type":       "object",
		"properties": map[string]any{"value": map[string]any{"type": "string"}},
	}
	request, err := provider.buildResponsesRequest(zeroruntime.CompletionRequest{
		Messages: []zeroruntime.Message{{Role: zeroruntime.MessageRoleUser, Content: "Call the custom tool."}},
		Tools: []zeroruntime.ToolDefinition{{
			Name: "apply_patch", Description: "Custom integration tool.", Parameters: parameters,
		}},
	})
	if err != nil {
		t.Fatalf("buildResponsesRequest: %v", err)
	}
	if len(request.Tools) != 1 {
		t.Fatalf("tools = %#v, want one", request.Tools)
	}
	tool := request.Tools[0]
	if tool.Type != string(zeroruntime.ToolDefinitionFunction) || tool.Format != nil ||
		tool.Description != "Custom integration tool." || !reflect.DeepEqual(tool.Parameters, parameters) {
		t.Fatalf("custom apply_patch function was rewritten: %#v", tool)
	}
}

func TestHandleTerminalResponseNilPayload(t *testing.T) {
	t.Parallel()
	p := &CodexProvider{}

	// response.failed with no Response payload must emit an error, not a silent done.
	failed := make(chan zeroruntime.StreamEvent, 4)
	st := &responsesState{}
	p.handleTerminalResponse(context.Background(), &responsesEvent{Type: responsesEventFailed}, st, failed)
	close(failed)
	sawError := false
	for ev := range failed {
		if ev.Type == zeroruntime.StreamEventError {
			sawError = true
		}
	}
	if !sawError {
		t.Error("response.failed with nil payload should emit StreamEventError (M2)")
	}
	if !st.done {
		t.Error("state.done should be set")
	}

	// response.completed with no payload is a clean (empty) done, not an error.
	completed := make(chan zeroruntime.StreamEvent, 4)
	p.handleTerminalResponse(context.Background(), &responsesEvent{Type: responsesEventCompleted}, &responsesState{}, completed)
	close(completed)
	for ev := range completed {
		if ev.Type == zeroruntime.StreamEventError {
			t.Error("response.completed with nil payload should NOT emit an error")
		}
	}
}

func TestHandleTerminalResponseFailedPayloadWithoutError(t *testing.T) {
	t.Parallel()
	p := &CodexProvider{}

	// A response.failed carrying a payload whose error object is null/omitted (the
	// reason is in status) must still surface as an error, not fall through to a
	// clean done — the same silent-failure class the nil-payload branch guards.
	out := make(chan zeroruntime.StreamEvent, 8)
	st := &responsesState{}
	p.handleTerminalResponse(context.Background(),
		&responsesEvent{Type: responsesEventFailed, Response: &responsePayload{Status: "failed"}}, st, out)
	close(out)
	sawError, sawDone := false, false
	for ev := range out {
		switch ev.Type {
		case zeroruntime.StreamEventError:
			sawError = true
		case zeroruntime.StreamEventDone:
			sawDone = true
		}
	}
	if !sawError {
		t.Error("response.failed with a non-nil payload and nil error must emit StreamEventError, not a clean done")
	}
	if sawDone {
		t.Error("a failed terminal must not also emit a clean StreamEventDone")
	}
	if !st.done {
		t.Error("state.done should be set")
	}

	// A response.completed with a payload and no error remains a clean done.
	ok := make(chan zeroruntime.StreamEvent, 8)
	p.handleTerminalResponse(context.Background(),
		&responsesEvent{Type: responsesEventCompleted, Response: &responsePayload{ID: "resp-1", Status: "completed"}}, &responsesState{}, ok)
	close(ok)
	for ev := range ok {
		if ev.Type == zeroruntime.StreamEventError {
			t.Error("response.completed with a non-error payload must NOT emit an error")
		}
		if ev.Type == zeroruntime.StreamEventDone && ev.ResponseID != "resp-1" {
			t.Errorf("done response id = %q, want resp-1", ev.ResponseID)
		}
	}
}

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// TestOptionsModelSwitcherFieldExists asserts the escalation hook is an
// assignable field on Options with the agreed signature, and that nil is its
// zero value (escalation disabled by default).
func TestOptionsModelSwitcherFieldExists(t *testing.T) {
	var options Options
	if options.ModelSwitcher != nil {
		t.Fatalf("expected ModelSwitcher to default to nil, got non-nil")
	}
	options.ModelSwitcher = func(_ context.Context, modelID string) (Provider, error) {
		return &mockProvider{}, nil
	}
	provider, err := options.ModelSwitcher(context.Background(), "claude-sonnet-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(*mockProvider); !ok {
		t.Fatalf("expected ModelSwitcher to return a Provider, got %T", provider)
	}
}

// TestToolResultRequestedModelFieldExists asserts the loop-level escalation
// signal field is present on ToolResult and empty for a normal result.
func TestToolResultRequestedModelFieldExists(t *testing.T) {
	var result ToolResult
	if result.RequestedModel != "" {
		t.Fatalf("expected RequestedModel to default to empty, got %q", result.RequestedModel)
	}
	result.RequestedModel = "claude-opus-4.1"
	if result.RequestedModel != "claude-opus-4.1" {
		t.Fatalf("expected RequestedModel to round-trip, got %q", result.RequestedModel)
	}
	// Keep the zeroruntime import load-bearing so the file compiles standalone.
	_ = zeroruntime.MessageRoleAssistant
}

// escalatingTool is a registered fake tool that returns the escalation signal
// in result Meta (mirroring the real escalate_model tool's contract), used to
// drive the loop-level switch in tests without depending on the tools package.
type escalatingTool struct {
	target string
}

func (t escalatingTool) Name() string        { return "escalate" }
func (t escalatingTool) Description() string { return "requests a model switch for testing" }
func (t escalatingTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", AdditionalProperties: false}
}
func (t escalatingTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow}
}
func (t escalatingTool) Run(_ context.Context, _ map[string]any) tools.Result {
	meta := map[string]string{}
	if t.target != "" {
		meta["escalate_to_model"] = t.target
	}
	return tools.Result{Status: tools.StatusOK, Output: "escalating", Meta: meta}
}

// TestExecuteToolCallLiftsEscalationMeta verifies executeToolCall promotes the
// tool's Meta["escalate_to_model"] into ToolResult.RequestedModel.
func TestExecuteToolCallLiftsEscalationMeta(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(escalatingTool{target: "claude-opus-4.1"})

	result, abortErr := executeToolCall(
		context.Background(),
		registry,
		ToolCall{ID: "c1", Name: "escalate"},
		PermissionModeAuto,
		Options{},
	)
	if abortErr != nil {
		t.Fatal(abortErr)
	}
	if result.RequestedModel != "claude-opus-4.1" {
		t.Fatalf("expected RequestedModel lifted from meta, got %q", result.RequestedModel)
	}
}

// TestExecuteToolCallNoEscalationMetaLeavesRequestedModelEmpty verifies a normal
// tool result (no escalate_to_model meta) leaves RequestedModel empty.
func TestExecuteToolCallNoEscalationMetaLeavesRequestedModelEmpty(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(escalatingTool{target: ""})

	result, abortErr := executeToolCall(
		context.Background(),
		registry,
		ToolCall{ID: "c1", Name: "escalate"},
		PermissionModeAuto,
		Options{},
	)
	if abortErr != nil {
		t.Fatal(abortErr)
	}
	if result.RequestedModel != "" {
		t.Fatalf("expected empty RequestedModel without escalation meta, got %q", result.RequestedModel)
	}
}

// escalateThenAnswerTurns builds a two-turn provider script: turn 1 calls the
// escalate tool, turn 2 returns a final answer. Reused by the switch tests.
func escalateThenAnswerTurns(answer string) [][]zeroruntime.StreamEvent {
	return [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "c1", ToolName: "escalate"},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "c1"},
			{Type: zeroruntime.StreamEventDone},
		},
		{
			{Type: zeroruntime.StreamEventText, Content: answer},
			{Type: zeroruntime.StreamEventDone},
		},
	}
}

// TestRunSwitchesProviderOnEscalationRequest verifies that when a tool requests
// a model and a ModelSwitcher is wired, the loop swaps the active provider (the
// NEXT turn streams from the new provider) and updates options.Model.
func TestRunSwitchesProviderOnEscalationRequest(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(escalatingTool{target: "claude-opus-4.1"})

	firstProvider := &mockProvider{turns: escalateThenAnswerTurns("done")}
	secondProvider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventText, Content: "answered on the upgraded model"},
			{Type: zeroruntime.StreamEventDone},
		},
	}}

	var switchedTo string
	switchCount := 0
	result, err := Run(context.Background(), "go", firstProvider, Options{
		Registry:    registry,
		Model:       "claude-sonnet-4.5",
		ServiceTier: "priority",
		MaxTurns:    4,
		ModelSwitcher: func(_ context.Context, modelID string) (Provider, error) {
			switchCount++
			switchedTo = modelID
			return secondProvider, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if switchCount != 1 {
		t.Fatalf("expected exactly one switch, got %d", switchCount)
	}
	if switchedTo != "claude-opus-4.1" {
		t.Fatalf("expected switch to claude-opus-4.1, got %q", switchedTo)
	}
	// The first provider only handled the escalation turn (1 request); the second
	// provider handled the answer turn (1 request) — proving the swap took effect.
	if len(firstProvider.requests) != 1 {
		t.Fatalf("expected first provider to handle exactly the escalation turn, got %d requests", len(firstProvider.requests))
	}
	if got := firstProvider.requests[0].ServiceTier; got != "priority" {
		t.Fatalf("initial request service tier = %q, want priority", got)
	}
	if len(secondProvider.requests) != 1 {
		t.Fatalf("expected second provider to handle the post-switch turn, got %d requests", len(secondProvider.requests))
	}
	if got := secondProvider.requests[0].ServiceTier; got != "" {
		t.Fatalf("post-switch request retained service tier %q", got)
	}
	if result.FinalAnswer != "answered on the upgraded model" {
		t.Fatalf("expected final answer from the swapped provider, got %q", result.FinalAnswer)
	}
}

// TestRunIgnoresEscalationWhenSwitcherNil verifies that without a ModelSwitcher
// the escalation request is ignored: the original provider serves every turn.
func TestRunIgnoresEscalationWhenSwitcherNil(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(escalatingTool{target: "claude-opus-4.1"})

	provider := &mockProvider{turns: escalateThenAnswerTurns("done on original")}

	result, err := Run(context.Background(), "go", provider, Options{
		Registry: registry,
		Model:    "claude-sonnet-4.5",
		MaxTurns: 4,
		// ModelSwitcher intentionally nil.
	})
	if err != nil {
		t.Fatal(err)
	}
	// Both turns (escalate + answer) must run on the single original provider.
	if len(provider.requests) != 2 {
		t.Fatalf("expected the original provider to serve both turns, got %d requests", len(provider.requests))
	}
	if result.FinalAnswer != "done on original" {
		t.Fatalf("expected final answer from the original provider, got %q", result.FinalAnswer)
	}
}

// TestRunEscalationSwitcherErrorIsNonFatal verifies a ModelSwitcher error does
// not abort the run: the loop continues on the current provider and a note is
// recorded in the transcript.
func TestRunEscalationSwitcherErrorIsNonFatal(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(escalatingTool{target: "claude-opus-4.1"})

	provider := &mockProvider{turns: escalateThenAnswerTurns("recovered")}

	result, err := Run(context.Background(), "go", provider, Options{
		Registry:    registry,
		Model:       "claude-sonnet-4.5",
		ServiceTier: "priority",
		MaxTurns:    4,
		ModelSwitcher: func(_ context.Context, _ string) (Provider, error) {
			return nil, errors.New("provider build blew up")
		},
	})
	if err != nil {
		t.Fatalf("expected escalation error to be non-fatal, got %v", err)
	}
	// The run continues on the same provider through the answer turn.
	if len(provider.requests) != 2 {
		t.Fatalf("expected the run to continue on the original provider, got %d requests", len(provider.requests))
	}
	if got := provider.requests[1].ServiceTier; got != "priority" {
		t.Fatalf("failed-switch request service tier = %q, want priority", got)
	}
	if result.FinalAnswer != "recovered" {
		t.Fatalf("expected final answer after non-fatal switch error, got %q", result.FinalAnswer)
	}
	// A brief note about the failed switch must reach the model on the next turn.
	var sawNote bool
	for _, m := range provider.requests[1].Messages {
		if m.Role == zeroruntime.MessageRoleUser && strings.Contains(strings.ToLower(m.Content), "could not switch") {
			sawNote = true
		}
	}
	if !sawNote {
		t.Fatalf("expected a non-fatal switch-failure note on the next turn, messages: %+v", provider.requests[1].Messages)
	}
}

// TestRunEscalationSwitcherNilProviderKeepsOriginal verifies that when a
// ModelSwitcher returns (nil, nil) — no error, but also no replacement provider —
// the loop does NOT null out the active provider. The run must continue on the
// ORIGINAL provider and return a result rather than panic on a nil provider.
func TestRunEscalationSwitcherNilProviderKeepsOriginal(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(escalatingTool{target: "claude-opus-4.1"})

	provider := &mockProvider{turns: escalateThenAnswerTurns("answered on original")}

	switchCount := 0
	result, err := Run(context.Background(), "go", provider, Options{
		Registry: registry,
		Model:    "claude-sonnet-4.5",
		MaxTurns: 4,
		ModelSwitcher: func(_ context.Context, _ string) (Provider, error) {
			switchCount++
			// No error, but no provider either: the loop must NOT swap to nil.
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("expected (nil,nil) switch to be non-fatal, got %v", err)
	}
	if switchCount != 1 {
		t.Fatalf("expected exactly one switch attempt, got %d", switchCount)
	}
	// Both turns (escalate + answer) must run on the single original provider; if
	// the loop nil'd out the provider on a (nil,nil) return, the second turn would
	// panic or never run.
	if len(provider.requests) != 2 {
		t.Fatalf("expected the original provider to serve both turns, got %d requests", len(provider.requests))
	}
	if result.FinalAnswer != "answered on original" {
		t.Fatalf("expected final answer from the original provider, got %q", result.FinalAnswer)
	}
}

// TestRunSwitchesAtMostOncePerTurn verifies that when a single turn carries two
// escalation requests, only the first triggers a switch.
func TestRunSwitchesAtMostOncePerTurn(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(escalatingTool{target: "claude-opus-4.1"})

	firstProvider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "c1", ToolName: "escalate"},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "c1"},
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "c2", ToolName: "escalate"},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "c2"},
			{Type: zeroruntime.StreamEventDone},
		},
	}}
	secondProvider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventText, Content: "done"},
			{Type: zeroruntime.StreamEventDone},
		},
	}}

	switchCount := 0
	if _, err := Run(context.Background(), "go", firstProvider, Options{
		Registry: registry,
		Model:    "claude-sonnet-4.5",
		MaxTurns: 4,
		ModelSwitcher: func(_ context.Context, _ string) (Provider, error) {
			switchCount++
			return secondProvider, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if switchCount != 1 {
		t.Fatalf("expected at most one switch per turn, got %d", switchCount)
	}
}

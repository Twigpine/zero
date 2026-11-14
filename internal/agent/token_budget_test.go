package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// spendingTurn is one provider turn that calls a tool and reports usage.
func spendingTurn(id string, tokens int) []zeroruntime.StreamEvent {
	return []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: id, ToolName: "run_tests"},
		{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: id, ArgumentsFragment: `{}`},
		{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: id},
		{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{PromptTokens: tokens, CompletionTokens: 0}},
		{Type: zeroruntime.StreamEventDone},
	}
}

func budgetProvider(turns int, perTurn int, finalText string) *mockProvider {
	streams := make([][]zeroruntime.StreamEvent, 0, turns+1)
	for i := 0; i < turns; i++ {
		streams = append(streams, spendingTurn("call-"+string(rune('a'+i)), perTurn))
	}
	streams = append(streams, []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: finalText},
		{Type: zeroruntime.StreamEventDone},
	})
	return &mockProvider{turns: streams}
}

func budgetRegistry() *tools.Registry {
	registry := tools.NewRegistry()
	registry.Register(goTestTool{output: "ok\tpkg\t0.10s\n"})
	return registry
}

// A RUN MUST BE BOUNDED BY WHAT IT SPENDS, not only by how many round trips it
// makes.
//
// MaxTurns is a proxy for cost that does not track cost: a measured heavy run
// reached its 320-turn limit having spent 35,781,390 tokens, where a cheap run
// reaches the same limit at roughly a tenth of that. The turn count bounds round
// trips; nothing bounded spend.
func TestARunStopsOnItsTokenBudget(t *testing.T) {
	provider := budgetProvider(6, 1000, "done")
	result, err := Run(context.Background(), "work", provider, Options{
		Cwd: t.TempDir(), Registry: budgetRegistry(), SessionID: "budget",
		MaxTurns: 50, MaxTokens: 2500,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 1000 per turn against 2500: turns 1-3 run (3000 spent), turn 4 is refused.
	if len(provider.requests) > 5 {
		t.Fatalf("the run made %d requests; the token budget did not bind", len(provider.requests))
	}
	if strings.TrimSpace(result.FinalAnswer) == "" {
		t.Error("a budget expiry produced no final answer; it must end with a written summary, not silence")
	}
}

// THE READER MUST BE TOLD WHICH BOUND FIRED. Reporting a turn limit for a spend
// stop sends someone to raise a number that was never what ran out.
func TestATokenStopSaysSoRatherThanBlamingTheTurnCount(t *testing.T) {
	provider := budgetProvider(6, 1000, "done")
	result, err := Run(context.Background(), "work", provider, Options{
		Cwd: t.TempDir(), Registry: budgetRegistry(), SessionID: "budget-reason",
		MaxTurns: 50, MaxTokens: 2500, RequireCompletionSignal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.IncompleteReason, "token budget") {
		t.Errorf("IncompleteReason = %q; a spend stop must name the token budget", result.IncompleteReason)
	}
	if strings.Contains(result.IncompleteReason, "max-turns") {
		t.Errorf("a token stop was reported as a turn-limit stop: %q", result.IncompleteReason)
	}
	// ...and the model is told the same thing, so its summary is about the right
	// constraint.
	last := provider.requests[len(provider.requests)-1]
	rendered := renderZeromaxingMessages(last.Messages)
	if !strings.Contains(rendered, "token budget") {
		t.Errorf("the final-answer prompt does not mention the token budget:\n%s", rendered[max(0, len(rendered)-400):])
	}
}

// A TURN-COUNT STOP STILL READS AS ONE. The new branch must not relabel the old
// ending.
func TestATurnStopStillSaysMaxTurns(t *testing.T) {
	provider := budgetProvider(6, 10, "done")
	result, err := Run(context.Background(), "work", provider, Options{
		Cwd: t.TempDir(), Registry: budgetRegistry(), SessionID: "turn-reason",
		MaxTurns: 3, RequireCompletionSignal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.IncompleteReason, "max-turns") {
		t.Errorf("IncompleteReason = %q; a turn stop must still name the turn limit", result.IncompleteReason)
	}
}

// AN UNSET BUDGET IS UNBOUNDED, so every caller that never sets it behaves
// exactly as before.
func TestAnUnsetTokenBudgetBoundsNothing(t *testing.T) {
	provider := budgetProvider(4, 1_000_000, "finished")
	result, err := Run(context.Background(), "work", provider, Options{
		Cwd: t.TempDir(), Registry: budgetRegistry(), SessionID: "unbounded",
		MaxTurns: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 5 {
		t.Fatalf("an unbounded run made %d requests, want all 5", len(provider.requests))
	}
	if !strings.Contains(result.FinalAnswer, "finished") {
		t.Errorf("final answer = %q", result.FinalAnswer)
	}
}

// THE BUDGET IS CHECKED AT A TURN BOUNDARY, so a turn whose tool calls have
// already run is not discarded. Stopping mid-turn would throw away work the run
// has already paid for.
func TestTheBudgetDoesNotDiscardAnAlreadyPaidTurn(t *testing.T) {
	ran := 0
	registry := tools.NewRegistry()
	registry.Register(countingTool{count: &ran})
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		// One turn that blows the entire budget in a single exchange.
		{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "c1", ToolName: "counted"},
			{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "c1", ArgumentsFragment: `{}`},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "c1"},
			{Type: zeroruntime.StreamEventUsage, Usage: zeroruntime.Usage{PromptTokens: 9999}},
			{Type: zeroruntime.StreamEventDone},
		},
		{{Type: zeroruntime.StreamEventText, Content: "summary"}, {Type: zeroruntime.StreamEventDone}},
	}}
	if _, err := Run(context.Background(), "work", provider, Options{
		Cwd: t.TempDir(), Registry: registry, SessionID: "boundary",
		MaxTurns: 50, MaxTokens: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if ran != 1 {
		t.Errorf("the tool ran %d times; the turn's already-issued call must still execute", ran)
	}
}

type countingTool struct{ count *int }

func (countingTool) Name() string        { return "counted" }
func (countingTool) Description() string { return "counts" }
func (countingTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", AdditionalProperties: false}
}
func (countingTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectNone, Permission: tools.PermissionAllow, AdvertiseInAuto: true}
}
func (t countingTool) Run(context.Context, map[string]any) tools.Result {
	*t.count++
	return tools.Result{Status: tools.StatusOK, Output: "ok"}
}

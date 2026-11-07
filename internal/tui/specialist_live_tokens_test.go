package tui

import (
	"testing"

	"github.com/Twigpine/zero/internal/streamjson"
)

// A SUB-AGENT'S TOKEN SPEND SHOWS WHILE IT WORKS, not only after.
//
// OnToolProgress bridged EventToolCall (tool count, current tool) but ignored
// EventUsage, so a Task sub-agent's card sat at "0 tok" for its whole life while
// a plan task's moved — the gap named in the tracker's own setTokens comment.
// Usage events are per-turn, so they add cumulatively.
func TestASubAgentsTokenSpendAccumulatesLive(t *testing.T) {
	m := sidebarTestModel()
	m.activeRunID = 7
	updated, _ := m.Update(specialistStartMsg{
		runID: 7, name: "worker", description: "W1", childSessionID: "call_1", model: "glm-5.2",
	})
	m = updated.(model)

	for _, turn := range []int{12000, 8000, 5000} {
		updated, _ = m.Update(specialistUsageMsg{runID: 7, toolCallID: "call_1", totalTokens: turn})
		m = updated.(model)
	}

	info, _ := m.specialists.getBySessionID("call_1")
	if info.tokenCount != 25000 {
		t.Fatalf("live tokens = %d, want 25000 (12k+8k+5k accumulated)", info.tokenCount)
	}
}

// addTokens ignores unknown children and non-positive deltas, like every setter.
func TestAddTokensIsSafeAndAdditive(t *testing.T) {
	m := sidebarTestModel()
	m.specialists.start("w", "d", "c1", m.now())

	m.specialists.addTokens("ghost", 999) // unknown child
	m.specialists.addTokens("c1", 0)      // no-op
	m.specialists.addTokens("c1", -5)     // no-op
	m.specialists.addTokens("c1", 100)
	m.specialists.addTokens("c1", 50)

	info, _ := m.specialists.getBySessionID("c1")
	if info.tokenCount != 150 {
		t.Fatalf("tokenCount = %d, want 150", info.tokenCount)
	}
	if _, ok := m.specialists.getBySessionID("ghost"); ok {
		t.Fatal("addTokens invented a child")
	}
}

// THE USAGE-BRIDGE DECISION, pinned — the emission lives in the untestable
// agent loop, so a mutation there passes a test that drives the message by hand.
func TestOnlyAUsageEventWithTokensBridges(t *testing.T) {
	n := 9000
	zero := 0
	for _, tc := range []struct {
		name  string
		event streamjson.Event
		want  int
		ok    bool
	}{
		{"usage with tokens", streamjson.Event{Type: streamjson.EventUsage, TotalTokens: &n}, 9000, true},
		{"usage, zero tokens", streamjson.Event{Type: streamjson.EventUsage, TotalTokens: &zero}, 0, false},
		{"usage, nil tokens", streamjson.Event{Type: streamjson.EventUsage}, 0, false},
		{"a tool call is not usage", streamjson.Event{Type: streamjson.EventToolCall, Name: "grep"}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := specialistProgressTokens(tc.event)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("specialistProgressTokens = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

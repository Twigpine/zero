package agent

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/zeroruntime"
)

func planUpdateTurn(status string) zeroruntime.CollectedStream {
	return zeroruntime.CollectedStream{ToolCalls: []zeroruntime.ToolCall{
		{Name: "update_plan", Arguments: `{"plan":[{"content":"step one","status":"` + status + `"}]}`},
	}}
}

// Turn-based staleness: a plan that goes many turns without an update — while
// items are still pending — draws the stale reminder even though few tool calls
// have run (the tool-call streak alone would not have tripped).
func TestPlanReminderFiresOnTurnStaleness(t *testing.T) {
	state := newGuardState()
	state.observeTurn(planUpdateTurn("in_progress"))

	// Text-only turns advance the turn counter without adding tool calls.
	for range stalePlanTurnThreshold {
		state.observeTurn(zeroruntime.CollectedStream{Text: "still working…"})
	}

	if state.toolCallsSincePlanUpdate >= staleToolCallThreshold {
		t.Fatalf("precondition: the tool-call trigger must not fire (%d calls)", state.toolCallsSincePlanUpdate)
	}
	if got := state.planReminder(stalePlanTurnThreshold + 2); !strings.Contains(got, planStaleReminderMarker) {
		t.Fatalf("expected a turn-based stale reminder, got %q", got)
	}
}

// A fully-completed plan is NOT stale: no pending items means the turn-based
// trigger stays quiet even after many turns, so a finished plan isn't nagged.
func TestPlanReminderSkipsTurnStalenessWhenComplete(t *testing.T) {
	state := newGuardState()
	state.observeTurn(planUpdateTurn("completed"))

	for range stalePlanTurnThreshold + 3 {
		state.observeTurn(zeroruntime.CollectedStream{Text: "wrapping up…"})
	}

	if got := state.planReminder(stalePlanTurnThreshold + 5); got != "" {
		t.Fatalf("a completed plan must not draw a stale reminder, got %q", got)
	}
}

// A plan update resets the turn counter, so the stale reminder does not fire
// right after a fresh update.
func TestPlanUpdateResetsTurnStaleness(t *testing.T) {
	state := newGuardState()
	state.observeTurn(planUpdateTurn("in_progress"))
	for range stalePlanTurnThreshold - 1 {
		state.observeTurn(zeroruntime.CollectedStream{Text: "working…"})
	}
	// Refresh the plan — resets the turn streak.
	state.observeTurn(planUpdateTurn("in_progress"))
	if got := state.planReminder(stalePlanTurnThreshold + 1); got != "" {
		t.Fatalf("a just-refreshed plan must not be stale, got %q", got)
	}
}

package specialist

import (
	"fmt"
	"sync/atomic"
)

// SessionBudget bounds how many sub-agents ONE SESSION may start.
//
// WHAT IT CATCHES THAT THE OTHER BOUNDS DO NOT. Depth is capped at 8, a plan
// runs at most maxPlanWorkers at once, a session shows one plan at a time, and
// since the run-level token budget a single run is bounded by spend. All of
// those bound one run or one plan. None of them bounds a CONVERSATION that keeps
// starting more work — plan after plan, or Task after Task — because each new
// run arrives with a fresh budget.
//
// A COUNT, NOT A COST, and that is the honest limit of it. The measured failure
// that shaped the token budget was one run spending 35.8M tokens, which a count
// would not have seen at all. This is the cheap coarse backstop underneath that,
// not a replacement for it.
//
// A nil *SessionBudget is unbounded and every method tolerates it, so a caller
// that never wires one keeps today's behaviour exactly.
type SessionBudget struct {
	started atomic.Int64
	max     int
}

// DefaultSessionSubagents is the ceiling on sub-agents started in one session.
//
// ANCHORED ON MEASUREMENT: across 75 recorded sessions the most any one started
// was 22. This is roughly nine times that, so it sits far above observed use and
// still stops a conversation that has begun spawning without end. It is a
// backstop, not a target — a session that legitimately needs more should raise
// it rather than have this number quietly shape the work.
const DefaultSessionSubagents = 200

// NewSessionBudget returns a budget for max sub-agents, or nil when max is not
// positive — so "no bound" is represented by the same nil every method already
// handles rather than by a second spelling of it.
func NewSessionBudget(max int) *SessionBudget {
	if max <= 0 {
		return nil
	}
	return &SessionBudget{max: max}
}

// admit counts one sub-agent and reports whether it may start.
//
// COUNTED ON ADMISSION, never decremented. The question is "how much work has
// this conversation set going", not "how much is running now" — a session that
// started three hundred children and finished them has still spent three hundred
// children's worth. Concurrency is bounded separately, by maxPlanWorkers.
func (budget *SessionBudget) admit() error {
	if budget == nil || budget.max <= 0 {
		return nil
	}
	if started := budget.started.Add(1); started > int64(budget.max) {
		return fmt.Errorf(
			"this session has started %d sub-agents, which is its limit of %d: "+
				"finish what is running, or start a new session for further work",
			started-1, budget.max)
	}
	return nil
}

// Started reports how many sub-agents this session has begun, for display.
func (budget *SessionBudget) Started() int {
	if budget == nil {
		return 0
	}
	return int(budget.started.Load())
}

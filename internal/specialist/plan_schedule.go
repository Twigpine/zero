package specialist

import (
	"runtime"
	"sync"
	"time"
)

// How many tasks a plan runs at once.
//
// THE SEQUENTIAL PATH IS NOT A SEPARATE PATH. There is one scheduler, and with
// one worker it walks plan.Order() one task at a time, applying the same checks
// in the same order and producing the same report — which is why every test
// written against the sequential executor is the oracle for this. Two executors
// would have been safer to write and impossible to keep in step: invariant 5
// says a duplicated rule drifts, and a duplicated EXECUTOR drifts faster.
//
// The walk is the thing that preserves order. Rather than maintaining a ready
// SET and choosing from it — which would let task 7 start before task 3 merely
// because its dependencies resolved first — the scheduler walks the validated
// topological order and, for each task in turn, waits until that task's
// dependencies are resolved and a worker is free. With one worker the wait is
// "until the previous task finished", which is exactly today.

const (
	// maxPlanWorkers is the absolute ceiling on what a plan may ASK for. Small
	// on purpose: each task is a child process inheriting a 320-turn budget
	// under this posture.
	maxPlanWorkers = 16
	// minPlanWorkers keeps the machine cap from collapsing to zero on a
	// single-core box, where the answer must still be "run the plan".
	minPlanWorkers = 2
)

// machinePlanWorkers is what the HOST can carry, independent of what a plan
// asked for. Leaving two cores is not superstition: the parent agent, the TUI
// render loop and every child's own I/O all want one.
func machinePlanWorkers() int {
	available := runtime.NumCPU() - 2
	if available < minPlanWorkers {
		available = minPlanWorkers
	}
	if available > maxPlanWorkers {
		available = maxPlanWorkers
	}
	return available
}

// effectivePlanWorkers is what the plan will ACTUALLY run with: the smaller of
// what it asked for and what the machine can carry.
//
// The two numbers are kept apart and both reported. A plan that asked for
// sixteen and ran six has not been given sixteen, and saying so is the
// difference between a bound and a fiction — the same reason max_workers is
// rejected outside its range rather than trimmed into it.
func effectivePlanWorkers(requested int) int {
	if requested < 1 {
		requested = 1
	}
	if requested == 1 {
		// Never consult the machine for a sequential plan. One worker must mean
		// one worker on every host, or the sequential path stops being
		// reproducible.
		return 1
	}
	if machine := machinePlanWorkers(); requested > machine {
		return machine
	}
	return requested
}

// planSlots is the worker pool: a counting semaphore plus the completions.
//
// A channel of results rather than a WaitGroup, because the scheduler needs to
// ACT on each completion as it lands — a finished task unblocks its dependents
// and returns its spend to the budget — and a WaitGroup only says "all done".
type planSlots struct {
	limit    int
	inFlight int
	done     chan taskCompletion
	mu       sync.Mutex
}

// taskCompletion is one finished dispatch on its way back to the scheduler.
type taskCompletion struct {
	id     string
	result TaskResult
	err    error
	// started is when the dispatch began, so a runner that reported no duration
	// is measured by the scheduler rather than recorded as instantaneous.
	started time.Time
}

func newPlanSlots(limit int) *planSlots {
	if limit < 1 {
		limit = 1
	}
	return &planSlots{limit: limit, done: make(chan taskCompletion, limit)}
}

// full reports whether every worker is busy.
func (slots *planSlots) full() bool {
	slots.mu.Lock()
	defer slots.mu.Unlock()
	return slots.inFlight >= slots.limit
}

// busy reports whether anything is still running, which is what tells the
// scheduler it must keep draining before it can finish.
func (slots *planSlots) busy() bool {
	slots.mu.Lock()
	defer slots.mu.Unlock()
	return slots.inFlight > 0
}

// take claims a worker slot.
func (slots *planSlots) take() {
	slots.mu.Lock()
	slots.inFlight++
	slots.mu.Unlock()
}

// release frees a slot. Called by the scheduler when it harvests a completion,
// not by the goroutine that produced it — so a slot is free exactly when the
// scheduler has finished acting on the result, never while it is still applying
// it to the budget.
func (slots *planSlots) release() {
	slots.mu.Lock()
	if slots.inFlight > 0 {
		slots.inFlight--
	}
	slots.mu.Unlock()
}

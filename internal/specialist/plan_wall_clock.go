package specialist

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// planWallClock is a plan's wall budget, measured in RUNNING time.
//
// A PAUSED PLAN IS NOT SPENDING ITS WALL BUDGET. The bound used to be a plain
// context.WithTimeout started at admission, so the clock kept running while the
// user had the plan paused: pausing a thirty-minute plan for twenty minutes left
// it ten, and pausing one for longer than its budget meant it was already dead
// when the user resumed it — killed, with the reason "max_wall_seconds elapsed
// while this task was running", by time in which nothing ran at all. The budget
// exists to bound what a plan SPENDS, and a paused plan spends nothing.
//
// WHAT IS CREDITED, EXACTLY: time during which dispatch is held at the pause
// gate AND no task is in flight. A pause holds only the dispatch walk — a child
// already talking to a provider cannot be suspended — so with max_workers > 1 a
// task can keep running for part or all of a pause. That time is real work and
// stays charged; only the stretch where the plan genuinely ran nothing is given
// back. Crediting the whole pause would let a plan run for longer than its
// budget by pausing while work was in flight.
//
// THE STATE IS THE CLOCK'S OWN, under one lock, and the watcher decides expiry
// under that same lock. So a pause that begins as the timer fires is serialised
// against it: either the watcher saw the budget gone before the pause began (and
// it really was), or it sees the plan idle and parks until the next transition.
// Crediting time only after the pause returned — the previous design — let the
// original timer fire mid-pause and cancel a plan whose credit had not been
// applied yet.
//
// Nothing is subtracted from a plan that is never paused, so an ordinary run is
// bounded exactly as before.
type planWallClock struct {
	budget  time.Duration
	started time.Time
	source  wallTime

	mu sync.Mutex
	// credited is idle time already given back, across every closed idle
	// stretch.
	credited time.Duration
	// held is true while the dispatch walk is parked at the pause gate.
	held bool
	// running is the number of tasks in flight.
	running int
	// idleSince opens the current idle stretch; meaningful only while
	// idleLocked().
	idleSince time.Time
	// wake tells the watcher the state moved, so it re-evaluates rather than
	// sleeping to a deadline that no longer applies. Capacity one: a pending
	// wake already means "look again".
	wake chan struct{}

	// expired records that THIS clock cancelled the plan, which is what tells a
	// wall-budget stop apart from a person pressing stop. The context can no
	// longer answer that: it is cancelled rather than deadlined, precisely so its
	// expiry time can move.
	expired atomic.Bool
}

// wallTime is where a clock reads the time and arms its timer. The zero value is
// the real clock; tests inject a controllable one so the watcher's lifecycle can
// be driven deterministically.
type wallTime struct {
	now func() time.Time
	// timerAt fires once at or after deadline. stop releases it early.
	timerAt func(deadline time.Time) (fired <-chan time.Time, stop func() bool)
}

func (w wallTime) withDefaults() wallTime {
	if w.now == nil {
		w.now = time.Now
	}
	if w.timerAt == nil {
		now := w.now
		w.timerAt = func(deadline time.Time) (<-chan time.Time, func() bool) {
			timer := time.NewTimer(deadline.Sub(now()))
			return timer.C, timer.Stop
		}
	}
	return w
}

// withWallTime runs a plan's wall budget on the given time source. Unexported:
// it exists so the pause/expiry lifecycle can be tested without real sleeps.
func withWallTime(source wallTime) ExecOption {
	return func(o *execOptions) { o.wallTime = source }
}

// newPlanWallClock returns nil when the plan has no wall bound, which every
// method below treats as unbounded.
func newPlanWallClock(budget time.Duration, source wallTime) *planWallClock {
	if budget <= 0 {
		return nil
	}
	source = source.withDefaults()
	return &planWallClock{
		budget:  budget,
		started: source.now(),
		source:  source,
		wake:    make(chan struct{}, 1),
	}
}

// idleLocked is the one condition under which time is not charged.
func (c *planWallClock) idleLocked() bool {
	return c.held && c.running == 0
}

// remaining is how much running time is left. Unbounded clocks return a positive
// duration forever.
func (c *planWallClock) remaining() time.Duration {
	if c == nil {
		return 1<<63 - 1
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.source.now()
	ran := now.Sub(c.started) - c.credited
	if c.idleLocked() {
		ran -= now.Sub(c.idleSince)
	}
	return c.budget - ran
}

// exhausted reports that the running-time budget is gone.
func (c *planWallClock) exhausted() bool {
	return c != nil && c.remaining() <= 0
}

// wallExpired reports that this clock is what stopped the plan, as opposed to a
// user cancelling it.
func (c *planWallClock) wallExpired() bool {
	return c != nil && c.expired.Load()
}

// transition applies one state change, closing the idle stretch it ends or
// opening the one it starts, and wakes the watcher.
func (c *planWallClock) transition(change func()) {
	c.mu.Lock()
	now := c.source.now()
	if c.idleLocked() {
		c.credited += now.Sub(c.idleSince)
	}
	change()
	if c.idleLocked() {
		c.idleSince = now
	}
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// holdDispatch marks the walk as parked at the pause gate. Called BEFORE the
// executor blocks, so the watcher can never observe a pause it has not been told
// about.
func (c *planWallClock) holdDispatch() {
	if c == nil {
		return
	}
	c.transition(func() { c.held = true })
}

// releaseDispatch marks the walk as moving again.
func (c *planWallClock) releaseDispatch() {
	if c == nil {
		return
	}
	c.transition(func() { c.held = false })
}

// taskStarted counts a task as in flight and returns the call that ends it.
// The returned func is idempotent, so a recover path that may or may not follow
// the normal one cannot count a task out twice.
func (c *planWallClock) taskStarted() (finished func()) {
	if c == nil {
		return func() {}
	}
	c.transition(func() { c.running++ })
	var once sync.Once
	return func() {
		once.Do(func() { c.transition(func() { c.running-- }) })
	}
}

// watch cancels the plan once its RUNNING time is spent.
//
// A rescheduling timer, not a poll: while the plan is charging time it sleeps
// until the budget would run out; while it is idle it does not arm a timer at
// all and waits for the next transition. The deadline is absolute (start +
// credit + budget), so a transition that lands between computing it and arming
// the timer cannot shift it.
func (c *planWallClock) watch(ctx context.Context, cancel context.CancelFunc) {
	if c == nil {
		return
	}
	go func() {
		// Every goroutine gets recover(): a panic in the backstop must not take
		// down the plan it exists to bound.
		defer func() { _ = recover() }()
		for {
			c.mu.Lock()
			if c.idleLocked() {
				c.mu.Unlock()
				select {
				case <-ctx.Done():
					return
				case <-c.wake:
				}
				continue
			}
			deadline := c.started.Add(c.credited + c.budget)
			if !c.source.now().Before(deadline) {
				// DECIDED UNDER THE LOCK: no pause transition can interleave
				// between seeing the budget gone and recording that it is.
				c.expired.Store(true)
				c.mu.Unlock()
				cancel()
				return
			}
			c.mu.Unlock()
			fired, stop := c.source.timerAt(deadline)
			select {
			case <-ctx.Done():
				stop()
				return
			case <-fired:
			case <-c.wake:
				stop()
			}
		}
	}()
}

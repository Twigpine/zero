package specialist

import (
	"context"
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
// Nothing is subtracted from a plan that is never paused, so an ordinary run is
// bounded exactly as before.
type planWallClock struct {
	budget  time.Duration
	started time.Time
	now     func() time.Time
	// paused is nanoseconds accumulated across every pause. Atomic because the
	// dispatch loop adds to it while the expiry goroutine reads it.
	paused atomic.Int64
	// expired records that THIS clock cancelled the plan, which is what tells a
	// wall-budget stop apart from a person pressing stop. The context can no
	// longer answer that: it is cancelled rather than deadlined, precisely so its
	// expiry time can move.
	expired atomic.Bool
}

// newPlanWallClock returns nil when the plan has no wall bound, which every
// method below treats as unbounded.
func newPlanWallClock(budget time.Duration, now func() time.Time) *planWallClock {
	if budget <= 0 {
		return nil
	}
	if now == nil {
		now = time.Now
	}
	return &planWallClock{budget: budget, started: now(), now: now}
}

// remaining is how much running time is left. Unbounded clocks return a positive
// duration forever.
func (c *planWallClock) remaining() time.Duration {
	if c == nil {
		return 1<<63 - 1
	}
	ran := c.now().Sub(c.started) - time.Duration(c.paused.Load())
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

// addPaused credits time the plan spent paused back to its budget.
func (c *planWallClock) addPaused(d time.Duration) {
	if c == nil || d <= 0 {
		return
	}
	c.paused.Add(int64(d))
}

// watch cancels the plan once its RUNNING time is spent.
//
// A rescheduling timer, not a poll: it sleeps until the budget would run out,
// and if pausing has since pushed that moment further away it sleeps again for
// exactly the difference. So a plan that is never paused wakes this goroutine
// once, and a paused one wakes it once per pause — no interval to tune, and no
// drift between the bound and the moment it is enforced.
func (c *planWallClock) watch(ctx context.Context, cancel context.CancelFunc) {
	if c == nil {
		return
	}
	go func() {
		// Every goroutine gets recover(): a panic in the backstop must not take
		// down the plan it exists to bound.
		defer func() { _ = recover() }()
		timer := time.NewTimer(c.remaining())
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				left := c.remaining()
				if left <= 0 {
					c.expired.Store(true)
					cancel()
					return
				}
				timer.Reset(left)
			}
		}
	}()
}

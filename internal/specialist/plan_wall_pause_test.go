package specialist

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// fakeWallTime is a manually advanced clock whose timers fire only when the test
// moves time past them, so the wall watcher's lifecycle runs without sleeps.
type fakeWallTime struct {
	mu     sync.Mutex
	now    time.Time
	timers map[*fakeWallTimer]struct{}
	// armed receives every deadline the watcher arms, so a test can wait for
	// the watcher to have settled on a specific deadline.
	armed chan time.Time
}

type fakeWallTimer struct {
	at    time.Time
	fired chan time.Time
}

func newFakeWallTime(start time.Time) *fakeWallTime {
	return &fakeWallTime{now: start, timers: map[*fakeWallTimer]struct{}{}, armed: make(chan time.Time, 256)}
}

func (f *fakeWallTime) source() wallTime {
	return wallTime{now: f.Now, timerAt: f.timerAt}
}

func (f *fakeWallTime) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeWallTime) timerAt(deadline time.Time) (<-chan time.Time, func() bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	timer := &fakeWallTimer{at: deadline, fired: make(chan time.Time, 1)}
	if !f.now.Before(deadline) {
		timer.fired <- f.now
	} else {
		f.timers[timer] = struct{}{}
	}
	select {
	case f.armed <- deadline:
	default:
	}
	return timer.fired, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		_, pending := f.timers[timer]
		delete(f.timers, timer)
		return pending
	}
}

func (f *fakeWallTime) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	for timer := range f.timers {
		if !f.now.Before(timer.at) {
			timer.fired <- f.now
			delete(f.timers, timer)
		}
	}
}

// pauseAtRecorder simulates a user pause at one task boundary by advancing the
// clock while the executor is parked in WaitWhilePaused.
type pauseAtRecorder struct {
	recordingRecorder
	clock    *fakeWallTime
	boundary int32
	pauseFor time.Duration
	seen     atomic.Int32
}

func (r *pauseAtRecorder) PlanRunning(context.CancelFunc) {}
func (r *pauseAtRecorder) WaitWhilePaused(context.Context) {
	if r.seen.Add(1) == r.boundary {
		// Settle the watcher while dispatch is still held, then keep the pause
		// in place until it has processed any timer fired by the clock jump.
		// Without the idle hold, that timer cancels the plan before resume.
		synctest.Wait()
		r.clock.Advance(r.pauseFor)
		synctest.Wait()
	}
}

// A PAUSE LONGER THAN WHAT IS LEFT OF THE BUDGET MUST NOT KILL THE PLAN.
//
// The watcher used to arm a timer for the pre-pause remainder and the credit was
// applied only after the pause returned, so a 20s pause with 4s left fired the
// timer mid-pause, cancelled the plan, and the credit landed too late. The pause
// now holds the clock BEFORE the executor blocks; after resume the next task
// runs, and the four seconds that were genuinely left are still enforced.
func TestAPauseLongerThanTheRemainingWallBudgetDoesNotKillThePlan(t *testing.T) {
	synctest.Test(t, testPauseLongerThanRemainingWallBudget)
}

func testPauseLongerThanRemainingWallBudget(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x"), task("b", "y"), task("c", "z")},
		map[string]any{"max_workers": float64(1), "max_wall_seconds": float64(10)}, readOnlyLimits())

	start := time.Unix(1_000_000, 0)
	clock := newFakeWallTime(start)
	// Boundary 2 is the gate before "b": "a" has run 6s of the 10s budget.
	recorder := &pauseAtRecorder{clock: clock, boundary: 2, pauseFor: 20 * time.Second}
	resumedDeadline := start.Add(20*time.Second + 10*time.Second)

	var bStartedLive, bLiveAt3s atomic.Bool
	var cRan atomic.Bool
	run := func(ctx context.Context, req PlanTaskRequest) (TaskResult, error) {
		switch req.Task.ID {
		case "a":
			clock.Advance(6 * time.Second)
			return TaskResult{Outcome: TaskSucceeded, Output: "a done"}, nil
		case "b":
			bStartedLive.Store(ctx.Err() == nil)
			// Wait for the watcher to settle on the post-pause deadline (start
			// + 20s credit + 10s budget). Without the fix it cancels instead.
			guard := time.After(10 * time.Second)
		settle:
			for {
				select {
				case deadline := <-clock.armed:
					if deadline.Equal(resumedDeadline) {
						break settle
					}
				case <-ctx.Done():
					return TaskResult{}, ctx.Err()
				case <-guard:
					t.Error("the watcher never re-armed for the post-pause deadline")
					return TaskResult{}, context.Canceled
				}
			}
			// Three of the four remaining seconds: still inside the budget.
			clock.Advance(3 * time.Second)
			synctest.Wait()
			bLiveAt3s.Store(ctx.Err() == nil)
			// Two more: real running time crosses the budget and the watcher
			// must stop this task exactly as it would without any pause.
			clock.Advance(2 * time.Second)
			select {
			case <-ctx.Done():
				return TaskResult{}, ctx.Err()
			case <-time.After(10 * time.Second):
				t.Error("running time past the budget never cancelled the plan")
				return TaskResult{Outcome: TaskSucceeded, Output: "b done"}, nil
			}
		default:
			cRan.Store(true)
			return TaskResult{Outcome: TaskSucceeded, Output: "c done"}, nil
		}
	}

	report := ExecutePlan(context.Background(), plan, []string{"read_file"}, run, recorder,
		withWallTime(clock.source()))

	byID := map[string]TaskResult{}
	for _, result := range report.Tasks {
		byID[result.ID] = result
	}
	if byID["a"].Outcome != TaskSucceeded {
		t.Fatalf("a = %+v", byID["a"])
	}
	if !bStartedLive.Load() {
		t.Fatalf("b was dispatched into a cancelled plan: the pause spent the wall budget (b = %+v)", byID["b"])
	}
	if !bLiveAt3s.Load() {
		t.Fatalf("the plan was cancelled with budget left after the pause (b = %+v)", byID["b"])
	}
	if got := byID["b"]; got.Outcome != TaskCancelled || !strings.Contains(got.Err, "max_wall_seconds") {
		t.Fatalf("b = %+v; running past the budget must cancel it as a wall-budget stop", got)
	}
	if cRan.Load() {
		t.Fatal("c ran after the wall budget was spent")
	}
	if got := byID["c"]; got.Outcome != TaskCancelled || !strings.Contains(got.Err, "max_wall_seconds") {
		t.Fatalf("c = %+v; want cancelled by the wall budget", got)
	}
}

// holdingPauseRecorder pauses at one boundary, advances the clock, and stays
// paused until the plan is cancelled — a pause the user never ends.
type holdingPauseRecorder struct {
	recordingRecorder
	clock    *fakeWallTime
	boundary int32
	pauseFor time.Duration
	seen     atomic.Int32
	t        *testing.T
}

func (r *holdingPauseRecorder) PlanRunning(context.CancelFunc) {}
func (r *holdingPauseRecorder) WaitWhilePaused(ctx context.Context) {
	if r.seen.Add(1) != r.boundary {
		return
	}
	r.clock.Advance(r.pauseFor)
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		r.t.Error("a task running through the pause never spent the wall budget")
	}
}

// A PAUSE DOES NOT REFUND TIME A TASK SPENT RUNNING. A pause holds only the
// dispatch walk; with max_workers > 1 a task already in flight keeps working, and
// its running time is real spend. Here "a" runs through a 20s pause of a 10s
// budget, so the budget expires mid-pause and stops it.
func TestRunningThroughAPauseStillSpendsTheWallBudget(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x"), task("b", "y"), task("c", "z")},
		map[string]any{"max_workers": float64(2), "max_wall_seconds": float64(10)}, readOnlyLimits())

	clock := newFakeWallTime(time.Unix(1_000_000, 0))
	// Boundary 3 is the gate before "c", reached after "b" is harvested and
	// while "a" is still running.
	recorder := &holdingPauseRecorder{clock: clock, boundary: 3, pauseFor: 20 * time.Second, t: t}
	var cRan atomic.Bool
	run := func(ctx context.Context, req PlanTaskRequest) (TaskResult, error) {
		switch req.Task.ID {
		case "a":
			select {
			case <-ctx.Done():
				return TaskResult{}, ctx.Err()
			case <-time.After(10 * time.Second):
				return TaskResult{Outcome: TaskSucceeded, Output: "a done"}, nil
			}
		case "b":
			return TaskResult{Outcome: TaskSucceeded, Output: "b done"}, nil
		default:
			cRan.Store(true)
			return TaskResult{Outcome: TaskSucceeded, Output: "c done"}, nil
		}
	}

	report := ExecutePlan(context.Background(), plan, []string{"read_file"}, run, recorder,
		withWallTime(clock.source()))

	byID := map[string]TaskResult{}
	for _, result := range report.Tasks {
		byID[result.ID] = result
	}
	if got := byID["a"]; got.Outcome != TaskCancelled || !strings.Contains(got.Err, "max_wall_seconds") {
		t.Fatalf("a = %+v; its running time during the pause must exhaust the budget", got)
	}
	if byID["b"].Outcome != TaskSucceeded {
		t.Fatalf("b = %+v", byID["b"])
	}
	if cRan.Load() {
		t.Fatal("c ran after the wall budget was spent")
	}
}

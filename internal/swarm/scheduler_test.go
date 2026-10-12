package swarm

import (
	"math"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/testutil"
)

// testTicker returns a ticker factory backed by a single unbounded-handshake
// channel: each send on ticks unblocks exactly one run-loop iteration, so a test
// drives fires deterministically with no real time.
func testTicker(ticks chan time.Time) tickerFunc {
	return func(time.Duration) (<-chan time.Time, func()) {
		return ticks, func() {}
	}
}

func findJob(jobs []JobStatus, id string) (JobStatus, bool) {
	for _, j := range jobs {
		if j.ID == id {
			return j, true
		}
	}
	return JobStatus{}, false
}

func TestSchedulerFiresAndCountsRuns(t *testing.T) {
	l := newLauncher(okFor) // members complete immediately
	sw := newSwarmFor(t, l)
	sched := sw.Scheduler()
	ticks := make(chan time.Time)
	sched.newTicker = testTicker(ticks)

	id, err := sched.Add(Policy{Model: "m"}, "team", "teammate", "ping", "", Schedule{Every: time.Hour, MaxRuns: 3})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	for i := 0; i < 3; i++ {
		ticks <- time.Time{}
		want := i + 1
		testutil.WaitFor(t, "task completed", func() bool { return sw.Coordinator().Summarize().Done == want })
	}

	// After MaxRuns the job retires itself.
	testutil.WaitFor(t, "job retired", func() bool { _, ok := findJob(sched.List(), id); return !ok })
	if got := len(l.recorded()); got != 3 {
		t.Fatalf("spawned %d members, want 3", got)
	}
	if done := sw.Coordinator().Summarize().Done; done != 3 {
		t.Fatalf("done = %d, want 3", done)
	}
}

func TestSchedulerSkipsWhilePreviousRuns(t *testing.T) {
	gate := make(chan struct{})
	l := newLauncher(okFor)
	l.gate = gate // members block until released
	sw := newSwarmFor(t, l)
	sched := sw.Scheduler()
	ticks := make(chan time.Time)
	sched.newTicker = testTicker(ticks)

	id, err := sched.Add(Policy{Model: "m"}, "team", "teammate", "ping", "", Schedule{Every: time.Hour})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Fire 1: spawns and the member stays running (gated).
	ticks <- time.Time{}
	testutil.WaitFor(t, "first spawn", func() bool { return len(l.recorded()) == 1 })

	// Fire 2: previous still running => skipped, no new spawn.
	ticks <- time.Time{}
	testutil.WaitFor(t, "skip recorded", func() bool {
		j, ok := findJob(sched.List(), id)
		return ok && j.Skipped == 1
	})
	if got := len(l.recorded()); got != 1 {
		t.Fatalf("a skip must not spawn: recorded %d, want 1", got)
	}

	// Release the first member, then fire 3: previous terminal => spawns again.
	close(gate)
	testutil.WaitFor(t, "first done", func() bool { return sw.Coordinator().Summarize().Done == 1 })
	ticks <- time.Time{}
	testutil.WaitFor(t, "second spawn", func() bool { return len(l.recorded()) == 2 })
	// fireIfIdle's spawn (what "second spawn" observes via the launcher) and
	// run's subsequent job.incRuns() are sequential but distinct steps in the
	// scheduler's goroutine; wait for Runs itself rather than assuming the
	// launcher recording it means the job's counter is updated too.
	testutil.WaitFor(t, "second run recorded", func() bool {
		j, ok := findJob(sched.List(), id)
		return ok && j.Runs == 2
	})

	j, ok := findJob(sched.List(), id)
	if !ok {
		t.Fatal("job should still be active (unbounded)")
	}
	if j.Runs != 2 || j.Skipped != 1 {
		t.Fatalf("job runs=%d skipped=%d, want 2/1", j.Runs, j.Skipped)
	}
}

func TestSchedulerAddValidation(t *testing.T) {
	sw := newSwarmFor(t, newLauncher(okFor))
	sched := sw.Scheduler()

	if _, err := sched.Add(Policy{}, "team", "teammate", "t", "", Schedule{Every: time.Millisecond}); err == nil {
		t.Fatal("sub-second interval should be rejected")
	}
	if _, err := sched.Add(Policy{}, "team", "nope", "t", "", Schedule{Every: time.Hour}); err == nil {
		t.Fatal("unknown agent type should be rejected")
	}
	if _, err := sched.Add(Policy{}, "team", "teammate", "   ", "", Schedule{Every: time.Hour}); err == nil {
		t.Fatal("empty task should be rejected")
	}
	if _, err := sched.Add(Policy{}, "team", "teammate", "t", "", Schedule{Every: time.Hour, MaxRuns: -1}); err == nil {
		t.Fatal("negative max_runs should be rejected")
	}
}

func TestSchedulerCancel(t *testing.T) {
	sw := newSwarmFor(t, newLauncher(okFor))
	sched := sw.Scheduler()
	ticks := make(chan time.Time)
	sched.newTicker = testTicker(ticks) // never ticked => never fires

	id, err := sched.Add(Policy{}, "team", "teammate", "t", "", Schedule{Every: time.Hour})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !sched.Cancel(id) {
		t.Fatal("Cancel of an active job should return true")
	}
	if _, ok := findJob(sched.List(), id); ok {
		t.Fatal("cancelled job should be gone from List")
	}
	if sched.Cancel(id) {
		t.Fatal("Cancel of an unknown job should return false")
	}
}

func TestSchedulerCloseStopsJobs(t *testing.T) {
	sw, err := New(Options{BaseDir: t.TempDir(), Launcher: newLauncher(okFor)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sched := sw.Scheduler()
	ticks := make(chan time.Time)
	sched.newTicker = testTicker(ticks)
	for i := 0; i < 3; i++ {
		if _, err := sched.Add(Policy{}, "team", "teammate", "t", "", Schedule{Every: time.Hour}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	sw.Close() // must return (wg waits) and leave no active jobs
	if got := len(sched.List()); got != 0 {
		t.Fatalf("after Close, %d jobs remain, want 0", got)
	}
	if _, err := sched.Add(Policy{}, "team", "teammate", "t", "", Schedule{Every: time.Hour}); err == nil {
		t.Fatal("Add after Close should fail")
	}
}

func TestSchedulerCreatedAfterSwarmCloseIsClosed(t *testing.T) {
	sw := newSwarmFor(t, newLauncher(okFor))
	sw.Close()

	sched := sw.Scheduler()
	sched.mu.Lock()
	closed := sched.closed
	sched.mu.Unlock()
	if !closed {
		t.Fatal("scheduler created after Swarm.Close was not closed")
	}
	if _, err := sched.Add(Policy{}, "team", "teammate", "t", "", Schedule{Every: time.Hour}); err == nil {
		t.Fatal("Add on a scheduler created after Swarm.Close should fail")
	}
	if got := len(sched.List()); got != 0 {
		t.Fatalf("scheduler created after Swarm.Close has %d jobs, want 0", got)
	}
}

func TestSchedulerAddRejectedAfterContextCancel(t *testing.T) {
	sw := newSwarmFor(t, newLauncher(okFor))
	sched := sw.Scheduler()
	// Parent context canceled but s.closed not yet set: Add must still refuse so it
	// never reports a job whose loop exits immediately.
	sched.cancel()
	if _, err := sched.Add(Policy{}, "team", "teammate", "t", "", Schedule{Every: time.Hour}); err == nil {
		t.Fatal("Add after the scheduler context is canceled should fail")
	}
}

func TestSchedulerDailyRecomputesNextDelay(t *testing.T) {
	l := newLauncher(okFor)
	sw := newSwarmFor(t, l)
	sched := sw.Scheduler()
	ticks := make(chan time.Time)
	var delays []time.Duration
	sched.newTicker = func(d time.Duration) (<-chan time.Time, func()) {
		delays = append(delays, d)
		return ticks, func() {}
	}
	// Freeze the clock at 10:00 local so the next 11:00 is deterministically 1h —
	// not the fixed 24h Every. This is what guards against DST drift.
	base := time.Date(2026, 1, 1, 10, 0, 0, 0, time.Local)
	sched.now = func() time.Time { return base }

	if _, err := sched.Add(Policy{Model: "m"}, "team", "teammate", "ping", "",
		Schedule{Every: 24 * time.Hour, Daily: true, Hour: 11, Minute: 0, FirstDelay: nextDailyDelay(base, 11, 0), MaxRuns: 2}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	for i := 0; i < 2; i++ {
		ticks <- time.Time{}
		testutil.WaitFor(t, "task completed", func() bool { return sw.Coordinator().Summarize().Done == i+1 })
	}
	if len(delays) < 2 {
		t.Fatalf("expected at least two requested delays, got %v", delays)
	}
	// The second iteration's delay is recomputed from the clock (1h), not Every (24h).
	if delays[1] != time.Hour {
		t.Fatalf("recomputed daily delay = %s, want 1h (next 11:00 from frozen 10:00)", delays[1])
	}
}

func TestNextDailyDelay(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 6, 15, 10, 0, 0, 0, loc)
	if got := nextDailyDelay(now, 11, 0); got != time.Hour {
		t.Fatalf("ahead-today delay = %s, want 1h", got)
	}
	if got := nextDailyDelay(now, 9, 0); got != 23*time.Hour {
		t.Fatalf("passed-today delay = %s, want 23h", got)
	}
	// Exactly now => next is tomorrow (strictly-after guard).
	if got := nextDailyDelay(now, 10, 0); got != 24*time.Hour {
		t.Fatalf("equal-now delay = %s, want 24h", got)
	}
}

func TestNextDailyDelayHoldsWallClockAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// US spring-forward 2026-03-08: clocks jump 02:00 -> 03:00 (a 23-hour day).
	// At 06:00 EDT the 01:30 target has passed, so the next fire must be 01:30
	// local tomorrow; a fixed Add(24h) would land at 02:30 instead.
	now := time.Date(2026, 3, 8, 6, 0, 0, 0, loc)
	got := now.Add(nextDailyDelay(now, 1, 30))
	if got.Day() != 9 || got.Hour() != 1 || got.Minute() != 30 {
		t.Fatalf("next fire = %s, want 2026-03-09 01:30 local (DST-safe)", got.Format("2006-01-02 15:04 MST"))
	}
}

func TestParseClock(t *testing.T) {
	h, m, err := parseClock(" 09:30 ")
	if err != nil || h != 9 || m != 30 {
		t.Fatalf("parseClock(09:30) = %d,%d,%v", h, m, err)
	}
	for _, bad := range []string{"24:00", "10:60", "9", "x:y", "10:", ":30", "-1:00"} {
		if _, _, err := parseClock(bad); err == nil {
			t.Fatalf("parseClock(%q) should error", bad)
		}
	}
}

func TestSwarmInt(t *testing.T) {
	if v, ok := swarmInt(map[string]any{"n": float64(5)}, "n"); !ok || v != 5 {
		t.Fatalf("float64 => %d,%v", v, ok)
	}
	if v, ok := swarmInt(map[string]any{"n": "7"}, "n"); !ok || v != 7 {
		t.Fatalf("string => %d,%v", v, ok)
	}
	if _, ok := swarmInt(map[string]any{"n": "x"}, "n"); ok {
		t.Fatal("non-numeric string should not parse")
	}
	if _, ok := swarmInt(nil, "n"); ok {
		t.Fatal("nil args should not parse")
	}
	// A non-integer JSON number must be rejected, not truncated to an int.
	if v, ok := swarmInt(map[string]any{"n": 1.9}, "n"); ok {
		t.Fatalf("non-integer float should not parse, got %d", v)
	}
	if _, ok := swarmInt(map[string]any{"n": math.Inf(1)}, "n"); ok {
		t.Fatal("infinity should not parse")
	}
	if _, ok := swarmInt(map[string]any{"n": math.NaN()}, "n"); ok {
		t.Fatal("NaN should not parse")
	}
}

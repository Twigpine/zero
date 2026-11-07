package specialist

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/streamjson"
)

// fakeClock lets a watchdog test advance time without sleeping for minutes.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

// SILENCE is what the watchdog measures, not elapsed time. A task reading forty
// files for four minutes is working; killing it would be worse than having no
// watchdog at all, because a timeout that fires on healthy work teaches users
// to raise it until it never fires.
func TestActivityKeepsATaskAliveHoweverLongItRuns(t *testing.T) {
	clock := &fakeClock{at: time.Unix(1000, 0)}
	watchdog := newStallWatchdog(time.Minute, clock.now)

	// Ten minutes of work, an event every 30s: never stalled.
	for range 20 {
		clock.advance(30 * time.Second)
		if got := watchdog.stalledFor(); got >= time.Minute {
			t.Fatalf("a working task was judged stalled after %s of silence", got)
		}
		watchdog.touch()
	}
	if watchdog.didFire() {
		t.Fatal("the watchdog fired on a task that never went quiet")
	}
}

// ...and silence past the timeout is a stall.
func TestSilencePastTheTimeoutIsAStall(t *testing.T) {
	clock := &fakeClock{at: time.Unix(1000, 0)}
	watchdog := newStallWatchdog(time.Minute, clock.now)

	clock.advance(59 * time.Second)
	if watchdog.stalledFor() >= time.Minute {
		t.Fatal("fired one second early")
	}
	clock.advance(2 * time.Second)
	if watchdog.stalledFor() < time.Minute {
		t.Fatal("did not register a stall past the timeout")
	}
}

// The watchdog cancels the task's OWN context, and the goroutine stops with it.
func TestTheWatchdogCancelsTheTaskAndStops(t *testing.T) {
	clock := &fakeClock{at: time.Unix(1000, 0)}
	watchdog := newStallWatchdog(minStallTimeout, clock.now)
	// Poll fast so the REAL goroutine is exercised in milliseconds. The clock
	// it reads is still fake, so what is being tested is the watcher's logic,
	// not the passage of time.
	watchdog.poll = 2 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := watchdog.watch(ctx, cancel)
	defer stop()

	clock.advance(2 * minStallTimeout)
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-ctx.Done():
			if !watchdog.didFire() {
				t.Fatal("the context was cancelled but the watchdog does not own it")
			}
			return
		case <-deadline:
			t.Fatal("the watchdog never fired on a wedged task")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A too-small timeout is refused rather than honoured: below the floor the
// watchdog fires on ordinary think-time and becomes a random task-killer.
func TestATooSmallStallTimeoutIsRejected(t *testing.T) {
	budget := okBudget()
	budget["max_stall_seconds"] = float64(5)
	_, err := ParsePlan(planArgs([]any{task("a", "x")}, budget), readOnlyLimits())
	if err == nil || !strings.Contains(err.Error(), "max_stall_seconds") {
		t.Fatalf("a 5-second stall timeout must be refused, got %v", err)
	}
}

// An omitted timeout gets the default rather than zero — zero would mean "no
// bound", and the whole point is that something bounds a task.
func TestAnOmittedStallTimeoutFallsBackToTheDefault(t *testing.T) {
	if got := stallTimeoutFor(Budget{}); got != defaultStallTimeout {
		t.Fatalf("stall timeout = %s, want the default %s", got, defaultStallTimeout)
	}
	if got := stallTimeoutFor(Budget{MaxStall: 10 * time.Minute}); got != 10*time.Minute {
		t.Fatalf("an explicit timeout must win, got %s", got)
	}
	// Zero means the default; a positive value is honoured as given, because
	// the floor is enforced on model input in planBudget rather than a second
	// time here.
	if w := newStallWatchdog(0, nil); w.timeout != defaultStallTimeout {
		t.Fatalf("a zero timeout must fall back to the default, got %s", w.timeout)
	}
	if w := newStallWatchdog(90*time.Second, nil); w.timeout != 90*time.Second {
		t.Fatalf("a validated timeout must be honoured as given, got %s", w.timeout)
	}
}

// The progress wrapper feeds the watchdog AND the caller. Dropping either would
// be a silent regression: the watchdog would kill working tasks, or the UI
// would go dark.
func TestWatchedProgressFeedsBothTheWatchdogAndTheCaller(t *testing.T) {
	clock := &fakeClock{at: time.Unix(1000, 0)}
	watchdog := newStallWatchdog(time.Minute, clock.now)
	forwarded := 0

	wrapped := watchedProgress(watchdog, func(streamjson.Event) { forwarded++ })
	clock.advance(30 * time.Second)
	wrapped(streamjson.Event{Type: streamjson.EventToolCall})

	if forwarded != 1 {
		t.Fatalf("the caller's callback was invoked %d times, want 1", forwarded)
	}
	if got := watchdog.stalledFor(); got != 0 {
		t.Fatalf("the event did not reset the stall clock: %s", got)
	}

	// A caller that wired no callback still feeds the watchdog.
	silent := watchedProgress(watchdog, nil)
	clock.advance(20 * time.Second)
	silent(streamjson.Event{Type: streamjson.EventToolCall})
	if got := watchdog.stalledFor(); got != 0 {
		t.Fatalf("the watchdog is not fed when no UI callback is wired: %s", got)
	}
}

// A STALL AND A CANCELLATION ARE DIFFERENT EVENTS. Both surface as a context
// error, and a user who stopped a plan and a plan that hung are looking at very
// different problems.
func TestAStalledTaskReportsAStallNotACancellation(t *testing.T) {
	executor := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(ctx context.Context, _ string, _ []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			// A wedged child: never emits, never returns until cancelled.
			<-ctx.Done()
			return ChildRunResult{Started: true, ExitCode: -1}, ctx.Err()
		},
	}
	runner := NewPlanRunner(PlanTaskContext{Executor: executor, Cwd: t.TempDir(), SpecialistName: "explorer"})

	// A DEADLINE ON THE TEST ITSELF. Without it, a watchdog that fails to fire
	// leaves the wedged child blocking forever and the test hangs rather than
	// failing — which is exactly what happened: a mutation run that disabled
	// the watchdog wedged the whole sweep instead of reporting a caught
	// mutation. A test for a hang must not be able to hang.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// A short timeout here, not the 30s floor: the floor guards MODEL input in
	// planBudget, and a test that sleeps for it is a test nobody runs.
	result, err := runner(ctx, PlanTaskRequest{
		Task:         Task{ID: "wedged", Prompt: "x"},
		Tools:        []string{"read_file"},
		StallTimeout: 40 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("a stalled task is a task result, not a runner error: %v", err)
	}
	if result.Outcome != TaskFailed {
		t.Fatalf("outcome = %q, want a failure", result.Outcome)
	}
	if !strings.Contains(result.Err, "produced no output") {
		t.Fatalf("a stall must say so rather than reading as a cancellation: %q", result.Err)
	}
	if !strings.Contains(result.Err, "max_stall_seconds") {
		t.Fatalf("the message must name the knob that changes it: %q", result.Err)
	}
	// AND IT MUST BE FLAGGED RETRYABLE. Stalled is what the executor reads to
	// decide on another attempt, and every retry test supplies it from a fake
	// runner — so nothing else in the suite can catch the real runner failing to
	// set it. That is the shape of the defect where the budget meter was fed by
	// a counter NewPlanRunner never populated: a fake that fabricates its own
	// inputs cannot test the producer.
	if !result.Stalled {
		t.Fatal("the runner did not flag the stall, so the executor would never retry it")
	}
}

// A task that FAILED rather than stalled must not be flagged retryable — the
// child ran and reported, and the flag is what would spend another one.
func TestAFailedTaskIsNotFlaggedAsStalled(t *testing.T) {
	executor := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000b", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(_ context.Context, _ string, _ []string, emit func(streamjson.Event)) (ChildRunResult, error) {
			// Emits, then fails. The watchdog never fires.
			emit(streamjson.Event{Type: "assistant"})
			return ChildRunResult{Started: true, ExitCode: 1}, errors.New("the child died")
		},
	}
	runner := NewPlanRunner(PlanTaskContext{Executor: executor, Cwd: t.TempDir(), SpecialistName: "explorer"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, _ := runner(ctx, PlanTaskRequest{
		Task:         Task{ID: "broken", Prompt: "x"},
		Tools:        []string{"read_file"},
		StallTimeout: time.Minute,
	})
	if result.Outcome != TaskFailed {
		t.Fatalf("outcome = %q, want a failure", result.Outcome)
	}
	if result.Stalled {
		t.Fatal("an ordinary failure was flagged as a stall, so the executor would retry it")
	}
}

// A wedged task must not take the PLAN with it: the watchdog cancels that
// task's own context, and the plan carries on with the dependents skipped.
func TestAStalledTaskDoesNotCancelTheWholePlan(t *testing.T) {
	plan := mustPlan(t, []any{task("a", "x"), task("b", "y")}, okBudget(), readOnlyLimits())
	parent := context.Background()

	ran := map[string]int{}
	report := ExecutePlan(parent, plan, []string{"read_file"},
		func(ctx context.Context, req PlanTaskRequest) (TaskResult, error) {
			ran[req.Task.ID]++
			if req.Task.ID == "a" {
				// Stalled is what MAKES this a stall. Without the flag the result
				// is an ordinary failure, and this test would have gone on
				// passing while asserting nothing about stalls at all.
				return TaskResult{ID: "a", Outcome: TaskFailed, Stalled: true, Err: "produced no output for 3m0s"}, nil
			}
			return TaskResult{ID: req.Task.ID, Outcome: TaskSucceeded}, nil
		}, nil)

	// a stalls on both its attempts (one plus the default retry); b still runs.
	if ran["a"] != 2 || ran["b"] != 1 {
		t.Fatalf("attempts = %v; a stalled task must exhaust its retries and must not stop independent work", ran)
	}
	if parent.Err() != nil {
		t.Fatal("the parent context was cancelled by a task-level stall")
	}
	if report.Failed != 1 || report.Succeeded != 1 {
		t.Fatalf("report = %+v, want one stall and one success", report)
	}
}

// The plan resolves ONE stall timeout and every task gets it, rather than each
// runner re-deriving it from a budget it would have to be handed anyway.
func TestPlanPassesTheStallTimeoutToEveryTask(t *testing.T) {
	budget := okBudget()
	budget["max_stall_seconds"] = float64(90)
	plan := mustPlan(t, []any{task("a", "x"), task("b", "y")}, budget, readOnlyLimits())

	var seen []time.Duration
	ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			seen = append(seen, req.StallTimeout)
			return TaskResult{ID: req.Task.ID, Outcome: TaskSucceeded}, nil
		}, nil)

	if len(seen) != 2 {
		t.Fatalf("expected two dispatches, got %d", len(seen))
	}
	for index, got := range seen {
		if got != 90*time.Second {
			t.Fatalf("task %d received a stall timeout of %s, want the plan's 90s", index, got)
		}
	}
}

// A CHILD THAT IS TALKING MUST SURVIVE, however long it runs. This is the
// watchdog's whole premise, and the wedged-child test cannot check it: a child
// that emits nothing stalls whether or not its events are wired to the clock.
//
// STALLPOLL IS WHAT MAKES THIS TEST ABLE TO FAIL. watch() floors its interval at
// one second, so a 60ms StallTimeout still polled once a second and a child
// living 300ms got zero ticks — the watcher never looked, and the test passed
// identically with watchedProgress unwired, which is the one thing it claims to
// cover. Driving the poll in milliseconds makes the goroutine actually run
// several times inside the child's life.
func TestAChattyChildOutlivesItsStallTimeout(t *testing.T) {
	emitted := 0
	executor := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(ctx context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			// Work for well past the stall timeout, speaking throughout.
			deadline := time.After(300 * time.Millisecond)
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return ChildRunResult{Started: true, ExitCode: -1}, ctx.Err()
				case <-deadline:
					return ChildRunResult{Started: true}, nil
				case <-ticker.C:
					if progress != nil {
						progress(streamjson.Event{Type: streamjson.EventToolCall, Name: "read_file"})
						emitted++
					}
				}
			}
		},
	}
	runner := NewPlanRunner(PlanTaskContext{Executor: executor, Cwd: t.TempDir(), SpecialistName: "explorer"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := runner(ctx, PlanTaskRequest{
		Task:  Task{ID: "chatty", Prompt: "x"},
		Tools: []string{"read_file"},
		// Far shorter than the child's runtime: only silence may stop it.
		StallTimeout: 60 * time.Millisecond,
		// Roughly thirty checks inside the child's 300ms, each one a real chance
		// to declare it stalled. Without this the watcher ticks zero times.
		StallPoll: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("a working task must not error: %v", err)
	}
	if emitted == 0 {
		t.Fatal("setup: the child never emitted, so this proves nothing")
	}
	if strings.Contains(result.Err, "produced no output") {
		t.Fatalf("a child that spoke every 10ms was killed by a 60ms stall timeout: %q", result.Err)
	}
	if result.Outcome != TaskSucceeded {
		t.Fatalf("outcome = %q (%s), want success", result.Outcome, result.Err)
	}
}

// A STALL CANCELS THAT TASK, NOT THE RUN. The watchdog owns a context derived
// from the caller's; cancelling the caller's instead would end the whole plan
// on one wedged task.
func TestAStallLeavesTheParentContextAlive(t *testing.T) {
	executor := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return "specialist_00000000000000000000000a", nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(ctx context.Context, _ string, _ []string, _ func(streamjson.Event)) (ChildRunResult, error) {
			<-ctx.Done()
			return ChildRunResult{Started: true, ExitCode: -1}, ctx.Err()
		},
	}
	runner := NewPlanRunner(PlanTaskContext{Executor: executor, Cwd: t.TempDir(), SpecialistName: "explorer"})

	// A plain background context with NO deadline: if the runner cancels this
	// one, the plan is over. Nothing else here would cancel it, so its state
	// after the call is the assertion.
	parent := context.Background()
	result, _ := runner(parent, PlanTaskRequest{
		Task:         Task{ID: "wedged", Prompt: "x"},
		Tools:        []string{"read_file"},
		StallTimeout: 40 * time.Millisecond,
	})
	if !strings.Contains(result.Err, "produced no output") {
		t.Fatalf("setup: expected a stall, got %q", result.Err)
	}
	if parent.Err() != nil {
		t.Fatalf("the stall cancelled the caller's context: %v", parent.Err())
	}
}

// THE PARENT'S PREPARATION IS NOT THE CHILD'S SILENCE. The watchdog was armed
// when the runner began, before the executor resolved the manifest, built the
// arguments and wrote the start accounting — so a slow launch was charged as
// the child going quiet, and a task could be killed as stalled before it
// existed. On a loaded Windows runner that is exactly how this file's chatty-
// child test once failed with the child never having been called.
//
// Here the pre-launch step (minting the child's session id) takes several
// stall windows; the child, once launched, speaks every 10ms and must run to
// completion.
func TestASlowLaunchIsNotChargedAsTheChildsSilence(t *testing.T) {
	launched := false
	executor := Executor{
		BinaryPath: "/bin/true",
		NewSessionID: func() (string, error) {
			time.Sleep(250 * time.Millisecond) // parent-side, before launch
			return "specialist_00000000000000000000000b", nil
		},
		Load: func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(ctx context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			launched = true
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			deadline := time.After(100 * time.Millisecond)
			for {
				select {
				case <-ctx.Done():
					return ChildRunResult{Started: true, ExitCode: -1}, ctx.Err()
				case <-deadline:
					return ChildRunResult{Started: true}, nil
				case <-ticker.C:
					if progress != nil {
						progress(streamjson.Event{Type: streamjson.EventToolCall, Name: "read_file"})
					}
				}
			}
		},
	}
	runner := NewPlanRunner(PlanTaskContext{Executor: executor, Cwd: t.TempDir(), SpecialistName: "explorer"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := runner(ctx, PlanTaskRequest{
		Task:         Task{ID: "slow-launch", Prompt: "x"},
		Tools:        []string{"read_file"},
		StallTimeout: 60 * time.Millisecond,
		StallPoll:    10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("a working task must not error: %v", err)
	}
	if !launched {
		t.Fatalf("the child was never launched: the watchdog fired during the parent's own preparation (%s)", result.Err)
	}
	if result.Outcome != TaskSucceeded {
		t.Fatalf("outcome = %q (%s), want success", result.Outcome, result.Err)
	}
}

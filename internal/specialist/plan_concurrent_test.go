package specialist

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/tools"
)

// concurrencyProbe records the maximum number of tasks in flight at once, and
// the order dispatches began.
type concurrencyProbe struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	started  []string
	release  chan struct{}
}

func (p *concurrencyProbe) runner() PlanRunner {
	return func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
		p.mu.Lock()
		p.inFlight++
		if p.inFlight > p.peak {
			p.peak = p.inFlight
		}
		p.started = append(p.started, req.Task.ID)
		p.mu.Unlock()

		if p.release != nil {
			<-p.release
		}
		p.mu.Lock()
		p.inFlight--
		p.mu.Unlock()
		return TaskResult{Outcome: TaskSucceeded}, nil
	}
}

func fanOutPlan(t *testing.T, workers int, n int) Plan {
	t.Helper()
	tasks := make([]any, 0, n)
	for i := 0; i < n; i++ {
		tasks = append(tasks, task(string(rune('a'+i)), "x"))
	}
	budget := map[string]any{"max_workers": float64(workers)}
	return mustPlan(t, tasks, budget, readOnlyLimits())
}

// ONE WORKER IS STILL ONE AT A TIME. The whole equivalence claim rests on this:
// there is one scheduler, and with a single worker it must never overlap.
func TestOneWorkerNeverOverlaps(t *testing.T) {
	probe := &concurrencyProbe{}
	report := ExecutePlan(context.Background(), fanOutPlan(t, 1, 6), []string{"read_file"}, probe.runner(), nil)
	if probe.peak != 1 {
		t.Fatalf("peak concurrency %d with one worker; it must be 1", probe.peak)
	}
	if report.Succeeded != 6 {
		t.Fatalf("report = %+v", report)
	}
	if report.Workers != 1 || report.WorkersRequested != 1 {
		t.Fatalf("workers reported %d/%d", report.Workers, report.WorkersRequested)
	}
}

// ...and one worker dispatches in the plan's validated order, which is what
// makes the sequential path reproducible rather than merely serial.
func TestOneWorkerDispatchesInPlanOrder(t *testing.T) {
	probe := &concurrencyProbe{}
	plan := fanOutPlan(t, 1, 6)
	ExecutePlan(context.Background(), plan, []string{"read_file"}, probe.runner(), nil)
	for index, id := range plan.Order() {
		if probe.started[index] != id {
			t.Fatalf("dispatch order %v, want the plan's order %v", probe.started, plan.Order())
		}
	}
}

// TASKS ACTUALLY RUN AT ONCE. Asserting only that a plan with 4 workers
// completes would pass against a scheduler that ignored the setting entirely.
//
// MEASURED AGAINST WHAT THIS MACHINE WILL RUN, not against what the plan asked
// for. effectivePlanWorkers caps the request at runtime.NumCPU()-2, so a plan
// asking for four gets two on a four-core CI runner — and the literal 4 this
// once asserted passed on a ten-core laptop while failing on every CI platform,
// deterministically. It was measuring the host, not the scheduler.
//
// The floor is what keeps it honest: minPlanWorkers is 2, so want is never
// below 2 and this still fails against a scheduler that runs tasks one at a
// time, which is the only thing it exists to catch.
func TestIndependentTasksRunConcurrently(t *testing.T) {
	probe := &concurrencyProbe{release: make(chan struct{})}
	const requested = 4
	want := effectivePlanWorkers(requested)
	if want < 2 {
		t.Fatalf("effectivePlanWorkers(%d) = %d; the floor guarantees at least 2", requested, want)
	}
	plan := fanOutPlan(t, requested, 4)

	done := make(chan PlanReport, 1)
	go func() {
		done <- ExecutePlan(context.Background(), plan, []string{"read_file"}, probe.runner(), nil)
	}()

	// Wait until as many are in flight together as this host allows, which can
	// only happen if they genuinely overlap.
	deadline := time.After(5 * time.Second)
	for {
		probe.mu.Lock()
		peak := probe.peak
		probe.mu.Unlock()
		if peak >= want {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("peak concurrency reached only %d of %d (plan asked for %d, this host allows %d)",
				peak, want, requested, machinePlanWorkers())
		case <-time.After(5 * time.Millisecond):
		}
	}
	close(probe.release)
	report := <-done
	if report.Succeeded != 4 {
		t.Fatalf("report = %+v", report)
	}
}

// DEPENDENCIES STILL HOLD. Concurrency may overlap independent work and must
// never start a task before what it waits on has finished.
func TestADependentTaskNeverStartsEarly(t *testing.T) {
	// a -> (b, c) -> d, with room to run everything at once if the scheduler
	// forgot the edges.
	plan := mustPlan(t, []any{
		task("a", "root"), task("b", "left", "a"), task("c", "right", "a"), task("d", "join", "b", "c"),
	}, map[string]any{"max_workers": float64(4)}, readOnlyLimits())

	var mu sync.Mutex
	finished := map[string]bool{}
	var violations []string

	ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			mu.Lock()
			for _, dep := range req.Task.DependsOn {
				if !finished[dep] {
					violations = append(violations, req.Task.ID+" started before "+dep)
				}
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			finished[req.Task.ID] = true
			mu.Unlock()
			return TaskResult{Outcome: TaskSucceeded}, nil
		}, nil)

	if len(violations) > 0 {
		t.Fatalf("dependency order broken: %v", violations)
	}
}

// A DIAMOND OVERLAPS ITS MIDDLE. b and c are independent, so the scheduler must
// run them together — otherwise concurrency is wired but inert.
func TestADiamondsIndependentTasksOverlap(t *testing.T) {
	plan := mustPlan(t, []any{
		task("a", "root"), task("b", "left", "a"), task("c", "right", "a"), task("d", "join", "b", "c"),
	}, map[string]any{"max_workers": float64(4)}, readOnlyLimits())

	var mu sync.Mutex
	inFlight, peak := 0, 0
	ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			return TaskResult{Outcome: TaskSucceeded}, nil
		}, nil)

	if peak < 2 {
		t.Fatalf("peak concurrency %d on a diamond; b and c are independent and must overlap", peak)
	}
}

// THE WORKER COUNT IS A CAP. A plan asking for 2 must never run 3, however many
// tasks are ready.
func TestTheWorkerCountIsRespected(t *testing.T) {
	probe := &concurrencyProbe{}
	report := ExecutePlan(context.Background(), fanOutPlan(t, 2, 8), []string{"read_file"}, probe.runner(), nil)
	if probe.peak > 2 {
		t.Fatalf("peak concurrency %d exceeded the 2 workers asked for", probe.peak)
	}
	if report.Succeeded != 8 {
		t.Fatalf("report = %+v", report)
	}
}

// THE MACHINE'S CAPACITY BOUNDS THE REQUEST, and both numbers are reported. A
// plan that asked for sixteen and ran six has not been given sixteen.
func TestTheEffectiveWorkerCountIsReported(t *testing.T) {
	if got := effectivePlanWorkers(1); got != 1 {
		t.Fatalf("one worker must stay one on every host, got %d", got)
	}
	if got := effectivePlanWorkers(maxPlanWorkers); got > machinePlanWorkers() {
		t.Fatalf("effective workers %d exceeds the machine's %d", got, machinePlanWorkers())
	}
	if machinePlanWorkers() < minPlanWorkers {
		t.Fatalf("machine workers %d fell below the floor", machinePlanWorkers())
	}

	report := ExecutePlan(context.Background(), fanOutPlan(t, maxPlanWorkers, 2), []string{"read_file"},
		func(context.Context, PlanTaskRequest) (TaskResult, error) {
			return TaskResult{Outcome: TaskSucceeded}, nil
		}, nil)
	if report.WorkersRequested != maxPlanWorkers {
		t.Fatalf("requested %d, want %d", report.WorkersRequested, maxPlanWorkers)
	}
	if report.Workers > report.WorkersRequested || report.Workers < 1 {
		t.Fatalf("effective workers %d is not a bound on %d", report.Workers, report.WorkersRequested)
	}
}

// A PANICKING TASK MUST NOT HANG THE PLAN. The slot has to come back, or the
// scheduler waits forever for a worker that will never report.
func TestAPanickingTaskFreesItsWorker(t *testing.T) {
	plan := fanOutPlan(t, 2, 4)
	var runs atomic.Int32
	done := make(chan PlanReport, 1)
	go func() {
		done <- ExecutePlan(context.Background(), plan, []string{"read_file"},
			func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
				if runs.Add(1) == 1 {
					panic("boom")
				}
				return TaskResult{Outcome: TaskSucceeded}, nil
			}, nil)
	}()
	select {
	case report := <-done:
		if report.Failed != 1 {
			t.Fatalf("report = %+v; the panicking task must be recorded as failed", report)
		}
		if report.Succeeded != 3 {
			t.Fatalf("report = %+v; the other tasks must still run", report)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a panicking task hung the plan; its worker slot was never returned")
	}
}

// EVERY TASK IS HARVESTED before the report is assembled. A plan that reported
// while work was still in flight would report on tasks that had not finished.
func TestNoTaskIsLeftInFlightWhenThePlanReports(t *testing.T) {
	plan := fanOutPlan(t, 4, 12)
	var running atomic.Int32
	report := ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(context.Context, PlanTaskRequest) (TaskResult, error) {
			running.Add(1)
			time.Sleep(2 * time.Millisecond)
			running.Add(-1)
			return TaskResult{Outcome: TaskSucceeded}, nil
		}, nil)
	if left := running.Load(); left != 0 {
		t.Fatalf("%d tasks still running when the plan reported", left)
	}
	if len(report.Tasks) != 12 || report.Succeeded != 12 {
		t.Fatalf("report = %+v", report)
	}
	// The report is in the PLAN's order, not completion order — a concurrent run
	// finishes out of order and the record must not.
	for index, id := range plan.Order() {
		if report.Tasks[index].ID != id {
			t.Fatalf("report order %v, want plan order %v", report.Tasks[index].ID, id)
		}
	}
}

// THE EVENT LOG STAYS SINGLE-WRITER under concurrency, and that is what makes
// the resume reducer safe.
//
// Tasks run on their own goroutines; the five lifecycle events do NOT. Dispatch
// is recorded by the walk before the goroutine starts, and every terminal event
// by the harvest that the same walk performs. So the log is written by one
// goroutine in a well-defined order however many tasks overlap — which is why
// ReducePlanEvents can rely on Sequence and why nothing here needs a lock.
func TestLifecycleEventsAreWrittenBySingleGoroutine(t *testing.T) {
	plan := fanOutPlan(t, 4, 12)
	recorder := &goroutineWitness{}
	ExecutePlan(context.Background(), plan, []string{"read_file"},
		func(context.Context, PlanTaskRequest) (TaskResult, error) {
			time.Sleep(2 * time.Millisecond)
			return TaskResult{Outcome: TaskSucceeded}, nil
		}, recorder)

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.goroutines) != 1 {
		t.Fatalf("the event log was written from %d goroutines: %v", len(recorder.goroutines), recorder.goroutines)
	}
	if recorder.dispatched != 12 || recorder.completed != 12 {
		t.Fatalf("recorded %d dispatches and %d completions for 12 tasks", recorder.dispatched, recorder.completed)
	}
}

// goroutineWitness records which goroutine each recorder call arrived on.
type goroutineWitness struct {
	mu         sync.Mutex
	goroutines map[string]bool
	dispatched int
	completed  int
}

func (w *goroutineWitness) note() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.goroutines == nil {
		w.goroutines = map[string]bool{}
	}
	w.goroutines[goroutineLabel()] = true
}

func (w *goroutineWitness) TaskDispatched(Task) {
	w.note()
	w.mu.Lock()
	w.dispatched++
	w.mu.Unlock()
}

func (w *goroutineWitness) TaskCompleted(TaskResult) {
	w.note()
	w.mu.Lock()
	w.completed++
	w.mu.Unlock()
}

func (w *goroutineWitness) TaskFailed(TaskResult) { w.note() }

// goroutineLabel identifies the calling goroutine from its stack header. Only a
// test uses this; production code has no business knowing which goroutine it is
// on, which is exactly the property being asserted.
func goroutineLabel() string {
	buffer := make([]byte, 64)
	n := runtime.Stack(buffer, false)
	return string(buffer[:n])[:20]
}

// RESUME AFTER A PARTIAL CONCURRENT RUN. Several tasks can be in flight when the
// process dies, so the reducer must report EVERY unfinished one — not just the
// last — and the remainder must re-run all of them.
func TestResumeAfterAPartialConcurrentRun(t *testing.T) {
	plan := mustPlan(t, []any{
		task("a", "one"), task("b", "two"), task("c", "three"), task("d", "four"),
		task("e", "join", "a", "b", "c", "d"),
	}, map[string]any{"max_workers": float64(4)}, readOnlyLimits())

	// a and c finished; b and d were dispatched and never came back — the shape
	// a crash during a four-wide fan-out actually leaves.
	events := []sessions.Event{
		planEvent(1, func() (sessions.EventType, map[string]any) { return PlanAdmittedEvent(plan) }),
		planEvent(2, func() (sessions.EventType, map[string]any) { return TaskDispatchedEvent(Task{ID: "a"}) }),
		planEvent(3, func() (sessions.EventType, map[string]any) { return TaskDispatchedEvent(Task{ID: "b"}) }),
		planEvent(4, func() (sessions.EventType, map[string]any) { return TaskDispatchedEvent(Task{ID: "c"}) }),
		planEvent(5, func() (sessions.EventType, map[string]any) { return TaskDispatchedEvent(Task{ID: "d"}) }),
		// Terminal events INTERLEAVED and out of dispatch order, which is what
		// concurrency produces.
		planEvent(6, func() (sessions.EventType, map[string]any) { return TaskCompletedEvent(TaskResult{ID: "c"}) }),
		planEvent(7, func() (sessions.EventType, map[string]any) { return TaskCompletedEvent(TaskResult{ID: "a"}) }),
	}

	progress, ok := ReducePlanEvents(events)
	if !ok {
		t.Fatal("the reducer found no plan")
	}
	if len(progress.Unfinished) != 2 {
		t.Fatalf("unfinished = %v; BOTH in-flight tasks must be reported", progress.Unfinished)
	}
	unfinished := map[string]bool{}
	for _, id := range progress.Unfinished {
		unfinished[id] = true
	}
	if !unfinished["b"] || !unfinished["d"] {
		t.Fatalf("unfinished = %v, want b and d", progress.Unfinished)
	}

	remaining, err := RemainingPlan(plan, progress, readOnlyLimits())
	if err != nil {
		t.Fatalf("RemainingPlan: %v", err)
	}
	got := map[string]bool{}
	for _, id := range remaining.Order() {
		got[id] = true
	}
	for _, id := range []string{"b", "d", "e"} {
		if !got[id] {
			t.Fatalf("the remainder %v must re-run %q", remaining.Order(), id)
		}
	}
	for _, id := range []string{"a", "c"} {
		if got[id] {
			t.Fatalf("the remainder %v re-runs %q, which already succeeded", remaining.Order(), id)
		}
	}
	// e depended on all four; the two that succeeded are stripped and the two
	// that did not survive as edges, or the remainder would run e before its
	// real predecessors.
	for _, task := range remaining.Tasks() {
		if task.ID != "e" {
			continue
		}
		deps := map[string]bool{}
		for _, dep := range task.DependsOn {
			deps[dep] = true
		}
		if !deps["b"] || !deps["d"] {
			t.Fatalf("e depends on %v; the unfinished predecessors must survive", task.DependsOn)
		}
		if deps["a"] || deps["c"] {
			t.Fatalf("e depends on %v; a satisfied dependency must be dropped", task.DependsOn)
		}
	}
	// And the remainder still runs concurrently — narrowing must not quietly
	// serialise what was a fan-out.
	if remaining.Budget().MaxWorkers != 4 {
		t.Fatalf("the remainder's max_workers = %d, want 4", remaining.Budget().MaxWorkers)
	}
}

// busySurface is a recorder that reports a plan already on screen — the state
// the TUI bridge is in from the moment a background plan is launched until it
// completes.
type busySurface struct {
	name     string
	running  bool
	admitted []string
}

func (s *busySurface) TaskDispatched(Task)             {}
func (s *busySurface) TaskCompleted(TaskResult)        {}
func (s *busySurface) TaskFailed(TaskResult)           {}
func (s *busySurface) PlanAdmitted(plan Plan)          { s.admitted = append(s.admitted, plan.Name()) }
func (s *busySurface) PlanCompleted(Plan, PlanReport)  {}
func (s *busySurface) RunningPlanName() (string, bool) { return s.name, s.running }

// ONE PLAN AT A TIME, on the path the MODEL drives.
//
// The surface holds one plan and the card table is keyed by task id — unique
// within a plan, not between two. The TUI refused a second plan on the path a
// USER drives (/plans restart) and not on this one, and this one is the
// reachable one: a background plan returns immediately by design, so the very
// next tool call lands while it is still running.
func TestASecondPlanIsRefusedWhileOneIsRunning(t *testing.T) {
	gate := &PostureGate{}
	gate.Set(true)
	surface := &busySurface{name: "sweep", running: true}

	tool := &OrchestrateTool{
		PostureActive: gate.Active,
		ParentTools:   []string{"read_file"},
		Recorder:      surface,
		RunTask: NewPlanRunner(PlanTaskContext{
			Executor:       progressExecutor(t),
			Cwd:            t.TempDir(),
			SpecialistName: "explorer",
		}),
	}
	args := map[string]any{
		"name":   "fix",
		"tasks":  []any{map[string]any{"id": "a", "prompt": "one"}},
		"budget": map[string]any{"max_workers": float64(1), "max_tokens": float64(100000)},
	}

	result := tool.RunWithOptions(context.Background(), args, tools.RunOptions{})
	if result.Status != tools.StatusError {
		t.Fatalf("a second plan must be refused while one runs, got %s: %s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "sweep") {
		t.Errorf("the refusal must name the plan that is running: %q", result.Output)
	}
	if !strings.Contains(result.Output, "later turn in this session") {
		t.Fatalf("the busy response must scope background delivery to this session: %q", result.Output)
	}
	if len(surface.admitted) != 0 {
		t.Errorf("a refused plan must leave no record of having started, got %v", surface.admitted)
	}

	// And it runs the moment the surface is free again.
	surface.running = false
	if result := tool.RunWithOptions(context.Background(), args, tools.RunOptions{}); result.Status == tools.StatusError {
		t.Fatalf("the plan must run once the surface is free: %s", result.Output)
	}
	if len(surface.admitted) != 1 {
		t.Errorf("expected exactly one admission, got %v", surface.admitted)
	}
}

// An INVALID plan is still reported as invalid. Blaming a bad plan on the plan
// already running would send the model off to wait for something that was never
// the problem.
func TestABadPlanIsStillCalledBadWhileAnotherRuns(t *testing.T) {
	gate := &PostureGate{}
	gate.Set(true)
	tool := &OrchestrateTool{
		PostureActive: gate.Active,
		ParentTools:   []string{"read_file"},
		Recorder:      &busySurface{name: "sweep", running: true},
		RunTask: NewPlanRunner(PlanTaskContext{
			Executor: progressExecutor(t), Cwd: t.TempDir(), SpecialistName: "explorer",
		}),
	}
	result := tool.RunWithOptions(context.Background(), map[string]any{
		"name":   "fix",
		"tasks":  []any{map[string]any{"id": "a", "prompt": "one", "depends_on": []any{"nope"}}},
		"budget": map[string]any{"max_workers": float64(1), "max_tokens": float64(100000)},
	}, tools.RunOptions{})
	if result.Status != tools.StatusError {
		t.Fatal("a plan with a dangling dependency must be refused")
	}
	if strings.Contains(result.Output, "still running") {
		t.Errorf("the dependency error was replaced by the concurrency refusal: %q", result.Output)
	}
}

// A recorder that cannot answer is treated as FREE. The headless path runs one
// plan per process and has no surface to contend for; gating on a question it
// cannot be asked would refuse every plan `zero exec` ever runs.
func TestARecorderThatCannotAnswerDoesNotBlockThePlan(t *testing.T) {
	gate := &PostureGate{}
	gate.Set(true)
	tool := &OrchestrateTool{
		PostureActive: gate.Active,
		ParentTools:   []string{"read_file"},
		RunTask: NewPlanRunner(PlanTaskContext{
			Executor: progressExecutor(t), Cwd: t.TempDir(), SpecialistName: "explorer",
		}),
	}
	if _, busy := runningPlanOn(nil); busy {
		t.Error("a nil recorder must read as free")
	}
	result := tool.RunWithOptions(context.Background(), map[string]any{
		"name": "p", "tasks": []any{map[string]any{"id": "a", "prompt": "one"}},
		"budget": map[string]any{"max_workers": float64(1), "max_tokens": float64(100000)},
	}, tools.RunOptions{})
	if result.Status == tools.StatusError {
		t.Fatalf("a plan with no surface to contend for must run: %s", result.Output)
	}
}

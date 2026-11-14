package specialist

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/streamjson"
)

// A TASK THE PLAN KILLED MUST NOT BE REPORTED AS A TASK THAT FAILED.
//
// THE MEASURED RUN. Two plan tasks died at 300,017ms and 300,066ms — 49ms apart,
// dispatched together, on a plan with max_tokens 0 and no max_wall_seconds. The
// report said "cancelled: 0, failed: 2", and each carried:
//
//	"Subagent terminated by a signal (signal: killed) — it was killed before it
//	 finished. Common causes: an out-of-memory kill, a timeout, or cancellation;
//	 check the signal to tell which."
//
// A guess list, printed by the component that had made the decision. The reader
// concluded their machine had run out of memory and went to raise sub-agent
// limits that were never involved.
//
// THE MECHANISM. osexec.CommandContext with no WaitDelay SIGKILLs the child when
// its context is cancelled, and Executor.Run returns that as a StatusError
// result with a NIL error. So `concluded := err == nil && !result.Stalled` was
// true for a task the plan had just killed, cutShort was false, and it fell
// through to TaskFailed.

func signalKilledPlan(t *testing.T) Plan {
	t.Helper()
	return mustParsePlan(t, map[string]any{
		"name":   "audit",
		"tasks":  []any{map[string]any{"id": "checker", "prompt": "audit this"}},
		"budget": map[string]any{"max_workers": float64(1)},
	}, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})
}

// killedByItsOwnContext reproduces the shape exactly: the context is cancelled,
// the child comes back signal-terminated, and the error is nil.
func killedByItsOwnContext(t *testing.T) PlanRunner {
	t.Helper()
	return func(ctx context.Context, req PlanTaskRequest) (TaskResult, error) {
		cancelParent, ok := ctx.Value(planCancelKey{}).(context.CancelFunc)
		if ok {
			cancelParent()
			// Let the cancellation land before returning, as a real child's
			// teardown would.
			<-ctx.Done()
		}
		return TaskResult{
			ID:      req.Task.ID,
			Outcome: TaskFailed,
			Signal:  "signal: killed",
			Err: "Subagent terminated by a signal (signal: killed) — it was killed before it finished. " +
				"Common causes: an out-of-memory kill, a timeout, or cancellation; check the signal to tell which.",
		}, nil
	}
}

type planCancelKey struct{}

func TestATaskThePlanKilledIsReportedAsCancelledNotFailed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, planCancelKey{}, context.CancelFunc(cancel))

	report := ExecutePlan(ctx, signalKilledPlan(t), PlanReadOnlyToolNames(), killedByItsOwnContext(t), nil)

	if report.Cancelled != 1 || report.Failed != 0 {
		t.Fatalf("a task the plan killed was reported as cancelled=%d failed=%d, want cancelled=1 failed=0",
			report.Cancelled, report.Failed)
	}
}

// AND THE REASON MUST BE NAMED, not guessed. The guess list is the thing that
// sent a reader to the wrong fix.
func TestACancelledTasksReasonNamesWhatStoppedIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, planCancelKey{}, context.CancelFunc(cancel))

	var got TaskResult
	run := func(taskCtx context.Context, req PlanTaskRequest) (TaskResult, error) {
		result, err := killedByItsOwnContext(t)(taskCtx, req)
		return result, err
	}
	recorder := recordingPlanRecorder(&got)
	ExecutePlan(ctx, signalKilledPlan(t), PlanReadOnlyToolNames(), run, recorder)

	if !strings.Contains(got.Err, "cancelled") {
		t.Fatalf("the reason does not say the task was cancelled: %q", got.Err)
	}
	if strings.Contains(got.Err, "Common causes") || strings.Contains(got.Err, "out-of-memory") {
		t.Fatalf("the guess list survived into a reason the plan actually knows: %q", got.Err)
	}
	// The signal is still reported — as a DETAIL of a stated reason, not in
	// place of one.
	if !strings.Contains(got.Err, "signal: killed") {
		t.Fatalf("the signal was dropped entirely: %q", got.Err)
	}
}

// A WALL-BUDGET STOP SAYS SO. Two stops that feel identical to the child are
// different facts to the reader: one is the plan spending what it was allowed.
func TestAWallExpiredKillNamesTheWallBudget(t *testing.T) {
	plan := mustParsePlan(t, map[string]any{
		"name":   "audit",
		"tasks":  []any{map[string]any{"id": "checker", "prompt": "audit this"}},
		"budget": map[string]any{"max_workers": float64(1), "max_wall_seconds": float64(1)},
	}, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})

	var got TaskResult
	run := func(ctx context.Context, req PlanTaskRequest) (TaskResult, error) {
		// Outlive the wall budget, then come back signal-killed with a nil error.
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
		return TaskResult{
			ID: req.Task.ID, Outcome: TaskFailed, Signal: "signal: killed",
			Err: "Subagent terminated by a signal (signal: killed) — Common causes: an out-of-memory kill…",
		}, nil
	}
	report := ExecutePlan(context.Background(), plan, PlanReadOnlyToolNames(), run, recordingPlanRecorder(&got))

	if report.Cancelled != 1 {
		t.Fatalf("a wall-expired task was reported cancelled=%d failed=%d", report.Cancelled, report.Failed)
	}
	if !strings.Contains(got.Err, "max_wall_seconds") {
		t.Fatalf("the reason does not name the wall budget: %q", got.Err)
	}
}

// A CHILD KILLED BY SOMETHING OUTSIDE THE PLAN IS STILL A FAILURE, and its
// message is still the honest guess list — because there the plan genuinely does
// not know. Widening the cancelled class must not swallow real kills.
func TestAnExternalKillIsStillAFailureWithItsOriginalReason(t *testing.T) {
	original := "Subagent terminated by a signal (signal: killed) — it was killed before it finished. " +
		"Common causes: an out-of-memory kill, a timeout, or cancellation."
	var got TaskResult
	run := func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
		// Signal-killed, but NOTHING cancelled the plan's context.
		return TaskResult{ID: req.Task.ID, Outcome: TaskFailed, Signal: "signal: killed", Err: original}, nil
	}
	report := ExecutePlan(context.Background(), signalKilledPlan(t), PlanReadOnlyToolNames(), run, recordingPlanRecorder(&got))

	if report.Failed != 1 || report.Cancelled != 0 {
		t.Fatalf("an outside kill was reported cancelled=%d failed=%d, want failed=1", report.Cancelled, report.Failed)
	}
	if got.Err != original {
		t.Fatalf("the honest guess list was rewritten for a kill the plan did not make:\n  %q", got.Err)
	}
}

// A task that ran to the end and reported its own failure is untouched — the
// case the original `concluded` guard exists to protect.
func TestATaskThatFailedOnItsOwnIsStillAFailure(t *testing.T) {
	var got TaskResult
	run := func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
		return TaskResult{ID: req.Task.ID, Outcome: TaskFailed, Err: "the build does not compile"}, nil
	}
	report := ExecutePlan(context.Background(), signalKilledPlan(t), PlanReadOnlyToolNames(), run, recordingPlanRecorder(&got))
	if report.Failed != 1 || report.Cancelled != 0 {
		t.Fatalf("a self-reported failure was reclassified: cancelled=%d failed=%d", report.Cancelled, report.Failed)
	}
	if got.Err != "the build does not compile" {
		t.Fatalf("its own reason was overwritten: %q", got.Err)
	}
}

// recordingPlanRecorder captures the terminal result the executor reports, which
// is what a reader actually sees — the report's counts and the task's own reason.
type capturingRecorder struct{ into *TaskResult }

func (r capturingRecorder) TaskDispatched(Task)             {}
func (r capturingRecorder) TaskCompleted(result TaskResult) { *r.into = result }
func (r capturingRecorder) TaskFailed(result TaskResult)    { *r.into = result }

func recordingPlanRecorder(into *TaskResult) PlanRecorder { return capturingRecorder{into: into} }

// THE SIGNAL MUST TRAVEL, not just be honoured once it arrives.
//
// Every test above hands the executor a TaskResult with Signal already set, so
// they prove the classification and nothing about the plumbing that fills it. A
// mutation deleting `Signal: run.Signal` from ExecResult passed all of them
// cleanly — the child's signal never reached TaskResult and no test noticed.
// This drives the real executor seam instead.
func TestTheChildsSignalReachesTheTaskResult(t *testing.T) {
	const childSession = "specialist_00000000000000000000000a"
	executor := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return childSession, nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(_ context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			events := []streamjson.Event{{Type: streamjson.EventRunStart, SessionID: childSession}}
			for _, event := range events {
				if progress != nil {
					progress(event)
				}
			}
			// A child SIGKILLed by its own context: exit -1, a signal, nil error.
			return ChildRunResult{Started: true, ExitCode: -1, Signal: "signal: killed", Events: events}, nil
		},
	}
	run := NewPlanRunner(PlanTaskContext{Executor: executor, Cwd: t.TempDir(), SpecialistName: "explorer"})

	result, _ := run(context.Background(), PlanTaskRequest{
		Task:  Task{ID: "checker", Prompt: "audit"},
		Tools: []string{"grep"},
	})
	if result.Signal == "" {
		t.Fatal("the child's signal never reached TaskResult: the executor cannot tell a kill it made from one it did not")
	}
	if !strings.Contains(result.Signal, "killed") {
		t.Fatalf("Signal = %q, want the child's own signal description", result.Signal)
	}
}

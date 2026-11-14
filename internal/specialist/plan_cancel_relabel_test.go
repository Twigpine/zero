package specialist

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pausingRecorder parks the scheduler at a chosen task boundary so a test can
// decide exactly when a completed task is harvested.
type pausingRecorder struct {
	gate      chan struct{}
	once      sync.Once
	boundary  int32
	boundries atomic.Int32
}

func (r *pausingRecorder) TaskDispatched(Task)            {}
func (r *pausingRecorder) TaskCompleted(TaskResult)       {}
func (r *pausingRecorder) TaskFailed(TaskResult)          {}
func (r *pausingRecorder) PlanRunning(context.CancelFunc) {}

func (r *pausingRecorder) WaitWhilePaused(ctx context.Context) {
	// The first boundary must pass through or nothing is ever dispatched.
	if r.boundries.Add(1) < r.boundary {
		return
	}
	r.once.Do(func() {
		select {
		case <-r.gate:
		case <-ctx.Done():
		}
	})
}

// A TASK THAT FAILED ON ITS OWN MERITS STAYS FAILED, even when the plan is
// stopped before its result is harvested.
//
// The harvest relabels a cut-short task as cancelled so that stopping a
// twenty-task plan does not read as twenty defects. That check was `ctx.Err() !=
// nil` alone, which is true of EVERY task harvested after the stop — including
// one that had already finished failing for a reason of its own. A genuine
// compile error became "cancelled: the run was stopped while this task was
// running", the plan reported as stopped rather than broken, and the single
// result worth reading was overwritten with a sentence about the user.
func TestARealFailureSurvivesACancelThatArrivesAfterIt(t *testing.T) {
	plan := mustPlan(t, []any{
		task("boom", "fails for real"),
		task("later", "never runs"),
	}, map[string]any{"max_workers": float64(2)}, readOnlyLimits())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Park at the SECOND boundary: after "boom" has been dispatched, before its
	// completion is harvested.
	recorder := &pausingRecorder{gate: make(chan struct{}), boundary: 2}

	failed := make(chan struct{})
	done := make(chan PlanReport, 1)
	go func() {
		done <- ExecutePlan(ctx, plan, []string{"read_file"},
			func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
				if req.Task.ID == "boom" {
					close(failed)
					// A REAL failure, complete and self-reported: err is nil,
					// exactly as the runner returns when a child ran to the end
					// and its task did not succeed.
					return TaskResult{Outcome: TaskFailed, Err: "compile error in main.go"}, nil
				}
				return TaskResult{Outcome: TaskSucceeded}, nil
			}, recorder)
	}()

	<-failed
	// Let the completion land in the scheduler's buffer before the stop, so it is
	// harvested with ctx already cancelled — the exact ordering that mislabelled it.
	time.Sleep(50 * time.Millisecond)
	cancel()
	close(recorder.gate)

	report := <-done
	var boom TaskResult
	for _, result := range report.Tasks {
		if result.ID == "boom" {
			boom = result
		}
	}
	if boom.ID == "" {
		t.Fatalf("the failing task is missing from the report: %+v", report.Tasks)
	}
	if boom.Outcome != TaskFailed {
		t.Errorf("outcome = %q, want %q: a task that failed on its own merits was relabelled because the plan was stopped afterwards",
			boom.Outcome, TaskFailed)
	}
	if !strings.Contains(boom.Err, "compile error in main.go") {
		t.Errorf("the failure's own reason was overwritten: %q", boom.Err)
	}
	if report.Failed == 0 {
		t.Errorf("report.Failed = 0 with a genuinely broken task: the plan reads as merely stopped")
	}
}

// ...and the property that check exists for still holds: a task the cancellation
// actually cut short is CANCELLED, not a defect. Stopping a plan must never read
// as a plan full of failures.
func TestATaskTheCancelActuallyCutShortIsStillCancelled(t *testing.T) {
	plan := mustPlan(t, []any{task("wedged", "runs until stopped")}, okBudget(), readOnlyLimits())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	report := ExecutePlan(ctx, plan, []string{"read_file"},
		func(taskCtx context.Context, _ PlanTaskRequest) (TaskResult, error) {
			cancel()
			<-taskCtx.Done()
			// What the runner returns for a task the context killed: the
			// process-level error travels back with the result.
			return TaskResult{Outcome: TaskFailed, Err: taskCtx.Err().Error()}, taskCtx.Err()
		}, nil)

	if len(report.Tasks) != 1 {
		t.Fatalf("expected one task in the report, got %d", len(report.Tasks))
	}
	if got := report.Tasks[0].Outcome; got != TaskCancelled {
		t.Fatalf("outcome = %q, want %q: a task the stop actually killed must not read as a defect", got, TaskCancelled)
	}
	if report.Failed != 0 {
		t.Errorf("report.Failed = %d for a plan that was merely stopped", report.Failed)
	}
}

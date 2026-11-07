package specialist

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sessions"
)

// planEvent builds a session event the way the recorders write one, so the
// reducer is tested against the shape actually stored rather than a convenient
// one. Sequence is explicit: the reduction depends on order.
func planEvent(seq int, build func() (sessions.EventType, map[string]any)) sessions.Event {
	eventType, payload := build()
	body, _ := json.Marshal(payload)
	return sessions.Event{Sequence: seq, Type: eventType, Payload: body}
}

func resumeFixturePlan(t *testing.T) Plan {
	t.Helper()
	// a → (b, c) → d. A diamond, so narrowing has edges to rewrite.
	return mustPlan(t, []any{
		task("a", "root"),
		task("b", "left", "a"),
		task("c", "right", "a"),
		task("d", "join", "b", "c"),
	}, okBudget(), readOnlyLimits())
}

// A DISPATCH WITH NO TERMINAL EVENT IS UNFINISHED, never done. It is the task
// most likely to have been interrupted mid-work, and treating it as complete
// would silently drop exactly that one.
func TestADispatchedTaskWithNoTerminalEventIsUnfinished(t *testing.T) {
	plan := resumeFixturePlan(t)
	events := []sessions.Event{
		planEvent(1, func() (sessions.EventType, map[string]any) { return PlanAdmittedEvent(plan) }),
		planEvent(2, func() (sessions.EventType, map[string]any) { return TaskDispatchedEvent(Task{ID: "a"}) }),
		planEvent(3, func() (sessions.EventType, map[string]any) {
			return TaskCompletedEvent(TaskResult{ID: "a", Attempts: 1})
		}),
		// b was dispatched and the process went away.
		planEvent(4, func() (sessions.EventType, map[string]any) { return TaskDispatchedEvent(Task{ID: "b"}) }),
	}

	progress, ok := ReducePlanEvents(events)
	if !ok {
		t.Fatal("the reducer found no plan")
	}
	if !reflect.DeepEqual(progress.Succeeded, []string{"a"}) {
		t.Fatalf("succeeded = %v, want [a]", progress.Succeeded)
	}
	if !reflect.DeepEqual(progress.Unfinished, []string{"b"}) {
		t.Fatalf("unfinished = %v, want [b] — a dispatch with no terminal event", progress.Unfinished)
	}
	if progress.Complete {
		t.Fatal("a plan with no plan_completed must not report itself complete")
	}
}

// A FAILED task is remaining too. Unlike a success it produced no work to keep,
// so resuming should attempt it again.
func TestAFailedTaskIsRemaining(t *testing.T) {
	plan := resumeFixturePlan(t)
	events := []sessions.Event{
		planEvent(1, func() (sessions.EventType, map[string]any) { return PlanAdmittedEvent(plan) }),
		planEvent(2, func() (sessions.EventType, map[string]any) { return TaskDispatchedEvent(Task{ID: "a"}) }),
		planEvent(3, func() (sessions.EventType, map[string]any) {
			return TaskFailedEvent(TaskResult{ID: "a", Outcome: TaskFailed})
		}),
	}
	progress, _ := ReducePlanEvents(events)
	if !reflect.DeepEqual(progress.Failed, []string{"a"}) {
		t.Fatalf("failed = %v, want [a]", progress.Failed)
	}
	// The resume narrowing re-includes a failed task — asserted through the real
	// gate (RemainingPlan), not a query helper the resume path never calls.
	remaining, err := RemainingPlan(plan, progress, readOnlyLimits())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, task := range remaining.Tasks() {
		if task.ID == "a" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a failed task must be re-run on resume")
	}
}

// A SECOND PLAN RESETS THE STATE. A session can run several plans, and resume
// means the last one — merging would mark this plan's tasks done because an
// earlier plan happened to use the same ids.
func TestASecondPlanDoesNotInheritTheFirstPlansProgress(t *testing.T) {
	first := mustPlan(t, []any{task("a", "x"), task("b", "y")}, okBudget(), readOnlyLimits())
	second := mustPlan(t, []any{task("a", "different"), task("z", "new")}, okBudget(), readOnlyLimits())
	events := []sessions.Event{
		planEvent(1, func() (sessions.EventType, map[string]any) { return PlanAdmittedEvent(first) }),
		planEvent(2, func() (sessions.EventType, map[string]any) { return TaskDispatchedEvent(Task{ID: "a"}) }),
		planEvent(3, func() (sessions.EventType, map[string]any) { return TaskCompletedEvent(TaskResult{ID: "a"}) }),
		planEvent(4, func() (sessions.EventType, map[string]any) { return TaskDispatchedEvent(Task{ID: "b"}) }),
		planEvent(5, func() (sessions.EventType, map[string]any) { return TaskCompletedEvent(TaskResult{ID: "b"}) }),
		planEvent(6, func() (sessions.EventType, map[string]any) {
			return PlanCompletedEvent(first, PlanReport{Status: PlanCompleted, Succeeded: 2})
		}),
		// A second plan starts and is interrupted immediately.
		planEvent(7, func() (sessions.EventType, map[string]any) { return PlanAdmittedEvent(second) }),
		planEvent(8, func() (sessions.EventType, map[string]any) { return TaskDispatchedEvent(Task{ID: "a"}) }),
	}

	progress, _ := ReducePlanEvents(events)
	if len(progress.Succeeded) != 0 {
		t.Fatalf("succeeded = %v; the second plan inherited the first plan's completions", progress.Succeeded)
	}
	if progress.Complete {
		t.Fatal("the first plan's completion was carried into the second")
	}
	if !reflect.DeepEqual(progress.Order, []string{"a", "z"}) {
		t.Fatalf("order = %v; it must be the SECOND plan's", progress.Order)
	}
}

// The reduction is over SEQUENCE, not file order: a log read out of order must
// still produce the same state.
func TestTheReductionIsDeterministicRegardlessOfInputOrder(t *testing.T) {
	plan := resumeFixturePlan(t)
	ordered := []sessions.Event{
		planEvent(1, func() (sessions.EventType, map[string]any) { return PlanAdmittedEvent(plan) }),
		planEvent(2, func() (sessions.EventType, map[string]any) { return TaskDispatchedEvent(Task{ID: "a"}) }),
		planEvent(3, func() (sessions.EventType, map[string]any) { return TaskCompletedEvent(TaskResult{ID: "a"}) }),
	}
	shuffled := []sessions.Event{ordered[2], ordered[0], ordered[1]}

	want, _ := ReducePlanEvents(ordered)
	got, _ := ReducePlanEvents(shuffled)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("the reduction depends on input order:\n%+v\nvs\n%+v", want, got)
	}
}

// A session with no plan events reports so, rather than an empty plan that a
// caller might narrow to nothing.
func TestASessionWithNoPlanReportsNoPlan(t *testing.T) {
	if _, ok := ReducePlanEvents(nil); ok {
		t.Fatal("an empty log produced a plan")
	}
	if _, ok := ReducePlanEvents([]sessions.Event{{Sequence: 1, Type: sessions.EventType("user_message")}}); ok {
		t.Fatal("a log with no plan events produced a plan")
	}
}

// NARROWING REMOVES THE COMPLETED TASK AND EVERY REFERENCE TO IT. Leaving the
// edge behind would produce a plan whose dependency names nothing — which
// ParsePlan refuses, correctly — so a resume would fail on its own output.
func TestNarrowingDropsCompletedTasksAndTheirEdges(t *testing.T) {
	plan := resumeFixturePlan(t)
	progress := PlanProgress{Order: plan.Order(), Succeeded: []string{"a", "b"}}

	remaining, err := RemainingPlan(plan, progress, readOnlyLimits())
	if err != nil {
		t.Fatalf("RemainingPlan: %v", err)
	}
	ids := remaining.Order()
	if !reflect.DeepEqual(ids, []string{"c", "d"}) {
		t.Fatalf("remaining order = %v, want [c d]", ids)
	}
	byID := map[string]Task{}
	for _, task := range remaining.Tasks() {
		byID[task.ID] = task
	}
	// c depended on a, which is done: the edge is gone, not rewritten.
	if len(byID["c"].DependsOn) != 0 {
		t.Fatalf("c still depends on %v; a satisfied dependency must be dropped", byID["c"].DependsOn)
	}
	// d depended on b (done) and c (not): only the live edge survives.
	if !reflect.DeepEqual(byID["d"].DependsOn, []string{"c"}) {
		t.Fatalf("d depends on %v, want [c]", byID["d"].DependsOn)
	}
}

// The narrowed plan goes back through ParsePlan, so it cannot acquire anything
// the original did not have — and the current run's limits still apply.
func TestANarrowedPlanIsRevalidated(t *testing.T) {
	plan := resumeFixturePlan(t)
	progress := PlanProgress{Order: plan.Order(), Succeeded: []string{"a"}}

	// A task that NAMES a tool the run does not hold is refused in the
	// remainder exactly as it would be in the original. Named explicitly
	// because admission only checks what a task asks for — an unqualified task
	// inherits at dispatch, where planToolGrant does the refusing.
	explicit := mustPlan(t, []any{
		map[string]any{"id": "a", "prompt": "x"},
		map[string]any{"id": "b", "prompt": "y", "depends_on": []any{"a"}, "tools": []any{"grep"}},
	}, okBudget(), readOnlyLimits())
	if _, err := RemainingPlan(explicit, PlanProgress{Order: explicit.Order(), Succeeded: []string{"a"}},
		Limits{MaxTasks: 20, ParentTools: []string{"read_file"}}); err == nil {
		t.Fatal("a narrowed plan kept a tool grant this run does not hold")
	}
	// ...nor one whose tier no longer fits it.
	if _, err := RemainingPlan(plan, progress, Limits{MaxTasks: 2, ParentTools: []string{"read_file"}}); err == nil {
		t.Fatal("a narrowed plan bypassed the task ceiling")
	}
}

// Nothing left is not an error state to hide: it is the ordinary end of a plan,
// reported as such.
func TestNarrowingACompletedPlanSaysThereIsNothingLeft(t *testing.T) {
	plan := resumeFixturePlan(t)
	progress := PlanProgress{Order: plan.Order(), Succeeded: plan.Order()}
	_, err := RemainingPlan(plan, progress, readOnlyLimits())
	if err == nil || !strings.Contains(err.Error(), "nothing left to run") {
		t.Fatalf("err = %v; it must say the plan is finished", err)
	}
}

// A malformed plan_admitted must not let the PREVIOUS plan's state be reported
// as this one's. Malformed persisted data is an error, never a silent skip.
func TestAMalformedAdmissionAbandonsTheReduction(t *testing.T) {
	plan := resumeFixturePlan(t)
	events := []sessions.Event{
		planEvent(1, func() (sessions.EventType, map[string]any) { return PlanAdmittedEvent(plan) }),
		planEvent(2, func() (sessions.EventType, map[string]any) { return TaskCompletedEvent(TaskResult{ID: "a"}) }),
		{Sequence: 3, Type: sessions.EventPlanAdmitted, Payload: json.RawMessage(`{"order":`)},
	}
	if _, ok := ReducePlanEvents(events); ok {
		t.Fatal("a malformed admission was skipped and the previous plan's state survived")
	}
}

package specialist

import (
	"context"
	"encoding/json"
	"testing"
)

// identityCaptureRecorder turns the executor's completion callback into the SAME
// event payload both real surfaces write (TaskCompletedEvent), so a test can
// assert what actually lands in the log — not just that a helper works.
type identityCaptureRecorder struct {
	completed map[string]map[string]any
}

func (r *identityCaptureRecorder) TaskDispatched(Task) {}
func (r *identityCaptureRecorder) TaskCompleted(result TaskResult) {
	if r.completed == nil {
		r.completed = map[string]map[string]any{}
	}
	_, payload := TaskCompletedEvent(result)
	r.completed[result.ID] = payload
}
func (r *identityCaptureRecorder) TaskFailed(TaskResult) {}

// A completed task records its identity, and it reaches the task_completed event
// — end to end through the real executor, because a stamp that never reaches the
// event is precisely the defect this pins.
func TestACompletedTaskRecordsItsIdentity(t *testing.T) {
	plan := mustParsePlan(t, map[string]any{
		"name":   "p",
		"tasks":  []any{map[string]any{"id": "find", "prompt": "look", "model": "m1"}},
		"budget": map[string]any{"max_workers": float64(1)},
	}, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})

	run := func(_ context.Context, req PlanTaskRequest) (TaskResult, error) {
		return TaskResult{ID: req.Task.ID, Outcome: TaskSucceeded, Output: "done"}, nil
	}
	rec := &identityCaptureRecorder{}
	report := ExecutePlan(context.Background(), plan, PlanReadOnlyToolNames(), run, rec)
	if report.Succeeded != 1 || report.Failed != 0 {
		t.Fatalf("plan did not succeed cleanly: %+v", report)
	}

	payload := rec.completed["find"]
	if payload == nil {
		t.Fatal("no task_completed event captured for find")
	}
	got, _ := payload["identity"].(string)
	if got == "" {
		t.Fatal("the completed task recorded no identity — a resume cannot detect an edit")
	}
	if want := taskIdentity(plan.Tasks()[0]); got != want {
		t.Fatalf("recorded identity %q != the task's identity %q", got, want)
	}
}

// The identity survives the JSON the store persists — it is not lost to the key
// name or serialization.
func TestTheRecordedIdentitySurvivesJSON(t *testing.T) {
	_, payload := TaskCompletedEvent(TaskResult{ID: "a", Outcome: TaskSucceeded, Identity: "abc123"})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Identity string `json:"identity"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Identity != "abc123" {
		t.Fatalf("identity did not survive JSON: %q", back.Identity)
	}
}

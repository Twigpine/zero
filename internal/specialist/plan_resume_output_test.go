package specialist

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sessions"
)

func completedEvent(t *testing.T, seq int, id, output string) sessions.Event {
	t.Helper()
	typ, payload := TaskCompletedEvent(TaskResult{ID: id, Outcome: TaskSucceeded, Output: output})
	raw, _ := json.Marshal(payload)
	return sessions.Event{Type: typ, Sequence: seq, Payload: raw}
}

func admittedEvent(t *testing.T, seq int, name string, order []string) sessions.Event {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"name": name, "order": order})
	return sessions.Event{Type: sessions.EventPlanAdmitted, Sequence: seq, Payload: raw}
}

// A RESUMED DEPENDENT IS BRIEFED ON WHAT ITS COMPLETED DEPENDENCY FOUND.
//
// Before: TaskCompletedEvent stored no output and RemainingPlan stripped the
// completed dependency, so a plan cut short mid-run lost the finding the
// remaining task was meant to build on. Now the bounded output rides the event,
// the reducer captures it, and RemainingPlan folds it into the dependent's
// prompt.
func TestAResumedDependentSeesItsCompletedDependencysFinding(t *testing.T) {
	const finding = "The retry watchdog resets on every event (plan_watchdog.go:66)."
	events := []sessions.Event{
		admittedEvent(t, 1, "audit", []string{"find", "synth"}),
		completedEvent(t, 2, "find", finding),
	}
	progress, ok := ReducePlanEvents(events)
	if !ok {
		t.Fatal("reduce found no plan")
	}
	if progress.Outputs["find"] != finding {
		t.Fatalf("the reducer lost the output: %q", progress.Outputs["find"])
	}

	plan := mustParsePlan(t, map[string]any{
		"name": "audit",
		"tasks": []any{
			map[string]any{"id": "find", "prompt": "find it"},
			map[string]any{"id": "synth", "prompt": "combine the findings", "depends_on": []any{"find"}},
		},
		"budget": map[string]any{"max_workers": float64(1)},
	}, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})

	remaining, err := RemainingPlan(plan, progress, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})
	if err != nil {
		t.Fatalf("RemainingPlan: %v", err)
	}
	if remaining.TaskCount() != 1 {
		t.Fatalf("expected only synth to remain, got %d tasks", remaining.TaskCount())
	}
	synth := remaining.Tasks()[0]
	if synth.ID != "synth" {
		t.Fatalf("the wrong task remained: %s", synth.ID)
	}
	if !strings.Contains(synth.Prompt, finding) {
		t.Fatalf("the resumed task was not briefed on its dependency's finding:\n%s", synth.Prompt)
	}
	if !strings.Contains(synth.Prompt, "combine the findings") {
		t.Fatal("the task's own prompt was lost")
	}
}

// The stored output is BOUNDED, so a large plan's log does not balloon.
func TestTheStoredResumeOutputIsCapped(t *testing.T) {
	huge := strings.Repeat("F", resumeOutputCap*3)
	_, payload := TaskCompletedEvent(TaskResult{ID: "t", Outcome: TaskSucceeded, Output: huge})
	stored, _ := payload["output"].(string)
	if len(stored) != resumeOutputCap {
		t.Fatalf("stored %d chars, want the cap %d", len(stored), resumeOutputCap)
	}
	// A task that produced nothing stores nothing — no empty key.
	_, empty := TaskCompletedEvent(TaskResult{ID: "t", Outcome: TaskSucceeded, Output: "   "})
	if _, present := empty["output"]; present {
		t.Fatal("an empty output was stored")
	}
}

// A COMPLETED DEPENDENCY WITH NO STORED OUTPUT still gets a heading, never a
// silent gap that reads as "it found nothing".
func TestAResumedDependencyWithoutOutputIsStillNamed(t *testing.T) {
	// An older event: completed, but no output field.
	old := sessions.Event{Type: sessions.EventTaskCompleted, Sequence: 2}
	rawOld, _ := json.Marshal(map[string]any{"id": "find"})
	old.Payload = rawOld

	progress, _ := ReducePlanEvents([]sessions.Event{
		admittedEvent(t, 1, "audit", []string{"find", "synth"}),
		old,
	})
	plan := mustParsePlan(t, map[string]any{
		"name": "audit",
		"tasks": []any{
			map[string]any{"id": "find", "prompt": "find"},
			map[string]any{"id": "synth", "prompt": "combine", "depends_on": []any{"find"}},
		},
		"budget": map[string]any{"max_workers": float64(1)},
	}, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})
	remaining, _ := RemainingPlan(plan, progress, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})
	synth := remaining.Tasks()[0]
	if !strings.Contains(synth.Prompt, "find") || !strings.Contains(synth.Prompt, "not recorded") {
		t.Fatalf("a completed-but-unrecorded dependency was not named:\n%s", synth.Prompt)
	}
}

// A plan with no completed dependencies is unchanged — the brief only appears
// when there is completed work to carry.
func TestAResumeWithNoCompletedDepsAddsNoBrief(t *testing.T) {
	progress, _ := ReducePlanEvents([]sessions.Event{
		admittedEvent(t, 1, "p", []string{"a", "b"}),
	})
	plan := mustParsePlan(t, map[string]any{
		"name": "p",
		"tasks": []any{
			map[string]any{"id": "a", "prompt": "alpha"},
			map[string]any{"id": "b", "prompt": "beta", "depends_on": []any{"a"}},
		},
		"budget": map[string]any{"max_workers": float64(1)},
	}, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})
	remaining, _ := RemainingPlan(plan, progress, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})
	for _, task := range remaining.Tasks() {
		if strings.Contains(task.Prompt, "earlier run") {
			t.Fatalf("a brief was added with no completed deps:\n%s", task.Prompt)
		}
	}
}

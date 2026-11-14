package specialist

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sessions"
)

// chainPlan builds a → b → c with the given per-task prompts, so a test can
// "edit" a task by rebuilding the chain with one prompt changed.
func chainPlan(t *testing.T, prompts map[string]string) Plan {
	t.Helper()
	mk := func(id, dep string) map[string]any {
		entry := map[string]any{"id": id, "prompt": prompts[id]}
		if dep != "" {
			entry["depends_on"] = []any{dep}
		}
		return entry
	}
	return mustParsePlan(t, map[string]any{
		"name":   "chain",
		"tasks":  []any{mk("a", ""), mk("b", "a"), mk("c", "b")},
		"budget": map[string]any{"max_workers": float64(1)},
	}, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})
}

// succeededProgress records every task of a plan as completed, with the identity
// and output it would have written when it ran — the state a resume reduces to
// after a full run.
func succeededProgress(plan Plan) PlanProgress {
	progress := PlanProgress{Name: plan.Name(), Order: plan.Order(),
		Identities: map[string]string{}, Outputs: map[string]string{}}
	for _, task := range plan.Tasks() {
		progress.Succeeded = append(progress.Succeeded, task.ID)
		progress.Identities[task.ID] = taskIdentity(task)
		progress.Outputs[task.ID] = "output of " + task.ID
	}
	return progress
}

func taskByID(plan Plan) map[string]Task {
	byID := map[string]Task{}
	for _, task := range plan.Tasks() {
		byID[task.ID] = task
	}
	return byID
}

func chainLimits() Limits { return Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()} }

// C4, positive: an UNEDITED fully-succeeded plan has nothing to resume. This is
// what proves identity MATCHING works — a broken match would fail to recognise
// the unchanged tasks and stage the whole plan.
func TestResumeWithNoEditsHasNothingLeft(t *testing.T) {
	plan := chainPlan(t, map[string]string{"a": "A", "b": "B", "c": "C"})
	if _, err := RemainingPlan(plan, succeededProgress(plan), chainLimits()); err == nil {
		t.Fatal("an unedited, fully-succeeded plan staged a remainder — unchanged tasks are not recognised as done")
	}
}

// C4: editing the LEAF re-runs only the leaf; its done dependencies are dropped
// and folded in as briefings.
func TestResumeReRunsOnlyAnEditedLeaf(t *testing.T) {
	orig := chainPlan(t, map[string]string{"a": "A", "b": "B", "c": "C"})
	progress := succeededProgress(orig)

	edited := chainPlan(t, map[string]string{"a": "A", "b": "B", "c": "C EDITED"})
	remaining, err := RemainingPlan(edited, progress, chainLimits())
	if err != nil {
		t.Fatal(err)
	}
	if remaining.TaskCount() != 1 || remaining.Order()[0] != "c" {
		t.Fatalf("editing only the leaf re-ran %v, want just c", remaining.Order())
	}
	c := remaining.Tasks()[0]
	if len(c.DependsOn) != 0 {
		t.Fatalf("c still names its done dependency b: %v", c.DependsOn)
	}
	if !strings.Contains(c.Prompt, "output of b") {
		t.Fatalf("the re-running leaf was not briefed on its done dependency:\n%s", c.Prompt)
	}
}

// C5: ResumeChangedTasks names exactly the edited task and everything downstream
// of it — the recovery diagnostic a resume notice reads. Editing the MIDDLE task
// reports b and c, never the untouched root a.
func TestResumeChangedTasksNamesEditedAndCascaded(t *testing.T) {
	orig := chainPlan(t, map[string]string{"a": "A", "b": "B", "c": "C"})
	progress := succeededProgress(orig)

	edited := chainPlan(t, map[string]string{"a": "A", "b": "B EDITED", "c": "C"})
	changed := ResumeChangedTasks(edited, progress)
	if len(changed) != 2 || changed[0] != "b" || changed[1] != "c" {
		t.Fatalf("changed = %v, want [b c] — the edited task and its dependent, in order", changed)
	}
	// An unedited resume reports nothing changed.
	if got := ResumeChangedTasks(orig, progress); len(got) != 0 {
		t.Fatalf("an unedited resume reported %v as changed", got)
	}
}

// C4: editing the ROOT cascades — the whole chain re-runs, edges intact, and no
// re-running dependency is briefed as if it were done.
func TestResumeReRunsAnEditedRootAndEverythingDownstream(t *testing.T) {
	orig := chainPlan(t, map[string]string{"a": "A", "b": "B", "c": "C"})
	progress := succeededProgress(orig)

	edited := chainPlan(t, map[string]string{"a": "A EDITED", "b": "B", "c": "C"})
	remaining, err := RemainingPlan(edited, progress, chainLimits())
	if err != nil {
		t.Fatal(err)
	}
	if remaining.TaskCount() != 3 {
		t.Fatalf("editing the root re-ran %d task(s), want the whole chain of 3: %v", remaining.TaskCount(), remaining.Order())
	}
	byID := taskByID(remaining)
	if got := byID["b"].DependsOn; len(got) != 1 || got[0] != "a" {
		t.Fatalf("b lost its edge to the re-running root a: %v", got)
	}
	if got := byID["c"].DependsOn; len(got) != 1 || got[0] != "b" {
		t.Fatalf("c lost its edge to b: %v", got)
	}
	if strings.Contains(byID["b"].Prompt, "output of a") {
		t.Fatal("b was briefed on a's stale output even though a re-runs — the fresh result must come from the edge, not a briefing")
	}
}

// completedWithIdentity is completedEvent plus the recorded fingerprint, so a
// reduction test can drive the real event both surfaces write.
func completedWithIdentity(t *testing.T, seq int, id, output, identity string) sessions.Event {
	t.Helper()
	typ, payload := TaskCompletedEvent(TaskResult{ID: id, Outcome: TaskSucceeded, Output: output, Identity: identity})
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return sessions.Event{Type: typ, Sequence: seq, Payload: raw}
}

// C3: reduction captures each succeeded task's recorded identity, and a
// pre-identity event leaves NO entry (absent, not empty) so RemainingPlan can
// tell "unknown identity" from a real one.
func TestReduceCapturesEachTasksRecordedIdentity(t *testing.T) {
	progress, ok := ReducePlanEvents([]sessions.Event{
		admittedEvent(t, 1, "p", []string{"find", "old"}),
		completedWithIdentity(t, 2, "find", "out", "id-find"),
		completedEvent(t, 3, "old", "out"), // recorded before identity existed
	})
	if !ok {
		t.Fatal("no plan reduced")
	}
	if progress.Identities["find"] != "id-find" {
		t.Fatalf("identity not captured for find: %q", progress.Identities["find"])
	}
	if _, present := progress.Identities["old"]; present {
		t.Fatal("a pre-identity completion recorded an identity — it must be absent, so resume matches it by id alone")
	}
}

// fanInPlan builds four independent finders and a synth that depends on them in
// the given order.
func fanInPlan(t *testing.T, order []string) Plan {
	t.Helper()
	tasks := []any{}
	for _, id := range []string{"a", "b", "c", "d"} {
		tasks = append(tasks, map[string]any{"id": id, "prompt": "find " + id})
	}
	deps := make([]any, 0, len(order))
	for _, id := range order {
		deps = append(deps, id)
	}
	tasks = append(tasks, map[string]any{"id": "synth", "prompt": "combine", "depends_on": deps})
	return mustParsePlan(t, map[string]any{
		"name":   "fan-in",
		"tasks":  tasks,
		"budget": map[string]any{"max_workers": float64(1)},
	}, chainLimits())
}

// REORDERED DEPENDENCIES ARE A DIFFERENT TASK. The briefing walks depends_on in
// declared order and spends one shared budget, so with long dependency outputs
// the order decides which outputs the dependent actually receives. The identity
// once sorted depends_on, so a reordered synth matched its old fingerprint and
// resume replayed a result built from a different briefing.
func TestReorderedDependenciesAreADifferentTask(t *testing.T) {
	original := fanInPlan(t, []string{"a", "b", "c", "d"})
	reordered := fanInPlan(t, []string{"d", "c", "b", "a"})

	// Setup: under a budget that fits two outputs, the two orders really do
	// brief the synth differently — otherwise this proves nothing.
	results := map[string]TaskResult{}
	for _, id := range []string{"a", "b", "c", "d"} {
		results[id] = TaskResult{ID: id, Outcome: TaskSucceeded, Output: strings.Repeat(id, 100)}
	}
	synthOf := func(plan Plan) Task { return taskByID(plan)["synth"] }
	before := withDependencyBriefingBudget(synthOf(original), results, 100, 200)
	after := withDependencyBriefingBudget(synthOf(reordered), results, 100, 200)
	if before == after {
		t.Fatal("SETUP INVALID: dependency order did not change the briefing under this budget")
	}
	if !strings.Contains(before, `"a"`) || strings.Contains(before, `"d"`) {
		t.Fatalf("SETUP INVALID: expected the declared-first outputs to win the budget:\n%s", before)
	}

	progress := succeededProgress(original)
	if !planDoneSet(original, progress)["synth"] {
		t.Fatal("SETUP INVALID: the unedited synth is not recognised as done")
	}
	if planDoneSet(reordered, progress)["synth"] {
		t.Fatal("resume reused synth's result although its dependency order — and so its briefing — changed")
	}
	for _, id := range []string{"a", "b", "c", "d"} {
		if !planDoneSet(reordered, progress)[id] {
			t.Fatalf("finder %s was not edited and must stay done", id)
		}
	}
}

package specialist

import (
	"strings"
	"testing"
)

// A MALFORMED EDGE MUST BE REFUSED, NOT DROPPED.
//
// planStrings skips an entry it cannot decode, which is right for a display
// label and wrong for a dependency: "depends_on": [42] decoded to no dependency
// at all, so the task was admitted as dependency-free and ran BEFORE the
// precondition it declared. Silently — nothing failed, nothing warned, and the
// only symptom is work happening in the wrong order. Reproduced before this fix.
func TestAMalformedDependencyIsRefusedRatherThanSilentlyDropped(t *testing.T) {
	for name, entry := range map[string]any{
		"a number":  float64(42),
		"an object": map[string]any{"id": "a"},
		"a list":    []any{"a"},
		"empty":     "",
		"blank":     "   ",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParsePlan(planArgs([]any{
				task("a", "x"),
				map[string]any{"id": "b", "prompt": "y", "depends_on": []any{entry}},
			}, okBudget()), readOnlyLimits())
			if err == nil {
				t.Fatal("a task whose dependency could not be read was admitted as dependency-free")
			}
			if !strings.Contains(err.Error(), "depends_on") {
				t.Errorf("the refusal does not name the field: %v", err)
			}
			// WHICH entry, not just that one was wrong — the author has to be able
			// to find it without diffing what they sent against what ran.
			if !strings.Contains(err.Error(), "[0]") {
				t.Errorf("the refusal does not name the offending index: %v", err)
			}
			if !strings.Contains(err.Error(), "task at position 1") {
				t.Errorf("the refusal does not name the task: %v", err)
			}
		})
	}
}

// The same for tools, where a dropped entry quietly NARROWS a grant the caller
// believed they asked for — the mirror image of widening, and just as invisible.
func TestAMalformedToolEntryIsRefused(t *testing.T) {
	_, err := ParsePlan(planArgs([]any{
		map[string]any{"id": "a", "prompt": "x", "tools": []any{"grep", float64(7)}},
	}, okBudget()), readOnlyLimits())
	if err == nil {
		t.Fatal("a task with an unreadable tool entry was admitted")
	}
	if !strings.Contains(err.Error(), "tools") || !strings.Contains(err.Error(), "[1]") {
		t.Errorf("the refusal does not locate the entry: %v", err)
	}
}

// WELL-FORMED LISTS ARE UNCHANGED, including the ordinary absent and empty
// cases, so nothing about a normal plan moves.
func TestWellFormedDependencyAndToolListsStillParse(t *testing.T) {
	plan, err := ParsePlan(planArgs([]any{
		map[string]any{"id": "a", "prompt": "x", "tools": []any{"read_file", " grep "}},
		map[string]any{"id": "b", "prompt": "y", "depends_on": []any{"a"}},
		map[string]any{"id": "c", "prompt": "z"},
	}, okBudget()), readOnlyLimits())
	if err != nil {
		t.Fatalf("a well-formed plan was refused: %v", err)
	}
	byID := map[string]Task{}
	for _, task := range plan.Tasks() {
		byID[task.ID] = task
	}
	if got := byID["a"].Tools; len(got) != 2 || got[1] != "grep" {
		t.Errorf("tools = %v, want the trimmed pair", got)
	}
	if got := byID["b"].DependsOn; len(got) != 1 || got[0] != "a" {
		t.Errorf("depends_on = %v", got)
	}
	if len(byID["c"].DependsOn) != 0 || len(byID["c"].Tools) != 0 {
		t.Errorf("an absent list became non-empty: %+v", byID["c"])
	}
}

// A CALLER MUST NOT BE ABLE TO REWRITE AN ADMITTED PLAN.
//
// Tasks() used copy(), which duplicates the Task structs and SHARES their
// slices. Task.Tools is the validated grant, so plan.Tasks()[0].Tools[0] =
// "bash" edited the admitted plan in place, from outside, after every check had
// passed — the widening this file exists to make impossible, reached through
// the accessor rather than around it.
func TestTasksCannotBeUsedToRewriteTheAdmittedPlan(t *testing.T) {
	plan, err := ParsePlan(planArgs([]any{
		map[string]any{"id": "a", "prompt": "x", "tools": []any{"read_file"}},
		map[string]any{"id": "b", "prompt": "y", "depends_on": []any{"a"}},
	}, okBudget()), readOnlyLimits())
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}

	handed := plan.Tasks()
	handed[0].Tools[0] = "bash"
	handed[1].DependsOn[0] = "nonexistent"

	fresh := plan.Tasks()
	if fresh[0].Tools[0] != "read_file" {
		t.Errorf("a caller widened the admitted plan's grant to %q", fresh[0].Tools[0])
	}
	if fresh[1].DependsOn[0] != "a" {
		t.Errorf("a caller rewrote the admitted plan's dependency to %q", fresh[1].DependsOn[0])
	}
}

// A SAVED PLAN STILL RUNS THE WAY THE CALLER ASKED.
//
// resolveSavedPlan replaces the caller's arguments with the stored plan's, which
// is right for plan CONTENT — a half-overridden plan is not the plan that was
// saved. It is wrong for the two flags that say HOW to run it rather than what:
// `{"saved":"sweep","background":true}` passed the refusal list, then ran in the
// FOREGROUND, because background was read from the map that had replaced it.
func TestASavedPlanKeepsTheCallersExecutionDirectives(t *testing.T) {
	dir := t.TempDir()
	stored := mustPlan(t, []any{task("a", "x")}, okBudget(), readOnlyLimits())
	if _, err := SavePlan(dir, dir, "sweep", stored); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
	tool := &OrchestrateTool{Plans: PlanPaths{UserDir: dir}}

	resolved, err := tool.resolveSavedPlan(map[string]any{
		"saved": "sweep", "background": true, "auto_assign": false,
	})
	if err != nil {
		t.Fatalf("resolveSavedPlan: %v", err)
	}
	if got, _ := resolved["background"].(bool); !got {
		t.Error("the caller asked for background and the saved plan's args replaced it")
	}
	value, present := resolved["auto_assign"]
	if !present {
		t.Fatal("auto_assign was dropped, so a configured default cannot be overridden per run")
	}
	if enabled, _ := value.(bool); enabled {
		t.Error("auto_assign: false was lost")
	}
	// The plan itself is still the STORED one — the refusal's whole point.
	if tasks, _ := resolved["tasks"].([]any); len(tasks) != 1 {
		t.Errorf("the stored plan's tasks did not survive: %v", resolved["tasks"])
	}
}

// Supplying plan CONTENT alongside `saved` is still refused — the directive
// carve-out must not become a way to half-override a saved plan.
//
// THE PLAN IS SEEDED FIRST, and that is the whole difference between this test
// and a tautology. Against an empty directory resolveSavedPlan fails because
// "sweep" does not exist, so every case here returned a non-nil error and passed
// whether or not the override policy existed at all. With the plan present, the
// only thing left that can refuse is the policy — which is why the reason is
// asserted too, not just the failure.
func TestASavedPlanStillRefusesInlineContent(t *testing.T) {
	dir := t.TempDir()
	if _, err := SavePlan(dir, dir, "sweep", savedPlanFixture(t)); err != nil {
		t.Fatalf("seed the saved plan: %v", err)
	}
	tool := &OrchestrateTool{Plans: PlanPaths{UserDir: dir}}

	// The control: the seeded plan resolves cleanly on its own, so a failure
	// below cannot be blamed on the fixture.
	if _, err := tool.resolveSavedPlan(map[string]any{"saved": "sweep"}); err != nil {
		t.Fatalf("the seeded plan does not resolve, so nothing below proves anything: %v", err)
	}

	for _, field := range []string{"tasks", "budget", "name", "description"} {
		_, err := tool.resolveSavedPlan(map[string]any{"saved": "sweep", field: "anything"})
		if err == nil {
			t.Errorf("%q was accepted alongside a saved plan", field)
			continue
		}
		if !strings.Contains(err.Error(), field) {
			t.Errorf("%q was refused for a reason that does not name it, so the refusal may not be the override policy: %v", field, err)
		}
	}
}

// THE VERIFY CONVENTION IS TAUGHT, because nothing enforces it.
//
// No verdict is parsed and no claim is filtered — a plan author adopts this
// shape with the tasks they already write, or does not. So the only place it can
// exist is the description the author reads.
//
// Earned by measurement: two runs of the same audit on this repo, one ending in
// a verify task and one not. The verified run dropped five overclaims the other
// passed through, including an inference the unverified run stated as fact.
func TestTheToolTeachesTheFindVerifySynthesizeShape(t *testing.T) {
	tasks, ok := (&OrchestrateTool{}).Parameters().Properties["tasks"]
	if !ok {
		t.Fatal("the tasks property is missing")
	}
	for _, required := range []string{
		"end it in verification",
		"A verify task depends on the finders",
		"reports only what survived",
		"try to REFUTE each claim",
		"default to refuted when uncertain",
		"judge each claim independently",
		"a claim, not a trace",
	} {
		if !strings.Contains(tasks.Description, required) {
			t.Errorf("the description does not teach %q", required)
		}
	}
	// The task-authoring rules must survive alongside it — they are upstream of
	// verification and a badly split plan cannot be verified into a good one.
	if !strings.Contains(tasks.Description, "Split by SUBJECT") {
		t.Error("the task-authoring guidance was displaced by the verify convention")
	}
}

// THE TOOL TEACHES THE CONFLICT/RELAXATION SPLIT, because merging them is how a
// relaxation stops being reported.
//
// A measured run built to a specification, listed its requirement conflicts
// cleanly, and filed no relaxation at all — while having lowered a one-million
// bound to ten thousand, cut a sixty-second soak to five, and excluded a latency
// class from its own numbers. Each reached the reader as a cell in a results
// table. Nothing here parses or enforces the split; the tool description is the
// only place a plan author reads it, so its absence is the whole regression.
func TestTheToolTeachesConflictsAndRelaxationsApart(t *testing.T) {
	tasks, ok := (&OrchestrateTool{}).Parameters().Properties["tasks"]
	if !ok {
		t.Fatal("the tasks property is missing")
	}
	for _, required := range []string{
		// Both names, so an author can tell which they are looking at.
		"CONFLICT",
		"RELAXATION",
		// What separates them.
		"spec disagreeing with itself",
		"work coming in under the spec",
		// The reporting rule that was actually violated.
		"never only a cell in a results table",
		// ...and that a defensible relaxation is still reported.
		"even when it was the right call",
	} {
		if !strings.Contains(tasks.Description, required) {
			t.Errorf("the tasks description no longer teaches %q", required)
		}
	}
}

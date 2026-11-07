package specialist

import (
	"strings"
	"testing"
)

func paramPlan(t *testing.T, dir string) {
	t.Helper()
	plan := mustPlan(t, []any{
		map[string]any{"id": "trace", "prompt": "Trace every caller in ${scope} and quote file:line.",
			"tools": []any{"grep"}},
		map[string]any{"id": "judge", "prompt": "Judge whether ${scope} holds the ${property} guarantee.",
			"depends_on": []any{"trace"}},
	}, okBudget(), readOnlyLimits())
	if _, err := SavePlan(dir, dir, "sweep", plan); err != nil {
		t.Fatalf("SavePlan: %v", err)
	}
}

func resolveSaved(t *testing.T, dir string, args map[string]any) (map[string]any, error) {
	t.Helper()
	args["saved"] = "sweep"
	return (&OrchestrateTool{Plans: PlanPaths{UserDir: dir}}).resolveSavedPlan(args)
}

// ONE REVIEWED PLAN, MANY TARGETS. Without this a saved plan is fixed at the
// prose its author typed, so "audit internal/tui" gets copied and edited to
// audit anything else — and the copies drift from the reviewed original.
func TestASavedPlanIsFilledInFromParams(t *testing.T) {
	dir := t.TempDir()
	paramPlan(t, dir)

	resolved, err := resolveSaved(t, dir, map[string]any{
		"params": map[string]any{"scope": "internal/cli", "property": "additivity"},
	})
	if err != nil {
		t.Fatalf("resolveSavedPlan: %v", err)
	}
	tasks, _ := resolved["tasks"].([]any)
	if len(tasks) != 2 {
		t.Fatalf("got %d tasks", len(tasks))
	}
	first := planString(tasks[0].(map[string]any), "prompt")
	second := planString(tasks[1].(map[string]any), "prompt")
	if !strings.Contains(first, "internal/cli") || strings.Contains(first, "${") {
		t.Errorf("task 1 not substituted: %q", first)
	}
	if !strings.Contains(second, "internal/cli") || !strings.Contains(second, "additivity") {
		t.Errorf("task 2 not substituted: %q", second)
	}
}

// THE EXPANDED PLAN MUST STILL BE ADMITTED. Substituting after validation would
// mean the plan that ran was never the plan that was checked.
func TestAnExpandedPlanStillGoesThroughParsePlan(t *testing.T) {
	dir := t.TempDir()
	paramPlan(t, dir)
	resolved, err := resolveSaved(t, dir, map[string]any{
		"params": map[string]any{"scope": "internal/cli", "property": "additivity"},
	})
	if err != nil {
		t.Fatalf("resolveSavedPlan: %v", err)
	}
	plan, err := ParsePlan(resolved, readOnlyLimits())
	if err != nil {
		t.Fatalf("the expanded plan did not re-admit: %v", err)
	}
	for _, task := range plan.Tasks() {
		if strings.Contains(task.Prompt, "${") {
			t.Errorf("task %q reached admission still holding a placeholder: %q", task.ID, task.Prompt)
		}
	}
}

// FAILS CLOSED BOTH WAYS. A missing value would leave a literal "${scope}" in a
// prompt, which reads to the model as a directory that does not exist; a value
// matching nothing is almost always a typo, and ignoring it runs the plan
// against the wrong target while reporting success.
func TestParamMismatchesAreRefused(t *testing.T) {
	dir := t.TempDir()
	paramPlan(t, dir)

	t.Run("a placeholder with no value", func(t *testing.T) {
		_, err := resolveSaved(t, dir, map[string]any{"params": map[string]any{"scope": "internal/cli"}})
		if err == nil {
			t.Fatal("a plan ran with an unfilled placeholder")
		}
		if !strings.Contains(err.Error(), "property") {
			t.Errorf("the error does not name what is missing: %v", err)
		}
	})
	t.Run("a value with no placeholder", func(t *testing.T) {
		_, err := resolveSaved(t, dir, map[string]any{"params": map[string]any{
			"scope": "internal/cli", "property": "additivity", "scpoe": "typo"}})
		if err == nil {
			t.Fatal("a misspelled parameter was accepted, so the plan would run against the wrong target")
		}
		if !strings.Contains(err.Error(), "scpoe") {
			t.Errorf("the error does not name the typo: %v", err)
		}
	})
	t.Run("no params at all for a plan that needs them", func(t *testing.T) {
		if _, err := resolveSaved(t, dir, map[string]any{}); err == nil {
			t.Fatal("a parameterised plan ran with no parameters")
		}
	})
	t.Run("params for a plan that takes none", func(t *testing.T) {
		plain := t.TempDir()
		if _, err := SavePlan(plain, plain, "sweep", savedPlanFixture(t)); err != nil {
			t.Fatal(err)
		}
		_, err := resolveSaved(t, plain, map[string]any{"params": map[string]any{"scope": "x"}})
		if err == nil {
			t.Fatal("a plan with no placeholders accepted a parameter")
		}
	})
}

// A PARAMETER MUST NOT REACH AUTHORITY OR THE GRAPH. Substituting into tools
// would make "run the sweep plan with scope=x" a way to widen what the plan may
// do; substituting into an id or depends_on would let one argument silently
// detach a dependency edge. Both are fixed when the plan is saved and reviewed.
func TestParamsCannotReachToolsOrTheGraph(t *testing.T) {
	args := map[string]any{
		"tasks": []any{
			map[string]any{"id": "${scope}", "prompt": "look at ${scope}",
				"tools": []any{"${scope}"}, "depends_on": []any{"${scope}"}, "model": "${scope}"},
		},
	}
	// Only "prompt" is scanned, so the placeholders in tools/id/depends_on/model
	// are not parameters this plan takes — supplying scope fills the prompt alone.
	expanded, err := expandPlanParams(args, map[string]string{"scope": "bash"})
	if err != nil {
		t.Fatalf("expandPlanParams: %v", err)
	}
	task := expanded["tasks"].([]any)[0].(map[string]any)
	if got := planString(task, "prompt"); strings.Contains(got, "${") {
		t.Errorf("prompt not substituted: %q", got)
	}
	for _, field := range []string{"id", "model"} {
		if got := planString(task, field); got != "${scope}" {
			t.Errorf("%s was substituted to %q; structure and authority are fixed at save time", field, got)
		}
	}
	if got := task["tools"].([]any)[0].(string); got != "${scope}" {
		t.Errorf("a parameter reached the TOOL GRANT: %q — this is an authority-widening path", got)
	}
	if got := task["depends_on"].([]any)[0].(string); got != "${scope}" {
		t.Errorf("a parameter reached depends_on: %q", got)
	}
}

// THE STORED PLAN IS NOT MUTATED. LoadPlans caches by path, so rewriting the
// stored map would leave the next run carrying the previous run's arguments.
func TestExpandingDoesNotMutateTheStoredPlan(t *testing.T) {
	args := map[string]any{
		"description": "audit ${scope}",
		"tasks":       []any{map[string]any{"id": "a", "prompt": "read ${scope}"}},
	}
	if _, err := expandPlanParams(args, map[string]string{"scope": "internal/cli"}); err != nil {
		t.Fatalf("expandPlanParams: %v", err)
	}
	if got := planString(args, "description"); got != "audit ${scope}" {
		t.Errorf("the source description was rewritten to %q", got)
	}
	original := args["tasks"].([]any)[0].(map[string]any)
	if got := planString(original, "prompt"); got != "read ${scope}" {
		t.Errorf("the source task was rewritten to %q", got)
	}
}

// A non-string parameter would substitute something like "map[]" into a prompt.
func TestParamValuesMustBeStrings(t *testing.T) {
	for name, value := range map[string]any{
		"a number":  float64(7),
		"an object": map[string]any{"a": "b"},
		"a list":    []any{"a"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := planParamsFromArgs(map[string]any{"params": map[string]any{"scope": value}}); err == nil {
				t.Error("a non-string parameter was accepted")
			}
		})
	}
	if _, err := planParamsFromArgs(map[string]any{"params": "not-an-object"}); err == nil {
		t.Error("a non-object params was accepted")
	}
	if got, err := planParamsFromArgs(map[string]any{}); err != nil || got != nil {
		t.Errorf("absent params should be absent, got %v %v", got, err)
	}
}

// An ordinary plan with no placeholders is untouched — the whole feature is
// inert for every plan saved before it existed.
func TestAPlanWithoutPlaceholdersIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	if _, err := SavePlan(dir, dir, "sweep", savedPlanFixture(t)); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveSaved(t, dir, map[string]any{})
	if err != nil {
		t.Fatalf("resolveSavedPlan: %v", err)
	}
	if tasks, _ := resolved["tasks"].([]any); len(tasks) != 3 {
		t.Fatalf("got %d tasks, want the stored 3", len(tasks))
	}
}

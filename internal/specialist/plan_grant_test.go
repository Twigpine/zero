package specialist

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func mustParsePlan(t *testing.T, args map[string]any, limits Limits) Plan {
	t.Helper()
	plan, err := ParsePlan(args, limits)
	if err != nil {
		t.Fatalf("ParsePlan: %v", err)
	}
	return plan
}

func singleTaskPlanArgs(task map[string]any) map[string]any {
	return map[string]any{
		"name":   "p",
		"tasks":  []any{task},
		"budget": map[string]any{"max_workers": 1, "max_tokens": 500_000},
	}
}

// THE EXPRESSIBILITY FIX. An empty ResolvedTools used to be indistinguishable
// from an unset one, so a deliberately-empty grant expanded to the default
// read-only category — the narrower the parent, the wider the child.
func TestResolvedToolAllowlistTreatsADeliberatelyEmptyGrantAsEmpty(t *testing.T) {
	resolved, err := resolvedToolAllowlist(Manifest{ResolvedTools: []string{}, ToolsResolved: true})
	if err != nil {
		t.Fatalf("resolvedToolAllowlist: %v", err)
	}
	if len(resolved) != 0 {
		t.Fatalf("an authoritative empty tool list expanded to %v; empty must mean empty", resolved)
	}
}

// The other side of the same flag: a manifest that never resolved its tools
// still gets the default expansion, so no existing caller changes behaviour.
func TestResolvedToolAllowlistStillExpandsAnUnresolvedManifest(t *testing.T) {
	resolved, err := resolvedToolAllowlist(Manifest{Metadata: Metadata{Name: "x"}})
	if err != nil {
		t.Fatalf("resolvedToolAllowlist: %v", err)
	}
	if len(resolved) == 0 {
		t.Fatal("an unresolved manifest must still expand to the default selection")
	}
}

// A bare []string cannot carry this distinction across a round trip: the field
// is omitempty, so a deliberately-empty list marshals away entirely and comes
// back nil. This is why the fix is a flag and not a nil check.
func TestToolsResolvedSurvivesAJSONRoundTrip(t *testing.T) {
	manifest := Manifest{ResolvedTools: []string{}, ToolsResolved: true}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round Manifest
	if err := json.Unmarshal(encoded, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round.ResolvedTools != nil {
		t.Fatalf("omitempty was expected to erase the empty slice, got %v", round.ResolvedTools)
	}
	if !round.ToolsResolved {
		t.Fatalf("the authoritative flag did not survive the round trip: %s", encoded)
	}
	resolved, err := resolvedToolAllowlist(round)
	if err != nil {
		t.Fatalf("resolvedToolAllowlist: %v", err)
	}
	if len(resolved) != 0 {
		t.Fatalf("after a round trip the empty grant expanded to %v", resolved)
	}
}

// THE SECOND LAYER. ExecutePlan refuses an empty grant before a manifest is
// ever built, but if that check were bypassed the manifest itself must still
// not expand. This is the belt to planToolGrant's braces.
func TestPlanTaskManifestMarksItsGrantAuthoritative(t *testing.T) {
	manifest := planTaskManifest("explorer", "", "", []string{})
	if !manifest.ToolsResolved {
		t.Fatal("a plan task's manifest carries an already-intersected grant; it must be marked authoritative")
	}
	resolved, err := resolvedToolAllowlist(manifest)
	if err != nil {
		t.Fatalf("resolvedToolAllowlist: %v", err)
	}
	if len(resolved) != 0 {
		t.Fatalf("an empty plan grant expanded to %v instead of refusing", resolved)
	}
}

// planToolGrant refuses rather than returning an empty slice, so the empty case
// never reaches a caller that has to guess what it meant.
func TestPlanToolGrantRefusesWhenTheParentHoldsNothing(t *testing.T) {
	_, err := planToolGrant(Task{ID: "a"}, nil)
	if err == nil {
		t.Fatal("an empty parent grant must be refused, not returned as an empty list")
	}
	if !strings.Contains(err.Error(), "parent grant: none") {
		t.Fatalf("the refusal must name the empty grant explicitly, got: %v", err)
	}
}

// The intersection is unconditional on both sides. Guarding it on a non-empty
// parent list is what made the rule inert.
func TestPlanToolGrantNarrowsToTheParentsHoldings(t *testing.T) {
	granted, err := planToolGrant(Task{ID: "a", Tools: []string{"read_file", "grep"}}, []string{"grep"})
	if err != nil {
		t.Fatalf("planToolGrant: %v", err)
	}
	if strings.Join(granted, ",") != "grep" {
		t.Fatalf("granted %v, want [grep]: read_file is not held by the parent", granted)
	}
}

// An empty request inherits the parent's grant — narrowed, never widened.
func TestPlanToolGrantInheritsOnlyWhatTheParentHolds(t *testing.T) {
	granted, err := planToolGrant(Task{ID: "a"}, []string{"grep", "write_file"})
	if err != nil {
		t.Fatalf("planToolGrant: %v", err)
	}
	if strings.Join(granted, ",") != "grep" {
		t.Fatalf("granted %v, want [grep]: write_file is not a read-only plan tool", granted)
	}
}

// Validation rejects a widening even when the caller supplied a grant that does
// not contain the requested tool. Previously this only fired when the grant was
// non-empty, which no production caller ever made it.
func TestParsePlanRejectsAToolTheParentDoesNotHold(t *testing.T) {
	_, err := ParsePlan(
		singleTaskPlanArgs(map[string]any{"id": "a", "prompt": "p", "tools": []any{"read_file"}}),
		Limits{MaxTasks: 20, ParentTools: []string{"grep"}},
	)
	if err == nil {
		t.Fatal("a task requesting a tool the run does not hold must be rejected")
	}
	if !strings.Contains(err.Error(), "never widen it") {
		t.Fatalf("unexpected rejection reason: %v", err)
	}
}

// With no grant supplied at all, every request is rejected. Fail closed: an
// unsupplied grant is a wiring bug, and the run must stop rather than assume
// authority — which is exactly what the old escape hatch did.
func TestParsePlanRejectsEveryToolWhenNoGrantWasSupplied(t *testing.T) {
	_, err := ParsePlan(
		singleTaskPlanArgs(map[string]any{"id": "a", "prompt": "p", "tools": []any{"read_file"}}),
		Limits{MaxTasks: 20, MaxTokens: 200000},
	)
	if err == nil {
		t.Fatal("an unwired parent grant must reject, not silently allow")
	}
}

// End to end through ExecutePlan: an ungrantable task is a recorded FAILURE
// with a reason, never a dispatch. A dispatched-then-failed task would put a
// task_dispatched event in the log for work that never started.
func TestExecutePlanFailsAnUngrantableTaskWithoutDispatchingIt(t *testing.T) {
	plan := mustParsePlan(t,
		singleTaskPlanArgs(map[string]any{"id": "a", "prompt": "p"}),
		Limits{MaxTasks: 20, ParentTools: []string{"grep"}},
	)
	dispatched := 0
	report := ExecutePlan(context.Background(), plan, nil,
		func(context.Context, PlanTaskRequest) (TaskResult, error) {
			dispatched++
			return TaskResult{Outcome: TaskSucceeded}, nil
		}, nil)

	if dispatched != 0 {
		t.Fatalf("the runner was invoked %d times for a task that could be granted nothing", dispatched)
	}
	if report.Status != PlanFailed {
		t.Fatalf("status = %q, want %q", report.Status, PlanFailed)
	}
	if len(report.Tasks) != 1 || report.Tasks[0].Outcome != TaskFailed {
		t.Fatalf("outcome = %+v, want a recorded failure", report.Tasks)
	}
	if !strings.Contains(report.Tasks[0].Err, "resolved no tools") {
		t.Fatalf("the failure must say why, got %q", report.Tasks[0].Err)
	}
}

// THE SIBLING COMPARISON for the tool grant.
//
// The authority rule has two enforcement points: validateTaskTools at admission
// and planToolGrant at dispatch, the second commented "BELT AND BRACES … so a
// validation bug cannot widen a task's authority". Both were fixed together —
// and two sites fixed together is exactly the shape that drifts apart later,
// because each ends up with its own test asserting its own expectation.
//
// This asserts them AGAINST EACH OTHER on the same inputs: whatever validation
// admits, dispatch must grant, and dispatch must never grant a tool validation
// would have rejected.
func TestBothGrantEnforcementPointsAgree(t *testing.T) {
	cases := []struct {
		name   string
		tools  []any
		parent []string
	}{
		{"no request, parent holds reads", nil, []string{"read_file", "grep"}},
		{"no request, parent holds one read", nil, []string{"grep"}},
		{"no request, parent holds only mutators", nil, []string{"write_file", "bash"}},
		{"no request, parent holds nothing", nil, nil},
		{"request inside the grant", []any{"grep"}, []string{"read_file", "grep"}},
		{"request outside the grant", []any{"read_file"}, []string{"grep"}},
		{"request partly outside the grant", []any{"read_file", "grep"}, []string{"grep"}},
		{"request a mutator", []any{"write_file"}, []string{"read_file", "write_file"}},
		{"request with no grant at all", []any{"read_file"}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]any{"id": "a", "prompt": "p"}
			if tc.tools != nil {
				fields["tools"] = tc.tools
			}
			limits := Limits{MaxTasks: 20, MaxTokens: 200000, ParentTools: tc.parent}

			_, parseErr := ParsePlan(singleTaskPlanArgs(fields), limits)

			task := Task{ID: "a", Prompt: "p"}
			for _, name := range tc.tools {
				task.Tools = append(task.Tools, name.(string))
			}
			granted, grantErr := planToolGrant(task, tc.parent)

			// THE RELATIONSHIP IS SUBSET, NOT EQUALITY, and the direction is
			// what matters. Admission is the stricter layer: it REJECTS a task
			// naming anything outside the grant. Dispatch is the narrowing
			// layer: it DROPS such a tool and grants the rest. Those differ,
			// and they should — dispatch only ever runs on an already-admitted
			// task, so its job when it disagrees is to hand over less, never
			// more. Requiring the two to refuse identically would be asserting
			// a symmetry the design does not want.
			//
			// So the invariant tested here is one-directional: whatever
			// dispatch grants must be something admission would also have
			// allowed. A violation means the second layer widened authority,
			// which is the defect the "belt and braces" comment promises
			// cannot happen.
			if grantErr != nil {
				if len(granted) != 0 {
					t.Fatalf("a refused grant must hand over nothing, got %v", granted)
				}
				return
			}
			for _, name := range granted {
				// ADMISSION IS ASKED, not re-described. This used to assert
				// "granted implies read-only", which was a restatement of the
				// rule as it stood — so when the rule changed to permit a named
				// write tool, the test failed against correct code and would
				// have been "fixed" by loosening the very thing it guards.
				// Calling validateTaskTools makes the assertion the invariant
				// itself: whatever dispatch hands over, admission would have
				// allowed, whatever admission currently means.
				if err := validateTaskTools(Task{ID: "a", Prompt: "p", Tools: []string{name}}, limits); err != nil {
					t.Fatalf("dispatch granted %q, which admission would have rejected: %v", name, err)
				}
				if !containsGrant(tc.parent, name) {
					t.Fatalf("dispatch granted %q, which the parent does not hold %v; "+
						"the second layer widened authority", name, tc.parent)
				}
			}
			// And the converse direction: when admission ACCEPTS, dispatch must
			// have something to hand over, or an admitted task could never run.
			if parseErr == nil && len(granted) == 0 {
				t.Fatal("admission accepted the task but dispatch granted nothing")
			}
		})
	}
}

func containsGrant(grant []string, name string) bool {
	for _, held := range grant {
		if held == name {
			return true
		}
	}
	return false
}

// The real tools must actually declare what the loop now keys on. The agent
// package proves the MECHANISM with probes; this proves the two production
// tools opt in, which is what makes their children visible.
func TestSubagentToolsDeclareChildProgress(t *testing.T) {
	if !(&TaskTool{}).StreamsChildProgress() {
		t.Error("the Task tool must declare child progress; its sub-agents streamed before this change")
	}
	if !(&OrchestrateTool{}).StreamsChildProgress() {
		t.Error("orchestrate must declare child progress; without it a plan runs invisibly")
	}
}

// TWO LISTS THAT MUST NOT DRIFT (invariant 5).
//
// planReadOnlyTools bounds what a plan task may hold; readOnlySpecialistTools
// decides whether a specialist counts as read-only for the Task tool's
// permission gate. They are different questions with the same answer, and they
// drifted: lsp_navigate was in the first and not the second, so a plan task
// could hold a tool that made its own manifest fail the read-only check.
//
// The relationship is SUBSET, not equality, and the direction matters: anything
// a plan task may hold must be something the wider gate also calls read-only.
// The reverse is not required — update_plan is read-only for a specialist and
// deliberately not a plan tool.
func TestReadOnlyToolSetsAgree(t *testing.T) {
	for name := range planReadOnlyTools {
		if !readOnlySpecialistTools[name] {
			t.Errorf("planReadOnlyTools allows %q but readOnlySpecialistTools does not call it read-only; "+
				"a plan task holding it would fail the specialist read-only check", name)
		}
	}
}

// A manifest holding the full plan grant must pass the read-only check. This is
// the consequence the drift produced, asserted at the behaviour rather than at
// the two maps.
func TestAFullPlanGrantIsAReadOnlyManifest(t *testing.T) {
	manifest := planTaskManifest("explorer", "", "", PlanReadOnlyToolNames())
	if !manifestIsReadOnly(manifest) {
		t.Fatalf("a manifest holding exactly the plan grant %v is not considered read-only", PlanReadOnlyToolNames())
	}
}

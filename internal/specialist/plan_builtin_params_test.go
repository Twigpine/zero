package specialist

import (
	"strings"
	"testing"
)

// A BUNDLED PLAN MUST NOT CARRY A STAND-IN FOR ITS OWN SUBJECT.
//
// research.json read "Find where THE SUBJECT is DEFINED" — prose describing a
// hole rather than a parameter that declares one, so nothing refused it and
// nothing filled it. Run from the documented path it dispatched five child
// agents to search the repository for the literal words "THE SUBJECT". A
// placeholder that is not a ${parameter} is invisible to expandPlanParams, which
// is the one thing in this package that would otherwise have caught it.
//
// Checked over EVERY builtin, not just research: the next bundled plan gets the
// same guarantee without anyone remembering to ask for it.
func TestNoBuiltinPlanUsesAProsePlaceholder(t *testing.T) {
	// Shapes a human writes when they mean "fill this in" and a model reads as an
	// instruction about something that exists.
	standIns := []string{
		"THE SUBJECT", "THE TOPIC", "THE TARGET", "THE QUESTION",
		"<subject>", "<topic>", "<target>", "TODO", "FIXME", "XXX",
	}
	for _, plan := range builtinPlans() {
		haystack := strings.ToUpper(planParamSearchText(plan.Args))
		for _, standIn := range standIns {
			if strings.Contains(haystack, strings.ToUpper(standIn)) {
				t.Errorf("builtin plan %q contains the stand-in %q: it must be a ${parameter} so a run without one is refused, "+
					"not prose that sends every task searching for the literal words", plan.Name, standIn)
			}
		}
	}
}

// The research plan declares exactly one parameter, and it reaches every task
// that needs it.
func TestTheResearchPlanTakesItsSubjectAsAParameter(t *testing.T) {
	var research SavedPlan
	for _, plan := range builtinPlans() {
		if plan.Name == "research" {
			research = plan
		}
	}
	if research.Name == "" {
		t.Fatal("the research plan is no longer bundled")
	}

	params := PlanParams(research.Args)
	if len(params) != 1 || params[0] != "subject" {
		t.Fatalf("research must take exactly the subject, takes %v", params)
	}

	// EVERY SEARCH TASK, not just one. The three searches are deliberately
	// independent angles on the same subject; one left un-parameterised would
	// search for nothing in particular while the other two worked.
	tasks, ok := research.Args["tasks"].([]any)
	if !ok {
		t.Fatal("research has no tasks")
	}
	searchers := 0
	for _, raw := range tasks {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if planString(entry, "phase") != "search" {
			continue
		}
		searchers++
		if !strings.Contains(planString(entry, "prompt"), "${subject}") {
			t.Errorf("search task %q never mentions the subject, so it searches for nothing in particular",
				planString(entry, "id"))
		}
	}
	if searchers == 0 {
		t.Fatal("research has no search tasks: the assertion above proved nothing")
	}
}

// Filled in, it must still be a runnable plan. A parameter that produced a plan
// failing admission would have moved the failure rather than fixed it.
func TestTheResearchPlanAdmitsOnceItsSubjectIsSupplied(t *testing.T) {
	var research SavedPlan
	for _, plan := range builtinPlans() {
		if plan.Name == "research" {
			research = plan
		}
	}
	if research.Name == "" {
		t.Fatal("the research plan is no longer bundled")
	}

	expanded, err := expandPlanParams(research.Args, map[string]string{"subject": "the retry watchdog"})
	if err != nil {
		t.Fatalf("supplying the subject must expand: %v", err)
	}
	plan, err := ParsePlan(expanded, Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()})
	if err != nil {
		t.Fatalf("the expanded research plan does not admit: %v", err)
	}
	if plan.TaskCount() == 0 {
		t.Fatal("the expanded plan has no tasks")
	}
	// And the subject actually reached the prompts, rather than admitting with
	// the placeholder still in them.
	for _, task := range plan.Tasks() {
		if strings.Contains(task.Prompt, "${") {
			t.Fatalf("task %q still carries an unfilled placeholder: %s", task.ID, task.Prompt)
		}
	}
	// Unsupplied, it must REFUSE rather than run five agents on a placeholder.
	if _, err := expandPlanParams(research.Args, nil); err == nil {
		t.Fatal("research ran without a subject")
	}
}

// planParamSearchText gathers the prose a stand-in could hide in: the same
// fields expandPlanParams substitutes into, plus the description.
func planParamSearchText(args map[string]any) string {
	var b strings.Builder
	b.WriteString(planString(args, "description"))
	b.WriteString("\n")
	tasks, ok := args["tasks"].([]any)
	if !ok {
		return b.String()
	}
	for _, raw := range tasks {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, field := range planParamTaskFields {
			b.WriteString(planString(entry, field))
			b.WriteString("\n")
		}
	}
	return b.String()
}

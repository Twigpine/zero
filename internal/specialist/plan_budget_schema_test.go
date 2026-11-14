package specialist

import (
	"fmt"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

// THE SCHEMA MUST CARRY THE BOUNDS IT ENFORCES.
//
// A manual run emitted budget.max_tokens_per_task: 5000. Admission refused it
// for sitting below minimumPlausibleTaskTokens, correctly — and the model spent
// a turn and a call discovering a rule that was written down only in prose, in a
// description it had already read. A declared bound is checked by the provider
// before the request is ever made.
//
// EACH ASSERTION PAIRS THE SCHEMA WITH ITS ENFORCEMENT CONSTANT rather than with
// a literal, so a bound that changes in one place and not the other fails here
// instead of shipping as a schema that lies about the rules.
func budgetProperties(t *testing.T) map[string]tools.PropertySchema {
	t.Helper()
	tool := &OrchestrateTool{}
	budget, ok := tool.Parameters().Properties["budget"]
	if !ok {
		t.Fatal("the orchestrate schema no longer declares a budget")
	}
	if len(budget.Properties) == 0 {
		t.Fatal("budget declares no fields, so a model composing one must invent the names")
	}
	return budget.Properties
}

func TestTheBudgetSchemaDeclaresTheBoundsItEnforces(t *testing.T) {
	props := budgetProperties(t)

	for _, want := range []struct {
		field   string
		minimum *int
		maximum *int
	}{
		// The one that actually cost a retry.
		{field: "max_tokens_per_task", minimum: planSchemaBound(minimumPlausibleTaskTokens)},
		// A whole-plan budget below the floor cannot buy its first task either.
		{field: "max_tokens", minimum: planSchemaBound(minimumPlausibleTaskTokens)},
		{field: "max_workers", minimum: planSchemaBound(1), maximum: planSchemaBound(maxPlanWorkers)},
		{field: "max_retries", minimum: planSchemaBound(0), maximum: planSchemaBound(maxPlanRetries)},
		{field: "max_stall_seconds", minimum: planSchemaBound(int(minStallTimeout.Seconds()))},
		{field: "max_wall_seconds", minimum: planSchemaBound(1)},
	} {
		field, ok := props[want.field]
		if !ok {
			t.Errorf("budget no longer declares %s", want.field)
			continue
		}
		if got := boundText(field.Minimum); got != boundText(want.minimum) {
			t.Errorf("%s declares minimum %s, enforces %s", want.field, got, boundText(want.minimum))
		}
		if got := boundText(field.Maximum); got != boundText(want.maximum) {
			t.Errorf("%s declares maximum %s, enforces %s", want.field, got, boundText(want.maximum))
		}
	}
}

// The bound and the enforcement must agree on the SAME VALUE, proved by feeding
// the schema's own minimum minus one to the real admission path and requiring a
// refusal — and the minimum itself and requiring acceptance. A declared bound
// that is merely present but wrong is worse than none: it teaches the model a
// rule the code does not hold.
func TestTheDeclaredPerTaskFloorIsTheOneAdmissionEnforces(t *testing.T) {
	props := budgetProperties(t)
	field := props["max_tokens_per_task"]
	if field.Minimum == nil {
		t.Fatal("max_tokens_per_task declares no minimum")
	}
	floor := *field.Minimum

	args := func(perTask int) map[string]any {
		return map[string]any{
			"name":   "p",
			"tasks":  []any{map[string]any{"id": "a", "prompt": "look"}},
			"budget": map[string]any{"max_workers": float64(1), "max_tokens_per_task": float64(perTask)},
		}
	}
	limits := Limits{MaxTasks: 20, ParentTools: PlanReadOnlyToolNames()}

	if _, err := ParsePlan(args(floor-1), limits); err == nil {
		t.Fatalf("one below the declared minimum (%d) was admitted: the schema promises a bound nothing enforces", floor)
	}
	if _, err := ParsePlan(args(floor), limits); err != nil {
		t.Fatalf("the declared minimum (%d) was refused: the schema forbids a value that is actually legal: %v", floor, err)
	}
}

// The value from the real run, against the real path.
func TestTheRejectedRunValueIsNowOutsideTheDeclaredRange(t *testing.T) {
	props := budgetProperties(t)
	field := props["max_tokens_per_task"]
	if field.Minimum == nil || *field.Minimum <= 5000 {
		t.Fatalf("max_tokens_per_task: 5000 is still inside the declared range, so the model can still emit it")
	}
}

func boundText(bound *int) string {
	if bound == nil {
		return "none"
	}
	return fmt.Sprintf("%d", *bound)
}

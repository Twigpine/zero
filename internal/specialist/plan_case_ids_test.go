package specialist

import (
	"strings"
	"testing"
)

// CASE-VARIANT IDS ARE ONE ID. A task id becomes the scratchpad filename, so on
// a case-insensitive filesystem "Alpha" and "alpha" shared Alpha.md and the
// dependent read one task's answer as the other's, with a matching length so
// nothing looked wrong. The plan is refused at admission instead.
func TestParsePlanRejectsIDsThatDifferOnlyInCase(t *testing.T) {
	for _, pair := range [][2]string{{"Alpha", "alpha"}, {"alpha", "ALPHA"}, {"fix_Bug-1", "FIX_bug-1"}} {
		_, err := ParsePlan(planArgs([]any{task(pair[0], "x"), task(pair[1], "y")}, okBudget()), readOnlyLimits())
		if err == nil {
			t.Fatalf("%q and %q were admitted as distinct tasks", pair[0], pair[1])
		}
		for _, want := range []string{pair[0], pair[1], "case"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("the refusal must name %q: %v", want, err)
			}
		}
	}
	// Ids that differ in more than case are still distinct.
	if _, err := ParsePlan(planArgs([]any{task("Alpha", "x"), task("alpha2", "y")}, okBudget()), readOnlyLimits()); err != nil {
		t.Fatalf("distinct ids were refused: %v", err)
	}
}

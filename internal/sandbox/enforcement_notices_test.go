package sandbox

import "testing"

// THE CHANNEL IS PINNED WITHOUT A PRODUCER.
//
// Nothing in the tree fills CommandPlan.Notes today: the Windows denyRead trade
// notice was the last producer, and since #1006 refuses denyRead outright there
// is no trade to describe. The line in EnforcementFor that carries plan notes
// into Enforcement.Notices is still what hooks, plugins and MCP read, and the
// end-to-end test that used to reach it through the real producer went with the
// producer. So it is driven from a fixture plan instead: deleting that one line
// fails here, and nowhere else.
func TestEnforcementForCarriesThePlanNoticesToTheGenericContract(t *testing.T) {
	plan := CommandPlan{Notes: []string{
		"first fixed sentence from the sandbox",
		"second fixed sentence from the sandbox",
	}}
	notices := EnforcementFor(plan).Notices
	if len(notices) != len(plan.Notes) {
		t.Fatalf("EnforcementFor produced %d notices from %d plan notes; hooks, plugins and MCP read this field and would see nothing",
			len(notices), len(plan.Notes))
	}
	for index, note := range plan.Notes {
		if notices[index] != note {
			t.Errorf("notice %d = %q, want the plan note %q in the same position", index, notices[index], note)
		}
	}
}

// And a plan with nothing to say produces no notices, so a consumer cannot be
// handed an empty string it would then render as a card.
func TestEnforcementForCarriesNoNoticesFromASilentPlan(t *testing.T) {
	if notices := EnforcementFor(CommandPlan{}).Notices; len(notices) != 0 {
		t.Fatalf("EnforcementFor produced notices from a plan with none: %q", notices)
	}
}

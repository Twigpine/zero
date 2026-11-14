package tui

import (
	"testing"

	"github.com/Twigpine/zero/internal/execprofile"
	"github.com/Twigpine/zero/internal/modelregistry"
)

// THE TEST THAT WOULD HAVE CAUGHT IT.
//
// There are two doors onto the same decision — "does this profile's effort
// apply on this model?":
//
//	door 1  select the profile while already on the model  (handleProfileCommand)
//	door 2  switch to the model with the profile active    (reconcileProfileAfterModelSwitch)
//
// Each door already had tests. Both passed. They disagreed anyway, because each
// asserted its OWN expectation and nothing compared them: door 1 was fixed to
// stop treating an uncatalogued model's empty ring as a refusal, and door 2 was
// left calling the old predicate. The result was that the same user on the same
// model got a different effort depending on which door they came through.
//
// This test asserts the doors against EACH OTHER, so a future change to one of
// them fails here rather than shipping a second disagreement.
func TestProfileEffortDoorsAgree(t *testing.T) {
	const want = modelregistry.ReasoningEffortHigh

	cases := []struct {
		name  string
		model string
	}{
		// Catalogued and supports the level.
		{"catalogued, ring includes the level", "claude-sonnet-4.5"},
		// Catalogued with an empty ring: the catalog VOUCHES that there are no
		// reasoning controls, so neither door may fill.
		{"catalogued, ring is empty", "gpt-4o"},
		// Not catalogued: the catalog cannot vouch either way. This is the case
		// the two doors disagreed on, and it is both of this user's models.
		{"uncatalogued, empty ring", "glm-5.2"},
		{"uncatalogued, name-inferred ring", "gpt-5-mini"},
		// No model at all: nothing to make a support claim about.
		{"no model selected", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Door 1: would selecting the profile here fill the level?
			selecting := model{modelName: tc.model}
			viaSelection := selecting.profileEffortApplies(want)

			// Door 2: the profile is active with its fill applied, and the
			// session switches to this model. Does the fill survive?
			switching := model{
				modelName:                tc.model,
				execProfileName:          execprofile.Name,
				execProfileAppliedEffort: want,
				reasoningEffort:          want,
			}
			efforts, ringKnown := switching.availableReasoningEffortsKnown()
			after := switching.reconcileProfileAfterModelSwitch(efforts, ringKnown)
			viaSwitch := after.reasoningEffort == want

			if viaSelection != viaSwitch {
				t.Fatalf("the doors disagree on %q: selecting the profile here fills=%v, "+
					"but switching here leaves effort %q (applied %q, unraised %q)",
					tc.model, viaSelection, after.reasoningEffort,
					after.execProfileAppliedEffort, after.execProfileEffortUnraised)
			}

			// The bookkeeping must agree too: a level that was never skipped
			// must not be reported as unraised, or the status line claims the
			// model refused something it was never asked for.
			if viaSelection && after.execProfileEffortUnraised != "" {
				t.Fatalf("%q takes the fill, but the switch recorded it as unraised (%q)",
					tc.model, after.execProfileEffortUnraised)
			}
		})
	}
}

// The shared rule, stated directly. profileEffortAppliesOn is the single
// definition both doors call; if it ever grows a second copy, the door
// agreement test above is what fails.
func TestProfileEffortRuleDistinguishesVouchedFromUnknown(t *testing.T) {
	const want = modelregistry.ReasoningEffortHigh

	if profileEffortAppliesOn("gpt-4o", nil, true, want) {
		t.Error("a catalogued model with an empty ring genuinely has no reasoning controls; the fill must not apply")
	}
	if !profileEffortAppliesOn("glm-5.2", nil, false, want) {
		t.Error("an uncatalogued model cannot be vouched for either way; declining silently drops the posture's effort")
	}
	if profileEffortAppliesOn("", nil, false, want) {
		t.Error("with no model selected there is nothing to make a support claim about")
	}
	if !profileEffortAppliesOn("claude-sonnet-4.5", []modelregistry.ReasoningEffort{want}, true, want) {
		t.Error("a ring that contains the level must apply")
	}
}

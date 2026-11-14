package tui

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/execprofile"
)

// THREE DOORS onto "which efforts can I set?" — the /effort card, the popup
// picker, and the command itself. The relationship is EQUALITY (RULES.md §3):
// every value either surface offers must be one the command accepts.
//
// Asserted by feeding each offered value back through the REAL command rather
// than by comparing lists — a list comparison would pass while the command
// rejected all of them. The picker is included because it was the surface that
// actually failed in use: it read the catalog directly, so on a model with no
// catalog entry it offered nothing but "auto".
func TestEveryAdvertisedEffortIsAccepted(t *testing.T) {
	for _, name := range []string{"glm-5.2", "claude-sonnet-4.5", "gpt-4o", "gpt-5-mini"} {
		t.Run(name, func(t *testing.T) {
			m := model{modelName: name}

			offered := map[string][]string{
				"card": m.settableEfforts(),
			}
			var fromPicker []string
			for _, item := range m.newEffortPicker().items {
				fromPicker = append(fromPicker, item.Value)
			}
			offered["picker"] = fromPicker

			for surface, values := range offered {
				for _, value := range values {
					_, out := m.handleEffortCommand(value)
					for _, refusal := range []string{"Unknown reasoning effort", "is not supported by", "does not expose reasoning effort"} {
						if strings.Contains(out, refusal) {
							t.Errorf("the %s offers %q on %s but the command refuses it: %s", surface, value, name, out)
						}
					}
				}
			}

			// And the two surfaces must offer the same set, "auto" aside — one
			// listing a level the other omits is the disagreement this test
			// exists to prevent.
			for _, value := range m.settableEfforts() {
				if !strings.Contains(strings.Join(fromPicker, ","), value) {
					t.Errorf("the card offers %q on %s but the picker does not", value, name)
				}
			}
		})
	}
}

// The picker must offer the posture on every model, and preselect it when it is
// active — under zeromaxing m.reasoningEffort holds "high", the level the
// posture FILLED, so preselecting from that would highlight the wrong row.
func TestThePickerOffersAndPreselectsThePosture(t *testing.T) {
	for _, name := range []string{"glm-5.2", "claude-sonnet-4.5", "gpt-4o"} {
		items := model{modelName: name}.newEffortPicker().items
		var labels []string
		for _, item := range items {
			labels = append(labels, item.Value)
		}
		if !strings.Contains(strings.Join(labels, ","), execprofile.Name) {
			t.Errorf("%s: the picker does not offer %q, which it routes straight into handleEffortCommand", name, execprofile.Name)
		}
	}

	active := model{modelName: "glm-5.2", execProfileName: execprofile.Name, reasoningEffort: "high"}
	picker := active.newEffortPicker()
	if got := picker.items[picker.selected].Value; got != execprofile.Name {
		t.Fatalf("under the posture the picker preselects %q, want %q", got, execprofile.Name)
	}
}

// A model the catalog says has no controls offers only auto and the posture —
// the picker's own version of the card's rule.
func TestThePickerRespectsCatalogAuthority(t *testing.T) {
	var labels []string
	for _, item := range (model{modelName: "gpt-4o"}).newEffortPicker().items {
		labels = append(labels, item.Value)
	}
	if strings.Join(labels, ",") != "auto,"+execprofile.Name {
		t.Fatalf("picker = %v, want only auto and the posture on a model with no reasoning controls", labels)
	}
}

// A model the catalog VOUCHES has no reasoning controls must not be offered
// any. gpt-4o is catalogued with an empty ring, and offering low/medium/high
// there would advertise exactly what the command rejects.
func TestACataloguedModelWithNoControlsOffersOnlyThePosture(t *testing.T) {
	m := model{modelName: "gpt-4o"}
	settable := m.settableEfforts()
	if len(settable) != 1 || settable[0] != execprofile.Name {
		t.Fatalf("settable = %v, want only the posture: the catalog says this model has no reasoning controls", settable)
	}
}

// An UNCATALOGUED model is a different case: Zero cannot vouch either way, the
// levels are forwarded, and telling the user "not listed" while showing nothing
// typeable leaves them with no way to act.
func TestAnUncataloguedModelStillOffersTheLevels(t *testing.T) {
	m := model{modelName: "glm-5.2"}
	card := m.effortText()
	if !strings.Contains(card, "you can set") {
		t.Fatalf("an uncatalogued model must still say what can be set:\n%s", card)
	}
	for _, want := range []string{"low", "medium", "high", execprofile.Name} {
		if !strings.Contains(card, want) {
			t.Errorf("the card does not offer %q on an uncatalogued model:\n%s", want, card)
		}
	}
}

// The posture is selected through this namespace, so it belongs in the list on
// every model — it was missing from all of them while the actions line offered
// it.
func TestThePostureIsOfferedOnEveryModel(t *testing.T) {
	for _, name := range []string{"glm-5.2", "claude-sonnet-4.5", "gpt-4o"} {
		m := model{modelName: name}
		if !strings.Contains(strings.Join(m.settableEfforts(), ","), execprofile.Name) {
			t.Errorf("%s does not offer %q, which /effort accepts", name, execprofile.Name)
		}
	}
}

// ...and NOT when the workspace disabled it. Advertising a command the run will
// refuse is worse than saying nothing.
func TestADisabledPostureIsNotOffered(t *testing.T) {
	m := model{modelName: "glm-5.2", zeromaxingDisabled: true}
	if strings.Contains(strings.Join(m.settableEfforts(), ","), execprofile.Name) {
		t.Error("the posture is disabled for this workspace and must not be offered")
	}
	if strings.Contains(m.effortText(), "for the maximal posture") {
		t.Error("the actions line still suggests a command the run will refuse")
	}
}

// With no model selected there is nothing to claim support for, but the posture
// is a Zero-side setting and stays available.
func TestWithNoModelOnlyThePostureIsOffered(t *testing.T) {
	m := model{}
	settable := m.settableEfforts()
	if len(settable) != 1 || settable[0] != execprofile.Name {
		t.Fatalf("settable = %v, want only the posture when no model is selected", settable)
	}
}

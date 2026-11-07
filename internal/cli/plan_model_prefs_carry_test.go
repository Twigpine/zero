package cli

import (
	"reflect"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/specialist"
)

// EVERY CONFIGURED PREFERENCE MUST SURVIVE THE TRANSLATION, and this is checked
// by reflection rather than field by field on purpose.
//
// planModelPreferences is a hand-written copy between two structs that are
// deliberately kept apart, so specialist need not import config. Hand-written
// copies rot silently: a field gets added to both sides and to nothing in the
// middle, and the symptom is a setting the user wrote in their config having no
// effect — with nothing logged, nothing failing, and no way to tell it from the
// feature simply not working. RouterGuidance was added this way; the next one
// will be too.
//
// Naming the fields here would only pin today's set. Walking them means a new
// field is caught the moment it exists.
func TestEveryPlanModelPreferenceSurvivesTheCarryIntoSpecialist(t *testing.T) {
	source := config.PlanModelsConfig{
		Scan:           "scan-model",
		Implement:      "implement-model",
		Verify:         "verify-model",
		Router:         "router-model",
		RouterGuidance: "prefer kimi for judgement",
		AutoAssign:     true,
		Exclude:        []string{"never-this"},
		MinSize:        7,
	}
	carried := planModelPreferences(source)

	from := reflect.ValueOf(source)
	to := reflect.ValueOf(carried)
	toType := to.Type()
	for i := 0; i < from.NumField(); i++ {
		name := from.Type().Field(i).Name
		if _, ok := toType.FieldByName(name); !ok {
			// A config field with no counterpart is a deliberate decision — say so
			// here when you make one, so the next reader knows it was not an
			// oversight. Today every field has one.
			t.Errorf("config field %q has no counterpart in specialist.ModelPreferences; "+
				"if that is intended, exempt it here with a reason", name)
			continue
		}
		want := from.Field(i).Interface()
		got := to.FieldByName(name).Interface()
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s was dropped or altered on the way into specialist: config had %#v, specialist got %#v",
				name, want, got)
		}
	}

	// Guard the guard: a zero source would make every comparison trivially pass,
	// so prove the fixture actually set something.
	if reflect.DeepEqual(carried, specialist.ModelPreferences{}) {
		t.Fatal("the fixture carried nothing, so this test proves nothing")
	}
}

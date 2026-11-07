package tui

import (
	"reflect"
	"testing"
	"time"
)

// EVERY FIELD OF specialistInfo MUST MOVE THE CACHE KEY.
//
// Three of them did not: tokenCount, model and result are all written AFTER the
// card's first render (setTokens, setModel, setResult), so the entry changed,
// the key did not, and the cache kept serving the render made before the values
// arrived — a finished plan task showing no tokens, no model and no output. A
// field that only ever arrives at start() would have hidden the bug; these
// arrive late, which is exactly when a cache key has to move.
//
// The mutator table below is CHECKED AGAINST THE STRUCT BY REFLECTION rather
// than merely written alongside it. specialistInfo's fields are unexported, so
// reflection cannot set them, but it can still enumerate them — which is what
// turns "someone forgot" into a failing test: a new field with no mutator fails
// here, and the fix is to add both a mutator and a fingerprint entry.
func TestSpecialistFingerprintCoversEveryField(t *testing.T) {
	// One entry per field of specialistInfo, each making a change a reader of
	// the card would see.
	mutators := map[string]func(*specialistInfo){
		"name":           func(info *specialistInfo) { info.name = "changed" },
		"description":    func(info *specialistInfo) { info.description = "changed" },
		"childSessionID": func(info *specialistInfo) { info.childSessionID = "changed" },
		"status":         func(info *specialistInfo) { info.status = specialistCancelled },
		"startedAt":      func(info *specialistInfo) { info.startedAt = time.Unix(1, 0) },
		"completedAt":    func(info *specialistInfo) { info.completedAt = time.Unix(1, 0) },
		"exitCode":       func(info *specialistInfo) { info.exitCode = 7 },
		"errorMsg":       func(info *specialistInfo) { info.errorMsg = "changed" },
		"toolCount":      func(info *specialistInfo) { info.toolCount = 7 },
		"tokenCount":     func(info *specialistInfo) { info.tokenCount = 7 },
		"currentTool":    func(info *specialistInfo) { info.currentTool = "changed" },
		"currentDetail":  func(info *specialistInfo) { info.currentDetail = "changed" },
		"model":          func(info *specialistInfo) { info.model = "changed" },
		"background":     func(info *specialistInfo) { info.background = true },
		"result":         func(info *specialistInfo) { info.result = "changed" },
	}

	structType := reflect.TypeOf(specialistInfo{})
	declared := map[string]bool{}
	for i := 0; i < structType.NumField(); i++ {
		declared[structType.Field(i).Name] = true
	}

	for name := range mutators {
		if !declared[name] {
			t.Errorf("mutator for %q, which specialistInfo no longer has: drop it and check the fingerprint still lists only real fields", name)
		}
	}

	baseline := specialistCacheFingerprint(&specialistInfo{})
	for i := 0; i < structType.NumField(); i++ {
		name := structType.Field(i).Name
		t.Run(name, func(t *testing.T) {
			mutate, ok := mutators[name]
			if !ok {
				t.Fatalf("specialistInfo grew field %q with no mutator here: add one, and add the field to specialistCacheFingerprint, or a card that changes through it will keep serving its previous render", name)
			}
			changed := specialistInfo{}
			mutate(&changed)
			if got := specialistCacheFingerprint(&changed); got == baseline {
				t.Fatalf("changing %s left the cache key identical: specialistCacheFingerprint does not read this field, so a card mutated through it never redraws", name)
			}
		})
	}
}

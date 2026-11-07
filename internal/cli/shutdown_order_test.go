package cli

import (
	"os"
	"strings"
	"testing"
)

// SHUTDOWN ORDER, guarded at the source because defers cannot be observed from
// a test.
//
// Required at shutdown: stop the plan, then close the runtime it was using, then
// cancel the session. Defers run LIFO, so registration order is the reverse —
// which is subtle enough that it was already wrong once. planLaunch.Close() was
// registered beside the launcher it belongs to, which reads naturally and meant
// it ran AFTER closeSpecialistRuntime. Close "cancels AND WAITS", so a
// background plan was waited on with the runtime it needs already torn down.
//
// A source-order assertion is a blunt instrument. It is here because the
// alternative is a comment, and a comment did not stop this happening: the
// ordering is invisible at the point where someone would add the next defer.
func TestShutdownDefersAreRegisteredInReverseOfTheirRequiredOrder(t *testing.T) {
	source, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("read app.go: %v", err)
	}
	text := string(source)

	// Registration order, top to bottom.
	positions := map[string]int{
		"cancelSession":          strings.Index(text, "defer cancelSession()"),
		"closeSpecialistRuntime": strings.Index(text, "defer closeSpecialistRuntime(stderr, specialistRuntime)"),
		"planLaunch.Close":       strings.Index(text, "defer planLaunch.Close()"),
	}
	for name, at := range positions {
		if at < 0 {
			t.Fatalf("could not find the %s defer; this guard has gone stale and must be re-pointed, not deleted", name)
		}
	}

	// LIFO: later registration runs earlier. The plan must stop first, so its
	// defer must be registered last.
	if positions["planLaunch.Close"] < positions["closeSpecialistRuntime"] {
		t.Error("planLaunch.Close() is registered before closeSpecialistRuntime, so it RUNS after it — " +
			"a background plan would be waited on with its specialist runtime already closed")
	}
	if positions["closeSpecialistRuntime"] < positions["cancelSession"] {
		t.Error("closeSpecialistRuntime is registered before cancelSession, so it RUNS after it — " +
			"the session would be cancelled while the runtime is still open")
	}
}

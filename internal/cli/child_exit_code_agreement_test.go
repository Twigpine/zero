package cli

import "testing"

// TWO DEFINITIONS OF ONE NUMBER, and this is the only defence against them
// drifting apart.
//
// internal/specialist decides whether to retry a task from the child's exit
// code — a structural signal rather than its prose. It cannot import this
// package to read exitIncomplete, because cli imports specialist and the
// dependency cannot run both ways, so the constant is duplicated there.
//
// If this file's exitIncomplete ever moves and specialist's copy does not, a
// declined task stops being recognised as declined and silently loses its one
// retry — with nothing failing anywhere. That is the exact shape of defect this
// session has produced repeatedly: a value that exists at one layer, is consumed
// at another, and quietly stops agreeing.
func TestTheChildIncompleteExitCodeAgreesWithWhatSpecialistExpects(t *testing.T) {
	// Kept as a literal on purpose. Reading specialist's unexported constant is
	// impossible from here, so this asserts the NUMBER both sides were written
	// against; changing one without the other now fails.
	const specialistChildExitIncomplete = 4
	if exitIncomplete != specialistChildExitIncomplete {
		t.Fatalf("exitIncomplete is %d but internal/specialist retries declines on %d — "+
			"a declined plan task will no longer be recognised, and will silently lose its retry",
			exitIncomplete, specialistChildExitIncomplete)
	}
}

// THE SAME HAZARD, for the same reason, on the provider exit.
//
// specialist retries a plan task ONCE when the child died on the provider
// rather than on the work, and it recognises that from this exit code. If
// exitProvider moves and specialist's childExitProvider does not, a provider
// failure stops being recognised, the task silently loses its retry, and a
// transient 500 goes back to costing the task and everything downstream of it —
// with nothing failing anywhere to say so.
func TestTheChildProviderExitCodeAgreesWithWhatSpecialistExpects(t *testing.T) {
	const specialistChildExitProvider = 3
	if exitProvider != specialistChildExitProvider {
		t.Fatalf("exitProvider is %d but internal/specialist retries provider failures on %d — "+
			"a task killed by a provider error will no longer be retried",
			exitProvider, specialistChildExitProvider)
	}
	// And the two codes must stay DISTINCT: one number meaning both would make
	// every provider failure look like a decline (or the reverse), and the two
	// retries have different safety rules — the provider one refuses write tasks.
	if exitProvider == exitIncomplete {
		t.Fatal("exitProvider and exitIncomplete collided; the two retries would be indistinguishable")
	}
}

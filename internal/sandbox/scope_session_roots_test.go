package sandbox

import (
	"slices"
	"testing"
)

// A TURN-SCOPED WRITE GRANT IS NOT A SESSION ROOT. SessionExtraRoots is what a
// child that may outlive the turn is handed; a temporary root in it would leave
// as a permanent --add-dir the parent's end-of-turn release can never revoke.
func TestSessionExtraRootsOmitsATurnScopedWriteGrant(t *testing.T) {
	scope, temporary := grantOutsideDefaults(t)
	permanent := resolvedFixturePath(t, t.TempDir())
	if _, err := scope.Add(permanent); err != nil {
		t.Fatalf("Add: %v", err)
	}
	root, release, err := scope.AddTemporaryWrite(temporary)
	if err != nil {
		t.Fatalf("AddTemporaryWrite: %v", err)
	}
	defer release()

	if !slices.Contains(scope.ExtraRoots(), root) {
		t.Fatalf("setup: the turn-scoped grant is not live: %v", scope.ExtraRoots())
	}
	session := scope.SessionExtraRoots()
	if slices.Contains(session, root) {
		t.Fatalf("SessionExtraRoots carried the turn-scoped grant %q: %v", root, session)
	}
	if !slices.Contains(session, permanent) {
		t.Fatalf("SessionExtraRoots dropped the permanent root %q: %v", permanent, session)
	}
}

// A temporary grant PROMOTED by a later permanent grant of the same root is
// permanent, and is a session root from then on.
func TestSessionExtraRootsCarriesAPromotedGrant(t *testing.T) {
	scope, granted := grantOutsideDefaults(t)
	root, release, err := scope.AddTemporaryWrite(granted)
	if err != nil {
		t.Fatalf("AddTemporaryWrite: %v", err)
	}
	if _, err := scope.Add(granted); err != nil {
		t.Fatalf("Add: %v", err)
	}
	release()
	if !slices.Contains(scope.SessionExtraRoots(), root) {
		t.Fatalf("a promoted grant is missing from SessionExtraRoots: %v", scope.SessionExtraRoots())
	}
}

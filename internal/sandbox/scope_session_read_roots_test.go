package sandbox

import (
	"slices"
	"testing"
)

// SessionExtraReadRoots carries permanent read and write grants and no
// turn-scoped one of either kind.
func TestSessionExtraReadRootsOmitsTurnScopedGrants(t *testing.T) {
	scope, permanentRead := grantOutsideDefaults(t)
	if _, err := scope.AddRead(permanentRead); err != nil {
		t.Fatalf("AddRead: %v", err)
	}
	permanentWrite := resolvedFixturePath(t, t.TempDir())
	if _, err := scope.Add(permanentWrite); err != nil {
		t.Fatalf("Add: %v", err)
	}
	turnRead, releaseRead, err := scope.AddTemporaryRead(resolvedFixturePath(t, t.TempDir()))
	if err != nil {
		t.Fatalf("AddTemporaryRead: %v", err)
	}
	defer releaseRead()
	turnWrite, releaseWrite, err := scope.AddTemporaryWrite(resolvedFixturePath(t, t.TempDir()))
	if err != nil {
		t.Fatalf("AddTemporaryWrite: %v", err)
	}
	defer releaseWrite()

	live := scope.ExtraReadRoots()
	if !slices.Contains(live, turnRead) || !slices.Contains(live, turnWrite) {
		t.Fatalf("setup: the turn-scoped grants are not live: %v", live)
	}
	session := scope.SessionExtraReadRoots()
	for _, root := range []string{turnRead, turnWrite} {
		if slices.Contains(session, root) {
			t.Errorf("SessionExtraReadRoots carried the turn-scoped grant %q: %v", root, session)
		}
	}
	for _, root := range []string{permanentRead, permanentWrite} {
		if !slices.Contains(session, root) {
			t.Errorf("SessionExtraReadRoots dropped the permanent grant %q: %v", root, session)
		}
	}
}

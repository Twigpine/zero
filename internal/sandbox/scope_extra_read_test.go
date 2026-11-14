package sandbox

import (
	"path/filepath"
	"slices"
	"testing"
)

// THE ANTI-ESCALATION PROPERTY. A read grant (AddRead, where --add-read-dir lands)
// is READABLE but NOT WRITABLE. This is what makes propagating a parent's
// request_permissions read grant to a child safe: the child can audit the granted
// path but cannot modify it. Emitting the grant as a WRITE root (--add-dir) would
// break exactly this.
func TestAReadGrantIsReadableButNotWritable(t *testing.T) {
	scope, granted := grantOutsideDefaults(t)
	root, err := scope.AddRead(granted)
	if err != nil {
		t.Fatalf("AddRead: %v", err)
	}
	target := filepath.Join(root, "audit-me.go")

	if block := scope.validateRead(target); block != nil {
		t.Fatalf("a read grant did not allow READING %q: %v — the audit would fail 'outside the workspace'", target, block)
	}
	if block := scope.validate(target); block == nil {
		t.Fatalf("a read grant ALLOWED WRITING %q — a read grant escalated to write", target)
	}
}

// grantOutsideDefaults returns a scope and a directory that scope does not
// already cover, both under test-owned storage.
//
// THE CAUSE, NOT THE SYMPTOM. NewScope seeds the system temporary directory as a
// permanent write root, so a plain t.TempDir() grant is writable before any
// grant exists and the anti-escalation assertions below prove nothing. The first
// fix placed the fixture under os.UserHomeDir() to find ground the defaults do
// not own. That worked and bought two problems: a hermetic runner may expose
// HOME read-only, so these tests failed at MkdirTemp before asserting anything,
// and on an ordinary machine they wrote into the developer's real home and left
// zero-scope-* debris whenever a run was interrupted. Reported by @jatmn.
//
// Building the Scope directly removes the seeded defaults, which is what made
// HOME necessary in the first place. t.TempDir() is then outside by
// construction, the fixture is hermetic, and nothing touches HOME.
func grantOutsideDefaults(t *testing.T) (*Scope, string) {
	t.Helper()
	workspace := resolvedFixturePath(t, t.TempDir())
	granted := resolvedFixturePath(t, t.TempDir())
	scope := &Scope{workspaceRoot: workspace}
	// PROVED AGAINST THIS SCOPE. The point of the helper is that the grant is
	// what changes access, so the target has to begin unauthorised — asserted
	// here once rather than trusted in every caller.
	if scope.validate(filepath.Join(granted, "probe.txt")) == nil {
		t.Fatalf("%s is writable before any grant exists; the assertions below would prove nothing", granted)
	}
	return scope, granted
}

func TestExtraReadRootsCarriesAReadGrantThatExtraRootsOmits(t *testing.T) {
	scope, granted := grantOutsideDefaults(t)
	readRoot, err := scope.AddRead(granted)
	if err != nil {
		t.Fatalf("AddRead: %v", err)
	}

	if slices.Contains(scope.ExtraRoots(), readRoot) {
		t.Fatalf("ExtraRoots carried the read grant %q", readRoot)
	}
	if !slices.Contains(scope.ExtraReadRoots(), readRoot) {
		t.Fatalf("ExtraReadRoots omitted the read grant %q: %v", readRoot, scope.ExtraReadRoots())
	}
	if slices.Contains(scope.ExtraReadRoots(), scope.WorkspaceRoot()) {
		t.Fatalf("ExtraReadRoots reopened the parent workspace %q", scope.WorkspaceRoot())
	}
}

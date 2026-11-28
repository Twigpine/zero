//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// COMPENSATION MUST HOLD THE OBJECT IT CHANGED, NOT THE NAME IT USED.
//
// The forward apply and its stamp go through one handle, so they are provably
// about one object. Compensation runs later, after a network or marker failure,
// and resolves those names again. Opening no-follow refuses a reparse point but
// accepts an ORDINARY directory moved into the name since the handle closed. So
// a rename plus a substitute made rollback restore the pre-apply DACL onto the
// substitute and report success, while the moved original kept this run's
// capability ACE: a completed rollback with the modified object still reachable
// elsewhere, and a directory mutated that the forward operation never touched.
func TestRollbackRefusesASubstituteAndReportsTheOriginal(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "target")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	const capability = "S-1-5-32-546"
	plan := WindowsACLPlan{Entries: []WindowsACLEntry{
		{Action: WindowsACLAllowWrite, Path: root, Capability: capability},
	}}

	rollback, err := applyWindowsACLPlanWithStamp(plan, nil)
	if err != nil {
		t.Fatalf("applyWindowsACLPlanWithStamp: %v", err)
	}
	if !windowsACLPlanStillApplied(plan) {
		t.Fatal("SETUP INVALID: the grant is not present after a successful apply")
	}

	// Exactly the swap the compensation cannot see by name: move the object that
	// was modified aside, and put an ordinary directory where it was.
	moved := filepath.Join(base, "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Skipf("cannot rename the applied target here: %v", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	substituteBefore := daclOf(t, root)

	err = rollback()
	if err == nil {
		t.Fatal("rollback reported success against a substitute directory")
	}
	if !strings.Contains(err.Error(), root) {
		t.Errorf("the failure does not name the path left in an unrestored state: %v", err)
	}

	// The substitute must be byte-for-byte the directory the test created.
	if after := daclOf(t, root); !equalACEs(after, substituteBefore) {
		t.Errorf("rollback mutated a directory it never modified:\nbefore %v\nafter  %v", substituteBefore, after)
	}

	// And the original is honestly residual: it still carries this run's grant,
	// which is what the error is telling the operator.
	movedPlan := WindowsACLPlan{Entries: []WindowsACLEntry{
		{Action: WindowsACLAllowWrite, Path: moved, Capability: capability},
	}}
	if !windowsACLPlanStillApplied(movedPlan) {
		t.Error("the moved original lost its grant, so the error over-reported what was left behind")
	}
}

// And an unswapped rollback still restores, or the guard would have disabled
// compensation rather than bounding it.
func TestRollbackStillRestoresTheObjectItModified(t *testing.T) {
	root := filepath.Join(t.TempDir(), "target")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	plan := WindowsACLPlan{Entries: []WindowsACLEntry{
		{Action: WindowsACLAllowWrite, Path: root, Capability: "S-1-5-32-546"},
	}}
	before := daclOf(t, root)

	rollback, err := applyWindowsACLPlanWithStamp(plan, nil)
	if err != nil {
		t.Fatalf("applyWindowsACLPlanWithStamp: %v", err)
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback of an unswapped target failed: %v", err)
	}
	if after := daclOf(t, root); !equalACEs(after, before) {
		t.Errorf("rollback did not restore the original DACL:\nbefore %v\nafter  %v", before, after)
	}
}

func equalACEs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

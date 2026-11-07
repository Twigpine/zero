package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// A FAILED SETUP LEAVES NO NEW PERSISTENT STATE.
//
// Runtime roots are materialized before the network plan, the ACL apply, the
// network apply and the marker write. Any of those can fail, and the rollback
// that existed restored only ACL snapshots, so a run that reported failure still
// left new runtime directories behind. It could not have cleaned them up even in
// principle, because provisioning returned nothing about what it had made.
func TestRuntimeRootProvisioningRecordsOnlyWhatItCreated(t *testing.T) {
	base := t.TempDir()
	// A pre-existing ancestor the user owns, and a leaf below it that does not
	// exist yet. Only the latter may be recorded.
	existing := filepath.Join(base, "cache", "zero")
	if err := os.MkdirAll(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(existing, "runtime", "v1", "abc123")

	created, err := createRuntimeDirRecording(target)
	if err != nil {
		t.Fatalf("createRuntimeDirRecording: %v", err)
	}
	if len(created) != 3 {
		t.Fatalf("recorded %v, want the three components below the pre-existing ancestor", created)
	}
	// Outermost first, so the undo walking backwards removes the leaf before its
	// parent and never meets a non-empty directory of its own making.
	want := []string{
		filepath.Join(existing, "runtime"),
		filepath.Join(existing, "runtime", "v1"),
		target,
	}
	for index := range want {
		if created[index].path != want[index] {
			t.Fatalf("recorded %v, want %v", created, want)
		}
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("target was not created: %v", err)
	}

	rollback := windowsRuntimeRootRollback{created: created}
	if err := rollback.run(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := os.Stat(filepath.Join(existing, "runtime")); !os.IsNotExist(err) {
		t.Errorf("rollback left the created tree behind: %v", err)
	}
	// The pre-existing ancestor is not ours and must survive.
	if _, err := os.Stat(existing); err != nil {
		t.Errorf("rollback removed a directory it did not create: %v", err)
	}
}

// Provisioning that finds everything already there records nothing, so a failed
// setup on a machine that was already set up removes none of it.
func TestRuntimeRootProvisioningRecordsNothingWhenAlreadyPresent(t *testing.T) {
	target := filepath.Join(t.TempDir(), "zero", "runtime", "v1", "abc123")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	created, err := createRuntimeDirRecording(target)
	if err != nil {
		t.Fatalf("createRuntimeDirRecording: %v", err)
	}
	if len(created) != 0 {
		t.Fatalf("recorded %v for a tree that already existed", created)
	}
	if err := (windowsRuntimeRootRollback{created: created}).run(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("rollback removed a pre-existing tree: %v", err)
	}
}

// Rollback refuses rather than destroys. A directory that has gained content is
// holding something this run did not create, and RemoveAll there would turn a
// failed setup into data loss.
func TestRuntimeRootRollbackRefusesToRemoveANonEmptyDirectory(t *testing.T) {
	target := filepath.Join(t.TempDir(), "zero", "runtime", "v1", "abc123")
	created, err := createRuntimeDirRecording(target)
	if err != nil {
		t.Fatalf("createRuntimeDirRecording: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "someone-elses.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (windowsRuntimeRootRollback{created: created}).run(); err == nil {
		t.Error("rollback reported success while a non-empty directory remained")
	}
	if _, err := os.Stat(filepath.Join(target, "someone-elses.txt")); err != nil {
		t.Errorf("rollback destroyed content it did not create: %v", err)
	}
}

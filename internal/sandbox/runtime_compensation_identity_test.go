package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// COMPENSATION MUST PROVE IT HOLDS THE OBJECT IT CHANGED.
//
// The forward apply and its stamp go through one handle, so they are provably
// about one object. Compensation runs later, after those handles have closed,
// and used to resolve the pathname again. Rename the original aside, put an
// ordinary directory at the name, and a pathname-only undo strips a stamp from
// the substitute, or writes bytes snapshotted from another object onto it, and
// then removes it as though this run had created it, while the moved original
// keeps this run's grant and stamp.
func TestStampCompensationRefusesAReplacementDirectory(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "runtime-root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	stampPath := windowsSandboxRuntimeStampPath(root, testStampPlanHash)
	if err := os.WriteFile(stampPath, []byte("previous-plan-hash"), 0o600); err != nil {
		t.Fatal(err)
	}

	snapshot, err := snapshotWindowsSandboxRuntimeStamp(root, testStampPlanHash)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.priorState != runtimeStampPresent {
		t.Fatal("SETUP INVALID: the snapshot did not record the pre-existing stamp")
	}

	// The original is moved aside and an ordinary directory takes the name.
	moved := filepath.Join(parent, "moved-aside")
	if err := os.Rename(root, moved); err != nil {
		t.Skipf("cannot rename the runtime root on this filesystem: %v", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	substituteStamp := filepath.Join(root, filepath.Base(stampPath))
	if err := os.WriteFile(substituteStamp, []byte("not-ours"), 0o600); err != nil {
		t.Fatal(err)
	}

	err = snapshot.restore()
	if err == nil {
		t.Fatal("compensation mutated a directory it never touched and reported success")
	}
	for _, want := range []string{"no longer the directory", "replacement"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say what was left behind (%q): %v", want, err)
		}
	}
	if body, readErr := os.ReadFile(substituteStamp); readErr != nil || string(body) != "not-ours" {
		t.Errorf("the substitute's stamp was overwritten: %q %v", string(body), readErr)
	}
	if body, readErr := os.ReadFile(filepath.Join(moved, filepath.Base(stampPath))); readErr != nil || string(body) != "previous-plan-hash" {
		t.Errorf("the original lost its stamp: %q %v", string(body), readErr)
	}
}

// And the created-directory ledger applies the same rule, or the stamp check
// just moves the damage one line down.
func TestCreatedDirectoryCompensationRefusesAReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "created-root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	rollback := windowsRuntimeRootRollback{created: createdRuntimeDirsForTest(root)}

	moved := filepath.Join(parent, "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Skipf("cannot rename on this filesystem: %v", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

	err := rollback.run()
	if err == nil {
		t.Fatal("a substitute directory was removed as though this run had created it")
	}
	if !strings.Contains(err.Error(), "no longer the directory this run created") {
		t.Errorf("the error does not name the replacement: %v", err)
	}
	if _, statErr := os.Stat(root); statErr != nil {
		t.Errorf("the substitute was removed: %v", statErr)
	}
}

// The ordinary case still cleans up, or the guard would be refusing everything.
func TestCompensationStillRemovesWhatThisRunCreated(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "mine")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	rollback := windowsRuntimeRootRollback{created: createdRuntimeDirsForTest(root)}
	if err := rollback.run(); err != nil {
		t.Fatalf("compensation refused a directory it really did create: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("the directory this run created survived compensation: %v", err)
	}
}

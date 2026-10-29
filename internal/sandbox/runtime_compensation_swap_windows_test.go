//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// swapDuringCompensation installs a seam that runs once, between the identity
// check and the mutation, and replaces root with a fresh directory carrying a
// file of its own.
func swapDuringCompensation(t *testing.T, root string, aside string) *string {
	t.Helper()
	fired := false
	previous := runtimeCompensationSwapSeam
	t.Cleanup(func() { runtimeCompensationSwapSeam = previous })
	runtimeCompensationSwapSeam = func() {
		if fired {
			return
		}
		fired = true
		if err := os.Rename(root, aside); err != nil {
			t.Logf("could not rename the original aside: %v", err)
			return
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatalf("create the substitute: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, "substitute.txt"), []byte("belongs to whoever put it here"), 0o600); err != nil {
			t.Fatalf("seed the substitute: %v", err)
		}
	}
	return &aside
}

// A REPLACEMENT AFTER THE CHECK MUST NOT REACH THE SUBSTITUTE.
//
// Compensation used to read the identity through a handle, close it, and then
// resolve the pathname again for the write or the delete. A rename plus a
// replacement in that interval made the comparison true about one object while
// the mutation landed on another, under elevation. Holding the handle across
// both means the mutation follows the object that was verified, and whatever now
// answers to the name is untouched.
func TestStampCompensationDoesNotReachASubstitute(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "zero", "runtime", "v1", "abcd")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime root: %v", err)
	}
	if err := writeWindowsSandboxRuntimeStamp(root, "planhash"); err != nil {
		t.Fatalf("write the stamp: %v", err)
	}
	identity, ok := runtimeDirIdentity(root)
	if !ok {
		t.Fatal("identify the runtime root")
	}
	aside := filepath.Join(parent, "renamed-aside")
	swapDuringCompensation(t, root, aside)

	// Removal of a stamp this run wrote, i.e. the fresh-setup rollback.
	_ = compensateRuntimeStampBound(root, identity, windowsSandboxRuntimeStampName(testStampPlanHash), nil, false)

	// Whatever the outcome, the substitute is not this run's business.
	substitute := filepath.Join(root, "substitute.txt")
	if _, err := os.Stat(substitute); err != nil {
		t.Errorf("compensation removed a file from the substitute directory: %v", err)
	}
	if _, err := os.Stat(windowsSandboxRuntimeStampPath(root, testStampPlanHash)); err == nil {
		t.Error("compensation created a stamp inside the substitute directory")
	}
	// And the original, which is the object that was verified, is the one that
	// lost the stamp this run wrote.
	if _, err := os.Stat(windowsSandboxRuntimeStampPath(aside, testStampPlanHash)); err == nil {
		t.Error("the stamp this run wrote is still on the original object, so compensation followed the name instead")
	}
}

// The same for the delete, where following the name would remove a directory
// this run never created.
func TestDirectoryCompensationDoesNotRemoveASubstitute(t *testing.T) {
	parent := t.TempDir()
	created := filepath.Join(parent, "created")
	if err := os.MkdirAll(created, 0o700); err != nil {
		t.Fatalf("create the directory: %v", err)
	}
	identity, ok := runtimeDirIdentity(created)
	if !ok {
		t.Fatal("identify the created directory")
	}
	aside := filepath.Join(parent, "renamed-aside")
	swapDuringCompensation(t, created, aside)

	_ = removeCreatedRuntimeDirBound(created, identity)

	if _, err := os.Stat(filepath.Join(created, "substitute.txt")); err != nil {
		t.Errorf("compensation removed the substitute directory's contents: %v", err)
	}
}

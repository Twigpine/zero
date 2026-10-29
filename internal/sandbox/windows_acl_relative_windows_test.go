//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// THE SWAP THAT PATHNAMES CANNOT SURVIVE.
//
// This is the race behind the materialization P1. A pathname-based create
// resolves the whole path again at the moment it runs, so an ancestor replaced
// between the verification and the create sends the new directory somewhere else
// entirely. Verifying afterwards is too late: the object already exists in the
// wrong place.
//
// A handle pins the OBJECT, not the name. This test performs the swap for real,
// in the window that used to be exploitable, and asserts the child still lands
// in the directory that was verified.
//
// Worth noting for anyone extending this: the fix is what makes the race
// testable at all. Against the old code the swap had to be threaded into the
// middle of a function; here it is three ordinary lines between an open and a
// create, because the handle is held across them.
func TestChildCreationFollowsTheHandleNotThePath(t *testing.T) {
	root := t.TempDir()
	approved := filepath.Join(root, "approved")
	elsewhere := filepath.Join(root, "elsewhere")
	for _, dir := range []string{approved, elsewhere} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("seed %s: %v", dir, err)
		}
	}

	// Verified once, exactly as setup does before it materializes anything.
	parent, err := openWindowsACLDirectoryNoFollow(approved)
	if err != nil {
		t.Fatalf("open approved directory: %v", err)
	}
	defer func() { _ = windows.CloseHandle(parent) }()

	// THE SWAP, in the window that used to be exploitable: move the verified
	// directory aside and leave a junction to somewhere else wearing its name.
	moved := filepath.Join(root, "approved-moved")
	if err := os.Rename(approved, moved); err != nil {
		t.Skipf("cannot rename a directory with an open handle on this filesystem: %v", err)
	}
	makeJunction(t, approved, elsewhere)

	// The create resolves against the handle, so it must ignore the junction now
	// sitting at the original pathname.
	child, created, err := createWindowsACLChildDirectory(parent, "materialized")
	if err != nil {
		t.Fatalf("create child relative to the pinned handle: %v", err)
	}
	defer func() { _ = windows.CloseHandle(child) }()
	if !created {
		t.Error("created = false for a directory that did not exist")
	}

	if _, err := os.Stat(filepath.Join(moved, "materialized")); err != nil {
		t.Errorf("the child did not land in the verified directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "materialized")); err == nil {
		t.Fatal("ESCAPED: the child was created through the junction, outside the approved tree")
	}
}

// Materialization runs on trees that may already be half-built, so creating an
// existing directory has to be a no-op rather than a failure. created must still
// report the truth, because rollback deletes only what this call made.
func TestCreatingAnExistingChildOpensItInstead(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "already"), 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}
	parent, err := openWindowsACLDirectoryNoFollow(root)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer func() { _ = windows.CloseHandle(parent) }()

	handle, created, err := createWindowsACLChildDirectory(parent, "already")
	if err != nil {
		t.Fatalf("open existing child: %v", err)
	}
	_ = windows.CloseHandle(handle)
	if created {
		t.Error("created = true for a directory that already existed; rollback would delete a user's data")
	}
}

// The rollback counterpart. os.RemoveAll on a pathname whose ancestor has since
// become a junction recurses outside the workspace and deletes unrelated trees,
// which is the second P1. Resolving relative to the parent handle cannot.
func TestDeleteFollowsTheHandleNotThePath(t *testing.T) {
	root := t.TempDir()
	approved := filepath.Join(root, "approved")
	elsewhere := filepath.Join(root, "elsewhere")
	for _, dir := range []string{approved, elsewhere} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("seed %s: %v", dir, err)
		}
	}
	// A bystander under the decoy: if the delete ever resolves by pathname it is
	// reachable, and its survival is what proves the delete did not.
	if err := os.Mkdir(filepath.Join(elsewhere, "victim"), 0o700); err != nil {
		t.Fatalf("seed victim: %v", err)
	}
	if err := os.Mkdir(filepath.Join(approved, "victim"), 0o700); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	parent, err := openWindowsACLDirectoryNoFollow(approved)
	if err != nil {
		t.Fatalf("open approved directory: %v", err)
	}
	defer func() { _ = windows.CloseHandle(parent) }()

	moved := filepath.Join(root, "approved-moved")
	if err := os.Rename(approved, moved); err != nil {
		t.Skipf("cannot rename a directory with an open handle on this filesystem: %v", err)
	}
	makeJunction(t, approved, elsewhere)

	if err := deleteWindowsACLChildDirectory(parent, "victim"); err != nil {
		t.Fatalf("delete relative to the pinned handle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "victim")); err != nil {
		t.Fatal("DESTRUCTIVE: rollback followed the junction and deleted a directory outside the approved tree")
	}
	if _, err := os.Stat(filepath.Join(moved, "victim")); err == nil {
		t.Error("the directory inside the approved tree was not removed")
	}
}

// A delete that cannot happen must SAY so.
//
// This is the gap that let a silent bug ship in the first version of this file.
// It deleted with FILE_DELETE_ON_CLOSE, which defers the removal to cleanup,
// where a non-empty directory makes it fail with nowhere to report it: the open
// returned success, the close returned success, and the directory was still
// there. The only test covering deletion used an EMPTY directory, so it passed
// throughout. Rollback built on that would have reported success while leaving
// materialized state on disk, which is worse than the os.RemoveAll it replaced,
// because os.RemoveAll actually removed it.
func TestDeletingANonEmptyDirectoryIsReported(t *testing.T) {
	root := t.TempDir()
	populated := filepath.Join(root, "populated")
	if err := os.Mkdir(populated, 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(populated, "occupant"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed occupant: %v", err)
	}
	parent, err := openWindowsACLDirectoryNoFollow(root)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer func() { _ = windows.CloseHandle(parent) }()

	err = deleteWindowsACLChildDirectory(parent, "populated")
	if _, statErr := os.Stat(populated); statErr != nil {
		t.Fatalf("the directory was removed with its contents, which this delete must never do: %v", statErr)
	}
	if err == nil {
		t.Fatal("reported success while leaving the directory in place")
	}
}

// The directory form must refuse a file rather than delete it, and the file form
// must handle the one materialized target that is a file.
func TestDeleteDistinguishesFilesFromDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "config"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	parent, err := openWindowsACLDirectoryNoFollow(root)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer func() { _ = windows.CloseHandle(parent) }()

	if err := deleteWindowsACLChildDirectory(parent, "config"); err == nil {
		t.Error("the directory delete accepted a file")
	}
	if err := deleteWindowsACLChildFile(parent, "config"); err != nil {
		t.Fatalf("deleteWindowsACLChildFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "config")); err == nil {
		t.Error("the file survived its delete")
	}
}

// Rollback runs on failure paths where the object may never have been created,
// so a missing child is success rather than an error to report.
func TestDeletingAMissingChildIsNotAnError(t *testing.T) {
	root := t.TempDir()
	parent, err := openWindowsACLDirectoryNoFollow(root)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer func() { _ = windows.CloseHandle(parent) }()

	if err := deleteWindowsACLChildDirectory(parent, "never-existed"); err != nil {
		t.Fatalf("deleting a missing child reported an error: %v", err)
	}
}

// The anchor open is the one pathname resolution in the walk, so it has to
// refuse a junction itself rather than leaving it to a later check.
func TestOpeningAJunctionAnchorIsRefused(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}
	link := filepath.Join(root, "link")
	makeJunction(t, link, real)

	handle, err := openWindowsACLDirectoryNoFollow(link)
	if err == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("opened a junction as the materialization anchor; every create beneath it would land outside the approved tree")
	}
}

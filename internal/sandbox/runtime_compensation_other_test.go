//go:build !windows

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ONLY ABSENCE IS NOTHING TO UNDO.
//
// removeCreatedRuntimeDirBound returned success on any failure to identify the
// directory, so one that was still there, but could not be looked at, was
// reported as removed. Here the parent loses search permission, which makes the
// directory exist and be unidentifiable at the same time.
func TestRemovingAnUnidentifiableCreatedDirectoryIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory search permission, so the lookup cannot be made to fail")
	}
	parent := filepath.Join(t.TempDir(), "zero")
	created := filepath.Join(parent, "runtime")
	if err := os.MkdirAll(created, 0o700); err != nil {
		t.Fatalf("create the directory: %v", err)
	}
	identity, ok := runtimeDirIdentity(created)
	if !ok {
		t.Fatal("identify the created directory")
	}
	if err := os.Chmod(parent, 0o600); err != nil {
		t.Fatalf("withdraw search permission: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	if _, err := os.Lstat(created); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SETUP INVALID: the directory should exist and be unidentifiable, Lstat said %v", err)
	}

	err := removeCreatedRuntimeDirBound(created, identity)
	if err == nil {
		t.Fatal("a created directory that could not be identified was reported as removed")
	}
	if !strings.Contains(err.Error(), "could not be identified") {
		t.Errorf("the error does not say why the directory was left: %v", err)
	}

	// And it is still there, which is why the error is owed.
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatalf("restore search permission: %v", err)
	}
	if _, err := os.Lstat(created); err != nil {
		t.Fatalf("the directory was gone after all, so the error was not owed: %v", err)
	}
}

// Absence stays a clean undo, or the test above would be satisfied by a function
// that reports every lookup failure.
func TestRemovingACreatedDirectoryThatIsAlreadyGoneSucceeds(t *testing.T) {
	created := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(created, 0o700); err != nil {
		t.Fatalf("create the directory: %v", err)
	}
	identity, ok := runtimeDirIdentity(created)
	if !ok {
		t.Fatal("identify the created directory")
	}
	if err := os.Remove(created); err != nil {
		t.Fatalf("remove the directory: %v", err)
	}
	if err := removeCreatedRuntimeDirBound(created, identity); err != nil {
		t.Fatalf("a created directory that is already gone was reported as a failed undo: %v", err)
	}
}

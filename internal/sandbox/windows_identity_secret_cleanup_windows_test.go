//go:build windows

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// CLEANUP HAS TO UNDO WHAT CREATE DID, ON THE SAME OBJECT.
//
// Everything between creating the secret file and writing it is handle-relative
// and no-follow. The failure path was not: it called os.Remove on the pathname,
// which is the one resolution the rest of the function exists to avoid, and it
// did not even work. The leaf is created with FILE_SHARE_READ alone, so a delete
// by name while that handle is open is a sharing violation; the error was
// discarded, the directory unwind did not cover the file, and a failed
// provisioning left an empty secret behind that a later run would read as a
// stale password.
func TestWriteWindowsSandboxSecretRemovesTheLeafWhenLockingFails(t *testing.T) {
	restore := failWindowsSecretLock(t, errors.New("SetSecurityInfo refused"))
	defer restore()

	path := filepath.Join(t.TempDir(), "cfg", "zero-sbx-test.secret")
	err := writeWindowsSandboxSecret(path, "Zs1!EXAMPLEPASSWORDVALUE")

	if err == nil {
		t.Fatal("SETUP INVALID: the injected lock failure was not reported")
	}
	if !strings.Contains(err.Error(), "SetSecurityInfo refused") {
		t.Fatalf("err = %v, want the injected cause", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("the secret file survived a failed lock (stat: %v); a later run would read it as a stale password", statErr)
	}
	// The directory this attempt created goes too, which is the half that
	// already worked and must keep working.
	if _, statErr := os.Stat(filepath.Dir(path)); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the directory chain this attempt created was left behind (stat: %v)", statErr)
	}
}

// AND A CLEANUP THAT CANNOT FINISH SAYS SO. Leftover state after a failed
// provisioning is the caller's business, and an empty secret file is exactly
// what a later run would mistake for a stored password.
//
// The delete is injected rather than blocked for real. Making a Windows delete
// fail means arranging a sharing conflict, and my first attempt at that held a
// second handle on the leaf and did not block anything: the cleanup succeeded
// and the test asserted its way to a false negative. A seam says what the code
// does with a failure, which is the thing under test here.
func TestWriteWindowsSandboxSecretReportsAFailedCleanup(t *testing.T) {
	restoreLock := failWindowsSecretLock(t, errors.New("SetSecurityInfo refused"))
	defer restoreLock()

	previousDelete := deleteWindowsSecretLeafFn
	deleteWindowsSecretLeafFn = func(windows.Handle, string) error {
		return errors.New("STATUS_CANNOT_DELETE")
	}
	defer func() { deleteWindowsSecretLeafFn = previousDelete }()

	path := filepath.Join(t.TempDir(), "cfg", "zero-sbx-test.secret")
	err := writeWindowsSandboxSecret(path, "Zs1!EXAMPLEPASSWORDVALUE")

	if err == nil {
		t.Fatal("SETUP INVALID: the injected lock failure was not reported")
	}
	if !strings.Contains(err.Error(), "SetSecurityInfo refused") {
		t.Errorf("err = %v, want the original cause preserved", err)
	}
	if !strings.Contains(err.Error(), "remove partially written secret") {
		t.Errorf("err = %v, want the failed cleanup reported rather than discarded", err)
	}
	if !strings.Contains(err.Error(), "STATUS_CANNOT_DELETE") {
		t.Errorf("err = %v, want the delete failure carried too", err)
	}
}

// The ordinary path is unchanged, or the two refusals above would prove only
// that this function fails.
func TestWriteWindowsSandboxSecretStillStoresAndReadsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cfg", "zero-sbx-test.secret")
	const password = "Zs1!EXAMPLEPASSWORDVALUE"
	if err := writeWindowsSandboxSecret(path, password); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readWindowsSandboxSecret(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != password {
		t.Fatalf("read %q, want the stored password", got)
	}
}

// failWindowsSecretLock makes the DACL step fail for one test.
func failWindowsSecretLock(t *testing.T, cause error) func() {
	t.Helper()
	previous := lockWindowsSecretHandleToOwnerFn
	lockWindowsSecretHandleToOwnerFn = func(windows.Handle, *windows.SID) error { return cause }
	return func() { lockWindowsSecretHandleToOwnerFn = previous }
}

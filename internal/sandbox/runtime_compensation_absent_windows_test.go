//go:build windows

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ABSENCE IS CLASSIFIED IN THE PRODUCER'S DOMAIN.
//
// The stamp is opened through NtCreateFile, so a stamp that is not there comes
// back as STATUS_OBJECT_NAME_NOT_FOUND. Compensation compared it with the Win32
// codes only. An NTStatus is a different type and errors.Is does not convert it,
// so removing a stamp that never existed was reported as a rollback failure.
//
// Native, against real directories: the question is what the API returns at this
// boundary, and a synthetic error value would only test the comparison.

func runtimeRootForCompensation(t *testing.T) (string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "zero", "runtime", "v1", "abcd")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime root: %v", err)
	}
	identity, ok := runtimeDirIdentity(root)
	if !ok {
		t.Fatal("identify the runtime root")
	}
	return root, identity
}

// A runtime directory that exists and holds no stamp: removal has nothing to do
// and says so by succeeding.
func TestRemovingAStampThatIsNotThereSucceeds(t *testing.T) {
	root, identity := runtimeRootForCompensation(t)
	name := windowsSandboxRuntimeStampName(testStampPlanHash)

	if err := compensateRuntimeStampBound(root, identity, name, nil, false); err != nil {
		t.Fatalf("removing a stamp that was never written reported a failure: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("the runtime directory itself was disturbed: %v", err)
	}
}

// A fresh setup that fails BEFORE its stamp is written, composed the way the
// setup transaction composes it: a real snapshot of the untouched tree, and the
// compensation run with the original failure as its cause. The cause has to come
// back alone. A second error about the stamp sends the operator looking for
// residue that does not exist.
func TestAFreshSetupFailureBeforeTheStampReportsOnlyItsCause(t *testing.T) {
	root, _ := runtimeRootForCompensation(t)
	snapshot, err := snapshotWindowsSandboxRuntimeStamp(root, testStampPlanHash)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.priorState != runtimeStampAbsent || !snapshot.rootIdentified {
		t.Fatalf("SETUP INVALID: the snapshot did not record an identified root with no stamp: %+v", snapshot)
	}

	cause := errors.New("windows ACL target does not exist: an earlier entry in the plan")
	reported := runWindowsSandboxSetupCompensations(cause, nil, windowsRuntimeRootRollback{stamp: snapshot})

	if reported == nil || !errors.Is(reported, cause) {
		t.Fatalf("the original failure is no longer visible: %v", reported)
	}
	if text := reported.Error(); strings.Contains(text, "rollback failed") || strings.Contains(text, "setup stamp") {
		t.Errorf("compensation added a failure of its own for a stamp that never existed:\n%s", text)
	}
}

// AND ONLY ABSENCE. Something at the stamp's name that cannot be opened as the
// stamp is not "already removed", and treating every failed open as success
// would report a clean undo over residue nobody looked at. A directory at that
// name is a failure the real API produces on demand.
func TestAnUninspectableStampIsStillReported(t *testing.T) {
	root, identity := runtimeRootForCompensation(t)
	name := windowsSandboxRuntimeStampName(testStampPlanHash)
	if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
		t.Fatalf("put a directory where the stamp would be: %v", err)
	}

	err := compensateRuntimeStampBound(root, identity, name, nil, false)
	if err == nil {
		t.Fatal("compensation reported success over something it could not open as the stamp")
	}
	if isWindowsNotFound(err) {
		t.Errorf("the failure classifies as not-found, so it would be swallowed: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, name)); statErr != nil {
		t.Errorf("the object it could not inspect was removed anyway: %v", statErr)
	}
}

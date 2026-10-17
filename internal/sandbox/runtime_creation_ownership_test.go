package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CREATION IS THE OWNERSHIP BOUNDARY, NOT THE STEP AFTER IT.
//
// Acquisition learns it created a directory or a lease file at the create
// itself, but used to publish that fact only once every later step had also
// succeeded: the identity read for a directory, and the inspection, wrapping and
// locking for a lease. A failure in between returned an error while leaving the
// object on disk with nothing above holding a record of it. Setup then reported
// failure and left invocation-owned state behind, and where an ancestor HAD been
// recorded, that ancestor's compensation failed afterwards on a child it could
// not account for.
//
// Both platforms are driven through acquireRuntimeLeaseForPlatform, the call
// production makes, rather than through either descent directly.

var errInjectedRuntimeFailure = errors.New("injected failure after the create")

// failRuntimeCreationAt makes the step after the creation of the named object
// fail, which is the only way to reach the paths under test.
func failRuntimeCreationAt(t *testing.T, match string) {
	t.Helper()
	runtimeCreationFailure = func(created string) error {
		if filepath.Base(created) == match {
			return errInjectedRuntimeFailure
		}
		return nil
	}
	t.Cleanup(func() { runtimeCreationFailure = nil })
}

// runtimeRootForAcquisition returns a root whose owned tail does not exist yet,
// so acquisition has to create it.
func runtimeRootForAcquisition(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "zero", "runtime"), 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(base, "zero", "runtime", "v1", "digest")
}

// A directory whose identity could not be read is removed, not abandoned.
func TestAcquisitionUndoesADirectoryItCreatedButCouldNotIdentify(t *testing.T) {
	root := runtimeRootForAcquisition(t)
	version := filepath.Dir(root)
	failRuntimeCreationAt(t, filepath.Base(version))

	lease, created, err := acquireRuntimeLeaseForPlatform(root)
	if err == nil {
		lease.release()
		t.Fatal("SETUP INVALID: acquisition succeeded, so the injected failure never fired")
	}
	if !errors.Is(err, errInjectedRuntimeFailure) {
		t.Fatalf("acquisition failed for some other reason: %v", err)
	}
	if len(created) != 0 {
		t.Errorf("acquisition returned a ledger for a failed create: %v", created)
	}
	if _, statErr := os.Stat(version); !os.IsNotExist(statErr) {
		t.Errorf("%s was created by this run and left behind after the failure, with nothing recording it (stat: %v)", version, statErr)
	}
	// And what was already there is untouched.
	if _, statErr := os.Stat(filepath.Dir(version)); statErr != nil {
		t.Errorf("a directory that existed before the run was removed: %v", statErr)
	}
}

// So is a lease file this run created but never got to lock.
func TestAcquisitionUndoesALeaseFileItCreatedButCouldNotLock(t *testing.T) {
	root := runtimeRootForAcquisition(t)
	leasePath := sandboxRuntimeLeasePath(root)
	failRuntimeCreationAt(t, filepath.Base(leasePath))

	lease, _, err := acquireRuntimeLeaseForPlatform(root)
	if err == nil {
		lease.release()
		t.Fatal("SETUP INVALID: acquisition succeeded, so the injected failure never fired")
	}
	if !errors.Is(err, errInjectedRuntimeFailure) {
		t.Fatalf("acquisition failed for some other reason: %v", err)
	}
	if _, statErr := os.Stat(leasePath); !os.IsNotExist(statErr) {
		t.Errorf("%s was created by this run and left behind after the failure, so its directory can never be compensated (stat: %v)", leasePath, statErr)
	}
}

// A LEASE FILE THAT WAS ALREADY THERE IS SOMEBODY ELSE'S.
//
// The undo is keyed on this call having created the object, so an acquisition
// that merely opened an existing lease must not remove it on its way out.
func TestAcquisitionLeavesALeaseFileItDidNotCreate(t *testing.T) {
	root := runtimeRootForAcquisition(t)
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	leasePath := sandboxRuntimeLeasePath(root)
	if err := os.WriteFile(leasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	failRuntimeCreationAt(t, filepath.Base(leasePath))

	lease, _, err := acquireRuntimeLeaseForPlatform(root)
	if err == nil {
		lease.release()
		t.Fatal("SETUP INVALID: acquisition succeeded, so the injected failure never fired")
	}
	if _, statErr := os.Stat(leasePath); statErr != nil {
		t.Errorf("acquisition removed a lease file it did not create: %v", statErr)
	}
}

// AND THE LEDGER STILL COVERS WHAT SURVIVES A LATER FAILURE.
//
// Undoing the component that failed must not undo the ones before it: those are
// recorded, and compensation is what removes them.
func TestAcquisitionKeepsTheLedgerForComponentsItFinished(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "zero", "runtime", "v1", "digest")
	failRuntimeCreationAt(t, "v1")

	lease, created, err := acquireRuntimeLeaseForPlatform(root)
	if err == nil {
		lease.release()
		t.Fatal("SETUP INVALID: acquisition succeeded, so the injected failure never fired")
	}
	var recorded []string
	for _, entry := range created {
		recorded = append(recorded, filepath.Base(entry.path))
	}
	// "zero" and "runtime" were created and identified before the injected
	// failure, so they belong to the caller to compensate.
	if len(recorded) != 2 || recorded[0] != "zero" || recorded[1] != "runtime" {
		t.Errorf("the completed components are not on the ledger the caller compensates from: %v", recorded)
	}
	if _, statErr := os.Stat(filepath.Join(base, "zero", "runtime")); statErr != nil {
		t.Errorf("a component that was created and recorded was removed anyway: %v", statErr)
	}
	if failed := filepath.Join(base, "zero", "runtime", "v1"); !isMissing(failed) {
		t.Errorf("%s failed after being created and was left behind", failed)
	}
	if !strings.Contains(err.Error(), "v1") {
		t.Errorf("the error does not name the component that failed: %v", err)
	}
}

func isMissing(path string) bool {
	_, err := os.Stat(path)
	return os.IsNotExist(err)
}

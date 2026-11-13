package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runtimeTreeAwaitingCompensation is what a setup that failed after taking its
// lease leaves behind: the owned tail, the lease file beside the leaf, and the
// ledger of directories this run created.
func runtimeTreeAwaitingCompensation(t *testing.T) (root string, created []windowsCreatedRuntimeDir, leasePath string) {
	t.Helper()
	root = runtimeRootUnderTest(t, "digest")
	leasePath = sandboxRuntimeLeasePath(root)
	if err := os.WriteFile(leasePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	version := filepath.Dir(root)
	runtime := filepath.Dir(version)
	zero := filepath.Dir(runtime)
	// Outermost first, the order the descent records them in.
	created = createdRuntimeDirsForTest(zero, runtime, version, root)
	return root, created, leasePath
}

// AN IN-USE ROOT IS LEFT ALONE, WHICH IS WHAT THE MESSAGE ALREADY PROMISED.
//
// Compensation reported "in use by another process, so the lease and the tree it
// protects were left in place" and then ran the directory walk anyway on the very
// next statement. The promise was in the error string and nowhere else: a
// contender holding the shared lease had the root deleted out from under it, and
// the operator was told it had not been.
func TestCompensationLeavesTheTreeWhenACommandStillHoldsTheRuntimeRoot(t *testing.T) {
	root, created, leasePath := runtimeTreeAwaitingCompensation(t)

	holder, _, err := acquireRuntimeLeaseForPlatform(root)
	if err != nil {
		t.Fatalf("SETUP INVALID: could not take the shared lease that stands in for a running command: %v", err)
	}
	defer holder.release()

	err = undoWindowsSetupRuntimeCreation(root, created, leasePath, true)
	if err == nil {
		t.Fatal("compensation reported success while another process held the runtime root")
	}
	if !strings.Contains(err.Error(), "in use") {
		t.Fatalf("compensation failed for some other reason than the root being in use: %v", err)
	}
	for _, path := range []string{root, leasePath} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Errorf("%s was removed while another process held the runtime root, though the error says it was left in place: %v", path, statErr)
		}
	}
}

// AND EXCLUSIVITY OUTLIVES THE MUTATIONS THAT DEPEND ON IT.
//
// The lease used to be released before the pathname removal and before the
// directory walk, so a command blocked on the shared lease was let in while
// cleanup was still deleting the tree that lease is supposed to protect. It could
// take the old lease object as its own, cleanup would unlink the name from under
// it, and a later cleanup locking a fresh object at that pathname would conclude
// the root was free while the command was still using it.
func TestCompensationKeepsExclusivityUntilItIsDone(t *testing.T) {
	root, _, leasePath := runtimeTreeAwaitingCompensation(t)
	// Only the leaf is this run's here, so the contender's own descent handle on
	// the version directory is not something compensation tries to remove out
	// from under it. That is a real race, not a test artifact, but it is a
	// different one from the exclusion this test is about. It also puts the lease
	// under a parent that was already there, which is the branch that removes the
	// lease after the walk instead of during it.
	created := createdRuntimeDirsForTest(root)

	entered := make(chan struct{})
	var contender *sandboxRuntimeLease
	barrierRan := false
	runtimeCleanupExclusivityBarrier = func() {
		barrierRan = true
		go func() {
			defer close(entered)
			if lease, _, err := acquireRuntimeLeaseForPlatform(root); err == nil {
				contender = lease
			}
		}()
		select {
		case <-entered:
			t.Error("a command took the runtime lease while compensation was still removing the tree that lease protects")
		case <-time.After(300 * time.Millisecond):
		}
	}
	t.Cleanup(func() { runtimeCleanupExclusivityBarrier = nil })

	if err := undoWindowsSetupRuntimeCreation(root, created, leasePath, true); err != nil {
		t.Fatalf("compensation failed: %v", err)
	}
	if !barrierRan {
		t.Fatal("SETUP INVALID: compensation never reached the point where it holds the cleanup lease")
	}
	<-entered
	if contender != nil {
		contender.release()
	}
}

// THE LEASE GOES JUST BEFORE THE DIRECTORY THAT CONTAINS IT.
//
// Not first, which ends the exclusion everything after it depends on, and not
// last, which leaves its parent non-empty so the parent can never be removed.
// Nothing this run created may survive either way.
func TestCompensationRemovesTheLeaseAndEveryDirectoryItCreated(t *testing.T) {
	root, created, leasePath := runtimeTreeAwaitingCompensation(t)

	if err := undoWindowsSetupRuntimeCreation(root, created, leasePath, true); err != nil {
		t.Fatalf("compensation failed: %v", err)
	}
	for _, path := range []string{leasePath, root, filepath.Dir(root), filepath.Dir(filepath.Dir(root))} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived compensation (stat: %v)", path, err)
		}
	}
}

// A lease this run did not create is not this run's to remove.
//
// The directory holding it therefore cannot come back either, and compensation
// says so rather than deleting somebody else's coordination file to tidy up. The
// root this run did create still goes.
func TestCompensationLeavesALeaseThisRunDidNotCreate(t *testing.T) {
	root, created, leasePath := runtimeTreeAwaitingCompensation(t)

	err := undoWindowsSetupRuntimeCreation(root, created, leasePath, false)
	if err == nil {
		t.Fatal("compensation reported it removed everything, but another process's lease file is still in the directory it claims to have removed")
	}
	if !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("compensation failed for some other reason than the lease file it may not remove: %v", err)
	}
	if _, statErr := os.Stat(leasePath); statErr != nil {
		t.Errorf("compensation removed a lease file this run did not create: %v", statErr)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Errorf("the runtime root this run created survived compensation (stat: %v)", statErr)
	}
}

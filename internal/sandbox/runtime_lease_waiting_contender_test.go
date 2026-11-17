package sandbox

import (
	"os"
	"testing"
)

// A CONTENDER THAT WAITED OUT A CLEANUP HOLDS THE CURRENT LEASE, NOT THE OLD ONE.
//
// The schedule the exclusivity test does not reach. A command opens the lease
// while cleanup holds it exclusively and waits for the shared lock. Cleanup
// removes the lease name and the tree before it lets go. The waiter then wakes on
// the old object: unlinked on POSIX, delete-pending on Windows. Locking it
// succeeds, and the acquisition used to return that as a lease. Nothing else
// could ever see it: the next cleanup created a fresh lease at the name, locked
// it unopposed, and reported the root unused while the command was running in
// it. The two locks each succeeded and coordinated nothing.
//
// Both seams are used as barriers, not as failures. The cleanup barrier is
// where the contender is started, so its open lands while cleanup owns the lease;
// the creation seam is installed there too, rather than up front, because
// cleanup's own open consults it on POSIX and would otherwise signal before the
// contender exists. Reported by @gnanam1990.
func TestAContenderThatWaitedOutACleanupHoldsTheCurrentLease(t *testing.T) {
	root, _, leasePath := runtimeTreeAwaitingCompensation(t)
	// Only the leaf is this run's, the same shape as the exclusivity test: the
	// contender's retained parent handle must survive compensation, which is a
	// different race from the one under test.
	created := createdRuntimeDirsForTest(root)

	opened := make(chan struct{})
	entered := make(chan struct{})
	var contender *sandboxRuntimeLease
	var contenderErr error
	runtimeCleanupExclusivityBarrier = func() {
		runtimeCreationFailure = func(string) error {
			select {
			case <-opened:
			default:
				close(opened)
			}
			return nil
		}
		go func() {
			defer close(entered)
			contender, _, contenderErr = acquireRuntimeLeaseForPlatform(root)
		}()
		// The contender now holds a handle on the lease cleanup is about to
		// remove, and is waiting for the lock cleanup holds.
		<-opened
	}
	t.Cleanup(func() {
		runtimeCleanupExclusivityBarrier = nil
		runtimeCreationFailure = nil
	})

	if err := undoWindowsSetupRuntimeCreation(root, created, leasePath, true); err != nil {
		t.Fatalf("compensation failed: %v", err)
	}
	<-entered
	runtimeCreationFailure = nil
	if contenderErr != nil {
		t.Fatalf("the contender could not take the lease after cleanup finished: %v", contenderErr)
	}
	if contender == nil {
		t.Fatal("SETUP INVALID: the contender returned neither a lease nor an error")
	}
	defer contender.release()

	// The proof is the next cleanup: if the contender holds the current object,
	// cleanup finds the root in use. If it holds the object cleanup already
	// removed, cleanup locks a fresh one and calls the root free.
	second, inUse, err := tryAcquireSandboxRuntimeCleanupLease(root)
	if err != nil {
		t.Fatalf("a second cleanup could not take the lease at all, so the contender is holding an object the name no longer resolves to: %v", err)
	}
	if !inUse {
		second.release()
		t.Fatal("a second cleanup found the root unused while the contender holds a lease on it: the contender locked the object cleanup had already removed")
	}
	if _, statErr := os.Stat(leasePath); statErr != nil {
		t.Errorf("the contender's lease is not at the lease name: %v", statErr)
	}

	// CONTROL: once the contender lets go, the root really is free.
	contender.release()
	third, inUse, err := tryAcquireSandboxRuntimeCleanupLease(root)
	if err != nil {
		t.Fatalf("cleanup after the contender released: %v", err)
	}
	if inUse {
		t.Fatal("the root is still reported in use after the contender released")
	}
	third.release()
}

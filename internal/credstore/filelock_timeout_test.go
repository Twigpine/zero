package credstore

import (
	"strings"
	"testing"
	"time"
)

// The credential lock and the provider config/key transaction lock are taken by
// the same operations, so a wedged holder must fail the same way through both.
// Before the deadline this call blocked forever with nothing on screen.
func TestFileLockReportsBusyInsteadOfBlockingForever(t *testing.T) {
	dir := t.TempDir()
	store := fileStore(t, dir)

	original := credentialLockTimeout
	credentialLockTimeout = 50 * time.Millisecond
	t.Cleanup(func() { credentialLockTimeout = original })

	release, err := store.acquireFileLock(true)
	if err != nil {
		t.Fatalf("acquireFileLock: %v", err)
	}
	defer func() {
		if err := release(); err != nil {
			t.Fatalf("release: %v", err)
		}
	}()

	started := time.Now()
	done := make(chan error, 1)
	go func() {
		contender, err := store.acquireFileLock(true)
		if err == nil {
			_ = contender()
		}
		done <- err
	}()

	// The bound is generous against a loaded CI box but far below anything that
	// would pass while still ignoring credentialLockTimeout.
	const bound = time.Second
	select {
	case err := <-done:
		elapsed := time.Since(started)
		if elapsed < credentialLockTimeout {
			t.Fatalf("contending acquireFileLock returned after %s, before credentialLockTimeout %s", elapsed, credentialLockTimeout)
		}
		if elapsed > bound {
			t.Fatalf("contending acquireFileLock took %s, want it bounded by %s", elapsed, bound)
		}
		if err == nil || !strings.Contains(err.Error(), "credential store is busy") {
			t.Fatalf("contending acquireFileLock = %v, want a busy error", err)
		}
		if !strings.Contains(err.Error(), store.lockPath()) {
			t.Fatalf("busy error %v does not name the lock file %q", err, store.lockPath())
		}
	case <-time.After(bound):
		t.Fatal("contending acquireFileLock did not return within the bound")
	}
}

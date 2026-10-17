package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

// runtimeRootUnderTest builds a root with the shape production uses, so the
// components refuseAliasedRuntimeComponents walks stay inside test-owned
// storage.
//
// Owned depth is the fixed names plus the digest, so a root placed directly in
// t.TempDir() puts an owned component on /tmp, which the Unix ownership guard
// correctly refuses because /tmp belongs to root. That is the guard working, not
// a test environment problem, and it only shows up off Windows.
func runtimeRootUnderTest(t *testing.T, leaf string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "zero", "runtime", "v1", leaf)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

// A LEASE IS OWNERSHIP FOR A TRANSACTION, NOT A SELECTION PROBE.
//
// Setup took a lease only to learn which root won and released it at once, so
// nothing owned the selected root while the elevated helper provisioned the
// tree, applied the ACL and stamp, installed network state and wrote the marker.
// A command for another workspace scans the same runtime parent and excludes
// only its own current root, so it can take this root's cleanup lease and
// RemoveAll it mid-transaction; setup then publishes success for a pathname that
// is gone.
//
// This pins the mechanism the fix relies on: a held lease is what makes the
// cleanup's exclusive acquire fail.
func TestAHeldRuntimeLeaseStopsConcurrentCleanup(t *testing.T) {
	root := runtimeRootUnderTest(t, "deadbeefdeadbeef")

	lease, err := prepareSandboxRuntimeLease(root)
	if err != nil {
		t.Fatalf("acquire the runtime lease: %v", err)
	}
	removeSandboxRuntimeRootIfUnused(root)
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("cleanup removed a root that setup was holding: %v", err)
	}
	lease.release()

	// And once nothing holds it, cleanup does its job: without this half the test
	// would pass against a cleanup that never removes anything.
	removeSandboxRuntimeRootIfUnused(root)
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("an unheld root survived cleanup: %v", err)
	}
}

// The lease is on the root the caller names, so holding one root does not
// protect a sibling: setup reserving its own selection must not stop the
// cleanup doing its work elsewhere.
func TestAHeldLeaseDoesNotProtectASiblingRoot(t *testing.T) {
	held := runtimeRootUnderTest(t, "aaaaaaaaaaaaaaaa")
	sibling := filepath.Join(filepath.Dir(held), "bbbbbbbbbbbbbbbb")
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	lease, err := prepareSandboxRuntimeLease(held)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.release()

	removeSandboxRuntimeRootIfUnused(sibling)
	if _, err := os.Stat(sibling); !os.IsNotExist(err) {
		t.Errorf("holding one root blocked cleanup of an unrelated sibling: %v", err)
	}
	if _, err := os.Stat(held); err != nil {
		t.Errorf("the held root was removed: %v", err)
	}
}

// createdRuntimeDirsForTest builds identity-bound rollback records for paths a
// test made itself, so a test can express "these are the directories this run
// created" without restating the identity capture.
func createdRuntimeDirsForTest(paths ...string) []windowsCreatedRuntimeDir {
	records := make([]windowsCreatedRuntimeDir, 0, len(paths))
	for _, path := range paths {
		identity, identified := runtimeDirIdentity(path)
		records = append(records, windowsCreatedRuntimeDir{path: path, identity: identity, identified: identified})
	}
	return records
}

//go:build !windows

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// unixLeaseRootUnder builds a production-shaped runtime root beneath a
// test-owned base, so the owned tail is real rather than assumed.
func unixLeaseRootUnder(t *testing.T, base string) string {
	t.Helper()
	workspace := t.TempDir()
	root, ok := deterministicSandboxRuntimeRoot(canonicalSandboxWorkspaceRoot(workspace), canonicalSandboxWorkspaceRoot(base))
	if !ok {
		t.Fatalf("SETUP INVALID: no deterministic runtime root under %s", base)
	}
	return root
}

// countEntriesUnder reports how many entries exist anywhere beneath dir, so a
// test can say "nothing was written here" rather than checking one name.
func countEntriesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var found []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		found = append(found, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return found
}

// A PRE-CHECK CANNOT AUTHORIZE A PATHNAME WRITE THAT COMES AFTER IT.
//
// The fallback root moved from an atomically minted MkdirTemp parent to a
// predictable /tmp/zero-u<uid>/runtime/v1/<digest>. The stable name is required,
// because setup and every later process have to agree on one root without talking
// to each other, but it also means another local account can name the first owned
// component first. refuseAliasedRuntimeComponents answers about an ABSENT
// component by saying there is nothing to alias, which is the state a fresh
// fallback is in, and os.MkdirAll plus the pathname lease open then followed a
// link planted after that answer.
//
// The link arrives after the last validation and before the first create, which
// is the only window that matters: one planted earlier was refused by the old
// pre-check too, so a test that plants it up front passes against the defect and
// proves nothing.
func TestRuntimeLeaseRefusesALinkPlantedAfterTheCheck(t *testing.T) {
	base := t.TempDir()
	target := t.TempDir()
	root := unixLeaseRootUnder(t, base)

	owned := filepath.Join(canonicalSandboxWorkspaceRoot(base), "zero")
	if !pathWithinRoot(owned, root) {
		t.Fatalf("SETUP INVALID: %s does not sit under the owned component %s", root, owned)
	}

	previous := runtimeDescentBarrier
	t.Cleanup(func() { runtimeDescentBarrier = previous })
	planted := false
	runtimeDescentBarrier = func() {
		if planted {
			return
		}
		planted = true
		if err := os.Symlink(target, owned); err != nil {
			t.Errorf("plant the link: %v", err)
		}
	}

	lease, _, err := prepareSandboxRuntimeLeaseRecording(root)
	if lease != nil {
		lease.release()
	}

	// SETUP: the swap really happened in the window, or nothing was under test.
	if !planted {
		t.Fatal("SETUP INVALID: the descent never reached the barrier, so no link was planted")
	}
	if err == nil {
		t.Fatal("lease acquisition succeeded through a link planted at the first owned component")
	}
	// ELOOP on Linux, and either that or ENOTDIR elsewhere. Asserted as a class
	// rather than one kernel's spelling.
	if !errors.Is(err, unix.ELOOP) && !errors.Is(err, unix.ENOTDIR) {
		t.Errorf("refused for the wrong reason, want a no-follow refusal: %v", err)
	}

	// THE POINT IS WHERE THE WRITES DID NOT GO. An error alone would also be
	// produced by a later guard noticing the link after the tree was built inside
	// somebody else's directory.
	if leaked := countEntriesUnder(t, target); len(leaked) != 0 {
		t.Fatalf("the redirected target received %v; the refusal happened after the writes rather than instead of them", leaked)
	}
}

// CONTROL: an ordinary tree still gets its lease, and the one path opened by name
// is the operator's base rather than a component Zero owns.
//
// Refusing everything would satisfy the test above. Asserting the base directly
// is what makes this discriminating: inferring it from whether a swap was caught
// says nothing about which name was trusted.
func TestRuntimeLeaseOpensOnlyTheBaseByName(t *testing.T) {
	base := t.TempDir()
	root := unixLeaseRootUnder(t, base)

	previousBase := runtimeBaseOpenedByName
	t.Cleanup(func() { runtimeBaseOpenedByName = previousBase })
	var byName []string
	runtimeBaseOpenedByName = func(path string) { byName = append(byName, path) }

	lease, created, err := prepareSandboxRuntimeLeaseRecording(root)
	if err != nil {
		t.Fatalf("lease acquisition failed on an ordinary tree: %v", err)
	}
	lease.release()

	if len(byName) != 1 {
		t.Fatalf("opened %v by name, want exactly the base", byName)
	}
	if got := byName[0]; got != canonicalSandboxWorkspaceRoot(base) && got != base {
		t.Fatalf("opened %q by name, want the base %q", got, base)
	}
	if len(created) == 0 {
		t.Fatal("the descent created the owned components but recorded none of them, so rollback has nothing to undo")
	}
	if _, err := os.Lstat(sandboxRuntimeLeasePath(root)); err != nil {
		t.Fatalf("no lease file was created on an ordinary tree: %v", err)
	}
}

// And a link at the LEASE NAME itself is refused by both holders, so the shared
// and exclusive sides cannot end up locking different objects.
func TestRuntimeLeaseRefusesALinkAtTheLeaseName(t *testing.T) {
	base := t.TempDir()
	root := unixLeaseRootUnder(t, base)

	lease, _, err := prepareSandboxRuntimeLeaseRecording(root)
	if err != nil {
		t.Fatalf("SETUP: cannot seed a lease: %v", err)
	}
	lease.release()
	leasePath := sandboxRuntimeLeasePath(root)
	if err := os.Remove(leasePath); err != nil {
		t.Fatalf("SETUP INVALID: cannot clear the seeded lease: %v", err)
	}
	target := filepath.Join(t.TempDir(), "target.lease")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, leasePath); err != nil {
		t.Fatalf("SETUP: cannot plant the link: %v", err)
	}

	shared, _, sharedErr := prepareSandboxRuntimeLeaseRecording(root)
	if shared != nil {
		shared.release()
	}
	if sharedErr == nil {
		t.Error("shared acquisition accepted a link at the lease name")
	}

	cleanup, inUse, cleanupErr := tryAcquireSandboxRuntimeCleanupLease(root)
	if cleanup != nil {
		cleanup.release()
	}
	if cleanupErr == nil {
		t.Errorf("cleanup accepted a link at the lease name (inUse=%t); it would treat a lock on %s as proof the root is free to delete", inUse, target)
	}
}

// CONTROL: the two sides still coordinate on an ordinary lease.
func TestUnixSharedLeaseMakesCleanupReportInUse(t *testing.T) {
	base := t.TempDir()
	root := unixLeaseRootUnder(t, base)

	held, _, err := prepareSandboxRuntimeLeaseRecording(root)
	if err != nil {
		t.Fatalf("cannot acquire a runtime lease: %v", err)
	}

	lease, inUse, err := tryAcquireSandboxRuntimeCleanupLease(root)
	if lease != nil {
		lease.release()
	}
	if err != nil {
		t.Fatalf("cleanup failed against a legitimately held lease: %v", err)
	}
	if !inUse {
		t.Fatal("cleanup reported the runtime root free while a shared lease was held; it would delete a tree a live command is using")
	}

	held.release()
	lease, inUse, err = tryAcquireSandboxRuntimeCleanupLease(root)
	if err != nil {
		t.Fatalf("cleanup failed after the shared lease was released: %v", err)
	}
	if inUse {
		t.Fatal("cleanup still reported the root in use after the only holder released it, so reclamation never happens")
	}
	lease.release()
}

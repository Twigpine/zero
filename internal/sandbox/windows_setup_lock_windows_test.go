//go:build windows

package sandbox

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A unique key per test run, so a leftover object from an earlier run cannot
// make these pass or fail for the wrong reason.
func lockTestKey(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("t%dx%d", time.Now().UnixNano(), len(t.Name()))
}

// The production DACL is Administrators only, so an unelevated test process
// cannot OPEN an existing lock and never reaches the mutex wait at all. These
// tests are about the wait, so they relax the DACL for themselves.
// TestSetupLockIsAdministratorsOnly pins the real value.
func withOpenableLock(t *testing.T) {
	t.Helper()
	previous := windowsSetupLockSDDL
	windowsSetupLockSDDL = "D:P(A;;GA;;;WD)"
	t.Cleanup(func() { windowsSetupLockSDDL = previous })
}

func withShortLockTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	previous := windowsSetupLockTimeout
	windowsSetupLockTimeout = d
	t.Cleanup(func() { windowsSetupLockTimeout = previous })
}

// THE POINT OF THE WHOLE FILE. A second setup for the same workspace must not
// get in while the first holds the lock.
//
// Without this, two elevated setups interleave: the account ends up with one
// process's password while the stored secret is the other's, and every later
// command fails to log on. The second acquisition runs on its own goroutine
// because Win32 mutex ownership is per THREAD and acquire pins the thread, so a
// same-thread re-entry would succeed recursively and prove nothing.
func TestSecondSetupIsExcludedWhileTheFirstHoldsTheLock(t *testing.T) {
	withShortLockTimeout(t, 300*time.Millisecond)
	withOpenableLock(t)
	key := lockTestKey(t)

	first, err := acquireWindowsSandboxSetupLock(key)
	if err != nil {
		t.Fatalf("first acquisition: %v", err)
	}
	defer first.release()

	result := make(chan error, 1)
	go func() {
		second, err := acquireWindowsSandboxSetupLock(key)
		if err == nil {
			second.release()
		}
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("a second setup acquired the lock while the first still held it, so the transaction is not serialized")
		}
		// It has to say what to do, since the operator's next question is always
		// whether to wait or to go looking for a stuck process.
		if !strings.Contains(err.Error(), "already running") {
			t.Errorf("contention error does not explain itself: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second acquisition neither succeeded nor timed out, so the bounded wait is not bounded")
	}
}

// Releasing must actually hand the lock on. A lock that excludes forever is a
// worse failure than one that never excluded: the machine could never be set up
// again without a reboot.
func TestReleasingTheLockLetsTheNextSetupIn(t *testing.T) {
	withShortLockTimeout(t, 5*time.Second)
	withOpenableLock(t)
	key := lockTestKey(t)

	first, err := acquireWindowsSandboxSetupLock(key)
	if err != nil {
		t.Fatalf("first acquisition: %v", err)
	}
	first.release()

	result := make(chan error, 1)
	go func() {
		second, err := acquireWindowsSandboxSetupLock(key)
		if err == nil {
			second.release()
		}
		result <- err
	}()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("the lock was not released, so no later setup could ever run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("acquiring a released lock hung")
	}
}

// Different workspaces must not block each other. A machine-wide lock would
// serialize unrelated setups for no reason.
func TestDifferentWorkspacesDoNotExcludeEachOther(t *testing.T) {
	withShortLockTimeout(t, 5*time.Second)
	withOpenableLock(t)
	base := lockTestKey(t)

	first, err := acquireWindowsSandboxSetupLock(base + "alpha")
	if err != nil {
		t.Fatalf("first workspace: %v", err)
	}
	defer first.release()

	result := make(chan error, 1)
	go func() {
		second, err := acquireWindowsSandboxSetupLock(base + "beta")
		if err == nil {
			second.release()
		}
		result <- err
	}()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("a second workspace was blocked by an unrelated one: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a second workspace hung behind an unrelated lock")
	}
}

// Release must be safe to call twice, since it is both deferred and reachable
// explicitly. A double ReleaseMutex would fail and, worse, a double
// UnlockOSThread panics the runtime.
func TestReleasingTwiceIsSafe(t *testing.T) {
	withShortLockTimeout(t, 5*time.Second)
	withOpenableLock(t)
	lock, err := acquireWindowsSandboxSetupLock(lockTestKey(t))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	lock.release()
	lock.release()
}

// The object name has to be per workspace and stable, since that is what makes
// the exclusion scoped correctly rather than machine-wide or per-run.
func TestLockNameIsPerWorkspaceAndStable(t *testing.T) {
	first := windowsSandboxSetupLockName("workspace-one")
	again := windowsSandboxSetupLockName("workspace-one")
	other := windowsSandboxSetupLockName("workspace-two")

	if first != again {
		t.Errorf("the same workspace derived two names, %q then %q", first, again)
	}
	if first == other {
		t.Errorf("two workspaces share the lock name %q, so unrelated setups would serialize", first)
	}
	// Global rather than Local: the account, its logon rights and the WFP
	// filters are machine state, so setups in different logon sessions must
	// still exclude each other.
	if !strings.HasPrefix(first, `Global\`) {
		t.Errorf("lock name %q is not in the Global namespace, so it would not span logon sessions", first)
	}
	// A backslash past the namespace prefix is rejected by the object manager,
	// so a key that smuggled one in would fail at CreateMutex.
	if strings.Contains(strings.TrimPrefix(first, `Global\`), `\`) {
		t.Errorf("lock name %q contains a separator past its namespace prefix", first)
	}
}

// The production lock must stay reachable only by accounts that could already
// run setup. A sandbox principal able to open it could hold it and stall every
// future setup, or squat the name so real setup believes it holds a lock it
// does not.
func TestSetupLockIsAdministratorsOnly(t *testing.T) {
	// BA is Administrators, SY is SYSTEM, and the leading P protects the DACL so
	// no inherited ACE can widen it.
	for _, want := range []string{"D:P", "(A;;GA;;;BA)", "(A;;GA;;;SY)"} {
		if !strings.Contains(windowsSetupLockSDDL, want) {
			t.Errorf("lock SDDL %q is missing %q", windowsSetupLockSDDL, want)
		}
	}
	// WD is Everyone. Granting it here would let any sandboxed process stall
	// setup for the whole machine.
	if strings.Contains(windowsSetupLockSDDL, ";WD)") {
		t.Errorf("lock SDDL %q grants Everyone, so any process could hold the setup lock", windowsSetupLockSDDL)
	}
}

// An empty or unusable key must still produce a valid name rather than a bare
// prefix, which every such workspace would then share.
func TestLockNameHandlesAnEmptyKey(t *testing.T) {
	name := windowsSandboxSetupLockName("")
	if name == `Global\ZeroSandboxSetup-` {
		t.Fatal("an empty key produced a bare prefix, so every such workspace would share one lock")
	}
	if !strings.HasPrefix(name, `Global\ZeroSandboxSetup-`) {
		t.Errorf("unexpected name %q", name)
	}
}

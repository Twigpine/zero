//go:build windows

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A secret this account cannot delete must be distinguishable from one that
// simply would not delete.
//
// Teardown carries on past the first and aborts on the second. Before the
// distinction existed, a secret owned by another administrator's setup stopped
// teardown at its very first step, leaving the account, its logon rights, its
// ACEs and its ledger all installed. The operator asked for removal, got an
// error, and kept a working principal.
func TestRemovingAnUndeletableSecretIsDistinguishable(t *testing.T) {
	// A non-empty directory standing in for a secret: os.Remove refuses it, which
	// gives a real removal failure without needing a second administrator.
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(path, "occupant"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed occupant: %v", err)
	}

	err := removeWindowsSandboxSecret(path)
	if err == nil {
		t.Fatal("a secret that could not be removed reported success")
	}
	// This is NOT the foreign-owner case, so teardown must still treat it as
	// fatal rather than shrugging and carrying on.
	if errors.Is(err, errWindowsSandboxSecretNotOurs) {
		t.Errorf("an ordinary removal failure was classified as a foreign owner: %v", err)
	}
}

// Absence is success. Teardown runs on machines that never provisioned a
// principal, and re-running it must converge rather than fail.
func TestRemovingAMissingSecretIsNotAnError(t *testing.T) {
	if err := removeWindowsSandboxSecret(filepath.Join(t.TempDir(), "never-existed")); err != nil {
		t.Fatalf("removing a missing secret reported an error: %v", err)
	}
}

// The ordinary case still works, so the tolerance above did not turn removal
// into a no-op.
func TestRemovingOurOwnSecretDeletesIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := removeWindowsSandboxSecret(path); err != nil {
		t.Fatalf("removeWindowsSandboxSecret: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the secret survived removal: stat err = %v", err)
	}
}

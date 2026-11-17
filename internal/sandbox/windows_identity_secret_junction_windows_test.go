//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE SECRET MUST NOT BE WRITTEN THROUGH A REPARSE POINT.
//
// The sandbox home belongs to the invoking user, who is the party this sandbox
// exists to contain, so they can put a junction where the secret directory is
// expected before elevated setup runs. The pathname version resolved the path
// four times over (MkdirAll, an O_TRUNC open, SetNamedSecurityInfo by name, then
// WriteFile by name) and every one of them followed it, so an Administrator
// process created the principal's password in a directory of the caller's
// choosing and then granted them control of it.
//
// A junction rather than a symlink on purpose: it needs no privilege, so it is
// reachable by exactly the unprivileged user this guards against. os.Symlink
// would need SeCreateSymbolicLinkPrivilege and would skip on an ordinary
// developer or CI account, which is where this most needs to run.
func TestWriteWindowsSandboxSecretRefusesAJunctionedDirectory(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// The swap: the directory the secret is addressed into is a junction that
	// leads out of the sandbox home. The pathname never changes.
	cfg := filepath.Join(base, "cfg")
	makeJunction(t, cfg, outside)

	path := filepath.Join(cfg, "zero-sbx-test.secret")
	err := writeWindowsSandboxSecret(path, "Zs1!EXAMPLEPASSWORDVALUE")
	if err == nil {
		t.Fatal("wrote the principal secret through a junction, so an elevated setup would have placed it in a directory the caller controls")
	}
	if !strings.Contains(err.Error(), "reparse") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	// Refusing is only half of it. Nothing may survive on the other side, or the
	// caller still ends up holding a file the sandbox created for them.
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("junction target holds %d entries after the refusal, want none", len(entries))
	}
}

// The ordinary path must still work, or the refusal above proves only that the
// function fails.
func TestWriteWindowsSandboxSecretStillWritesAnOrdinaryDirectory(t *testing.T) {
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

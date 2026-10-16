//go:build !windows

package credstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func secureTestDirectory(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestFileLockRejectsSymlinkRedirection(t *testing.T) {
	dir := t.TempDir()
	store := fileStore(t, dir)
	target := filepath.Join(t.TempDir(), "outside.lock")
	const sentinel = "outside"
	if err := os.WriteFile(target, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, store.lockPath()); err != nil {
		t.Fatal(err)
	}

	if err := store.Set("openai", "secret"); err == nil || !strings.Contains(err.Error(), "unsafe lock path") {
		t.Fatalf("Set error = %v, want unsafe lock path rejection", err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != sentinel {
		t.Fatalf("redirect target = %q, err %v; want unchanged", data, err)
	}
	if _, err := os.Stat(store.file); !os.IsNotExist(err) {
		t.Fatalf("credential file exists after rejected lock: %v", err)
	}
}

func TestFileLockRejectsUnsafeParentPermissions(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	store, err := New(Options{Dir: dir, Storage: "file"})
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Set("openai", "secret"); err == nil || !strings.Contains(err.Error(), "unsafe permissions") {
		t.Fatalf("Set error = %v, want unsafe permissions rejection", err)
	}
	if _, err := os.Stat(store.file); !os.IsNotExist(err) {
		t.Fatalf("credential file exists after rejected parent: %v", err)
	}
}

func TestFileLockAllowsTrustedSymlinkedDirectoryComponent(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(base, "linked")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	store, err := New(Options{Dir: linkDir, Storage: "file"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("openai", "secret"); err != nil {
		t.Fatalf("trusted user-owned directory symlink was rejected: %v", err)
	}
}

func TestFileLockRejectsHardlinkedLockFile(t *testing.T) {
	dir := t.TempDir()
	store := fileStore(t, dir)
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(other, store.lockPath()); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if err := store.Set("openai", "secret"); err == nil || !strings.Contains(err.Error(), "link count") {
		t.Fatalf("Set error = %v, want hardlinked lock rejection", err)
	}
}

func TestFileLockRejectsInsecureExistingLockFile(t *testing.T) {
	dir := t.TempDir()
	store := fileStore(t, dir)
	if err := os.WriteFile(store.lockPath(), nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(store.lockPath(), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("openai", "secret"); err == nil || !strings.Contains(err.Error(), "unsafe permissions") {
		t.Fatalf("Set error = %v, want insecure lock rejection", err)
	}
}

func TestOpenCredentialLockFileRetriesENOENT(t *testing.T) {
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()

	original := credentialLockOpenat
	calls := 0
	// This test replaces a package-level syscall seam and must not run in parallel.
	credentialLockOpenat = func(directoryFD int, name string, flags int, mode uint32) (int, error) {
		calls++
		if calls == 1 {
			return -1, unix.ENOENT
		}
		return unix.Openat(directoryFD, name, flags, mode)
	}
	t.Cleanup(func() { credentialLockOpenat = original })

	fd, err := openCredentialLockFile(int(dir.Fd()), "lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Close(fd); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("Openat calls = %d, want 2", calls)
	}
}

func TestOpenCredentialLockFileStopsAfterThreeENOENTs(t *testing.T) {
	original := credentialLockOpenat
	calls := 0
	// This test replaces a package-level syscall seam and must not run in parallel.
	credentialLockOpenat = func(int, string, int, uint32) (int, error) {
		calls++
		return -1, unix.ENOENT
	}
	t.Cleanup(func() { credentialLockOpenat = original })

	fd, err := openCredentialLockFile(-1, "lock")
	if fd != -1 || !errors.Is(err, unix.ENOENT) {
		t.Fatalf("openCredentialLockFile = (%d, %v), want (-1, ENOENT)", fd, err)
	}
	if calls != 3 {
		t.Fatalf("Openat calls = %d, want 3", calls)
	}
}

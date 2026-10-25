package securefile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestCreateSecretFileRetriesOnPermissionContention(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "k.secret")
	attempts := 0
	origOpen := openSecretLockFile
	openSecretLockFile = func(path string, flag int, perm os.FileMode) (*os.File, error) {
		attempts++
		if attempts == 1 {
			return nil, os.ErrPermission
		}
		return os.OpenFile(path, flag, perm)
	}
	t.Cleanup(func() {
		openSecretLockFile = origOpen
	})

	secret, err := createSecretFile(secretPath, "")
	if err != nil {
		t.Fatalf("createSecretFile should retry on permission contention: %v", err)
	}
	if len(secret) != secretBytes {
		t.Fatalf("secret length = %d, want %d", len(secret), secretBytes)
	}
	if attempts < 2 {
		t.Fatalf("expected at least 2 lock attempts, got %d", attempts)
	}
}

func TestSealOpenRoundTrip(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "k.secret")
	c := NewCrypter(secret)
	plain := []byte(`{"openai":{"type":"api","key":"sk-test-123"}}`)
	blob, err := c.Seal(plain)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if string(blob) == string(plain) {
		t.Fatal("blob must not equal plaintext")
	}
	// The secret file is created 0600.
	info, err := os.Stat(secret)
	if err != nil {
		t.Fatalf("secret not created: %v", err)
	}
	// Windows doesn't honor Unix permission bits (Chmod only toggles read-only), so
	// the 0600 guarantee only holds on Unix.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("secret perm = %o, want 600", perm)
		}
	}
	got, err := c.Open(blob)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if string(got) != string(plain) {
		t.Fatalf("round-trip = %q, want %q", got, plain)
	}
}

func TestOpenTamperedFailsClosed(t *testing.T) {
	c := NewCrypter(filepath.Join(t.TempDir(), "k.secret"))
	blob, err := c.Seal([]byte("secret-data"))
	if err != nil {
		t.Fatal(err)
	}
	blob[len(blob)-1] ^= 0xff // flip a ciphertext bit
	if _, err := c.Open(blob); err == nil {
		t.Fatal("expected tampered blob to fail authentication")
	}
}

func TestOpenMissingSecretFailsClosed(t *testing.T) {
	c := NewCrypter(filepath.Join(t.TempDir(), "absent.secret"))
	if _, err := c.Open([]byte("0123456789012")); err == nil {
		t.Fatal("expected open with no secret to fail, not mint a new key")
	}
}

func TestOpenShortBlobFails(t *testing.T) {
	c := NewCrypter(filepath.Join(t.TempDir(), "k.secret"))
	if _, err := c.Seal([]byte("x")); err != nil { // create the secret
		t.Fatal(err)
	}
	if _, err := c.Open([]byte("short")); err == nil {
		t.Fatal("expected too-short blob to fail")
	}
}

func TestWrongSecretCannotDecrypt(t *testing.T) {
	dir := t.TempDir()
	a := NewCrypter(filepath.Join(dir, "a.secret"))
	blob, err := a.Seal([]byte("data"))
	if err != nil {
		t.Fatal(err)
	}
	b := NewCrypter(filepath.Join(dir, "b.secret")) // different secret
	if _, err := b.Open(blob); err == nil {
		t.Fatal("expected decryption under a different secret to fail")
	}
}

func TestCreateSecretFileReclaimsStaleLock(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "k.secret")
	lockPath := secretPath + ".lock"

	// Create a stale lock file manually
	if err := os.WriteFile(lockPath, []byte("99999-12345-1"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Backdate the lock file's modification time so it is considered stale
	staleTime := time.Now().Add(-2 * secureLockStaleAfter)
	if err := os.Chtimes(lockPath, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}

	// Attempt to create the secret file; it should detect the stale lock, reclaim it, and succeed
	secret, err := createSecretFile(secretPath, "")
	if err != nil {
		t.Fatalf("createSecretFile failed to reclaim stale lock: %v", err)
	}
	if len(secret) != secretBytes {
		t.Fatalf("secret length = %d, want %d", len(secret), secretBytes)
	}

	// The lock file should be removed after completion
	if _, err := os.Stat(lockPath); err == nil {
		t.Fatal("expected lock file to be cleaned up after successful acquisition")
	}
}

func TestSecretCreationConcurrentConverges(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "k.secret")
	const n = 16
	blobs := make([][]byte, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			blobs[i], errs[i] = NewCrypter(secretPath).Seal([]byte("payload"))
		}(i)
	}
	wg.Wait()

	c := NewCrypter(secretPath)
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		got, err := c.Open(blobs[i])
		if err != nil {
			t.Fatalf("goroutine %d blob does not decrypt under the converged secret: %v", i, err)
		}
		if !bytes.Equal(got, []byte("payload")) {
			t.Fatalf("goroutine %d plaintext = %q", i, got)
		}
	}
}

func TestWrongSizedSecretFailsClosed(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "k.secret")
	if err := os.WriteFile(secretPath, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCrypter(secretPath).Seal([]byte("payload")); err == nil {
		t.Fatal("wrong-sized secret must error, not mint a new key")
	}
}

func TestNewCrypterWithPublicationDirPublishesThroughIt(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "tokens.json.secret")
	pubDir := secretPath + ".publish"
	c := NewCrypterWithPublicationDir(secretPath, pubDir)
	if _, err := c.Seal([]byte("payload")); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if info, err := os.Stat(secretPath); err != nil || info.Size() != secretBytes {
		t.Fatalf("secret = %v err=%v", info, err)
	}
	entries, err := os.ReadDir(pubDir)
	if err != nil {
		t.Fatalf("publication dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("publication dir left temp files: %v", entries)
	}
	if entries, err := os.ReadDir(dir); err != nil {
		t.Fatalf("parent dir: %v", err)
	} else {
		for _, entry := range entries {
			if entry.Name() != "tokens.json.secret" && entry.Name() != filepath.Base(pubDir) {
				t.Fatalf("unexpected parent-dir entry %q; secret must publish through the protected dir", entry.Name())
			}
		}
	}
}

func TestSecretCreationSyncsFileAndDirectory(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "k.secret")
	origFile, origDir := syncSecretFileFn, syncSecretDirFn
	t.Cleanup(func() {
		syncSecretFileFn, syncSecretDirFn = origFile, origDir
	})
	fileSynced, dirSynced := false, false
	syncSecretFileFn = func(file *os.File) error {
		fileSynced = true
		return origFile(file)
	}
	syncSecretDirFn = func(dir string) error {
		dirSynced = true
		return origDir(dir)
	}
	if _, err := createSecretFile(secretPath, ""); err != nil {
		t.Fatalf("createSecretFile: %v", err)
	}
	if !fileSynced {
		t.Fatal("secret temp file was not synced before rename")
	}
	if !dirSynced {
		t.Fatal("secret destination directory was not synced after rename")
	}
}

// TestSecretDirectorySyncFailureLeavesPublishedSecret pins the post-rename
// path: a destination-directory barrier failure reports an error but the
// renamed secret is already there and a later caller can load it.
func TestSecretDirectorySyncFailureLeavesPublishedSecret(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "k.secret")
	origDir := syncSecretDirFn
	t.Cleanup(func() { syncSecretDirFn = origDir })
	syncErr := errors.New("directory sync failed")
	syncSecretDirFn = func(string) error { return syncErr }

	if _, err := createSecretFile(secretPath, ""); !errors.Is(err, syncErr) {
		t.Fatalf("createSecretFile error = %v, want %v", err, syncErr)
	}
	published, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("read published secret: %v", err)
	}
	if len(published) != secretBytes {
		t.Fatalf("published secret size = %d, want %d", len(published), secretBytes)
	}
	loaded, err := createSecretFile(secretPath, "")
	if err != nil {
		t.Fatalf("later caller could not load the published secret: %v", err)
	}
	if !bytes.Equal(loaded, published) {
		t.Fatal("later caller did not reuse the published secret")
	}
}

// TestRestoreReclaimedLockDoesNotReplaceNewerLock covers the reclaim race: a
// lock that looked stale can be replaced before the reclaimer renames it. If a
// third holder then creates lockPath, restoring the quarantined lock must not
// replace that newer lock.
func TestRestoreReclaimedLockDoesNotReplaceNewerLock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "k.secret.lock")
	reclaimed := lockPath + ".stale.crashed"
	if err := os.WriteFile(reclaimed, []byte("quarantined-token"), 0o600); err != nil {
		t.Fatalf("write quarantined lock: %v", err)
	}
	if err := os.WriteFile(lockPath, []byte("new-holder-token"), 0o600); err != nil {
		t.Fatalf("write newer lock: %v", err)
	}

	if err := restoreReclaimedLock(reclaimed, lockPath); !errors.Is(err, os.ErrExist) {
		t.Fatalf("restoreReclaimedLock error = %v, want %v", err, os.ErrExist)
	}
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read newer lock: %v", err)
	}
	if string(data) != "new-holder-token" {
		t.Fatalf("newer lock was replaced: %q", data)
	}
	if _, err := os.Stat(reclaimed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantined lock was not discarded: %v", err)
	}
}

func TestRestoreReclaimedLockRestoresWhenDestinationIsFree(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "k.secret.lock")
	reclaimed := lockPath + ".stale.crashed"
	if err := os.WriteFile(reclaimed, []byte("fresh-token"), 0o600); err != nil {
		t.Fatalf("write quarantined lock: %v", err)
	}

	if err := restoreReclaimedLock(reclaimed, lockPath); err != nil {
		t.Fatalf("restoreReclaimedLock: %v", err)
	}
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read restored lock: %v", err)
	}
	if string(data) != "fresh-token" {
		t.Fatalf("restored lock = %q", data)
	}
	if _, err := os.Stat(reclaimed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantined lock was not discarded: %v", err)
	}
}

func TestSecretSyncFailureLeavesNoSecretAndRetrySucceeds(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "k.secret")
	origFile := syncSecretFileFn
	t.Cleanup(func() { syncSecretFileFn = origFile })
	syncErr := errors.New("sync failed")
	syncSecretFileFn = func(*os.File) error { return syncErr }
	if _, err := createSecretFile(secretPath, ""); !errors.Is(err, syncErr) {
		t.Fatalf("createSecretFile error = %v, want %v", err, syncErr)
	}
	if _, err := os.Stat(secretPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed publish left a secret: %v", err)
	}
	syncSecretFileFn = origFile
	if _, err := createSecretFile(secretPath, ""); err != nil {
		t.Fatalf("retry after sync failure: %v", err)
	}
}

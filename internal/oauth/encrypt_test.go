package oauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newEncryptedStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oauth-tokens.json")
	s, err := NewStore(StoreOptions{FilePath: path, Encrypted: true})
	if err != nil {
		t.Fatalf("NewStore(encrypted): %v", err)
	}
	return s, path
}

// The unified Storage="encrypted-file" selector must encrypt at rest, the same as
// the legacy Encrypted:true alias (the file/keyring/encrypted-file merge).
func TestNewStoreEncryptedFileStorageSelector(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-tokens.json")
	s, err := NewStore(StoreOptions{FilePath: path, Storage: "encrypted-file"})
	if err != nil {
		t.Fatalf("NewStore(encrypted-file): %v", err)
	}
	if err := s.Save(ProviderKey("demo"), Token{AccessToken: "super-secret-access"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(raw), "super-secret-access") || strings.Contains(string(raw), "schemaVersion") {
		t.Fatalf("Storage=encrypted-file did not encrypt at rest:\n%s", raw)
	}
	got, ok, err := s.Load(ProviderKey("demo"))
	if err != nil || !ok || got.AccessToken != "super-secret-access" {
		t.Fatalf("Load = %+v ok=%v err=%v", got, ok, err)
	}
}

func TestEncryptedStoreRoundTripAndCiphertextOnDisk(t *testing.T) {
	s, path := newEncryptedStore(t)
	tok := Token{AccessToken: "super-secret-access", RefreshToken: "super-secret-refresh", Account: "me@x"}
	if err := s.Save(ProviderKey("demo"), tok); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// On-disk file must be ciphertext: not valid JSON, no plaintext token.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if strings.Contains(string(raw), "super-secret-access") || strings.Contains(string(raw), "schemaVersion") {
		t.Fatalf("token file is not encrypted at rest:\n%s", raw)
	}
	// The secret file exists beside it.
	if _, err := os.Stat(path + ".secret"); err != nil {
		t.Fatalf("secret file missing: %v", err)
	}
	// A fresh Store (same path) decrypts and reads it back.
	s2, err := NewStore(StoreOptions{FilePath: path, Encrypted: true})
	if err != nil {
		t.Fatalf("NewStore 2: %v", err)
	}
	got, ok, err := s2.Load(ProviderKey("demo"))
	if err != nil || !ok || got.AccessToken != "super-secret-access" || got.Account != "me@x" {
		t.Fatalf("Load = %+v ok=%v err=%v", got, ok, err)
	}
	// Delete + Status work through the encrypted backend too.
	statuses, err := s2.Status(KeyPrefixProvider)
	if err != nil || len(statuses) != 1 {
		t.Fatalf("Status = %+v err=%v", statuses, err)
	}
	if removed, err := s2.Delete(ProviderKey("demo")); err != nil || !removed {
		t.Fatalf("Delete = %v %v", removed, err)
	}
}

func TestEncryptedStoreTamperFailsClosed(t *testing.T) {
	s, path := newEncryptedStore(t)
	if err := s.Save(ProviderKey("demo"), Token{AccessToken: "a"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("encrypted token file is empty")
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if _, _, err := s.Load(ProviderKey("demo")); err == nil {
		t.Fatal("a tampered encrypted store must fail closed")
	}
}

func TestEncryptedStoreMissingSecretFailsClosed(t *testing.T) {
	s, path := newEncryptedStore(t)
	if err := s.Save(ProviderKey("demo"), Token{AccessToken: "a"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.Remove(path + ".secret"); err != nil {
		t.Fatalf("remove secret: %v", err)
	}
	if _, _, err := s.Load(ProviderKey("demo")); err == nil {
		t.Fatal("missing secret must fail closed, not return empty")
	}
}

// TestEncryptedStoreReclaimsStaleSecretLock reproduces a crash while the
// encrypted-file backend was creating its AES key. The key-creation lock is an
// O_EXCL ownership marker, not a kernel lock, so a process killed between
// creating and removing it leaves a file that every later Save waits on and
// then rejects forever. A stale marker must be reclaimed instead.
func TestEncryptedStoreReclaimsStaleSecretLock(t *testing.T) {
	s, path := newEncryptedStore(t)
	lockPath := path + ".secret.lock"
	if err := os.WriteFile(lockPath, []byte("crashed-holder"), 0o600); err != nil {
		t.Fatalf("write stale secret lock: %v", err)
	}
	stale := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(lockPath, stale, stale); err != nil {
		t.Fatalf("backdate stale secret lock: %v", err)
	}

	if err := s.Save(ProviderKey("demo"), Token{AccessToken: "after-crash"}); err != nil {
		t.Fatalf("Save must reclaim a stale secret lock, got: %v", err)
	}
	if _, err := os.Stat(lockPath); err == nil {
		t.Fatal("stale secret lock was not cleaned up")
	}

	s2, err := NewStore(StoreOptions{FilePath: path, Encrypted: true})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	token, ok, err := s2.Load(ProviderKey("demo"))
	if err != nil || !ok || token.AccessToken != "after-crash" {
		t.Fatalf("Load after stale-lock recovery = %+v ok=%v err=%v", token, ok, err)
	}
}

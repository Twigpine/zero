package credstore

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeKeyring is an in-memory KeyringClient so tests never touch the real OS
// keychain.
type fakeKeyring struct {
	available bool
	data      map[string]string
}

func newFakeKeyring(available bool) *fakeKeyring {
	return &fakeKeyring{available: available, data: map[string]string{}}
}
func (f *fakeKeyring) Available() bool { return f.available }
func (f *fakeKeyring) Set(service, account, secret string) error {
	f.data[service+"/"+account] = secret
	return nil
}
func (f *fakeKeyring) Get(service, account string) (string, bool, error) {
	v, ok := f.data[service+"/"+account]
	return v, ok, nil
}
func (f *fakeKeyring) Delete(service, account string) (bool, error) {
	k := service + "/" + account
	_, ok := f.data[k]
	delete(f.data, k)
	return ok, nil
}

func noEnv(string) string { return "" }

func roundTrip(t *testing.T, s *Store) {
	t.Helper()
	if _, ok, _ := s.Get("openai"); ok {
		t.Fatal("expected no key initially")
	}
	if err := s.Set("OpenAI", "sk-abc-123"); err != nil { // mixed case normalizes
		t.Fatalf("set: %v", err)
	}
	got, ok, err := s.Get("openai")
	if err != nil || !ok || got != "sk-abc-123" {
		t.Fatalf("get = %q,%v,%v; want sk-abc-123,true,nil", got, ok, err)
	}
	if err := s.Set("openai", "sk-new-456"); err != nil { // overwrite
		t.Fatal(err)
	}
	if got, _, _ := s.Get("openai"); got != "sk-new-456" {
		t.Fatalf("overwrite get = %q, want sk-new-456", got)
	}
	existed, err := s.Delete("openai")
	if err != nil || !existed {
		t.Fatalf("delete = %v,%v; want true,nil", existed, err)
	}
	if _, ok, _ := s.Get("openai"); ok {
		t.Fatal("expected key gone after delete")
	}
	if existed, _ := s.Delete("openai"); existed {
		t.Fatal("delete of absent key should report not-existed")
	}
}

func TestKeyringBackendRoundTrip(t *testing.T) {
	s, err := New(Options{Storage: "keyring", Keyring: newFakeKeyring(true), Env: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if s.Backend() != "keyring" || !s.Encrypted() {
		t.Fatalf("backend=%q encrypted=%v", s.Backend(), s.Encrypted())
	}
	roundTrip(t, s)
}

func TestEncryptedFileBackendRoundTripAndAtRest(t *testing.T) {
	dir := t.TempDir()
	secureTestDirectory(t, dir)
	s, err := New(Options{Storage: "encrypted-file", Dir: dir, Keyring: newFakeKeyring(false), Env: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Encrypted() {
		t.Fatal("encrypted-file must report Encrypted()")
	}
	if err := s.Set("anthropic", "sk-ant-SECRET"); err != nil {
		t.Fatal(err)
	}
	// The on-disk file must NOT contain the plaintext key, and must be 0600.
	raw, err := os.ReadFile(filepath.Join(dir, encryptedFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-ant-SECRET") {
		t.Fatal("encrypted-file leaked the plaintext key on disk")
	}
	if runtime.GOOS != "windows" { // Windows doesn't honor Unix permission bits
		info, _ := os.Stat(filepath.Join(dir, encryptedFileName))
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("perm = %o, want 600", info.Mode().Perm())
		}
	}
	if got, ok, _ := s.Get("anthropic"); !ok || got != "sk-ant-SECRET" {
		t.Fatalf("get = %q,%v", got, ok)
	}
	roundTrip(t, s)
}

func TestPlaintextFileBackendIsOptOut(t *testing.T) {
	dir := t.TempDir()
	secureTestDirectory(t, dir)
	s, err := New(Options{Storage: "file", Dir: dir, Keyring: newFakeKeyring(false), Env: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	if s.Encrypted() {
		t.Fatal("plaintext file must NOT report Encrypted()")
	}
	roundTrip(t, s)
}

func TestAutoPrefersKeyringThenEncryptedFile(t *testing.T) {
	// macOS + keyring available -> keyring
	s, err := New(Options{Dir: t.TempDir(), Keyring: newFakeKeyring(true), GOOS: "darwin", Env: noEnv})
	if err != nil || s.Backend() != "keyring" {
		t.Fatalf("auto on darwin with keyring = %q,%v; want keyring", s.Backend(), err)
	}
	// macOS but keyring unavailable -> encrypted-file (never plaintext)
	s2, err := New(Options{Dir: t.TempDir(), Keyring: newFakeKeyring(false), GOOS: "darwin", Env: noEnv})
	if err != nil || s2.Backend() != "encrypted-file" {
		t.Fatalf("auto on darwin without keyring = %q,%v; want encrypted-file", s2.Backend(), err)
	}
	// Non-macOS (e.g. Linux) -> encrypted-file even when a keyring is available, since
	// secret-tool needs a running secret service (keyring stays opt-in via env).
	s3, err := New(Options{Dir: t.TempDir(), Keyring: newFakeKeyring(true), GOOS: "linux", Env: noEnv})
	if err != nil || s3.Backend() != "encrypted-file" {
		t.Fatalf("auto on linux = %q,%v; want encrypted-file", s3.Backend(), err)
	}
}

func TestKeyringRequestedButUnavailableErrors(t *testing.T) {
	if _, err := New(Options{Storage: "keyring", Keyring: newFakeKeyring(false), Env: noEnv}); err == nil {
		t.Fatal("expected error when keyring requested but unavailable")
	}
}

func TestEnvSelectsStorage(t *testing.T) {
	dir := t.TempDir()
	env := func(k string) string {
		if k == "ZERO_CRED_STORAGE" {
			return "encrypted-file"
		}
		return ""
	}
	s, err := New(Options{Dir: dir, Keyring: newFakeKeyring(true), Env: env}) // keyring available, but env forces encrypted-file
	if err != nil || s.Backend() != "encrypted-file" {
		t.Fatalf("env override = %q,%v; want encrypted-file", s.Backend(), err)
	}
}

func TestProvidersListsFileBackend(t *testing.T) {
	dir := t.TempDir()
	secureTestDirectory(t, dir)
	s, _ := New(Options{Storage: "encrypted-file", Dir: dir, Keyring: newFakeKeyring(false), Env: noEnv})
	_ = s.Set("openai", "a")
	_ = s.Set("gemini", "b")
	names, err := s.Providers()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "gemini" || names[1] != "openai" {
		t.Fatalf("providers = %v, want [gemini openai]", names)
	}
}

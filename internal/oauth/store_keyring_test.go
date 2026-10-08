package oauth

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// fakeKR is an in-memory KeyringClient for exercising the keyring backend
// without touching a real OS keychain.
type fakeKR struct {
	data map[string]string
	// budget mimics a backend that caps a single entry, as macOS does by
	// carrying the secret on the `security -i` command line. 0 leaves the
	// backend unbounded, which is Linux's secret-tool. The account is charged
	// against the budget for the same reason the real one charges it: both
	// share one command line, so a longer account name leaves less for the
	// secret.
	budget int
	// failSet, when non-nil, decides whether a write to account fails, standing
	// in for a locked or full keychain.
	failSet func(account string) error
	// sets counts successful writes per account, so a test can assert the order
	// a write publishes in.
	sets map[string]int
	// deletes counts delete calls per account.
	deletes map[string]int
	// failDelete, when non-nil, decides whether removing account fails, standing
	// in for a keychain that refuses a delete while it is busy or locked.
	failDelete func(account string) error
}

func newFakeKR() *fakeKR {
	return &fakeKR{data: map[string]string{}, sets: map[string]int{}, deletes: map[string]int{}}
}

// newCappedFakeKR returns a fake whose entries hold at most budget bytes once
// the account name is charged, mimicking the macOS keychain.
func newCappedFakeKR(budget int) *fakeKR {
	f := newFakeKR()
	f.budget = budget
	return f
}

func (f *fakeKR) Get(service, account string) (string, bool, error) {
	v, ok := f.data[service+"/"+account]
	return v, ok, nil
}
func (f *fakeKR) Set(service, account, secret string) error {
	if f.failSet != nil {
		if err := f.failSet(account); err != nil {
			return err
		}
	}
	if limit, bounded := f.MaxSecretLen(service, account); bounded && len(secret) > limit {
		return fmt.Errorf("keyring: secret too large (%d > %d)", len(secret), limit)
	}
	f.data[service+"/"+account] = secret
	f.sets[account]++
	return nil
}
func (f *fakeKR) Delete(service, account string) (bool, error) {
	if f.deletes == nil {
		f.deletes = map[string]int{}
	}
	f.deletes[account]++
	if f.failDelete != nil {
		if err := f.failDelete(account); err != nil {
			return false, err
		}
	}
	key := service + "/" + account
	_, ok := f.data[key]
	delete(f.data, key)
	return ok, nil
}
func (f *fakeKR) MaxSecretLen(_, account string) (int, bool) {
	if f.budget == 0 {
		return 0, false
	}
	limit := f.budget - len(account)
	if limit < 0 {
		limit = 0
	}
	return limit, true
}

// chunkAccounts returns the stored chunk accounts for family, in index order.
func (f *fakeKR) chunkAccounts(family string) []string {
	var accounts []string
	for index := 0; index < keyringMaxChunks; index++ {
		account := keyringAccount + "." + family + "." + strconv.Itoa(index)
		if _, ok := f.data[keyringService+"/"+account]; ok {
			accounts = append(accounts, account)
		}
	}
	return accounts
}

func TestStoreKeyringBackendRoundTrip(t *testing.T) {
	// Keep the cross-process keyring lock file inside a temp dir.
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	kr := newFakeKR()
	s, err := NewStore(StoreOptions{Storage: "keyring", Keyring: kr})
	if err != nil {
		t.Fatalf("NewStore(keyring): %v", err)
	}
	if !strings.HasPrefix(s.FilePath(), "keyring:") {
		t.Fatalf("FilePath = %q, want keyring identifier", s.FilePath())
	}

	if err := s.Save(ProviderKey("demo"), Token{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, ok, err := s.Load(ProviderKey("demo"))
	if err != nil || !ok {
		t.Fatalf("Load: ok=%v err=%v", ok, err)
	}
	if got.AccessToken != "a" || got.RefreshToken != "r" {
		t.Fatalf("Load = %#v", got)
	}

	// The blob is stored base64-encoded, so the raw JSON field names never appear.
	raw := kr.data[keyringService+"/"+keyringAccount]
	if raw == "" {
		t.Fatal("nothing stored in keyring")
	}
	if strings.Contains(raw, "access_token") {
		t.Fatalf("keyring blob is not encoded: %s", raw)
	}

	removed, err := s.Delete(ProviderKey("demo"))
	if err != nil || !removed {
		t.Fatalf("Delete: removed=%v err=%v", removed, err)
	}
	if _, ok, _ := s.Load(ProviderKey("demo")); ok {
		t.Fatal("token still present after delete")
	}
}

func TestNewStoreStorageSelection(t *testing.T) {
	// Unknown storage is rejected (fail closed).
	if _, err := NewStore(StoreOptions{Storage: "bogus"}); err == nil {
		t.Fatal("unknown storage should error")
	}
	// ZERO_OAUTH_STORAGE selects the keyring (with an injected client).
	s, err := NewStore(StoreOptions{
		Env:     map[string]string{"ZERO_OAUTH_STORAGE": "keyring"},
		Keyring: newFakeKR(),
	})
	if err != nil {
		t.Fatalf("NewStore(env keyring): %v", err)
	}
	if !strings.HasPrefix(s.FilePath(), "keyring:") {
		t.Fatalf("env did not select keyring backend: %q", s.FilePath())
	}
	// Default is the file backend.
	fileStore, err := NewStore(StoreOptions{FilePath: t.TempDir() + "/oauth-tokens.json"})
	if err != nil {
		t.Fatalf("NewStore(file): %v", err)
	}
	if strings.HasPrefix(fileStore.FilePath(), "keyring:") {
		t.Fatalf("default backend should be file, got %q", fileStore.FilePath())
	}
}

func TestStoreKeyringStatus(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	kr := newFakeKR()
	s, err := NewStore(StoreOptions{Storage: "keyring", Keyring: kr})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ProviderKey("demo"), Token{AccessToken: "a"}); err != nil {
		t.Fatal(err)
	}
	statuses, err := s.Status(KeyPrefixProvider)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].Key != ProviderKey("demo") || !statuses[0].HasToken {
		t.Fatalf("status = %#v", statuses)
	}
}

package config

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// A rejected case-variant write must not take the accepted profile's secret with
// it. The credential store is keyed case-insensitively while the config
// collision check compares exact names, so the capture and the check must happen
// as one transaction: capturing first and rejecting afterwards overwrote the
// working `foo` key with the rejected `FOO` write's key.
func TestCommitProviderProfileRejectedCollisionKeepsExistingKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	path := filepath.Join(dir, "config.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "foo",
		Providers: []ProviderProfile{
			{Name: "foo", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://a.example.com/v1", APIKeyStored: true, Model: "m1"},
		},
	}, 0o600)
	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Set("foo", "sk-original"); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	_, err = CommitProviderProfile(path, ProviderCommit{Profile: ProviderProfile{
		Name:         "FOO",
		ProviderKind: ProviderKindOpenAICompatible,
		BaseURL:      "https://b.example.com/v1",
		APIKey:       "sk-replacement",
		Model:        "m2",
	}})
	if err == nil {
		t.Fatal("CommitProviderProfile must reject a case-variant collision")
	}
	if !strings.Contains(err.Error(), "already exists as") {
		t.Fatalf("CommitProviderProfile error = %v, want a collision report", err)
	}

	key, ok, err := store.Get("foo")
	if err != nil || !ok {
		t.Fatalf("existing key must survive a rejected write, got ok=%v err=%v", ok, err)
	}
	if key != "sk-original" {
		t.Fatalf("rejected write replaced the existing profile's key: got %q", key)
	}
}

func TestCommitProviderProfileRejectsEmptyNameBeforeSideEffects(t *testing.T) {
	for _, key := range []string{"", "sk-secret"} {
		t.Run(fmt.Sprintf("key-length-%d", len(key)), func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
			path := filepath.Join(dir, "config.json")
			_, err := CommitProviderProfile(path, ProviderCommit{Profile: ProviderProfile{APIKey: key}})
			if err == nil || !strings.Contains(err.Error(), "provider name is required") {
				t.Fatalf("CommitProviderProfile error = %v, want provider-name validation", err)
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatalf("empty-name commit touched config: %v", statErr)
			}
			store, storeErr := ProviderKeyStoreAt(dir)
			if storeErr != nil {
				t.Fatal(storeErr)
			}
			if _, ok, getErr := store.Get("openai"); getErr != nil || ok {
				t.Fatalf("empty-name commit touched credential store: ok=%v err=%v", ok, getErr)
			}
		})
	}
}

// The same guarantee for a name that had no stored key before: a rejected write
// must not leave its captured secret behind under the shared identity.
func TestCommitProviderProfileRejectedCollisionRemovesCapturedKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	path := filepath.Join(dir, "config.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "work",
		Providers: []ProviderProfile{
			{Name: "work", ProviderKind: ProviderKindOpenAICompatible, BaseURL: "https://a.example.com/v1", Model: "m1"},
		},
	}, 0o600)

	if _, err := CommitProviderProfile(path, ProviderCommit{Profile: ProviderProfile{
		Name:         "WORK",
		ProviderKind: ProviderKindOpenAICompatible,
		BaseURL:      "https://b.example.com/v1",
		APIKey:       "sk-replacement",
	}}); err == nil {
		t.Fatal("CommitProviderProfile must reject a case-variant collision")
	}

	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if key, ok, err := store.Get("work"); err != nil || ok {
		t.Fatalf("rejected write left a captured key behind: key=%q ok=%v err=%v", key, ok, err)
	}
}

func TestCommitProviderProfileCapturesKeyAndPersistsRow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	path := filepath.Join(dir, "config.json")

	result, err := CommitProviderProfile(path, ProviderCommit{
		Profile: ProviderProfile{
			Name:         "work",
			ProviderKind: ProviderKindOpenAICompatible,
			BaseURL:      "https://a.example.com/v1",
			APIKey:       "sk-secret",
			Model:        "m1",
		},
		SetActive: true,
	})
	if err != nil {
		t.Fatalf("CommitProviderProfile() error = %v", err)
	}
	if result.Persisted.APIKey != "" || !result.Persisted.APIKeyStored {
		t.Fatalf("persisted profile must carry the capture flip, got %+v", result.Persisted)
	}
	if result.Config.ActiveProvider != "work" {
		t.Fatalf("active provider = %q, want work", result.Config.ActiveProvider)
	}
	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if key, ok, err := store.Get("work"); err != nil || !ok || key != "sk-secret" {
		t.Fatalf("captured key = %q ok=%v err=%v, want the secret in the store", key, ok, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "sk-secret") {
		t.Fatal("config.json must never hold the cleartext key")
	}
}

func TestRevokeProviderCredentialsHoldsLockAndDoesNotRestoreOnMarkerFailure(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfigFixture(t, path, FileConfig{Providers: []ProviderProfile{{Name: "work", APIKeyStored: true}}}, 0o600)
	store, err := ProviderKeyStoreForConfigPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("work", "fixture"); err != nil {
		t.Fatal(err)
	}
	oldAcquire := acquireProviderWriteLock
	held := false
	acquireProviderWriteLock = func(path string) (func() error, error) {
		release, err := lockProviderWrite(path)
		if err != nil {
			return nil, err
		}
		held = true
		return func() error { held = false; return release() }, nil
	}
	t.Cleanup(func() { acquireProviderWriteLock = oldAcquire })
	removed, err := RevokeProviderCredentials(path, "work", func(string) (bool, error) {
		if !held {
			t.Fatal("OAuth revoked outside transaction lock")
		}
		// Make publication fail after config has been read, on every platform.
		if err := os.Rename(path, path+".original"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		return true, nil
	})
	if !removed || err == nil || !strings.Contains(err.Error(), "credentials revoked, but stored-key marker update failed") {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	if held {
		t.Fatal("transaction lock not released")
	}
	if _, ok, err := store.Get("work"); err != nil || ok {
		t.Fatalf("revoked key restored: present=%v err=%v", ok, err)
	}
}

func TestCommitProviderProfileReturnsCommittedResultWhenLockReleaseFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	path := filepath.Join(dir, "config.json")
	oldAcquire := acquireProviderWriteLock
	acquireProviderWriteLock = func(path string) (func() error, error) {
		release, err := lockProviderWrite(path)
		if err != nil {
			return nil, err
		}
		return func() error {
			if err := release(); err != nil {
				return err
			}
			return errors.New("injected release failure")
		}, nil
	}
	t.Cleanup(func() { acquireProviderWriteLock = oldAcquire })

	result, err := CommitProviderProfile(path, ProviderCommit{
		Profile:   ProviderProfile{Name: "work", APIKey: "sk-secret"},
		SetActive: true,
	})
	if err == nil || !strings.Contains(err.Error(), "was committed") || !strings.Contains(err.Error(), "injected release failure") {
		t.Fatalf("CommitProviderProfile error = %v, want clear post-commit release failure", err)
	}
	if result.Config.ActiveProvider != "work" || result.Persisted.Name != "work" || !result.Persisted.APIKeyStored {
		t.Fatalf("committed result was discarded: %+v", result)
	}
	if cfg := readConfigFixture(t, path); cfg.ActiveProvider != "work" || len(cfg.Providers) != 1 {
		t.Fatalf("published config = %+v, want committed work profile", cfg)
	}
}

func TestProviderOperationDoesNotClaimSkippedPublicationOnReleaseFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	path := filepath.Join(dir, "config.json")
	oldAcquire := acquireProviderWriteLock
	// This test replaces a package-level lock seam and must not run in parallel.
	acquireProviderWriteLock = func(string) (func() error, error) {
		return func() error { return errors.New("injected release failure") }, nil
	}
	t.Cleanup(func() { acquireProviderWriteLock = oldAcquire })

	err := StoreProviderCredential(path, "work", "sk-secret")
	if err == nil || !strings.Contains(err.Error(), "releasing the provider config/key transaction lock failed") {
		t.Fatalf("StoreProviderCredential error = %v, want release failure", err)
	}
	if strings.Contains(err.Error(), "was committed") {
		t.Fatalf("skipped publication was reported as committed: %v", err)
	}
}

// KeepStoredKey is the path that must not touch the store at all: the profile
// already references a credential this write is not replacing.
func TestCommitProviderProfileKeepStoredKeyLeavesStoreUntouched(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	path := filepath.Join(dir, "config.json")
	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := store.Set("work", "sk-existing"); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	if _, err := CommitProviderProfile(path, ProviderCommit{
		Profile: ProviderProfile{
			Name:         "work",
			ProviderKind: ProviderKindOpenAICompatible,
			BaseURL:      "https://a.example.com/v1",
			APIKeyStored: true,
			Model:        "m1",
		},
		SetActive:     true,
		KeepStoredKey: true,
	}); err != nil {
		t.Fatalf("CommitProviderProfile() error = %v", err)
	}
	if key, ok, err := store.Get("work"); err != nil || !ok || key != "sk-existing" {
		t.Fatalf("stored key = %q ok=%v err=%v, want it untouched", key, ok, err)
	}
}

// Concurrent writers for the same case-insensitive identity must not both
// commit, and whichever loses must leave the winner's key intact. This is the
// race the transaction exists for; the lock also has to be released so the
// loser's attempt completes rather than hanging.
func TestCommitProviderProfileConcurrentCaseVariantsKeepOneKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	path := filepath.Join(dir, "config.json")

	names := []string{"foo", "FOO"}
	keys := map[string]string{"foo": "sk-lower", "FOO": "sk-upper"}
	errs := make([]error, len(names))
	var wait sync.WaitGroup
	start := make(chan struct{})
	for index, name := range names {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, errs[index] = CommitProviderProfile(path, ProviderCommit{
				Profile: ProviderProfile{
					Name:         name,
					ProviderKind: ProviderKindOpenAICompatible,
					BaseURL:      "https://a.example.com/v1",
					APIKey:       keys[name],
					Model:        "m1",
				},
				SetActive: true,
			})
		}()
	}
	close(start)
	wait.Wait()

	committed := ""
	for index, err := range errs {
		if err == nil {
			if committed != "" {
				t.Fatalf("both %q and %q committed; case variants must collide", committed, names[index])
			}
			committed = names[index]
		}
	}
	if committed == "" {
		t.Fatalf("neither write committed: %v", errs)
	}

	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	key, ok, err := store.Get(committed)
	if err != nil || !ok {
		t.Fatalf("committed profile lost its key: ok=%v err=%v", ok, err)
	}
	if key != keys[committed] {
		t.Fatalf("stored key = %q, want %q — the rejected write replaced it", key, keys[committed])
	}
	providers, err := persistedProviders(path)
	if err != nil {
		t.Fatalf("persistedProviders() error = %v", err)
	}
	if len(providers) != 1 || providers[0].Name != committed {
		t.Fatalf("persisted rows = %v, want only %q", providers, committed)
	}
}

func TestCommitProviderProfileCrossProcessCaseVariantsKeepOneKey(t *testing.T) {
	// The children below pin the encrypted-file backend, so this process must
	// pin it too: auto resolution picks the macOS keychain, and the parent would
	// then verify a different backend than the one the children wrote to.
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	commands := []*exec.Cmd{
		exec.Command(os.Args[0], "-test.run=^TestProviderCommitProcessHelper$"),
		exec.Command(os.Args[0], "-test.run=^TestProviderCommitProcessHelper$"),
	}
	commands[0].Env = append(os.Environ(), "ZERO_PROVIDER_COMMIT_HELPER=1", "ZERO_CRED_STORAGE=encrypted-file", "ZERO_PROVIDER_COMMIT_PATH="+path, "ZERO_PROVIDER_COMMIT_NAME=foo", "ZERO_PROVIDER_COMMIT_KEY=sk-lower")
	commands[1].Env = append(os.Environ(), "ZERO_PROVIDER_COMMIT_HELPER=1", "ZERO_CRED_STORAGE=encrypted-file", "ZERO_PROVIDER_COMMIT_PATH="+path, "ZERO_PROVIDER_COMMIT_NAME=FOO", "ZERO_PROVIDER_COMMIT_KEY=sk-upper")
	for _, command := range commands {
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
	}
	successes := 0
	for _, command := range commands {
		if command.Wait() == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful child commits = %d, want exactly one", successes)
	}
	cfg, err := loadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("persisted providers = %+v, want one", cfg.Providers)
	}
	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, ok, err := store.Get(cfg.Providers[0].Name)
	if err != nil || !ok || (key != "sk-lower" && key != "sk-upper") {
		t.Fatalf("committed key = %q ok=%v err=%v", key, ok, err)
	}
}

func TestProviderCommitProcessHelper(t *testing.T) {
	if os.Getenv("ZERO_PROVIDER_COMMIT_HELPER") != "1" {
		return
	}
	_, err := CommitProviderProfile(os.Getenv("ZERO_PROVIDER_COMMIT_PATH"), ProviderCommit{Profile: ProviderProfile{
		Name:   os.Getenv("ZERO_PROVIDER_COMMIT_NAME"),
		APIKey: os.Getenv("ZERO_PROVIDER_COMMIT_KEY"),
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCommitProviderProfileFailsClosedWhenLockIsBusy(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	heldRelease, err := lockProviderWrite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = heldRelease() })
	oldTimeout := providerWriteLockTimeout
	// This test replaces a package-level timeout and must not run in parallel.
	providerWriteLockTimeout = 20 * time.Millisecond
	t.Cleanup(func() { providerWriteLockTimeout = oldTimeout })

	_, err = CommitProviderProfile(path, ProviderCommit{Profile: ProviderProfile{Name: "work", APIKey: "sk-new"}})
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("CommitProviderProfile error = %v, want retryable busy error", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("config was written without the lock: %v", statErr)
	}
	store, storeErr := ProviderKeyStoreAt(dir)
	if storeErr != nil {
		t.Fatal(storeErr)
	}
	if key, ok, getErr := store.Get("work"); getErr != nil || ok {
		t.Fatalf("credential changed without the lock: key=%q ok=%v err=%v", key, ok, getErr)
	}
}

func TestCommitProviderProfileFailsClosedWhenLockCannotBeCreated(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocker, "config.json")
	_, err := CommitProviderProfile(path, ProviderCommit{Profile: ProviderProfile{Name: "work", APIKey: "sk-new"}})
	if err == nil || !strings.Contains(err.Error(), "transaction lock") {
		t.Fatalf("CommitProviderProfile error = %v, want lock acquisition error", err)
	}
}

func TestCommitProviderProfileRollsBackKeyWhenPublicationFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	path := filepath.Join(dir, "config.json")
	before := FileConfig{Providers: []ProviderProfile{{Name: "work", APIKeyStored: true}}}
	writeConfigFixture(t, path, before, 0o600)
	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("work", "sk-original"); err != nil {
		t.Fatal(err)
	}
	oldPublish := publishProviderConfig
	publishProviderConfig = func(string, FileConfig) error { return errors.New("disk full") }
	t.Cleanup(func() { publishProviderConfig = oldPublish })

	result, err := CommitProviderProfile(path, ProviderCommit{Profile: ProviderProfile{Name: "work", APIKey: "sk-new"}})
	if err == nil {
		t.Fatal("CommitProviderProfile error = nil, want publication failure")
	}
	if !reflect.DeepEqual(result, ProviderCommitResult{}) {
		t.Fatalf("rejected publication returned a committed result: %+v", result)
	}
	if got := readConfigFixture(t, path); !reflect.DeepEqual(got, before) {
		t.Fatalf("rejected publication changed config: %+v", got)
	}
	if key, ok, err := store.Get("work"); err != nil || !ok || key != "sk-original" {
		t.Fatalf("rollback key = %q ok=%v err=%v, want original", key, ok, err)
	}
}

func TestCommitProviderProfileReportsRollbackFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory write permissions are not represented by Unix mode bits")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory mode bits, so rollback cannot be made to fail")
	}
	dir := t.TempDir()
	t.Setenv("ZERO_CRED_STORAGE", "file")
	path := filepath.Join(dir, "config.json")
	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("work", "sk-original"); err != nil {
		t.Fatal(err)
	}
	oldPublish := publishProviderConfig
	publishProviderConfig = func(string, FileConfig) error {
		if err := os.Chmod(dir, 0o500); err != nil {
			return err
		}
		return errors.New("disk full")
	}
	t.Cleanup(func() {
		publishProviderConfig = oldPublish
		_ = os.Chmod(dir, 0o700)
	})

	_, err = CommitProviderProfile(path, ProviderCommit{Profile: ProviderProfile{Name: "work", APIKey: "sk-new"}})
	if err == nil || !strings.Contains(err.Error(), "disk full") || !strings.Contains(err.Error(), "restore credential") {
		t.Fatalf("CommitProviderProfile error = %v, want publication and rollback failures", err)
	}
}

func TestProviderWritePermissionErrorIsNotReportedAsContention(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reports delete-pending lock contention as access denied")
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory mode bits, so lock creation cannot be made to fail")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err := CommitProviderProfile(filepath.Join(dir, "config.json"), ProviderCommit{Profile: ProviderProfile{Name: "work"}})
	if err == nil || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("CommitProviderProfile error = %v, want permission failure", err)
	}
	if strings.Contains(err.Error(), "busy") {
		t.Fatalf("permission failure was misreported as contention: %v", err)
	}
}

func TestCommitCatalogProviderLoginHoldsLockThroughPersistence(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeConfigFixture(t, path, FileConfig{
		ActiveProvider: "xai",
		Providers:      []ProviderProfile{{Name: "xai", CatalogID: "xai"}},
	}, 0o600)

	persisting := make(chan struct{})
	releasePersist := make(chan struct{})
	commitDone := make(chan error, 1)
	go func() {
		commitDone <- CommitCatalogProviderLogin(path, "xai", func() error {
			close(persisting)
			<-releasePersist
			return nil
		})
	}()
	<-persisting

	removeDone := make(chan error, 1)
	go func() {
		_, err := RemoveProvider(path, "xai")
		removeDone <- err
	}()
	select {
	case err := <-removeDone:
		t.Fatalf("provider mutation completed while token persistence held the lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releasePersist)
	if err := <-commitDone; err != nil {
		t.Fatalf("CommitCatalogProviderLogin() error = %v", err)
	}
	if err := <-removeDone; err != nil {
		t.Fatalf("RemoveProvider() error = %v", err)
	}
}

func TestDeleteResolvedProviderCredentialsSerializesReassignment(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	writeConfigFixture(t, path, FileConfig{Providers: []ProviderProfile{{
		Name: "work-acme", CatalogID: "acme", APIKeyStored: true,
	}}}, 0o600)
	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("work-acme", "old-key"); err != nil {
		t.Fatal(err)
	}

	publishingRemoval := make(chan struct{})
	continueRemoval := make(chan struct{})
	originalPublish := publishProviderConfig
	// This test replaces a package-level publication seam and must not run in parallel.
	publishProviderConfig = func(configPath string, cfg FileConfig) error {
		if len(cfg.Providers) == 1 && !cfg.Providers[0].APIKeyStored {
			close(publishingRemoval)
			<-continueRemoval
		}
		return originalPublish(configPath, cfg)
	}
	t.Cleanup(func() { publishProviderConfig = originalPublish })

	removeDone := make(chan error, 1)
	go func() {
		_, _, err := DeleteResolvedProviderCredentials(path, "acme")
		removeDone <- err
	}()
	<-publishingRemoval

	reassignDone := make(chan error, 1)
	go func() {
		_, err := CommitProviderProfile(path, ProviderCommit{Profile: ProviderProfile{
			Name: "work-acme", CatalogID: "acme", APIKey: "new-key",
		}})
		reassignDone <- err
	}()
	select {
	case err := <-reassignDone:
		t.Fatalf("reassignment completed outside the removal transaction: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(continueRemoval)
	if err := <-removeDone; err != nil {
		t.Fatal(err)
	}
	if err := <-reassignDone; err != nil {
		t.Fatal(err)
	}
	if key, ok, err := store.Get("work-acme"); err != nil || !ok || key != "new-key" {
		t.Fatalf("reassigned credential = %q ok=%v err=%v, want new-key", key, ok, err)
	}
	final, err := loadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Providers) != 1 || !final.Providers[0].APIKeyStored {
		t.Fatalf("final config = %+v, want one row with apiKeyStored", final.Providers)
	}
}

func TestCommitCatalogProviderLoginRejectsOwnershipBeforePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	writeConfigFixture(t, path, FileConfig{Providers: []ProviderProfile{
		{Name: "work", CatalogID: "xai"},
		{Name: "personal", CatalogID: "xai"},
	}}, 0o600)
	called := false
	err := CommitCatalogProviderLogin(path, "xai", func() error {
		called = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("CommitCatalogProviderLogin error = %v, want ownership ambiguity", err)
	}
	if called {
		t.Fatal("token persistence ran after ownership validation failed")
	}
}

func TestCommitCatalogProviderKeyConcurrentWithCaseVariantAddStaysConsistent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	path := filepath.Join(dir, "config.json")
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		_, err := CommitProviderProfile(path, ProviderCommit{Profile: ProviderProfile{Name: "OpenRouter", CatalogID: "openrouter", APIKey: "sk-add"}})
		errs <- err
	}()
	go func() {
		<-start
		_, err := CommitCatalogProviderKey(path, "openrouter", "sk-auth")
		errs <- err
	}()
	close(start)
	firstErr, secondErr := <-errs, <-errs
	if firstErr != nil && secondErr != nil {
		t.Fatalf("both transactions failed: %v; %v", firstErr, secondErr)
	}
	cfg, err := loadConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 || !cfg.Providers[0].APIKeyStored {
		t.Fatalf("persisted providers = %+v, want one keyed row", cfg.Providers)
	}
	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, ok, err := store.Get(cfg.Providers[0].Name)
	if err != nil || !ok || (key != "sk-add" && key != "sk-auth") {
		t.Fatalf("resolved key = %q ok=%v err=%v", key, ok, err)
	}
}

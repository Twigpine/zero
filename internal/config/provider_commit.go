package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Gitlawb/zero/internal/credstore"
)

type providerCredentialSnapshot struct {
	name       string
	value      string
	present    bool
	written    string
	writtenSet bool
}

// providerProfileOperation is the authoritative write boundary for provider
// rows and their API-key entries. Callers mutate its in-memory config and use
// setKey/deleteKey; runProviderProfileOperation publishes config atomically and
// conditionally restores credential changes if publication fails.
type providerProfileOperation struct {
	path      string
	config    FileConfig
	store     *credstore.Store
	snapshots map[string]providerCredentialSnapshot
	publish   bool
	exists    bool
}

var publishProviderConfig = writeConfigFile
var acquireProviderWriteLock = lockProviderWrite

// runProviderProfileOperation runs mutate under the provider write lock and
// publishes the result atomically, rolling back credential changes when the
// publish fails.
//
// allowInvalidInput decides whether a legacy config that fails
// ValidatePersistedProviderNames may be operated on at all, and the split is
// deliberate: operations that REMOVE or REPAIR state pass true, because
// refusing to delete a row from a config the user cannot otherwise fix is a
// deadlock -- the row cannot be removed because the config is invalid, and the
// config stays invalid because the row cannot be removed. Operations that ADD
// or publish state pass false, so a new profile or credential is never written
// into a config whose persisted names are already ambiguous. When it is true
// the publish goes through writeProviderNameRepair, which re-encodes the whole
// config but permits remaining invalid names only when the operation strictly
// reduces existing name problems without introducing or worsening another.
func runProviderProfileOperation(path string, allowMissing bool, allowInvalidInput bool, mutate func(*providerProfileOperation) error) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	release, err := acquireProviderWriteLock(path)
	if err != nil {
		return FileConfig{}, err
	}
	published := false
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			if err == nil {
				if published {
					err = fmt.Errorf("provider configuration was committed, but releasing its transaction lock failed: %w", releaseErr)
				} else {
					err = fmt.Errorf("releasing the provider config/key transaction lock failed: %w", releaseErr)
				}
			} else {
				err = errors.Join(err, releaseErr)
			}
		}
	}()

	// Preference and MCP writers share this document without touching provider
	// credentials. Take their config lock after the provider lock, before reading,
	// so neither kind of writer can overwrite the other's acknowledged update.
	unlockConfig, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	defer func() { err = errors.Join(err, unlockConfig()) }()

	cfg := FileConfig{}
	exists := false
	if data, readErr := os.ReadFile(path); readErr == nil {
		exists = true
		if err := json.Unmarshal(data, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if os.IsNotExist(readErr) && !allowMissing {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, readErr)
	} else if !os.IsNotExist(readErr) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, readErr)
	}
	before := cfg
	before.Providers = append([]ProviderProfile(nil), cfg.Providers...)
	if !allowInvalidInput {
		if err := ValidatePersistedProviderNames(cfg); err != nil {
			return FileConfig{}, err
		}
	}
	op := &providerProfileOperation{path: path, config: cfg, snapshots: map[string]providerCredentialSnapshot{}, publish: true, exists: exists}
	if err := mutate(op); err != nil {
		return FileConfig{}, errors.Join(err, op.rollbackCredentials())
	}
	if op.publish {
		var publishErr error
		if allowInvalidInput {
			publishErr = writeProviderNameRepair(path, before, op.config)
		} else {
			publishErr = publishProviderConfig(path, op.config)
		}
		if publishErr != nil {
			if configWasPublished(publishErr) {
				// The config on disk already refers to the new credentials, so
				// rolling them back would recreate the mismatch this transaction
				// exists to prevent. Keep them and report the durability failure.
				published = true
				return FileConfig{}, fmt.Errorf("provider configuration was committed, but making it durable failed: %w", publishErr)
			}
			return FileConfig{}, errors.Join(publishErr, op.rollbackCredentials())
		}
		published = true
	}
	return op.config, nil
}

func (op *providerProfileOperation) credentialStore() (*credstore.Store, error) {
	if op.store != nil {
		return op.store, nil
	}
	store, err := ProviderKeyStoreAt(filepath.Dir(op.path))
	if err != nil {
		return nil, err
	}
	op.store = store
	return store, nil
}

func (op *providerProfileOperation) snapshotCredential(name string) (providerCredentialSnapshot, error) {
	identity := credstore.NormalizeProvider(name)
	if snapshot, ok := op.snapshots[identity]; ok {
		return snapshot, nil
	}
	store, err := op.credentialStore()
	if err != nil {
		return providerCredentialSnapshot{}, err
	}
	value, present, err := store.Get(name)
	if err != nil {
		return providerCredentialSnapshot{}, err
	}
	snapshot := providerCredentialSnapshot{name: name, value: value, present: present}
	op.snapshots[identity] = snapshot
	return snapshot, nil
}

func (op *providerProfileOperation) setKey(name, value string) error {
	identity := credstore.NormalizeProvider(name)
	snapshot, err := op.snapshotCredential(name)
	if err != nil {
		return err
	}
	store, err := op.credentialStore()
	if err != nil {
		return err
	}
	if err := store.Set(name, value); err != nil {
		return err
	}
	snapshot.written, snapshot.writtenSet = value, true
	op.snapshots[identity] = snapshot
	return nil
}

func (op *providerProfileOperation) deleteKey(name string) (bool, error) {
	identity := credstore.NormalizeProvider(name)
	snapshot, err := op.snapshotCredential(name)
	if err != nil {
		return false, err
	}
	store, err := op.credentialStore()
	if err != nil {
		return false, err
	}
	removed, err := store.Delete(name)
	if err != nil {
		return false, err
	}
	snapshot.written, snapshot.writtenSet = "", false
	op.snapshots[identity] = snapshot
	return removed, nil
}

func (op *providerProfileOperation) rollbackCredentials() error {
	if op.store == nil {
		return nil
	}
	var rollbackErr error
	for _, snapshot := range op.snapshots {
		current, present, err := op.store.Get(snapshot.name)
		if err != nil {
			rollbackErr = errors.Join(rollbackErr, fmt.Errorf("inspect credential %q during rollback: %w", snapshot.name, err))
			continue
		}
		if present != snapshot.writtenSet || (present && current != snapshot.written) {
			continue
		}
		if snapshot.present {
			if err := op.store.Set(snapshot.name, snapshot.value); err != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("restore credential %q during rollback: %w", snapshot.name, err))
			}
		} else {
			if _, err := op.store.Delete(snapshot.name); err != nil {
				rollbackErr = errors.Join(rollbackErr, fmt.Errorf("remove credential %q during rollback: %w", snapshot.name, err))
			}
		}
	}
	return rollbackErr
}

// ProviderCommit is one provider write: reject a colliding spelling, capture
// the inline key into the credential store, and persist the row.
type ProviderCommit struct {
	Profile   ProviderProfile
	SetActive bool
	// KeepStoredKey leaves the credential store untouched instead of capturing
	// Profile.APIKey — the case where the profile already references a stored
	// credential that this write must not replace.
	KeepStoredKey bool
}

// ProviderCommitResult reports the config as written and the profile exactly as
// it was persisted: the adopted name, and the key moved into the credential
// store (APIKey cleared, APIKeyStored set) unless KeepStoredKey was requested.
// A nonzero result alongside an error means publication succeeded but lock
// release failed; a rejected mutation or publication returns the zero result.
type ProviderCommitResult struct {
	Config    FileConfig
	Persisted ProviderProfile
}

// CommitProviderProfile runs the case-variant collision check, credential
// capture, and config publication as one serialized transaction.
//
// Splitting those steps is what made the credential store and config disagree.
// The capture writes under the credential store's NORMALIZED identity while the
// collision check reads config's EXACT names, so two concurrent writers for
// `foo` and `FOO` could both pass the check against an empty config; the loser
// then overwrote the winner's `foo` credential entry on its way to being
// correctly rejected by the config write, destroying a working profile's API
// key. A cross-process lock keeps a second writer out for the whole sequence,
// and a rejected write restores the credential entry it displaced, so a failed
// mutation cannot leave another profile's secret behind.
func CommitProviderProfile(path string, commit ProviderCommit) (ProviderCommitResult, error) {
	if strings.TrimSpace(commit.Profile.Name) == "" {
		return ProviderCommitResult{}, fmt.Errorf("provider name is required")
	}
	var persisted ProviderProfile
	cfg, err := runProviderProfileOperation(path, true, false, func(op *providerProfileOperation) error {
		persisted = commit.Profile
		if !commit.KeepStoredKey && strings.TrimSpace(persisted.APIKey) != "" {
			if err := op.setKey(persisted.Name, persisted.APIKey); err != nil {
				return fmt.Errorf("store API key for %q: %w", persisted.Name, err)
			}
			persisted.APIKey = ""
			persisted.APIKeyStored = true
		}
		return upsertProviderConfig(&op.config, persisted, commit.SetActive)
	})
	result := ProviderCommitResult{Config: cfg, Persisted: persisted}
	if err != nil {
		if len(cfg.Providers) == 0 {
			return ProviderCommitResult{}, err
		}
		return result, err
	}
	return result, nil
}

var providerWriteLockTimeout = 5 * time.Second

// lockProviderWrite takes a cross-process exclusive lock covering the provider
// rows of the config at path and the credential store beside it, which are one
// unit of state written by two files. The OS lock is released by its owning
// file handle, coupling ownership and release without a pathname check/remove
// race. The lock file remains in place; a crashed process releases the OS lock
// automatically, so no age-based stale-lock recovery is needed.
func lockProviderWrite(configPath string) (func() error, error) {
	lockPath := filepath.Join(filepath.Dir(configPath), ".zero-provider-write.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("acquire provider config/key transaction lock: %w", err)
	}
	deadline := time.Now().Add(providerWriteLockTimeout)
	for {
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, fmt.Errorf("acquire provider config/key transaction lock: %w", err)
		}
		release, acquired, lockErr := tryProviderWriteLock(file)
		if lockErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("acquire provider config/key transaction lock: %w", lockErr)
		}
		if acquired {
			return release, nil
		}
		_ = file.Close()
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("provider config/key transaction is busy; retry the operation (lock %s)", lockPath)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

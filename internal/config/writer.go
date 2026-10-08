package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Gitlawb/zero/internal/providercatalog"
)

func UpsertProvider(path string, profile ProviderProfile, setActive bool) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	return upsertProviderLocked(path, profile, setActive)
}

// upsertProviderLocked is UpsertProvider's body for a caller that already holds
// the config lock. EnsureCatalogProvider reads the document and then upserts
// into it, and the lock is not reentrant, so it must reach the work this way
// rather than through the exported function.
func upsertProviderLocked(path string, profile ProviderProfile, setActive bool) (FileConfig, error) {
	profile.Name = strings.TrimSpace(profile.Name)
	if profile.Name == "" {
		return FileConfig{}, fmt.Errorf("provider name is required")
	}

	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}

	mergeProvider(&cfg, profile)
	// mergeProfile deliberately ignores APIKeyStored — during resolve-time
	// layering a project config must not be able to claim the user's stored
	// keys. This user-config WRITE path re-applies the marker: capturing a key
	// via SecureProviderProfile onto a previously env/no-key profile must
	// persist apiKeyStored, or the secret sits in the credential store while
	// every ApplyStoredAPIKey gate skips it (PR #560 review).
	if profile.APIKeyStored {
		for index := range cfg.Providers {
			if cfg.Providers[index].Name == profile.Name {
				cfg.Providers[index].APIKeyStored = true
				break
			}
		}
	}
	if setActive || strings.TrimSpace(cfg.ActiveProvider) == "" {
		cfg.ActiveProvider = profile.Name
	}

	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// EnsuredProvider reports the outcome of EnsureCatalogProvider: the profile name
// that serves the catalog entry, whether it was newly created, and which provider
// is active after the call (unchanged unless it was blank).
type EnsuredProvider struct {
	Name    string
	Created bool
	Active  string
}

// EnsureCatalogProvider guarantees a provider profile exists in the config at
// path for the given catalog entry. OAuth login flows call this right after
// storing a token: a login is only reachable from the provider list and
// `zero providers use` when a profile exists, but a login must never replace or
// deactivate the user's current active provider — so an existing profile whose
// Name or CatalogID already matches is left completely untouched (its name,
// credentials, and model are the user's), and a created profile is NOT marked
// active unless no provider was active at all.
func EnsureCatalogProvider(path string, catalogID string) (result EnsuredProvider, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return EnsuredProvider{}, fmt.Errorf("config path is required")
	}
	descriptor, err := providercatalog.Require(catalogID)
	if err != nil {
		return EnsuredProvider{}, err
	}
	// One lock spans the existence scan AND the upsert: releasing between them
	// would let another process create the same catalog profile in the window,
	// and the second writer would clobber the first.
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return EnsuredProvider{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()

	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return EnsuredProvider{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return EnsuredProvider{}, fmt.Errorf("read config %s: %w", path, err)
	}
	for _, provider := range cfg.Providers {
		if strings.EqualFold(strings.TrimSpace(provider.CatalogID), descriptor.ID) ||
			strings.EqualFold(strings.TrimSpace(provider.Name), descriptor.ID) {
			return EnsuredProvider{Name: provider.Name, Active: cfg.ActiveProvider}, nil
		}
	}

	profile := ProviderProfile{
		Name:         descriptor.ID,
		ProviderKind: providerKindForCatalogTransport(descriptor.Transport),
		CatalogID:    descriptor.ID,
		BaseURL:      descriptor.DefaultBaseURL,
		Model:        descriptor.DefaultModel,
	}
	written, err := upsertProviderLocked(path, profile, false)
	if err != nil {
		return EnsuredProvider{}, err
	}
	return EnsuredProvider{Name: profile.Name, Created: true, Active: written.ActiveProvider}, nil
}

// MarkProviderAPIKeyStored records that provider's API key now lives in the
// credential store. It also clears inline/env key fields so the stored key is the
// runtime credential; an old apiKeyEnv value must not keep overriding a freshly
// captured key from `zero auth openrouter` or provider setup.
func MarkProviderAPIKeyStored(path string, provider string) (err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return fmt.Errorf("provider name is required")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	for index := range cfg.Providers {
		if strings.EqualFold(strings.TrimSpace(cfg.Providers[index].Name), provider) {
			cfg.Providers[index].APIKey = ""
			cfg.Providers[index].APIKeyEnv = ""
			cfg.Providers[index].APIKeyStored = true
			return writeConfigFile(path, cfg)
		}
	}
	return fmt.Errorf("provider %q not found", provider)
}

func SetActiveProvider(path string, name string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	name = strings.TrimSpace(name)
	if name == "" {
		return FileConfig{}, fmt.Errorf("provider name is required")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg := FileConfig{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}

	for _, provider := range cfg.Providers {
		if strings.EqualFold(provider.Name, name) {
			cfg.ActiveProvider = provider.Name
			if err := writeConfigFile(path, cfg); err != nil {
				return FileConfig{}, err
			}
			return cfg, nil
		}
	}

	return FileConfig{}, fmt.Errorf("provider %q not found", name)
}

// SetActiveProviderModel persists a provider selection as one lock-held
// read-modify-write. The active provider and that provider's model are one UI
// choice; publishing them separately can leave a partially applied selection
// if another writer wins the lock between calls.
func SetActiveProviderModel(path string, name string, model string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return FileConfig{}, fmt.Errorf("provider name is required")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return FileConfig{}, fmt.Errorf("model is required")
	}

	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	defer func() { err = errors.Join(err, unlock()) }()

	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := FileConfig{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	for index := range cfg.Providers {
		if strings.EqualFold(strings.TrimSpace(cfg.Providers[index].Name), name) {
			cfg.ActiveProvider = cfg.Providers[index].Name
			cfg.Providers[index].Model = model
			if err := writeConfigFile(path, cfg); err != nil {
				return FileConfig{}, err
			}
			return cfg, nil
		}
	}
	return FileConfig{}, fmt.Errorf("provider %q not found", name)
}

// ProviderPersisted reports whether a provider profile named name actually has
// a row in the config file at path. A provider can appear in the resolved/
// in-memory provider list without ever being written to config.json — e.g.
// applyProviderEnv synthesizes an "openai" profile purely from an ambient
// OPENAI_API_KEY environment variable on every Resolve() call, without ever
// persisting it. RemoveProvider/SetActiveProvider/SetProviderModel only ever
// look at what's on disk, so a caller offering to mutate a provider by name
// should check this first: "not on disk" needs different handling (nothing to
// persist/remove there) than a name that doesn't exist anywhere at all.
func ProviderPersisted(path string, name string) (bool, error) {
	path = strings.TrimSpace(path)
	name = strings.TrimSpace(name)
	if path == "" || name == "" {
		return false, nil
	}
	cfg, err := loadConfigFile(path)
	if err != nil {
		return false, err
	}
	for _, provider := range cfg.Providers {
		if strings.EqualFold(strings.TrimSpace(provider.Name), name) {
			return true, nil
		}
	}
	return false, nil
}

// RemoveProvider deletes the named provider profile from the config at path.
// When the removed profile was active, activeProvider hands off to the first
// remaining provider (or clears when none remain) so the config never points at
// a profile that no longer exists. The caller owns cleaning up the credential
// store entry — config stays pure of secret I/O on the read path, and remove
// keeps that symmetry by only touching config.json.
func RemoveProvider(path string, name string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	name = strings.TrimSpace(name)
	if name == "" {
		return FileConfig{}, fmt.Errorf("provider name is required")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := FileConfig{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}

	index := -1
	for i, provider := range cfg.Providers {
		if strings.EqualFold(strings.TrimSpace(provider.Name), name) {
			index = i
			break
		}
	}
	if index < 0 {
		return FileConfig{}, fmt.Errorf("provider %q not found", name)
	}
	removed := cfg.Providers[index]
	cfg.Providers = append(cfg.Providers[:index], cfg.Providers[index+1:]...)
	if strings.EqualFold(strings.TrimSpace(cfg.ActiveProvider), strings.TrimSpace(removed.Name)) {
		cfg.ActiveProvider = ""
		if len(cfg.Providers) > 0 {
			cfg.ActiveProvider = cfg.Providers[0].Name
		}
	}
	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// RenameProvider renames a provider profile, keeping everything keyed by the
// profile name consistent: the activeProvider pointer follows the rename, and a
// key in the encrypted credential store (APIKeyStored) is migrated to the new
// name BEFORE the config is rewritten — the store write must succeed first so a
// failed migration never strands the config pointing at a key that no longer
// resolves. OAuth tokens are deliberately not migrated: the runtime's login
// candidates fall back to the profile's CatalogID, which every OAuth-capable
// catalog profile carries, so a rename keeps the login reachable.
func RenameProvider(path string, oldName string, newName string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == "" || newName == "" {
		return FileConfig{}, fmt.Errorf("provider names are required")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := FileConfig{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}

	index := -1
	for i, provider := range cfg.Providers {
		providerName := strings.TrimSpace(provider.Name)
		if strings.EqualFold(providerName, oldName) {
			index = i
			continue
		}
		if strings.EqualFold(providerName, newName) {
			return FileConfig{}, fmt.Errorf("provider %q already exists", newName)
		}
	}
	if index < 0 {
		return FileConfig{}, fmt.Errorf("provider %q not found", oldName)
	}
	if strings.EqualFold(oldName, newName) && cfg.Providers[index].Name == newName {
		return cfg, nil
	}

	previousName := cfg.Providers[index].Name
	keyMigrated := false
	if cfg.Providers[index].APIKeyStored {
		if err := migrateStoredProviderKey(path, previousName, newName); err != nil {
			return FileConfig{}, fmt.Errorf("migrate stored key for %q: %w", oldName, err)
		}
		keyMigrated = true
	}
	if strings.EqualFold(strings.TrimSpace(cfg.ActiveProvider), strings.TrimSpace(previousName)) {
		cfg.ActiveProvider = newName
	}
	cfg.Providers[index].Name = newName
	if err := writeConfigFile(path, cfg); err != nil {
		if keyMigrated {
			// Compensate best-effort: config.json still names the OLD profile, so
			// move the key back where that config can find it — otherwise a failed
			// rewrite strands the key under a name no profile carries.
			_ = migrateStoredProviderKey(path, newName, previousName)
		}
		return FileConfig{}, err
	}
	return cfg, nil
}

// ProviderEdit is a field-level edit of one saved provider, applied by
// EditProvider in a single atomic write. Name is the CURRENT profile name
// (matched case-insensitively); NewName renames (case-only renames included).
// Empty BaseURL/Model/APIKey mean "leave unchanged"; Description is applied
// VERBATIM (the editor always knows the full desired text, so clearing works).
type ProviderEdit struct {
	Name         string
	NewName      string
	BaseURL      string
	Model        string
	APIKey       string
	APIKeyStored bool
	Description  string
}

// EditProvider applies a provider edit in ONE read-modify-write: rename
// (activeProvider follows; a stored key migrates, with a best-effort rollback
// if the config write fails), field updates, the stored-key marker, and the
// verbatim description. A single write keeps the operation atomic — the
// previous rename+upsert+describe sequence could fail halfway and leave
// config.json renamed while every in-memory consumer still held the old name —
// and, unlike UpsertProvider's exact-name merge, the case-insensitive match
// here makes a case-only rename (groq -> Groq) an in-place update instead of
// an appended duplicate profile.
func EditProvider(path string, edit ProviderEdit) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	oldName := strings.TrimSpace(edit.Name)
	if oldName == "" {
		return FileConfig{}, fmt.Errorf("provider name is required")
	}
	newName := strings.TrimSpace(edit.NewName)
	if newName == "" {
		newName = oldName
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := FileConfig{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}

	index := -1
	for i, provider := range cfg.Providers {
		providerName := strings.TrimSpace(provider.Name)
		if strings.EqualFold(providerName, oldName) {
			index = i
			continue
		}
		if strings.EqualFold(providerName, newName) {
			return FileConfig{}, fmt.Errorf("provider %q already exists", newName)
		}
	}
	if index < 0 {
		return FileConfig{}, fmt.Errorf("provider %q not found", oldName)
	}

	previousName := cfg.Providers[index].Name
	renamed := previousName != newName
	keyMigrated := false
	// A rename moves the stored key along: either the profile's existing entry,
	// or a replacement key the caller just captured — the contract is that a
	// captured key is stored under the CURRENT name before EditProvider runs, so
	// one migration covers both. migrateStoredProviderKey no-ops on case-only
	// renames (the store normalizes names), so it cannot delete the key it just
	// moved.
	if renamed && (cfg.Providers[index].APIKeyStored || edit.APIKeyStored) {
		if err := migrateStoredProviderKey(path, previousName, newName); err != nil {
			return FileConfig{}, fmt.Errorf("migrate stored key for %q: %w", oldName, err)
		}
		keyMigrated = true
	}
	if renamed && strings.EqualFold(strings.TrimSpace(cfg.ActiveProvider), strings.TrimSpace(previousName)) {
		cfg.ActiveProvider = newName
	}

	profile := &cfg.Providers[index]
	profile.Name = newName
	if baseURL := strings.TrimSpace(edit.BaseURL); baseURL != "" {
		profile.BaseURL = baseURL
	}
	if model := strings.TrimSpace(edit.Model); model != "" {
		profile.Model = model
	}
	if apiKey := strings.TrimSpace(edit.APIKey); apiKey != "" {
		profile.APIKey = apiKey
	}
	if edit.APIKeyStored {
		profile.APIKeyStored = true
	}
	profile.Description = strings.TrimSpace(edit.Description)

	if err := writeConfigFile(path, cfg); err != nil {
		if keyMigrated {
			// Compensate best-effort: config.json still names the OLD profile, so
			// move the key back where that config can find it.
			_ = migrateStoredProviderKey(path, newName, previousName)
		}
		return FileConfig{}, err
	}
	return cfg, nil
}

// migrateStoredProviderKey moves a credential-store entry to a new provider
// name: write-new-then-delete-old, so an interruption can leave a duplicate but
// never a missing key. A missing source entry is a no-op (the marker may be
// stale); only a failed WRITE aborts the rename.
func migrateStoredProviderKey(configPath string, oldName string, newName string) error {
	// The store normalizes names case-insensitively, so a case-only rename
	// (groq -> Groq) targets ONE entry: Set(new) rewrites it in place and
	// Delete(old) would then remove the key that was just "moved". Nothing to
	// migrate — the existing entry already serves the new name.
	if strings.EqualFold(strings.TrimSpace(oldName), strings.TrimSpace(newName)) {
		return nil
	}
	store, err := ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		return err
	}
	key, ok, err := store.Get(oldName)
	if err != nil {
		return err
	}
	if !ok || strings.TrimSpace(key) == "" {
		return nil
	}
	if err := store.Set(newName, key); err != nil {
		return err
	}
	_, _ = store.Delete(oldName)
	return nil
}

func SetProviderModel(path string, name string, model string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	name = strings.TrimSpace(name)
	if name == "" {
		return FileConfig{}, fmt.Errorf("provider name is required")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return FileConfig{}, fmt.Errorf("model is required")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg := FileConfig{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}

	for index := range cfg.Providers {
		if strings.EqualFold(cfg.Providers[index].Name, name) {
			cfg.Providers[index].Model = model
			if err := writeConfigFile(path, cfg); err != nil {
				return FileConfig{}, err
			}
			return cfg, nil
		}
	}

	return FileConfig{}, fmt.Errorf("provider %q not found", name)
}

func SetFavoriteModels(path string, models []string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()

	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg.Preferences.FavoriteModels = normalizeFavoriteModels(models)
	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// SetRecentModels persists the automatic recent-model-switch history,
// mirroring SetFavoriteModels (read-modify-atomic-write). Unlike favorites,
// order is preserved (newest first) rather than sorted, since it reflects
// switch recency, not an alphabetical preference list.
func SetRecentModels(path string, entries []RecentModelEntry) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()

	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg.Preferences.RecentModels = NormalizeRecentModels(entries)
	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// SetRecapsEnabled persists the idle recap preference, mirroring
// SetFavoriteModels (read-modify-atomic-write).
func SetRecapsEnabled(path string, enabled bool) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	v := enabled
	cfg.Preferences.Recaps = &v
	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// SetTheme persists the TUI theme preference, mirroring SetFavoriteModels
// (read-modify-atomic-write). A blank theme clears the stored preference.
func SetTheme(path string, theme string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg.Preferences.Theme = strings.TrimSpace(theme)
	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// SetPet persists only the terminal-pet preference while preserving every
// unrelated user setting through the config writer's atomic replace path.
func SetPet(path string, pet string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	cfg := FileConfig{}
	data := []byte("{}")
	if existing, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(existing, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
		data = existing
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	pet = strings.TrimSpace(pet)
	cfg.Preferences.Pet = pet
	data, err = setPetPreferenceJSON(data, pet)
	if err != nil {
		return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	if err := writeConfigData(path, data); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// SetSTTModel persists the dictation model and its provider, mirroring
// SetTheme (read-modify-atomic-write). provider must be one of the known STT
// provider kinds; a local provider stores the model as stt.localModelPath,
// otherwise as stt.model. A blank model clears the stored value for that slot.
func SetSTTModel(path string, provider STTProviderKind, model string) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	if provider != "" {
		cfg.STT.Provider = provider
	}
	model = strings.TrimSpace(model)
	if cfg.STT.STTProvider() == STTProviderLocal {
		cfg.STT.LocalModelPath = model
	} else {
		cfg.STT.Model = model
	}
	if err := validateSTTConfig(cfg.STT); err != nil {
		return FileConfig{}, err
	}
	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// SetSTTLocalEngine persists the paths of an auto-downloaded local engine +
// model and switches dictation to the local provider, mirroring SetTheme
// (read-modify-atomic-write). streaming selects the pipeline matching the
// downloaded model (a streaming transducer vs a batch model). Called after a
// download completes.
func SetSTTLocalEngine(path, binary, serverBinary, modelPath string, streaming bool) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg.STT.Provider = STTProviderLocal
	// Point BOTH pipelines at the local engine. Without resetting StreamProvider, a
	// previously-chosen cloud value (deepgram/openai) would still win in
	// buildStreamingTranscriber, so the live transcript would keep hitting the cloud
	// after the user switched to a downloaded local model.
	cfg.STT.StreamProvider = STTProviderLocal
	cfg.STT.LocalBinary = strings.TrimSpace(binary)
	cfg.STT.LocalServerBinary = strings.TrimSpace(serverBinary)
	cfg.STT.LocalModelPath = strings.TrimSpace(modelPath)
	// Match the pipeline to the downloaded model: a streaming transducer drives
	// the websocket server for a live transcript; a batch model uses the offline
	// binary.
	s := streaming
	cfg.STT.Streaming = &s
	if err := validateSTTConfig(cfg.STT); err != nil {
		return FileConfig{}, err
	}
	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

// SetSTTProvider persists just the dictation batch provider, mirroring SetTheme.
func SetSTTProvider(path string, provider STTProviderKind) (result FileConfig, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return FileConfig{}, fmt.Errorf("config path is required")
	}
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return FileConfig{}, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	cfg := FileConfig{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return FileConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return FileConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg.STT.Provider = provider
	if err := validateSTTConfig(cfg.STT); err != nil {
		return FileConfig{}, err
	}
	if err := writeConfigFile(path, cfg); err != nil {
		return FileConfig{}, err
	}
	return cfg, nil
}

func normalizeFavoriteModels(models []string) []string {
	seen := map[string]bool{}
	favorites := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		favorites = append(favorites, model)
	}
	sort.Strings(favorites)
	return favorites
}

// NormalizeRecentModels trims, drops entries with no model id, de-duplicates
// by provider+model pair (keeping the first/newest occurrence), and caps the
// result to MaxRecentModels. Order is preserved — the caller is responsible
// for passing entries newest-first. Exported so callers outside this package
// (e.g. the TUI, which keeps its own in-memory copy of recent history) apply
// the exact same normalization rules as the persisted config, instead of
// maintaining a second, independently-drifting copy of this logic.
func NormalizeRecentModels(entries []RecentModelEntry) []RecentModelEntry {
	seen := map[string]bool{}
	recent := make([]RecentModelEntry, 0, len(entries))
	for _, entry := range entries {
		provider := strings.TrimSpace(entry.Provider)
		model := strings.TrimSpace(entry.Model)
		if model == "" {
			continue
		}
		key := strings.ToLower(provider) + "\x00" + model
		if seen[key] {
			continue
		}
		seen[key] = true
		recent = append(recent, RecentModelEntry{Provider: provider, Model: model})
		if len(recent) >= MaxRecentModels {
			break
		}
	}
	return recent
}

func writeConfigFile(path string, cfg FileConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config JSON: %w", err)
	}
	return writeConfigData(path, data)
}

func writeConfigData(path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create config directory %s: %w", dir, err)
		}
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	// Write-to-temp + rename: an in-place write interrupted mid-way (crash,
	// disk full) would leave the user's only config truncated or corrupt.
	tmp, err := os.CreateTemp(dir, ".zero-config-*.tmp")
	if err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure config permissions %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Gitlawb/zero/internal/credstore"
	"github.com/Gitlawb/zero/internal/providercatalog"
)

// ProviderKeyStoreAt opens the encrypted credential store whose file backend lives
// in dir. The backend resolves keyring-first, then encrypted-file, with a plaintext
// opt-out via ZERO_CRED_STORAGE.
func ProviderKeyStoreAt(dir string) (*credstore.Store, error) {
	return credstore.New(credstore.Options{Dir: dir})
}

// ProviderKeyStore opens the credential store beside the default user config.
func ProviderKeyStore() (*credstore.Store, error) {
	configPath, err := DefaultUserConfigPath()
	if err != nil {
		return nil, err
	}
	return ProviderKeyStoreAt(filepath.Dir(configPath))
}

// ProviderKeyStoreForConfigPath opens the store beside configPath. An empty path
// retains the default-store behavior used by in-memory and ephemeral callers.
func ProviderKeyStoreForConfigPath(configPath string) (*credstore.Store, error) {
	if strings.TrimSpace(configPath) == "" {
		return ProviderKeyStore()
	}
	return ProviderKeyStoreAt(filepath.Dir(configPath))
}

// APIKeyGetter is the read surface of the credential store; *credstore.Store
// satisfies it. Kept here so callers can inject a fake in tests.
type APIKeyGetter interface {
	Get(provider string) (string, bool, error)
}

// APIKeySetter is the write surface of the credential store.
type APIKeySetter interface {
	Set(provider, key string) error
}

// SecureProviderProfile moves an inline APIKey on the profile into the credential
// store co-located with configPath, returning a profile with APIKeyStored set and
// APIKey cleared so the secret is never written to config.json. On any store error
// (e.g. no keyring) it returns the profile unchanged — the inline key persists and
// the startup migration retries later — so capturing a provider never fails on a
// flaky credential backend.
func SecureProviderProfile(profile ProviderProfile, configPath string) ProviderProfile {
	if strings.TrimSpace(profile.APIKey) == "" || strings.TrimSpace(profile.Name) == "" {
		return profile
	}
	store, err := ProviderKeyStoreAt(filepath.Dir(configPath))
	if err != nil {
		return profile
	}
	if err := store.Set(profile.Name, profile.APIKey); err != nil {
		return profile
	}
	secured := profile
	secured.APIKey = ""
	secured.APIKeyStored = true
	return secured
}

// PublishProviderCredential captures key into the credential store beside path
// and publishes the matching APIKeyStored marker for exactName as ONE operation,
// so a rejected publication cannot leave the user worse off than before the call.
//
// Hand-rolled Set-then-Mark sequences got this wrong in both directions: they
// wrote the secret before any validation could reject the config, and their
// rollback deleted the entry outright — destroying a working key that some
// other row (the store folds "openrouter" and "OPENROUTER" onto one entry) was
// still using. This validates first, snapshots whatever the store held, and on
// a marker failure restores that snapshot rather than deleting.
//
// exactName must be a persisted row's own spelling; callers holding user or
// session input resolve it with ResolvePersistedProviderName first.
func PublishProviderCredential(path string, exactName string, key string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("config path is required")
	}
	exactName = strings.TrimSpace(exactName)
	if exactName == "" {
		return fmt.Errorf("provider name is required")
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("api key is required")
	}
	_, err := runProviderProfileOperation(path, false, false, func(op *providerProfileOperation) error {
		index := -1
		for candidate := range op.config.Providers {
			if strings.TrimSpace(op.config.Providers[candidate].Name) == exactName {
				index = candidate
				break
			}
		}
		if index < 0 {
			return fmt.Errorf("provider %q not found", exactName)
		}
		if err := op.setKey(exactName, key); err != nil {
			return fmt.Errorf("store API key for %q: %w", exactName, err)
		}
		profile := &op.config.Providers[index]
		profile.APIKey = ""
		profile.APIKeyEnv = ""
		profile.APIKeyStored = true
		return nil
	})
	return err
}

// ForgetProviderKey removes a provider's stored API key and publishes the default
// user config with every apiKeyStored marker sharing its normalized credential
// identity cleared, in one transaction. It reports whether a credential existed.
// A missing config or invalid persisted provider names skip config publication.
func ForgetProviderKey(provider string) (bool, error) {
	path, err := DefaultUserConfigPath()
	if err != nil {
		return false, err
	}
	return DeleteProviderCredentials(path, []string{provider}, provider)
}

// StoreProviderCredential serializes a credential-only writer with provider
// profile transactions. It intentionally does not change config markers; STT
// setup uses the shared provider credential store without owning a profile row.
func StoreProviderCredential(path, provider, key string) error {
	_, err := runProviderProfileOperation(path, true, true, func(op *providerProfileOperation) error {
		op.publish = false
		return op.setKey(provider, key)
	})
	return err
}

// ClearProviderKeyStored unsets the APIKeyStored marker for a provider in the
// config at path, so credential checks no longer claim a stored key after one is
// removed. No-op when path/provider is empty, the config is absent, or the marker
// is already unset.
func ClearProviderKeyStored(path, provider string) (cleared bool, err error) {
	path = strings.TrimSpace(path)
	provider = strings.TrimSpace(provider)
	if path == "" || provider == "" {
		return false, nil
	}
	return clearProviderKeyStoredWhere(path, func(name string) bool {
		return strings.TrimSpace(name) == provider
	})
}

func clearProviderKeyStoredWhere(path string, matches func(string) bool) (bool, error) {
	changed := false
	_, err := runProviderProfileOperation(path, true, false, func(op *providerProfileOperation) error {
		for index := range op.config.Providers {
			if matches(op.config.Providers[index].Name) && op.config.Providers[index].APIKeyStored {
				op.config.Providers[index].APIKeyStored = false
				changed = true
			}
		}
		op.publish = changed
		return nil
	})
	return changed, err
}

// ClearProviderKeyStoredCaseVariants unsets the APIKeyStored marker on every
// row whose name normalizes to the same credential-store identity as provider,
// not just an exact-spelling match. Deleting the shared secret for one
// case-variant row (e.g. "WORK") must also clear the marker on any sibling row
// ("work") that pointed at the same now-gone entry — leaving it set would claim
// a key is available when ApplyStoredAPIKey's store lookup will always miss.
func ClearProviderKeyStoredCaseVariants(path, provider string) (bool, error) {
	path = strings.TrimSpace(path)
	provider = strings.TrimSpace(provider)
	if path == "" || provider == "" {
		return false, nil
	}
	providerIdentity := credstore.NormalizeProvider(provider)
	return clearProviderKeyStoredWhere(path, func(name string) bool {
		return credstore.NormalizeProvider(name) == providerIdentity
	})
}

// DeleteProviderCredentials deletes the candidate API-key entries and clears
// every marker sharing markerProvider's credential identity in one transaction.
func DeleteProviderCredentials(path string, candidates []string, markerProvider string) (bool, error) {
	removed := false
	_, err := runProviderProfileOperation(path, true, true, func(op *providerProfileOperation) error {
		var err error
		removed, err = op.deleteProviderCredentials(candidates, markerProvider)
		if err != nil {
			return err
		}
		if !op.exists || ValidatePersistedProviderNames(op.config) != nil {
			op.publish = false
		}
		return nil
	})
	return removed, err
}

// DeleteResolvedProviderCredentials resolves one persisted provider identity,
// deletes every exclusively owned credential candidate, and clears its stored
// marker under the provider config/key transaction lock.
func DeleteResolvedProviderCredentials(path, addressedName string) (removed bool, canonicalName string, err error) {
	_, err = runProviderProfileOperation(path, false, false, func(op *providerProfileOperation) error {
		row, match, resolveErr := resolvePersistedProviderIdentity(op.config.Providers, addressedName)
		if resolveErr != nil {
			return resolveErr
		}
		if match == PersistedIdentityNone {
			return fmt.Errorf("provider %q not found", strings.TrimSpace(addressedName))
		}
		// API-key ownership starts at the persisted row, not the requested OAuth
		// spelling: that spelling can also be a dictation-owned catalog key.
		candidates, canonical, resolveErr := providerCredentialCandidates(op.config, row.Name)
		if resolveErr != nil {
			return resolveErr
		}
		canonicalName = canonical
		removed, resolveErr = op.deleteProviderCredentials(candidates, canonicalName)
		return resolveErr
	})
	return removed, canonicalName, err
}

// RevokeProviderCredentials serializes explicit logout with provider mutations.
// Unlike reversible profile edits, revocation never restores deleted credentials
// because a later marker write failed. Unreadable config permits only the literal
// addressed credential, with an error explaining that aliases/markers were skipped.
func RevokeProviderCredentials(path, addressedName string, revokeOAuth func(string) (bool, error)) (removed bool, err error) {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(addressedName) == "" || revokeOAuth == nil {
		return false, fmt.Errorf("config path, provider name and OAuth revocation callback are required; credentials not revoked")
	}
	release, err := acquireProviderWriteLock(path)
	if err != nil {
		return false, fmt.Errorf("credentials not revoked: %w", err)
	}
	defer func() { err = errors.Join(err, release()) }()
	unlockConfig, err := lockConfigFileFn(path)
	if err != nil {
		return false, fmt.Errorf("credentials not revoked: %w", err)
	}
	defer func() { err = errors.Join(err, unlockConfig()) }()
	cfg, readErr := persistedFileConfig(path)
	candidates := []string{addressedName}
	keyCandidates := candidates
	canonical := ""
	if readErr == nil {
		candidates, canonical, err = providerCredentialCandidates(cfg, addressedName)
		if err != nil {
			return false, fmt.Errorf("credentials not revoked: %w", err)
		}
		keyCandidates = nil
		if canonical != "" {
			matches := 0
			for _, profile := range cfg.Providers {
				if SameProviderIdentity(profile.Name, canonical) {
					matches++
				}
			}
			if matches > 1 {
				return false, fmt.Errorf("credentials not revoked: ambiguous persisted provider names sharing %q", canonical)
			}
			keyCandidates, _, err = providerCredentialCandidates(cfg, canonical)
			if err != nil {
				return false, fmt.Errorf("credentials not revoked: %w", err)
			}
		}
	}
	for _, candidate := range candidates {
		deleted, deleteErr := revokeOAuth(candidate)
		removed = removed || deleted
		if deleteErr != nil {
			return removed, fmt.Errorf("credential revocation incomplete: %w", deleteErr)
		}
	}
	if len(keyCandidates) > 0 {
		store, storeErr := ProviderKeyStoreForConfigPath(path)
		if storeErr != nil {
			return removed, fmt.Errorf("API-key revocation incomplete: %w", storeErr)
		}
		for _, candidate := range keyCandidates {
			deleted, deleteErr := store.Delete(candidate)
			removed = removed || deleted
			if deleteErr != nil {
				return removed, fmt.Errorf("API-key revocation incomplete: %w", deleteErr)
			}
		}
	}
	if readErr != nil {
		return removed, fmt.Errorf("explicit credential revocation completed; config could not be read, so aliases and markers were not changed: %w", readErr)
	}
	changed := false
	for i := range cfg.Providers {
		if canonical != "" && SameProviderIdentity(cfg.Providers[i].Name, canonical) && cfg.Providers[i].APIKeyStored {
			cfg.Providers[i].APIKeyStored = false
			changed = true
		}
	}
	if changed {
		// A targeted revocation must not require repairing unrelated legacy rows.
		data, writeErr := json.MarshalIndent(cfg, "", "  ")
		if writeErr == nil {
			writeErr = writeConfigData(path, append(data, '\n'))
		}
		if writeErr != nil {
			return removed, fmt.Errorf("credentials revoked, but stored-key marker update failed: %w", writeErr)
		}
	}
	return removed, nil
}

func (op *providerProfileOperation) deleteProviderCredentials(candidates []string, markerProvider string) (bool, error) {
	removed := false
	for _, candidate := range candidates {
		candidateRemoved, err := op.deleteKey(candidate)
		if err != nil {
			return false, fmt.Errorf("delete stored key for %q: %w", candidate, err)
		}
		removed = removed || candidateRemoved
	}
	markerIdentity := credstore.NormalizeProvider(markerProvider)
	for index := range op.config.Providers {
		if credstore.NormalizeProvider(op.config.Providers[index].Name) == markerIdentity {
			op.config.Providers[index].APIKeyStored = false
		}
	}
	return removed, nil
}

// MigratePlaintextProviderKeys moves any inline plaintext API key in the config at
// path into the credential store, marking the profile APIKeyStored and stripping
// the inline secret — but ONLY after the store write succeeds, so a failed Set
// leaves the plaintext key in place and never strands a credential. Returns how
// many keys were migrated; a no-op (0, nil) when path is empty/absent or nothing
// needs migrating. Safe to run on every startup (idempotent).
func MigratePlaintextProviderKeys(path string, store APIKeySetter) (count int, err error) {
	path = strings.TrimSpace(path)
	if path == "" || store == nil {
		return 0, nil
	}
	// This runs on every startup, so it is the most likely writer to be racing
	// an interactive mutation in another Zero process.
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return 0, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return 0, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	if err := ValidatePersistedProviderNames(cfg); err != nil {
		return 0, err
	}
	migrated := 0
	for index := range cfg.Providers {
		profile := &cfg.Providers[index]
		key := strings.TrimSpace(profile.APIKey)
		if key == "" || strings.TrimSpace(profile.Name) == "" {
			continue
		}
		if err := store.Set(profile.Name, key); err != nil {
			// Leave the plaintext key untouched; a failed Set must not strand it.
			continue
		}
		profile.APIKey = ""
		profile.APIKeyStored = true
		migrated++
	}
	if migrated == 0 {
		return 0, nil
	}
	if err := writeConfigFile(path, cfg); err != nil {
		return migrated, fmt.Errorf("rewrite config after migration %s: %w", path, err)
	}
	return migrated, nil
}

// MigratePlaintextProviderKeysTransactional is the production startup migration.
// It serializes plaintext capture with every other provider config/key writer.
func MigratePlaintextProviderKeysTransactional(path string) (int, error) {
	migrated := 0
	var skipped error
	_, err := runProviderProfileOperation(path, true, false, func(op *providerProfileOperation) error {
		for index := range op.config.Providers {
			profile := &op.config.Providers[index]
			key := strings.TrimSpace(profile.APIKey)
			if key == "" || strings.TrimSpace(profile.Name) == "" {
				continue
			}
			if err := op.setKey(profile.Name, key); err != nil {
				// Leave the plaintext key untouched; a failed Set must not strand it.
				skipped = errors.Join(skipped, fmt.Errorf("migrate stored key for %q: %w", profile.Name, err))
				continue
			}
			profile.APIKey = ""
			profile.APIKeyStored = true
			migrated++
		}
		op.publish = migrated > 0
		return nil
	})
	return migrated, errors.Join(err, skipped)
}

// HasConfiguredCredential reports whether the profile is set up to authenticate
// with its own API key (inline, stored, or a raw auth header). It is the single
// definition of "this profile is key-authed", shared by OAuthLoginCandidates and
// the cli/tui setup surfaces so they never disagree about whether a profile is
// keyed. APIKeyEnv is deliberately NOT included: an env var may be unset at
// runtime while the profile actually relies on an OAuth login, so an
// env-configured-but-empty profile must still be allowed to resolve a token.
func (profile ProviderProfile) HasConfiguredCredential() bool {
	return strings.TrimSpace(profile.APIKey) != "" ||
		profile.APIKeyStored ||
		strings.TrimSpace(profile.AuthHeaderValue) != ""
}

// MissingCredentialEnv reports the environment variable this profile is
// expected to read an API key from, and whether that expectation is
// currently unmet. It is the single source of truth for "does this saved
// provider need a credential it doesn't have" — the CLI (`providers check`,
// `usableSavedProviders`) and TUI (`/providers` status, provider wizard
// summary) surfaces all delegate here so they never disagree about a
// profile's status.
//
// For the catalog's two "bring your own endpoint" descriptors
// (custom-openai-compatible, custom-anthropic-compatible), RequiresAuth is
// just a wizard template default, not a fact about the user's actual
// endpoint: a custom target may need no auth at all. So for those two only,
// a credential is expected exclusively when this profile carries its own
// explicit APIKeyEnv that differs from the catalog's guessed default — a
// value matching that default is indistinguishable from the wizard's old
// auto-stamp bug (issue #555) and is treated the same as unset, which also
// self-heals profiles saved before that bug was fixed.
func (profile ProviderProfile) MissingCredentialEnv() (string, bool) {
	if profile.HasConfiguredCredential() {
		return "", false
	}
	if catalogID := strings.TrimSpace(profile.CatalogID); catalogID != "" {
		descriptor, err := providercatalog.Require(catalogID)
		if err != nil {
			return "", false
		}
		defaultEnvVar := firstNonEmptyTrimmed(descriptor.AuthEnvVars...)
		if descriptor.Custom {
			envVar := strings.TrimSpace(profile.APIKeyEnv)
			if envVar == "" || envVar == defaultEnvVar {
				return "", false
			}
			return envVar, true
		}
		if !descriptor.RequiresAuth {
			return "", false
		}
		if envVar := strings.TrimSpace(profile.APIKeyEnv); envVar != "" {
			return envVar, true
		}
		return defaultEnvVar, true
	}

	switch profile.normalizedProviderKind() {
	case ProviderKindOpenAI, ProviderKindOpenAICompatible:
		return firstNonEmptyTrimmed(profile.APIKeyEnv, "OPENAI_API_KEY"), true
	case ProviderKindAnthropic, ProviderKindAnthropicCompat:
		return firstNonEmptyTrimmed(profile.APIKeyEnv, "ANTHROPIC_API_KEY"), true
	case ProviderKindGoogle:
		return firstNonEmptyTrimmed(profile.APIKeyEnv, "GEMINI_API_KEY"), true
	default:
		if envVar := strings.TrimSpace(profile.APIKeyEnv); envVar != "" {
			return envVar, true
		}
		return "", false
	}
}

// normalizedProviderKind resolves this profile's kind, falling back to the
// legacy Provider string field (lowercased) when ProviderKind is unset —
// matching how the resolver (resolver.go) treats the two fields as synonyms.
func (profile ProviderProfile) normalizedProviderKind() ProviderKind {
	if kind := strings.TrimSpace(string(profile.ProviderKind)); kind != "" {
		return ProviderKind(strings.ToLower(kind))
	}
	if provider := strings.TrimSpace(profile.Provider); provider != "" {
		return ProviderKind(strings.ToLower(provider))
	}
	return ""
}

func firstNonEmptyTrimmed(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// OAuthLoginCandidates returns the login names to try, in order, when resolving
// this profile's OAuth token — used by the runtime bearer resolver, the Codex
// account-header resolver, and the onboarding presence check so all three agree.
//
// It returns NO candidates for a profile that carries its own configured
// credential: attaching an OAuth resolver to such a profile makes withBearer
// erase the key the instant a token resolves, silently routing/billing the call
// through the OAuth account instead of the intended key (the cross-profile
// override the review caught — and, via the profile name, a same-name login
// overriding an explicit key). A stored-key profile that reaches this keyless
// (e.g. a transiently unreadable keyring) is likewise treated as key-auth, so it
// fails with a clear missing-credential error rather than silently borrowing an
// unrelated OAuth login. APIKeyEnv is intentionally not part of that gate (an
// env var may be unset while the profile relies on OAuth).
//
// For a keyless profile the profile name comes first (a login under your own
// profile name is unambiguously yours), then the catalog ID as a FALLBACK (so a
// profile renamed away from its catalog ID — e.g. {name:"codex",
// catalogID:"chatgpt"} — still finds a `zero auth login chatgpt` token).
// Candidates are trimmed, blank-skipped, and de-duplicated CASE-SENSITIVELY — the
// OAuth token store is a case-sensitive map, so "ChatGPT" and "chatgpt" are
// distinct keys and both must survive.
func (profile ProviderProfile) OAuthLoginCandidates() []string {
	if profile.HasConfiguredCredential() {
		return nil
	}
	var names []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		for _, existing := range names {
			if existing == s { // case-sensitive: the store lookup is case-sensitive
				return
			}
		}
		names = append(names, s)
	}
	add(profile.Name)
	add(profile.CatalogID)
	return names
}

// ApplyStoredAPIKey fills profile.APIKey from the credential store when it is not
// already resolved — an inline config key or a resolved APIKeyEnv always wins, so
// this only supplies a key for providers whose secret lives in the store. A nil
// store, a miss, or a store error leaves the profile unchanged.
func ApplyStoredAPIKey(profile ProviderProfile, store APIKeyGetter) ProviderProfile {
	// Only load for profiles that opted into stored-key auth (APIKeyStored). Without
	// this gate a stale keyring/file entry could silently reactivate credentials for a
	// profile that no longer uses the store.
	if store == nil || !profile.APIKeyStored || strings.TrimSpace(profile.APIKey) != "" {
		return profile
	}
	name := strings.TrimSpace(profile.Name)
	if name == "" {
		return profile
	}
	if key, ok, err := store.Get(name); err == nil && ok && strings.TrimSpace(key) != "" {
		profile.APIKey = key
	}
	return profile
}

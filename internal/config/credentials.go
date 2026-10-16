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
// opt-out via ZERO_CRED_STORAGE. The dir parameter exists so tests can point the file
// backend at a temp directory; production always uses the user config directory
// (ProviderKeyStore) because provider API keys are user-scoped by design — they are
// only ever captured under the user config, never project config (a cloned repo must
// not carry keys), so runtime lookups deliberately use the user store regardless of
// where a provider profile was resolved from.
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

// ForgetProviderKey removes a provider's stored API key from the credential store,
// reporting whether one existed. Used by the lifecycle "remove key" / auth logout.
func ForgetProviderKey(provider string) (bool, error) {
	store, err := ProviderKeyStore()
	if err != nil {
		return false, err
	}
	return store.Delete(provider)
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
	unlock, err := lockConfigFileFn(path)
	if err != nil {
		return false, err
	}
	// Joined, not chosen between: a release failure annotates the result
	// instead of masking the mutation error that actually explains what went
	// wrong. Reporting success after a failed unlock would claim a state the
	// next mutation cannot reproduce.
	defer func() { err = errors.Join(err, unlock()) }()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return false, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	changed := false
	for index := range cfg.Providers {
		if strings.EqualFold(strings.TrimSpace(cfg.Providers[index].Name), provider) && cfg.Providers[index].APIKeyStored {
			cfg.Providers[index].APIKeyStored = false
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	return true, writeConfigFile(path, cfg)
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

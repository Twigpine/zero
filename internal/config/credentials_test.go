package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeKeyGetter struct {
	keys map[string]string
	err  error
}

func (f fakeKeyGetter) Get(provider string) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	v, ok := f.keys[provider]
	return v, ok, nil
}

// fakeKeySetter records Set calls; setErr (when non-nil) makes Set fail.
type fakeKeySetter struct {
	keys   map[string]string
	setErr error
}

func (f *fakeKeySetter) Set(provider, key string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.keys[provider] = key
	return nil
}

func TestApplyStoredAPIKey(t *testing.T) {
	store := fakeKeyGetter{keys: map[string]string{"openai": "sk-stored"}}

	// Stored marker + empty key => filled from the store.
	got := ApplyStoredAPIKey(ProviderProfile{Name: "openai", APIKeyStored: true}, store)
	if got.APIKey != "sk-stored" {
		t.Fatalf("expected stored key to fill empty APIKey, got %q", got.APIKey)
	}

	// No APIKeyStored marker => store is NOT consulted (don't reactivate a stale key).
	got = ApplyStoredAPIKey(ProviderProfile{Name: "openai"}, store)
	if got.APIKey != "" {
		t.Fatalf("expected no load without APIKeyStored, got %q", got.APIKey)
	}

	// Inline key present => store is NOT consulted (inline wins).
	got = ApplyStoredAPIKey(ProviderProfile{Name: "openai", APIKeyStored: true, APIKey: "sk-inline"}, store)
	if got.APIKey != "sk-inline" {
		t.Fatalf("inline key must win, got %q", got.APIKey)
	}

	// Marker set but no stored key for this provider => unchanged (empty).
	got = ApplyStoredAPIKey(ProviderProfile{Name: "anthropic", APIKeyStored: true}, store)
	if got.APIKey != "" {
		t.Fatalf("expected no key for unstored provider, got %q", got.APIKey)
	}

	// Nil store => unchanged.
	got = ApplyStoredAPIKey(ProviderProfile{Name: "openai", APIKeyStored: true}, nil)
	if got.APIKey != "" {
		t.Fatalf("nil store must leave profile unchanged, got %q", got.APIKey)
	}

	// Empty name => unchanged (don't query the store).
	got = ApplyStoredAPIKey(ProviderProfile{APIKeyStored: true}, store)
	if got.APIKey != "" {
		t.Fatalf("empty name must not be filled, got %q", got.APIKey)
	}
}

func TestMigratePlaintextProviderKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfgJSON := `{"activeProvider":"openai","providers":[` +
		`{"name":"openai","apiKey":"sk-PLAINTEXT","model":"gpt"},` +
		`{"name":"local","baseURL":"http://localhost"},` +
		`{"name":"acme","apiKeyEnv":"ACME_KEY"}]}`
	if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &fakeKeySetter{keys: map[string]string{}}

	n, err := MigratePlaintextProviderKeys(path, store)
	if err != nil || n != 1 {
		t.Fatalf("migrate = %d,%v; want 1,nil", n, err)
	}
	if store.keys["openai"] != "sk-PLAINTEXT" {
		t.Fatalf("key not moved to store: %v", store.keys)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "sk-PLAINTEXT") {
		t.Fatalf("plaintext key still in config.json:\n%s", raw)
	}
	if !strings.Contains(string(raw), "apiKeyStored") {
		t.Fatalf("apiKeyStored marker not written:\n%s", raw)
	}
	// Idempotent: a second run migrates nothing.
	if n2, _ := MigratePlaintextProviderKeys(path, store); n2 != 0 {
		t.Fatalf("second migrate = %d, want 0", n2)
	}
}

func TestMigrateLeavesKeyWhenStoreSetFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"providers":[{"name":"openai","apiKey":"sk-KEEP"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &fakeKeySetter{keys: map[string]string{}, setErr: errors.New("keychain locked")}
	n, err := MigratePlaintextProviderKeys(path, store)
	if err != nil || n != 0 {
		t.Fatalf("migrate with failing store = %d,%v; want 0,nil", n, err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "sk-KEEP") {
		t.Fatalf("a failed Set must not strand the key; config.json:\n%s", raw)
	}
}

func TestMigratePlaintextProviderKeysValidatesBeforeStoreWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := []byte(`{"providers":[{"name":"","apiKey":"sk-implicit"},{"name":"openai","apiKey":"sk-openai"}]}`)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	store := &fakeKeySetter{keys: map[string]string{}}

	n, err := MigratePlaintextProviderKeys(path, store)
	if err == nil || !strings.Contains(err.Error(), "persisted provider name cannot be empty") {
		t.Fatalf("migrate = %d,%v; want validation error", n, err)
	}
	if n != 0 || len(store.keys) != 0 {
		t.Fatalf("invalid config mutated credential store: migrated=%d keyCount=%d", n, len(store.keys))
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(before) {
		t.Fatalf("invalid config was rewritten: beforeBytes=%d afterBytes=%d", len(before), len(after))
	}
}

func TestClearProviderKeyStored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"providers":[{"name":"openai","apiKeyStored":true},{"name":"other","apiKeyStored":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cleared, err := ClearProviderKeyStored(path, "openai")
	if err != nil || !cleared {
		t.Fatalf("clear = %v,%v; want true,nil", cleared, err)
	}
	raw, _ := os.ReadFile(path)
	// openai's marker is gone; other's remains.
	var cfg FileConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, p := range cfg.Providers {
		if p.Name == "openai" && p.APIKeyStored {
			t.Fatal("openai marker should be cleared")
		}
		if p.Name == "other" && !p.APIKeyStored {
			t.Fatal("other marker should be untouched")
		}
	}
	// Idempotent: clearing again reports no change.
	if cleared, _ := ClearProviderKeyStored(path, "openai"); cleared {
		t.Fatal("second clear should report no change")
	}
	// Unknown provider: no change.
	if cleared, _ := ClearProviderKeyStored(path, "nope"); cleared {
		t.Fatal("unknown provider should report no change")
	}
	if err := os.WriteFile(path, []byte(`{"providers":[{"name":"work","apiKeyStored":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cleared, err := ClearProviderKeyStored(path, "WORK"); err != nil || cleared {
		t.Fatalf("case-variant clear = %v,%v; want false,nil", cleared, err)
	}
	cfg = readConfigFixture(t, path)
	if !cfg.Providers[0].APIKeyStored {
		t.Fatalf("clear must require exact provider identity: %+v", cfg.Providers)
	}
}

func TestProviderProfileAPIKeyStoredRoundTrips(t *testing.T) {
	// The apiKeyStored marker survives JSON decode (custom UnmarshalJSON).
	var p ProviderProfile
	if err := p.UnmarshalJSON([]byte(`{"name":"openai","apiKeyStored":true}`)); err != nil {
		t.Fatal(err)
	}
	if !p.APIKeyStored {
		t.Fatal("expected apiKeyStored=true to decode")
	}
	if p.APIKey != "" {
		t.Fatalf("no inline key expected, got %q", p.APIKey)
	}
}

// OAuthLoginCandidates always offers the profile name, adds the catalog ID as a
// fallback ONLY when the profile has no effective own credential, and dedupes
// case-sensitively (the OAuth store is a case-sensitive map).
func TestOAuthLoginCandidates(t *testing.T) {
	cases := []struct {
		name    string
		profile ProviderProfile
		want    []string
	}{
		{
			name:    "renamed keyless profile falls back to catalog id",
			profile: ProviderProfile{Name: "codex", CatalogID: "chatgpt"},
			want:    []string{"codex", "chatgpt"},
		},
		{
			name:    "case-variant name keeps distinct catalog-id candidate",
			profile: ProviderProfile{Name: "ChatGPT", CatalogID: "chatgpt"},
			want:    []string{"ChatGPT", "chatgpt"},
		},
		{
			name:    "exact-duplicate name and catalog id collapse",
			profile: ProviderProfile{Name: "chatgpt", CatalogID: "chatgpt"},
			want:    []string{"chatgpt"},
		},
		{
			// A configured key must block ALL candidates (name included): a login
			// under the profile's own name would otherwise erase the key too.
			name:    "own inline key blocks every candidate",
			profile: ProviderProfile{Name: "anthropic-work", CatalogID: "anthropic", APIKey: "sk-work"},
			want:    nil,
		},
		{
			name:    "own auth header blocks every candidate",
			profile: ProviderProfile{Name: "acme", CatalogID: "anthropic", AuthHeaderValue: "Bearer x"},
			want:    nil,
		},
		{
			// APIKeyStored means key-auth even if the key isn't loaded here (e.g. a
			// transiently unreadable keyring): don't silently borrow an OAuth login.
			name:    "stored-key profile blocks every candidate",
			profile: ProviderProfile{Name: "openai-stored", CatalogID: "openai", APIKeyStored: true},
			want:    nil,
		},
		{
			// APIKeyEnv is NOT a configured credential for gating: the env var may be
			// unset while the profile relies on an OAuth login, so candidates stand.
			name:    "env-only profile still yields candidates",
			profile: ProviderProfile{Name: "xai", CatalogID: "xai", APIKeyEnv: "XAI_API_KEY"},
			want:    []string{"xai"},
		},
		{
			name:    "empty profile yields no candidates",
			profile: ProviderProfile{},
			want:    nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.profile.OAuthLoginCandidates()
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("OAuthLoginCandidates() = %#v, want %#v", got, c.want)
			}
		})
	}
}

func TestProviderProfileMissingCredentialEnv(t *testing.T) {
	cases := []struct {
		name    string
		profile ProviderProfile
		wantEnv string
		want    bool
	}{
		{
			name:    "catalog provider requiring auth",
			profile: ProviderProfile{Name: "groq", CatalogID: "groq"},
			wantEnv: "GROQ_API_KEY",
			want:    true,
		},
		{
			name:    "local catalog provider",
			profile: ProviderProfile{Name: "local", CatalogID: "ollama"},
			want:    false,
		},
		{
			// A profile saved against the hosted atomic-chat preset keeps its
			// remote base URL and key, so it must still be reported as missing a
			// credential. The keyless local runtime is a separate catalog ID
			// (atomic-chat-local) precisely so this identity is never repurposed.
			name:    "hosted atomic-chat profile still requires its key",
			profile: ProviderProfile{Name: "atomic-chat", CatalogID: "atomic-chat", BaseURL: "https://api.atomic.chat/v1"},
			wantEnv: "ATOMIC_CHAT_API_KEY",
			want:    true,
		},
		{
			name:    "local atomic chat runtime needs no credential",
			profile: ProviderProfile{Name: "atomic-local", CatalogID: "atomic-chat-local"},
			want:    false,
		},
		{
			name:    "credential resolved via inline key",
			profile: ProviderProfile{Name: "openai", ProviderKind: ProviderKindOpenAI, APIKey: "sk-test"},
			want:    false,
		},
		{
			// issue #555: a custom endpoint left with no credential means "no
			// auth needed" (e.g. a local llama.cpp server), not "missing".
			name: "custom openai compatible with no credential configured",
			profile: ProviderProfile{
				Name:      "local-llama",
				CatalogID: "custom-openai-compatible",
				BaseURL:   "http://192.168.1.50:8080/v1",
			},
			want: false,
		},
		{
			name: "custom openai compatible with explicit non-default api key env",
			profile: ProviderProfile{
				Name:      "local-llama",
				CatalogID: "custom-openai-compatible",
				APIKeyEnv: "LLAMA_CPP_API_KEY",
			},
			wantEnv: "LLAMA_CPP_API_KEY",
			want:    true,
		},
		{
			// Self-heal: a profile stamped by the pre-fix wizard with the
			// catalog's own guessed default is indistinguishable from that bug
			// and must not stay filtered out after upgrading.
			name: "custom openai compatible with stale legacy default env",
			profile: ProviderProfile{
				Name:      "local-llama",
				CatalogID: "custom-openai-compatible",
				APIKeyEnv: "OPENAI_API_KEY",
			},
			want: false,
		},
		{
			name:    "openai compatible without catalog falls back to provider kind",
			profile: ProviderProfile{Name: "custom", ProviderKind: ProviderKindOpenAICompatible},
			wantEnv: "OPENAI_API_KEY",
			want:    true,
		},
		{
			name:    "legacy Provider string field fallback",
			profile: ProviderProfile{Name: "legacy", Provider: "anthropic"},
			wantEnv: "ANTHROPIC_API_KEY",
			want:    true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotEnv, got := c.profile.MissingCredentialEnv()
			if got != c.want || gotEnv != c.wantEnv {
				t.Fatalf("MissingCredentialEnv() = (%q, %v), want (%q, %v)", gotEnv, got, c.wantEnv, c.want)
			}
		})
	}
}

func TestClearProviderKeyStoredCaseVariantsPreservesDistinctUnicodeIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"providers":[{"name":"s","apiKeyStored":true},{"name":"ſ","apiKeyStored":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cleared, err := ClearProviderKeyStoredCaseVariants(path, "s")
	if err != nil || !cleared {
		t.Fatalf("clear = %v,%v; want true,nil", cleared, err)
	}
	cfg := readConfigFixture(t, path)
	if cfg.Providers[0].APIKeyStored {
		t.Fatal("s marker should be cleared")
	}
	if !cfg.Providers[1].APIKeyStored {
		t.Fatal("long-s marker belongs to a distinct credential-store identity and must remain set")
	}
}

// Preflight rejection leaves an existing credential unchanged; this fixture
// never reaches marker publication or rollback.
func TestPublishProviderCredentialPreflightPreservesPreviousKey(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// Legacy duplicate rows are rejected before credential capture.
	original := []byte(`{"providers":[{"name":"openrouter","apiKeyStored":true},{"name":"OPENROUTER","apiKeyStored":true}]}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("openrouter", "sk-working"); err != nil {
		t.Fatal(err)
	}

	if err := PublishProviderCredential(path, "openrouter", "sk-new"); err == nil {
		t.Fatal("publication must be rejected for an ambiguous persisted config")
	}

	key, ok, err := store.Get("openrouter")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || key != "sk-working" {
		t.Fatalf("stored key does not match the previous value (present=%v, len=%d), want sk-working preserved", ok, len(key))
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Fatalf("config was rewritten by a rejected publication:\n%s", after)
	}
}

// Preflight rejection must not create a credential when no entry exists.
func TestPublishProviderCredentialPreflightDoesNotCreateEntry(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"providers":[{"name":"work"},{"name":"WORK"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PublishProviderCredential(path, "work", "sk-new"); err == nil {
		t.Fatal("publication must be rejected for an ambiguous persisted config")
	}
	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.Get("work"); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Fatal("rejected publication left an orphaned secret in the store")
	}
}

func TestPublishProviderCredentialStoresAndMarks(t *testing.T) {
	t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"providers":[{"name":"openrouter","apiKeyEnv":"OPENROUTER_API_KEY"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PublishProviderCredential(path, "openrouter", "sk-new"); err != nil {
		t.Fatal(err)
	}
	store, err := ProviderKeyStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, ok, err := store.Get("openrouter")
	if err != nil || !ok || key != "sk-new" {
		t.Fatalf("stored key does not match (present=%v, len=%d, err=%v), want sk-new", ok, len(key), err)
	}
	var cfg FileConfig
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.Providers[0].APIKeyStored || strings.TrimSpace(cfg.Providers[0].APIKeyEnv) != "" {
		t.Fatalf("marker not published: apiKeyStored=%v apiKeyEnv=%q", cfg.Providers[0].APIKeyStored, cfg.Providers[0].APIKeyEnv)
	}
}

func TestRepairUnnamedProviderStoredIdentity(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, destination := range []bool{false, true} {
			t.Run(fmt.Sprintf("shared=%t/destination=%t", shared, destination), func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("APPDATA", home)
				t.Setenv("LOCALAPPDATA", home)
				t.Setenv("XDG_CONFIG_HOME", home)
				t.Setenv("XDG_CACHE_HOME", home)
				t.Setenv("ZERO_CRED_STORAGE", "encrypted-file")
				store, err := ProviderKeyStore()
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Set("legacy", "original-key"); err != nil {
					t.Fatal(err)
				}
				if destination {
					if err := store.Set("work", "destination-key"); err != nil {
						t.Fatal(err)
					}
				}
				rows := []ProviderProfile{{ProviderKind: ProviderKindOpenAI, Model: "gpt-4o", APIKeyStored: true}}
				if shared {
					rows = append(rows, ProviderProfile{Name: "legacy", Model: "gpt-4o", APIKeyStored: true})
				}
				cfg := FileConfig{ActiveProvider: "legacy", Providers: rows}
				data, err := json.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "config.json")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				if _, _, err := RepairUnnamedProvider(path, "work"); err == nil || !strings.Contains(err.Error(), "stored credential") {
					t.Fatalf("identity-changing repair must refuse: %v", err)
				}
				after, err := os.ReadFile(path)
				if err != nil || string(after) != string(data) {
					t.Fatal("refusal changed config")
				}
				key, ok, err := store.Get("legacy")
				if err != nil || !ok || key != "original-key" {
					t.Fatal("old credential changed")
				}
				key, ok, err = store.Get("work")
				if err != nil || ok != destination || (ok && key != "destination-key") {
					t.Fatal("destination credential changed")
				}
				// Bare repair preserves the old identity, including the shared-owner merge.
				if _, _, err := RepairUnnamedProvider(path, ""); err != nil {
					t.Fatal(err)
				}
				resolved, err := Resolve(ResolveOptions{UserConfigPath: path, Env: map[string]string{}})
				if err != nil {
					t.Fatal(err)
				}
				profile := ApplyStoredAPIKey(resolved.Provider, store)
				if profile.Name != "legacy" || profile.APIKey != "original-key" {
					t.Fatal("repaired marker no longer retrieves the legacy credential")
				}
			})
		}
	}
}

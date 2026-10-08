package config

import (
	"encoding/json"
	"errors"
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

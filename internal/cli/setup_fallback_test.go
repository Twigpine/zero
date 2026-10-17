package cli

import (
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/oauth"
)

func TestFirstUsableProviderPrefersRemoteKeyed(t *testing.T) {
	withAuthStore(t)
	providers := []config.ProviderProfile{
		{Name: "ollama", CatalogID: "ollama", BaseURL: "http://localhost:11434/v1", APIKey: "k"},      // usable but local
		{Name: "moonshot", CatalogID: "moonshot", BaseURL: "https://api.moonshot.ai/v1", APIKey: "k"}, // usable, remote
		{Name: "xai", CatalogID: "xai", APIKeyEnv: "XAI_API_KEY"},                                     // not usable (env only, no inline key)
	}
	got, ok := firstUsableProvider(providers)
	if !ok || got.Name != "moonshot" {
		t.Fatalf("want remote keyed provider (moonshot), got %q ok=%v", got.Name, ok)
	}
}

func TestFirstUsableProviderFallsBackToLocal(t *testing.T) {
	withAuthStore(t)
	providers := []config.ProviderProfile{
		{Name: "xai", CatalogID: "xai", APIKeyEnv: "XAI_API_KEY"},                                // not usable
		{Name: "ollama", CatalogID: "ollama", BaseURL: "http://localhost:11434/v1", APIKey: "k"}, // local, usable
	}
	got, ok := firstUsableProvider(providers)
	if !ok || got.Name != "ollama" {
		t.Fatalf("want local usable fallback (ollama), got %q ok=%v", got.Name, ok)
	}
}

func TestFirstUsableProviderNoneUsable(t *testing.T) {
	withAuthStore(t)
	providers := []config.ProviderProfile{
		{Name: "xai", CatalogID: "xai", APIKeyEnv: "XAI_API_KEY"},
		{Name: "openai", CatalogID: "openai", APIKeyEnv: "OPENAI_API_KEY"},
	}
	if got, ok := firstUsableProvider(providers); ok {
		t.Fatalf("no provider has a credential, want ok=false, got %q", got.Name)
	}
}

// A keyless local proxy (chatgpt-proxy, RequiresAuth=false) is usable without a
// credential, so it can serve as a fallback rather than forcing onboarding.
func TestFirstUsableProviderAcceptsKeylessLocalProxy(t *testing.T) {
	// Isolate the OAuth token store: on a developer machine with a real xai
	// login, the env-only xai profile would count as usable and win over the
	// local proxy this test is about.
	withAuthStore(t)
	providers := []config.ProviderProfile{
		{Name: "xai", CatalogID: "xai", APIKeyEnv: "XAI_API_KEY"},
		{Name: "chatgpt", CatalogID: "chatgpt-proxy", BaseURL: "http://localhost:10531/v1"},
	}
	got, ok := firstUsableProvider(providers)
	if !ok || got.Name != "chatgpt" {
		t.Fatalf("want keyless local proxy fallback, got %q ok=%v", got.Name, ok)
	}
}

// A profile whose catalog entry no longer resolves and that carries no explicit
// BaseURL has no endpoint, so it must be skipped rather than selected as a
// fallback that fails at first use. A stale CatalogID with a BaseURL still works.
func TestFirstUsableProviderSkipsUnresolvableCatalogWithoutBaseURL(t *testing.T) {
	withAuthStore(t)
	providers := []config.ProviderProfile{
		{Name: "ghost", CatalogID: "no-such-catalog-entry", APIKey: "k"}, // unusable: no endpoint
		{Name: "custom", CatalogID: "no-such-catalog-entry", BaseURL: "https://api.custom.test/v1", APIKey: "k"},
	}
	got, ok := firstUsableProvider(providers)
	if !ok || got.Name != "custom" {
		t.Fatalf("want custom-endpoint fallback, got %q ok=%v", got.Name, ok)
	}
}

// An OAuth-only provider (no inline key, no env var) must be selectable as a
// fallback, matching setupRequired/usableSavedProviders — otherwise a fully
// authenticated user gets forced back into onboarding when activeProvider
// goes stale.
func TestFirstUsableProviderRecognizesOAuthLogin(t *testing.T) {
	withAuthStore(t)
	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Save(oauth.ProviderKey("xai"), oauth.Token{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	providers := []config.ProviderProfile{
		{Name: "xai", CatalogID: "xai", APIKeyEnv: "XAI_API_KEY"}, // no inline key/env, but logged in via OAuth
	}
	got, ok := firstUsableProvider(providers)
	if !ok || got.Name != "xai" {
		t.Fatalf("want OAuth-logged-in provider (xai), got %q ok=%v", got.Name, ok)
	}
}

func TestProviderProfileIsLocal(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		want    bool
	}{
		{"loopback name", "http://localhost:11434/v1", true},
		{"loopback v4", "http://127.0.0.1:8080", true},
		{"loopback v6", "http://[::1]:10531/v1", true},
		{"remote", "https://api.moonshot.ai/v1", false},
		{"contains-localhost-substring", "https://notlocalhost.com/v1", false},
		{"host-with-127-substring", "https://api127.0.0.1.example.com", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := providerProfileIsLocal(config.ProviderProfile{BaseURL: tc.baseURL})
			if got != tc.want {
				t.Fatalf("providerProfileIsLocal(%q) = %v, want %v", tc.baseURL, got, tc.want)
			}
		})
	}
}

// TestFirstUsableProviderAcceptsOAuthLoginProfile: a keyless catalog profile
// whose credential is a stored OAuth login (the shape the login flows persist)
// must be usable during active-pointer recovery — the same rule setupRequired
// and usableSavedProviders apply — instead of falling through to onboarding.
func TestFirstUsableProviderAcceptsOAuthLoginProfile(t *testing.T) {
	withAuthStore(t)
	providers := []config.ProviderProfile{
		{Name: "chatgpt", CatalogID: "chatgpt", BaseURL: "https://chatgpt.com/backend-api/codex", Model: "gpt-5.5"},
	}

	// No login stored: still not usable.
	if got, ok := firstUsableProvider(providers); ok {
		t.Fatalf("keyless profile without a login must not be usable, got %q", got.Name)
	}

	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		t.Fatalf("oauth store: %v", err)
	}
	if err := store.Save(oauth.ProviderKey("chatgpt"), oauth.Token{AccessToken: "bearer-123"}); err != nil {
		t.Fatalf("save token: %v", err)
	}

	got, ok := firstUsableProvider(providers)
	if !ok || got.Name != "chatgpt" {
		t.Fatalf("want OAuth-login profile to be usable, got %q ok=%v", got.Name, ok)
	}
}

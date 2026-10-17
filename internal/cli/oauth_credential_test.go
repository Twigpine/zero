package cli

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/oauth"
)

func TestProviderHasOAuthLogin(t *testing.T) {
	xai := config.ProviderProfile{Name: "xai", CatalogID: "xai"}
	if providerHasOAuthLogin(xai, nil) {
		t.Fatal("no stored login must be false")
	}
	if !providerHasOAuthLogin(xai, map[string]bool{"xai": true}) {
		t.Fatal("a stored login keyed by name must be true")
	}
	if !providerHasOAuthLogin(config.ProviderProfile{Name: "grok", CatalogID: "xai"}, map[string]bool{"xai": true}) {
		t.Fatal("a stored login keyed by catalog id must be true")
	}
}

func TestSetupRequiredRecognizesOAuthLogin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tok.json")
	t.Setenv("ZERO_OAUTH_STORAGE", "file") // an inherited "keyring" would ignore the temp path and hit the OS keychain
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", path)
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Save(oauth.ProviderKey("xai"), oauth.Token{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	// xai has no inline key (env-only) but IS logged in via OAuth → no setup.
	loggedIn := config.ResolvedConfig{Provider: config.ProviderProfile{Name: "xai", CatalogID: "xai", APIKeyEnv: "XAI_API_KEY"}}
	if setupRequired(loggedIn) {
		t.Fatal("a provider with a stored OAuth login must not require onboarding")
	}

	// A keyless provider with no OAuth login still requires setup.
	noLogin := config.ResolvedConfig{Provider: config.ProviderProfile{Name: "openai", CatalogID: "openai", APIKeyEnv: "OPENAI_API_KEY"}}
	if !setupRequired(noLogin) {
		t.Fatal("a keyless provider with no OAuth login must require onboarding")
	}
}

// A profile renamed away from its catalog ID must still get a runtime OAuth
// resolver when the login is stored under the catalog name — and a CASE-VARIANT
// profile name must not drop the catalog-ID candidate (the store is
// case-sensitive; a case-insensitive dedupe would swallow it).
func TestOAuthResolverForProfileFallsBackToCatalogID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tok.json")
	t.Setenv("ZERO_OAUTH_STORAGE", "file") // an inherited "keyring" would ignore the temp path and hit the OS keychain
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", path)
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Save(oauth.ProviderKey("chatgpt"), oauth.Token{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	// A renamed profile resolves the catalog-ID login, and the returned key MUST be
	// that login's key — the same key the Codex account-header resolver reads from,
	// so bearer and account can never come from different logins.
	if resolver, key := oauthLoginForProfile(config.ProviderProfile{Name: "codex", CatalogID: "chatgpt"}); resolver == nil {
		t.Fatal("renamed profile with a catalog-ID login must get a resolver (unauthenticated-children regression)")
	} else if key != oauth.ProviderKey("chatgpt") {
		t.Fatalf("bound key = %q, want the chatgpt login key (bearer/account must share it)", key)
	}
	// Name differs from catalog ID only in CASE: the exact-case store key is
	// "provider:chatgpt", so "chatgpt" must survive as a distinct candidate.
	if resolver, key := oauthLoginForProfile(config.ProviderProfile{Name: "ChatGPT", CatalogID: "chatgpt"}); resolver == nil {
		t.Fatal("case-variant profile name must still resolve the catalog-ID login (case-insensitive dedupe regression)")
	} else if key != oauth.ProviderKey("chatgpt") {
		t.Fatalf("case-variant bound key = %q, want the chatgpt login key", key)
	}
	if resolver, _ := oauthLoginForProfile(config.ProviderProfile{Name: "chatgpt", CatalogID: "chatgpt"}); resolver == nil {
		t.Fatal("catalog-named profile must get a resolver")
	}
	if resolver, key := oauthLoginForProfile(config.ProviderProfile{Name: "openai", CatalogID: "openai"}); resolver != nil || key != "" {
		t.Fatalf("a profile with no stored login must get no resolver and an empty key, got key=%q", key)
	}
}

// The catalog-ID fallback must NOT reach a sibling profile's OAuth login when
// this profile has its own key — withBearer would erase the key and silently
// bill the model call under the sibling's account (a real cross-profile
// override the fallback introduced). A profile WITH its own resolved key gets no
// resolver even when a catalog-shared login exists.
func TestOAuthResolverForProfileDoesNotOverrideExplicitKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tok.json")
	t.Setenv("ZERO_OAUTH_STORAGE", "file") // an inherited "keyring" would ignore the temp path and hit the OS keychain
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", path)
	store, err := oauth.NewStore(oauth.StoreOptions{FilePath: path})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	// A personal login stored under the catalog name.
	if err := store.Save(oauth.ProviderKey("anthropic"), oauth.Token{AccessToken: "personal-tok", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	// Work profile: same catalog, its OWN resolved API key, deliberately NOT the
	// personal OAuth account. It must keep using its key (no resolver attached).
	work := config.ProviderProfile{Name: "anthropic-work", CatalogID: "anthropic", APIKey: "sk-work-key"}
	if resolver, key := oauthLoginForProfile(work); resolver != nil || key != "" {
		t.Fatalf("a profile with its own API key must not get a catalog-shared OAuth resolver (cross-profile override), got key=%q", key)
	}
	// A raw auth-header credential must be protected the same way.
	header := config.ProviderProfile{Name: "anthropic-hdr", CatalogID: "anthropic", AuthHeaderValue: "Bearer byo"}
	if resolver, _ := oauthLoginForProfile(header); resolver != nil {
		t.Fatal("a profile with its own auth header must not get a catalog-shared OAuth resolver")
	}
	// A same-NAME login must not override an explicit key either (the name
	// candidate is gated on the profile being keyless, not just the catalog one).
	if err := store.Save(oauth.ProviderKey("anthropic-work"), oauth.Token{AccessToken: "name-tok", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("seed name token: %v", err)
	}
	if resolver, _ := oauthLoginForProfile(work); resolver != nil {
		t.Fatal("a profile with its own API key must not get an OAuth resolver even from a same-name login")
	}
	// Sanity: a keyless sibling on the same catalog DOES resolve the shared login.
	keyless := config.ProviderProfile{Name: "anthropic-oauth", CatalogID: "anthropic"}
	if resolver, key := oauthLoginForProfile(keyless); resolver == nil {
		t.Fatal("a keyless profile should still resolve the catalog-shared login")
	} else if key != oauth.ProviderKey("anthropic") {
		t.Fatalf("keyless sibling bound key = %q, want the catalog login key", key)
	}
}

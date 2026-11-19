package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/oauth"
	"github.com/Twigpine/zero/internal/providermodeldiscovery"
)

// A keyless ChatGPT profile authenticates headless discovery with its stored
// OAuth login: the live account probe only answers with the bearer and the
// account header present, and the discovered context window must come through
// instead of the generic fallback. Also pins that a profile with its own key
// never borrows the subscription login.
func TestDiscoveredModelContextWindowUsesChatGPTLogin(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		t.Setenv(key, root)
	}
	t.Setenv("ZERO_OAUTH_STORAGE", "file") // an inherited "keyring" would ignore the temp path and hit the OS keychain
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", filepath.Join(root, "oauth-tokens.json"))
	store, err := oauth.NewStore(oauth.StoreOptions{})
	if err != nil {
		t.Fatalf("oauth store: %v", err)
	}
	if err := store.Save(oauth.ProviderKey("chatgpt"), oauth.Token{
		AccessToken: "subscription-bearer", ExpiresAt: time.Now().Add(time.Hour), Account: "acct-123",
	}); err != nil {
		t.Fatalf("seed login: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api.json" {
			http.Error(w, "metadata unavailable", http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("Authorization") != "Bearer subscription-bearer" || r.Header.Get("chatgpt-account-id") != "acct-123" {
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-live-coder","visibility":"list","context_window":123456}]}`))
	}))
	defer server.Close()

	profile := config.ProviderProfile{
		Name: "chatgpt", CatalogID: "chatgpt", ProviderKind: config.ProviderKindOpenAICompatible,
		BaseURL: server.URL + "/backend-api/codex", Model: "gpt-live-coder",
	}
	// Keep the optional metadata enrichment off the network; the live probe is
	// what this test is about.
	options := providermodeldiscovery.Options{HTTPClient: server.Client(), ModelsDevURL: server.URL + "/api.json"}
	if window := discoveredModelContextWindow(context.Background(), profile, options); window != 123456 {
		t.Fatalf("context window = %d, want 123456 from the account-authenticated probe", window)
	}

	// A sibling profile that resolves its own key must not reach the stored
	// login; the endpoint rejects the key, so discovery yields no window.
	keyed := profile
	keyed.Name = "chatgpt-keyed"
	keyed.APIKey = "own-key"
	if window := discoveredModelContextWindow(context.Background(), keyed, options); window != 0 {
		t.Fatalf("keyed profile context window = %d, want 0 (must not borrow the OAuth login)", window)
	}
}

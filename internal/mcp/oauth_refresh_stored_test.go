package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/oauth"
)

const testServerIdentity = "0123456789abcdef0123456789abcdef"

// discoveryOAuthServer serves a standard `auth: oauth` setup: protected-resource
// metadata, authorization-server metadata with a registration endpoint, dynamic
// client registration, and a token endpoint that records refresh requests.
type discoveryOAuthServer struct {
	*httptest.Server
	refreshForms chan url.Values
}

func newDiscoveryOAuthServer(t *testing.T) *discoveryOAuthServer {
	t.Helper()
	fixture := &discoveryOAuthServer{refreshForms: make(chan url.Values, 4)}
	fixture.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		switch r.URL.Path {
		case "/mcp":
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+base+`/resource-metadata"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/resource-metadata":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resource":              base + "/mcp",
				"authorization_servers": []string{base},
			})
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 base,
				"authorization_endpoint": base + "/authorize",
				"token_endpoint":         base + "/token",
				"registration_endpoint":  base + "/register",
			})
		case "/register":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"client_id":     "registered-client",
				"client_secret": "registered-secret",
			})
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse token form: %v", err)
			}
			if r.Form.Get("grant_type") == "refresh_token" {
				fixture.refreshForms <- r.Form
				_ = json.NewEncoder(w).Encode(map[string]any{
					"access_token":  "access-refreshed",
					"refresh_token": "refresh-rotated",
					"token_type":    "Bearer",
					"expires_in":    3600,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "access-initial",
				"refresh_token": "refresh-initial",
				"token_type":    "Bearer",
				"expires_in":    3600,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.Close)
	return fixture
}

func driveLoopbackCallback(authURL string) error {
	parsed, err := url.Parse(authURL)
	if err != nil {
		return err
	}
	callbackURL := parsed.Query().Get("redirect_uri") + "?code=auth-code&state=" + url.QueryEscape(parsed.Query().Get("state"))
	go func() {
		for attempt := 0; attempt < 20; attempt++ {
			response, requestErr := http.Get(callbackURL)
			if requestErr == nil {
				response.Body.Close()
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()
	return nil
}

func TestLoginRecordsDiscoveredEndpointAndRegisteredClient(t *testing.T) {
	fixture := newDiscoveryOAuthServer(t)

	token, err := Login(context.Background(), LoginOptions{
		ServerName:  "notion-style",
		ServerURL:   fixture.URL + "/mcp",
		Config:      OAuthConfig{},
		HTTPClient:  fixture.Client(),
		OpenBrowser: driveLoopbackCallback,
		Timeout:     5 * time.Second,
		Now:         time.Now,
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if token.TokenEndpoint != fixture.URL+"/token" || !token.ProtectedTokenEndpoint {
		t.Fatalf("stored token endpoint = %q protected=%v, want the discovered protected endpoint", token.TokenEndpoint, token.ProtectedTokenEndpoint)
	}
	if token.ClientID != "registered-client" || token.ClientSecret != "registered-secret" {
		t.Fatalf("stored client = %q/%q, want the dynamically registered client", token.ClientID, token.ClientSecret)
	}
}

func TestLoginDoesNotCopyConfiguredClientOrEndpointIntoStore(t *testing.T) {
	fixture := newDiscoveryOAuthServer(t)

	token, err := Login(context.Background(), LoginOptions{
		ServerName: "configured",
		ServerURL:  fixture.URL + "/mcp",
		Config: OAuthConfig{
			ClientID:      "configured-client",
			ClientSecret:  "configured-secret",
			TokenEndpoint: fixture.URL + "/token",
		},
		HTTPClient:  fixture.Client(),
		OpenBrowser: driveLoopbackCallback,
		Timeout:     5 * time.Second,
		Now:         time.Now,
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if token.TokenEndpoint != "" || token.ProtectedTokenEndpoint || token.ClientID != "" || token.ClientSecret != "" {
		t.Fatalf("configured values were copied into the token store: %#v", token)
	}
}

// The standard `auth: oauth` setup configures no token endpoint or client, so
// refresh must work from what the discovery-based login stored.
func TestStoreTokenSourceRefreshUsesStoredDiscoveryAndClient(t *testing.T) {
	fixture := newDiscoveryOAuthServer(t)
	server := Server{Name: "notion-style", URL: fixture.URL + "/mcp", Identity: testServerIdentity, Auth: ServerAuthOAuth}

	loggedIn, err := Login(context.Background(), LoginOptions{
		ServerName:  server.Name,
		ServerURL:   server.URL,
		HTTPClient:  fixture.Client(),
		OpenBrowser: driveLoopbackCallback,
		Timeout:     5 * time.Second,
		Now:         time.Now,
	})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	store, err := NewTokenStore(TokenStoreOptions{FilePath: filepath.Join(t.TempDir(), "oauth-tokens.json")})
	if err != nil {
		t.Fatalf("NewTokenStore() error = %v", err)
	}
	if err := store.SaveForServer(server, loggedIn); err != nil {
		t.Fatalf("SaveForServer() error = %v", err)
	}

	source := &storeTokenSource{server: server, store: store, httpClient: http.DefaultClient, now: time.Now}
	access, err := source.Refresh(context.Background())
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if access != "access-refreshed" {
		t.Fatalf("Refresh() access token = %q", access)
	}
	form := <-fixture.refreshForms
	if form.Get("client_id") != "registered-client" || form.Get("client_secret") != "registered-secret" || form.Get("refresh_token") != "refresh-initial" {
		t.Fatalf("refresh form = %v, want the registered client and stored refresh token", form)
	}

	// The refresh must not drop what Login stored, or the next one would fail.
	saved, ok, err := store.LoadForServer(server)
	if err != nil || !ok {
		t.Fatalf("LoadForServer() = %v, %v", ok, err)
	}
	if saved.RefreshToken != "refresh-rotated" || saved.TokenEndpoint != fixture.URL+"/token" || !saved.ProtectedTokenEndpoint ||
		saved.ClientID != "registered-client" || saved.ClientSecret != "registered-secret" {
		t.Fatalf("refreshed token lost the stored endpoint or client: %#v", saved)
	}
	if _, err := source.Refresh(context.Background()); err != nil {
		t.Fatalf("second Refresh() error = %v", err)
	}
}

func TestRefreshSettingsConfigOverridesStored(t *testing.T) {
	cfg, client, err := refreshSettings(context.Background(), http.DefaultClient, "https://mcp.example.com/mcp",
		OAuthConfig{ClientID: "configured-client", ClientSecret: "configured-secret", TokenEndpoint: "https://configured.example.com/token"},
		StoredToken{TokenEndpoint: "https://stored.example.com/token", ProtectedTokenEndpoint: true, ClientID: "stored-client", ClientSecret: "stored-secret"})
	if err != nil {
		t.Fatalf("refreshSettings() error = %v", err)
	}
	if cfg.TokenEndpoint != "https://configured.example.com/token" || cfg.ClientID != "configured-client" || cfg.ClientSecret != "configured-secret" {
		t.Fatalf("config did not win over stored values: %#v", cfg)
	}
	if client != http.DefaultClient {
		t.Fatal("an explicitly configured endpoint must not switch to the protected-discovery client")
	}
}

func TestRefreshSettingsKeepsConfiguredSecretWithStoredClientID(t *testing.T) {
	cfg, _, err := refreshSettings(context.Background(), http.DefaultClient, "https://mcp.example.com/mcp",
		OAuthConfig{ClientSecret: "configured-secret"},
		StoredToken{ClientID: "stored-client", ClientSecret: "stored-secret"})
	if err != nil {
		t.Fatalf("refreshSettings() error = %v", err)
	}
	if cfg.ClientID != "stored-client" || cfg.ClientSecret != "configured-secret" {
		t.Fatalf("client = %q/%q, want stored id with the configured secret", cfg.ClientID, cfg.ClientSecret)
	}
}

func TestRefreshSettingsRejectsUnsafeStoredEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stored StoredToken
		want   error
	}{
		{
			name:   "cleartext endpoint",
			stored: StoredToken{TokenEndpoint: "http://auth.example.com/token", ClientID: "c"},
			want:   oauth.ErrInsecureTokenEndpoint,
		},
		{
			name:   "advertised loopback endpoint",
			stored: StoredToken{TokenEndpoint: "https://127.0.0.1/token", ProtectedTokenEndpoint: true, ClientID: "c"},
			want:   errUnsafeOAuthDiscoveryTarget,
		},
		{
			name:   "advertised private endpoint",
			stored: StoredToken{TokenEndpoint: "https://10.0.0.5/token", ProtectedTokenEndpoint: true, ClientID: "c"},
			want:   errUnsafeOAuthDiscoveryTarget,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := refreshSettings(context.Background(), http.DefaultClient, "https://mcp.example.com/mcp", OAuthConfig{}, tc.stored)
			if !errors.Is(err, tc.want) {
				t.Fatalf("refreshSettings() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestStoreTokenSourceRefreshDoesNotPostToUnsafeStoredEndpoint(t *testing.T) {
	var hits atomic.Int64
	loopback := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer loopback.Close()

	// A public MCP resource must not be able to point refresh at loopback.
	server := Server{Name: "public", URL: "https://mcp.example.com/mcp", Identity: testServerIdentity, Auth: ServerAuthOAuth}
	store, err := NewTokenStore(TokenStoreOptions{FilePath: filepath.Join(t.TempDir(), "oauth-tokens.json")})
	if err != nil {
		t.Fatalf("NewTokenStore() error = %v", err)
	}
	if err := store.SaveForServer(server, StoredToken{
		AccessToken: "a", RefreshToken: "r", ClientID: "c",
		TokenEndpoint: loopback.URL + "/token", ProtectedTokenEndpoint: true,
	}); err != nil {
		t.Fatalf("SaveForServer() error = %v", err)
	}
	source := &storeTokenSource{server: server, store: store, httpClient: http.DefaultClient, now: time.Now}
	if _, err := source.Refresh(context.Background()); !errors.Is(err, errUnsafeOAuthDiscoveryTarget) {
		t.Fatalf("Refresh() error = %v, want the discovery policy refusal", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("loopback endpoint received %d request(s)", hits.Load())
	}
}

func TestStoredTokenPersistsDiscoveryFields(t *testing.T) {
	store, err := NewTokenStore(TokenStoreOptions{FilePath: filepath.Join(t.TempDir(), "oauth-tokens.json")})
	if err != nil {
		t.Fatalf("NewTokenStore() error = %v", err)
	}
	want := StoredToken{
		AccessToken: "a", RefreshToken: "r",
		TokenEndpoint: "https://auth.example.com/token", ProtectedTokenEndpoint: true,
		ClientID: "client", ClientSecret: "secret",
	}
	if err := store.Save("demo", want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, ok, err := store.Load("demo")
	if err != nil || !ok {
		t.Fatalf("Load() = %v, %v", ok, err)
	}
	if got.TokenEndpoint != want.TokenEndpoint || !got.ProtectedTokenEndpoint || got.ClientID != want.ClientID || got.ClientSecret != want.ClientSecret {
		t.Fatalf("round trip lost discovery fields: %#v", got)
	}
	statuses, err := store.Status()
	if err != nil {
		t.Fatalf("Status() error = %v", err)
	}
	encoded, _ := json.Marshal(statuses)
	for _, secret := range []string{"secret", "https://auth.example.com/token"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("status output leaked %q: %s", secret, encoded)
		}
	}
}

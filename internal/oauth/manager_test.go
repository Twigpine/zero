package oauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProvider is an httptest server that plays a token + device endpoint.
type fakeProvider struct {
	server    *httptest.Server
	tokenHits atomic.Int32
}

func newFakeProvider(t *testing.T, tokenJSON string) *fakeProvider {
	t.Helper()
	fp := &fakeProvider{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		fp.tokenHits.Add(1)
		_, _ = io.WriteString(w, tokenJSON)
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, _ *http.Request) {
		// Approve immediately on poll; short interval so the test is fast.
		_, _ = io.WriteString(w, `{"device_code":"dc","user_code":"U-1","verification_uri":"https://example/dev","expires_in":600,"interval":1}`)
	})
	fp.server = httptest.NewServer(mux)
	t.Cleanup(fp.server.Close)
	return fp
}

func managerFor(t *testing.T, env map[string]string, openBrowser func(string) error) *Manager {
	t.Helper()
	store, err := NewStore(StoreOptions{FilePath: filepath.Join(t.TempDir(), "tok.json")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	m, err := NewManager(ManagerOptions{
		Store:       store,
		Env:         env,
		OpenBrowser: openBrowser,
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

// AllowPresets layers the opt-in onto the manager's env so a login/refresh for a
// preset provider resolves; without it (and no Env) the env stays nil so callers
// and tests remain hermetic.
func TestNewManagerAllowPresetsForcesOptIn(t *testing.T) {
	store, err := NewStore(StoreOptions{FilePath: filepath.Join(t.TempDir(), "tok.json")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	m, err := NewManager(ManagerOptions{Store: store, AllowPresets: true})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if m.env["ZERO_OAUTH_ALLOW_PRESETS"] != "1" {
		t.Fatalf("AllowPresets should force the opt-in; got env[%q]=%q", "ZERO_OAUTH_ALLOW_PRESETS", m.env["ZERO_OAUTH_ALLOW_PRESETS"])
	}
	m2, err := NewManager(ManagerOptions{Store: store})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if m2.env != nil {
		t.Fatalf("env should stay nil without AllowPresets, got %v", m2.env)
	}
}

func TestManagerLoginLoopback(t *testing.T) {
	fp := newFakeProvider(t, `{"access_token":"at","refresh_token":"rt","token_type":"Bearer","expires_in":3600}`)
	env := map[string]string{
		"ZERO_OAUTH_DEMO_CLIENT_ID":     "client",
		"ZERO_OAUTH_DEMO_AUTHORIZE_URL": "https://auth.example.com/authorize",
		"ZERO_OAUTH_DEMO_TOKEN_URL":     fp.server.URL + "/token",
	}
	// The fake browser drives the loopback redirect with the captured state.
	openBrowser := func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		redirect := u.Query().Get("redirect_uri")
		state := u.Query().Get("state")
		_, err = http.Get(redirect + "?code=the-code&state=" + url.QueryEscape(state))
		return err
	}
	m := managerFor(t, env, openBrowser)

	status, err := m.Login(context.Background(), LoginOptions{Provider: "demo"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !status.HasToken || status.Key != ProviderKey("demo") {
		t.Fatalf("status = %+v", status)
	}
	// The token is persisted and GetFresh returns it without a needless refresh.
	access, err := m.GetFresh(context.Background(), ProviderKey("demo"))
	if err != nil {
		t.Fatalf("GetFresh: %v", err)
	}
	if access != "at" {
		t.Fatalf("access = %q, want at", access)
	}
}

func TestManagerLoginDevice(t *testing.T) {
	fp := newFakeProvider(t, `{"access_token":"dev-at","token_type":"Bearer","expires_in":3600}`)
	env := map[string]string{
		"ZERO_OAUTH_DEMODEV_CLIENT_ID":  "client",
		"ZERO_OAUTH_DEMODEV_TOKEN_URL":  fp.server.URL + "/token",
		"ZERO_OAUTH_DEMODEV_DEVICE_URL": fp.server.URL + "/device",
	}
	m := managerFor(t, env, nil)
	status, err := m.Login(context.Background(), LoginOptions{Provider: "demodev", Device: true})
	if err != nil {
		t.Fatalf("device Login: %v", err)
	}
	if !status.HasToken {
		t.Fatalf("device status = %+v", status)
	}
}

func TestManagerPrepareAndCompleteDeviceLogin(t *testing.T) {
	fp := newFakeProvider(t, `{"access_token":"two-phase-at","token_type":"Bearer","expires_in":3600}`)
	env := map[string]string{
		"ZERO_OAUTH_DEMO2_CLIENT_ID":  "client",
		"ZERO_OAUTH_DEMO2_TOKEN_URL":  fp.server.URL + "/token",
		"ZERO_OAUTH_DEMO2_DEVICE_URL": fp.server.URL + "/device",
	}
	m := managerFor(t, env, nil)

	// Phase 1: request the device code (what the UI displays to the user).
	auth, cfg, err := m.PrepareDeviceLogin(context.Background(), LoginOptions{Provider: "demo2"})
	if err != nil {
		t.Fatalf("PrepareDeviceLogin: %v", err)
	}
	if auth.UserCode != "U-1" || auth.VerificationURI == "" {
		t.Fatalf("device auth missing user code/uri: %+v", auth)
	}

	// Phase 2: poll for the token and persist it.
	status, err := m.CompleteDeviceLogin(context.Background(), "demo2", cfg, auth)
	if err != nil {
		t.Fatalf("CompleteDeviceLogin: %v", err)
	}
	if !status.HasToken || status.Key != ProviderKey("demo2") {
		t.Fatalf("status = %+v", status)
	}
	access, err := m.GetFresh(context.Background(), ProviderKey("demo2"))
	if err != nil {
		t.Fatalf("GetFresh: %v", err)
	}
	if access != "two-phase-at" {
		t.Fatalf("access = %q, want two-phase-at", access)
	}
}

func TestManagerBeforeSaveFailureLeavesStoreUnchanged(t *testing.T) {
	beforeSaveErr := errors.New("config changed before save")
	for _, test := range []struct {
		name string
		run  func(context.Context, *Manager) error
	}{
		{
			name: "Login",
			run: func(ctx context.Context, manager *Manager) error {
				_, err := manager.Login(ctx, LoginOptions{Provider: "demo", Device: true})
				return err
			},
		},
		{
			name: "CompleteDeviceLogin",
			run: func(ctx context.Context, manager *Manager) error {
				auth, cfg, err := manager.PrepareDeviceLogin(ctx, LoginOptions{Provider: "demo"})
				if err != nil {
					return err
				}
				_, err = manager.CompleteDeviceLogin(ctx, "demo", cfg, auth)
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fp := newFakeProvider(t, `{"access_token":"must-not-be-saved","token_type":"Bearer","expires_in":3600}`)
			env := map[string]string{
				"ZERO_OAUTH_DEMO_CLIENT_ID":  "client",
				"ZERO_OAUTH_DEMO_TOKEN_URL":  fp.server.URL + "/token",
				"ZERO_OAUTH_DEMO_DEVICE_URL": fp.server.URL + "/device",
			}
			store, err := NewStore(StoreOptions{FilePath: filepath.Join(t.TempDir(), "tok.json")})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Save(ProviderKey("demo"), Token{AccessToken: "keep"}); err != nil {
				t.Fatal(err)
			}
			manager, err := NewManager(ManagerOptions{Store: store, Env: env, BeforeSave: func() error { return beforeSaveErr }})
			if err != nil {
				t.Fatal(err)
			}
			if err := test.run(context.Background(), manager); !errors.Is(err, beforeSaveErr) {
				t.Fatalf("error = %v, want exact BeforeSave error", err)
			}
			if token, ok, err := store.Load(ProviderKey("demo")); err != nil || !ok || token.AccessToken != "keep" {
				t.Fatalf("existing provider token was replaced: token=%+v ok=%v err=%v", token, ok, err)
			}
		})
	}
}

// A provider configured with only an issuer URL (no TOKEN_URL) must still be
// refreshable: GetFresh resolves the token endpoint via discovery before
// refreshing. Guards resolveConfigForKey calling resolveEndpoints.
func TestManagerGetFreshDiscoversIssuerEndpoints(t *testing.T) {
	var tokenHits int32
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"issuer":"`+server.URL+`","token_endpoint":"`+server.URL+`/token","authorization_endpoint":"`+server.URL+`/authorize"}`)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&tokenHits, 1)
		_, _ = io.WriteString(w, `{"access_token":"refreshed-at","token_type":"Bearer","expires_in":3600}`)
	})
	env := map[string]string{
		"ZERO_OAUTH_ISSUERONLY_CLIENT_ID":  "client",
		"ZERO_OAUTH_ISSUERONLY_ISSUER_URL": server.URL, // no TOKEN_URL → discovered
	}
	m := managerFor(t, env, nil)
	if err := m.store.Save(ProviderKey("issueronly"), Token{AccessToken: "stale", RefreshToken: "rt", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := m.GetFresh(context.Background(), ProviderKey("issueronly"))
	if err != nil {
		t.Fatalf("GetFresh (issuer-only) must discover the token endpoint and refresh: %v", err)
	}
	if got != "refreshed-at" {
		t.Fatalf("access = %q, want refreshed-at", got)
	}
	if atomic.LoadInt32(&tokenHits) == 0 {
		t.Fatal("token endpoint never hit — discovery/refresh did not run")
	}
}

// A discovery document that serves an insecure (non-https, non-loopback)
// endpoint must be rejected before it is merged, so discovery metadata cannot
// silently downgrade the login to an attacker-controlled origin (issue #511).
func TestResolveEndpointsRejectsInsecureDiscoveredEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		// Loopback issuer, but an attacker-controlled http authorization endpoint.
		_, _ = io.WriteString(w, `{"issuer":"`+server.URL+`","token_endpoint":"`+server.URL+`/token","authorization_endpoint":"http://evil.example/authorize"}`)
	})
	m := managerFor(t, map[string]string{}, nil)
	_, err := m.resolveEndpoints(context.Background(), Config{IssuerURL: server.URL})
	if !errors.Is(err, ErrInsecureTokenEndpoint) {
		t.Fatalf("resolveEndpoints err = %v, want ErrInsecureTokenEndpoint", err)
	}
}

func TestManagerGetFreshRefreshesExpired(t *testing.T) {
	fp := newFakeProvider(t, `{"access_token":"fresh-at","expires_in":3600}`)
	env := map[string]string{
		"ZERO_OAUTH_DEMO_CLIENT_ID": "client",
		"ZERO_OAUTH_DEMO_TOKEN_URL": fp.server.URL + "/token",
	}
	m := managerFor(t, env, nil)
	// Seed an expired token with a refresh token.
	if err := m.store.Save(ProviderKey("demo"), Token{AccessToken: "stale", RefreshToken: "rt", ExpiresAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	access, err := m.GetFresh(context.Background(), ProviderKey("demo"))
	if err != nil {
		t.Fatalf("GetFresh: %v", err)
	}
	if access != "fresh-at" {
		t.Fatalf("access = %q, want fresh-at (refreshed)", access)
	}
	if fp.tokenHits.Load() != 1 {
		t.Fatalf("token endpoint hit %d times, want 1 refresh", fp.tokenHits.Load())
	}
	// The refreshed token is persisted.
	stored, _, _ := m.store.Load(ProviderKey("demo"))
	if stored.AccessToken != "fresh-at" {
		t.Fatalf("stored token not updated: %+v", stored)
	}
}

func TestManagerGetFreshSkipsValidToken(t *testing.T) {
	fp := newFakeProvider(t, `{"access_token":"should-not-be-used"}`)
	env := map[string]string{
		"ZERO_OAUTH_DEMO_CLIENT_ID": "client",
		"ZERO_OAUTH_DEMO_TOKEN_URL": fp.server.URL + "/token",
	}
	m := managerFor(t, env, nil)
	_ = m.store.Save(ProviderKey("demo"), Token{AccessToken: "valid", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour)})
	access, err := m.GetFresh(context.Background(), ProviderKey("demo"))
	if err != nil {
		t.Fatalf("GetFresh: %v", err)
	}
	if access != "valid" || fp.tokenHits.Load() != 0 {
		t.Fatalf("a valid token must not be refreshed (access=%q hits=%d)", access, fp.tokenHits.Load())
	}
}

func TestManagerHandle401ForcesRefresh(t *testing.T) {
	fp := newFakeProvider(t, `{"access_token":"after-401"}`)
	env := map[string]string{
		"ZERO_OAUTH_DEMO_CLIENT_ID": "client",
		"ZERO_OAUTH_DEMO_TOKEN_URL": fp.server.URL + "/token",
	}
	m := managerFor(t, env, nil)
	// Token is still valid by clock, but Handle401 forces a refresh anyway.
	_ = m.store.Save(ProviderKey("demo"), Token{AccessToken: "valid", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour)})
	access, err := m.Handle401(context.Background(), ProviderKey("demo"))
	if err != nil {
		t.Fatalf("Handle401: %v", err)
	}
	if access != "after-401" || fp.tokenHits.Load() != 1 {
		t.Fatalf("Handle401 must force a refresh (access=%q hits=%d)", access, fp.tokenHits.Load())
	}
}

func TestManagerLogout(t *testing.T) {
	m := managerFor(t, map[string]string{"ZERO_OAUTH_DEMO_CLIENT_ID": "c"}, nil)
	_ = m.store.Save(ProviderKey("demo"), Token{AccessToken: "a"})
	removed, err := m.Logout("demo")
	if err != nil || !removed {
		t.Fatalf("Logout = %v %v", removed, err)
	}
	if removed2, _ := m.Logout("demo"); removed2 {
		t.Fatal("second logout should report nothing removed")
	}
}

func TestCompleteDeviceLoginBeforeSaveFailurePreservesPreviousToken(t *testing.T) {
	fp := newFakeProvider(t, `{"access_token":"replacement","refresh_token":"replacement-refresh","expires_in":3600}`)
	store, err := NewStore(StoreOptions{FilePath: filepath.Join(t.TempDir(), "oauth.json")})
	if err != nil {
		t.Fatal(err)
	}
	key := ProviderKey("demo")
	previous := Token{AccessToken: "previous", RefreshToken: "previous-refresh"}
	if err := store.Save(key, previous); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("config changed during login")
	manager, err := NewManager(ManagerOptions{
		Store:      store,
		HTTPClient: fp.server.Client(),
		BeforeSave: func() error { return wantErr },
	})
	if err != nil {
		t.Fatal(err)
	}
	auth := DeviceAuth{DeviceCode: "device", ExpiresAt: time.Now().Add(time.Minute), Interval: time.Millisecond}
	_, err = manager.CompleteDeviceLogin(context.Background(), "demo", Config{TokenEndpoint: fp.server.URL + "/token", ClientID: "client"}, auth)
	if !errors.Is(err, wantErr) {
		t.Fatalf("CompleteDeviceLogin error = %v, want pre-save rejection", err)
	}
	stored, ok, err := store.Load(key)
	if err != nil || !ok || stored.AccessToken != previous.AccessToken || stored.RefreshToken != previous.RefreshToken {
		t.Fatalf("pre-save rejection changed token: ok=%v err=%v token=%+v", ok, err, stored)
	}
}

// The browser (loopback) flow runs the same pre-save check as the device flow.
func TestLoginLoopbackBeforeSaveFailurePreservesPreviousToken(t *testing.T) {
	fp := newFakeProvider(t, `{"access_token":"replacement","refresh_token":"replacement-refresh","expires_in":3600}`)
	store, err := NewStore(StoreOptions{FilePath: filepath.Join(t.TempDir(), "oauth.json")})
	if err != nil {
		t.Fatal(err)
	}
	key := ProviderKey("demo")
	previous := Token{AccessToken: "previous", RefreshToken: "previous-refresh"}
	if err := store.Save(key, previous); err != nil {
		t.Fatal(err)
	}
	openBrowser := func(authURL string) error {
		u, err := url.Parse(authURL)
		if err != nil {
			return err
		}
		redirect := u.Query().Get("redirect_uri")
		state := u.Query().Get("state")
		_, err = http.Get(redirect + "?code=the-code&state=" + url.QueryEscape(state))
		return err
	}
	wantErr := errors.New("config changed during login")
	manager, err := NewManager(ManagerOptions{
		Store: store,
		Env: map[string]string{
			"ZERO_OAUTH_DEMO_CLIENT_ID":     "client",
			"ZERO_OAUTH_DEMO_AUTHORIZE_URL": "https://auth.example.com/authorize",
			"ZERO_OAUTH_DEMO_TOKEN_URL":     fp.server.URL + "/token",
		},
		OpenBrowser: openBrowser,
		BeforeSave:  func() error { return wantErr },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Login(context.Background(), LoginOptions{Provider: "demo"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Login error = %v, want pre-save rejection", err)
	}
	if fp.tokenHits.Load() == 0 {
		t.Fatal("Login rejected before the token exchange; the pre-save check was not reached")
	}
	stored, ok, err := store.Load(key)
	if err != nil || !ok || stored.AccessToken != previous.AccessToken || stored.RefreshToken != previous.RefreshToken {
		t.Fatalf("pre-save rejection changed token: ok=%v err=%v token=%+v", ok, err, stored)
	}
}

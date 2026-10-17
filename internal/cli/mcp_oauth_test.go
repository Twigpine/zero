package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/workspacetrust"
)

// syncBuffer is a goroutine-safe writer used when a background goroutine reads
// CLI output while the command is still writing it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRunMCPOAuthStatusReportsPresenceWithoutToken(t *testing.T) {
	store, err := mcp.NewTokenStore(mcp.TokenStoreOptions{
		FilePath: filepath.Join(t.TempDir(), "mcp-oauth-tokens.json"),
	})
	if err != nil {
		t.Fatalf("NewTokenStore() error = %v", err)
	}
	expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if err := store.Save("remote", mcp.StoredToken{
		AccessToken:  "super-secret-access",
		RefreshToken: "super-secret-refresh",
		ExpiresAt:    expiry,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	deps := appDeps{newMCPTokenStore: func() (*mcp.TokenStore, error) { return store, nil }}

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"mcp", "oauth", "status", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "super-secret-access") || strings.Contains(out, "super-secret-refresh") {
		t.Fatalf("status leaked token material: %s", out)
	}
	var payload struct {
		Tokens []mcp.TokenStatus `json:"tokens"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode status JSON: %v\n%s", err, out)
	}
	if len(payload.Tokens) != 1 {
		t.Fatalf("tokens = %#v, want one", payload.Tokens)
	}
	if !payload.Tokens[0].HasToken || !payload.Tokens[0].HasRefreshToken {
		t.Fatalf("status = %#v, want present token", payload.Tokens[0])
	}
}

func TestRunMCPOAuthLogout(t *testing.T) {
	store, err := mcp.NewTokenStore(mcp.TokenStoreOptions{
		FilePath: filepath.Join(t.TempDir(), "mcp-oauth-tokens.json"),
	})
	if err != nil {
		t.Fatalf("NewTokenStore() error = %v", err)
	}
	if err := store.Save("remote", mcp.StoredToken{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.SaveForServer(mcp.Server{Name: "remote", Identity: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, mcp.StoredToken{AccessToken: "identity", RefreshToken: "ir"}); err != nil {
		t.Fatalf("SaveForServer() error = %v", err)
	}
	deps := appDeps{newMCPTokenStore: func() (*mcp.TokenStore, error) { return store, nil }}

	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"mcp", "oauth", "logout", "remote", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"removed": true`) {
		t.Fatalf("logout output = %s", stdout.String())
	}
	if _, ok, _ := store.Load("remote"); ok {
		t.Fatal("token still present after logout")
	}
	if statuses, err := store.Status(); err != nil || len(statuses) != 0 {
		t.Fatalf("status after logout = %#v err=%v, want no tokens", statuses, err)
	}
}

func TestRunMCPOAuthLoginStoresTokens(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-final",
			"refresh_token": "refresh-final",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	defer tokenServer.Close()

	store, err := mcp.NewTokenStore(mcp.TokenStoreOptions{
		FilePath: filepath.Join(t.TempDir(), "mcp-oauth-tokens.json"),
	})
	if err != nil {
		t.Fatalf("NewTokenStore() error = %v", err)
	}

	cwd := t.TempDir()
	mcpConfig := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"remote": {
			Type: "http",
			URL:  "https://remote.invalid/mcp",
			Auth: "oauth",
			OAuth: &config.MCPOAuthConfig{
				ClientID:              "client-123",
				AuthorizationEndpoint: "https://remote.invalid/authorize",
				TokenEndpoint:         tokenServer.URL,
				Scopes:                []string{"read"},
			},
		},
	}}
	deps := appDeps{
		getwd:            func() (string, error) { return cwd, nil },
		newMCPTokenStore: func() (*mcp.TokenStore, error) { return store, nil },
		resolveMCPConfig: func(_ string, _ bool) (config.MCPConfig, error) {
			return mcpConfig, nil
		},
		now: time.Now,
	}

	// Drive the loopback redirect as soon as the authorization URL is printed.
	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			callbackURL := extractCallbackURL(stdout.String())
			if callbackURL != "" {
				_, _ = http.Get(callbackURL)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	// Run the command (which blocks waiting for the callback) off-goroutine and
	// bound it: if the callback is never driven, fail fast instead of hanging
	// until the package test timeout.
	done := make(chan int, 1)
	go func() {
		done <- runWithDeps([]string{"mcp", "oauth", "login", "remote"}, stdout, stderr, deps)
	}()
	var exitCode int
	select {
	case exitCode = <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("login did not complete within 10s; stderr=%s stdout=%s", stderr.String(), stdout.String())
	}
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s stdout=%s", exitCode, stderr.String(), stdout.String())
	}
	servers, err := mcp.NormalizeConfig(mcpConfig)
	if err != nil || len(servers) != 1 {
		t.Fatalf("NormalizeConfig() servers=%#v err=%v", servers, err)
	}
	token, ok, err := store.LoadForServer(servers[0])
	if err != nil || !ok {
		t.Fatalf("LoadForServer() ok=%v err=%v", ok, err)
	}
	if token.AccessToken != "access-final" || token.RefreshToken != "refresh-final" {
		t.Fatalf("stored token = %#v", token)
	}
	// The login output must never echo the issued tokens.
	if strings.Contains(stdout.String(), "access-final") || strings.Contains(stdout.String(), "refresh-final") {
		t.Fatalf("login stdout leaked token: %s", stdout.String())
	}
}

func TestRunMCPOAuthLoginRejectsNonOAuthServer(t *testing.T) {
	cwd := t.TempDir()
	store, err := mcp.NewTokenStore(mcp.TokenStoreOptions{
		FilePath: filepath.Join(t.TempDir(), "mcp-oauth-tokens.json"),
	})
	if err != nil {
		t.Fatalf("NewTokenStore() error = %v", err)
	}
	deps := appDeps{
		getwd:            func() (string, error) { return cwd, nil },
		newMCPTokenStore: func() (*mcp.TokenStore, error) { return store, nil },
		resolveMCPConfig: func(workspaceRoot string, _ bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"plain": {Type: "http", URL: "https://plain.invalid/mcp"},
			}}, nil
		},
	}
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"mcp", "oauth", "login", "plain"}, &stdout, &stderr, deps)
	if exitCode == exitSuccess {
		t.Fatal("login on non-oauth server should fail")
	}
	if !strings.Contains(stderr.String(), "oauth") {
		t.Fatalf("stderr = %q, want oauth guidance", stderr.String())
	}
}

// TestRunMCPOAuthLoginGatedInUntrustedWorkspace is the load-bearing security test for
// the OAuth login gate: an OAuth MCP server defined ONLY in an untrusted workspace's
// ./.zero/config.json must be dropped before login. `zero mcp oauth login` must refuse
// with "not configured", surface the trust notice, and store NO token — so the
// discovery/registration/token-exchange flow never fires against the project URL. If the
// gate is reverted to resolveMCPConfig(cwd, false), the server survives, login proceeds,
// a token is stored, and every assertion below fails.
func TestRunMCPOAuthLoginGatedInUntrustedWorkspace(t *testing.T) {
	setTrustConfigRoot(t) // isolates the trust store; the workspace stays untrusted
	// A live server standing in for the untrusted project's OAuth server. A gated login
	// must never reach it, so hits must stay 0 — a direct assertion of no outbound I/O
	// (discovery/registration/token exchange) against a cloned repo's configured URL. If
	// the gate leaks, mcp.Login contacts this URL and the hit count rises.
	var hits atomic.Int64
	oauthSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer oauthSrv.Close()

	cwd := t.TempDir()
	// A real project MCP config on disk so projectMCPConfigExists() is true and the
	// notice fires. resolveMCPConfig is faked, but the notice detector reads disk.
	if err := os.MkdirAll(filepath.Join(cwd, ".zero"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"mcp":{"servers":{"remote":{"type":"http","url":"` + oauthSrv.URL + `","auth":"oauth"}}}}`
	if err := os.WriteFile(filepath.Join(cwd, ".zero", "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := mcp.NewTokenStore(mcp.TokenStoreOptions{
		FilePath: filepath.Join(t.TempDir(), "mcp-oauth-tokens.json"),
	})
	if err != nil {
		t.Fatalf("NewTokenStore() error = %v", err)
	}
	// Points every OAuth endpoint at the live server: if the gate leaks and login
	// proceeds, the flow contacts oauthSrv and hits.Load() becomes non-zero.
	projectServer := config.MCPServerConfig{
		Type: "http",
		URL:  oauthSrv.URL,
		Auth: "oauth",
		OAuth: &config.MCPOAuthConfig{
			ClientID:              "client-123",
			AuthorizationEndpoint: oauthSrv.URL + "/authorize",
			TokenEndpoint:         oauthSrv.URL + "/token",
			Scopes:                []string{"read"},
		},
	}
	deps := appDeps{
		getwd:            func() (string, error) { return cwd, nil },
		newMCPTokenStore: func() (*mcp.TokenStore, error) { return store, nil },
		resolveMCPConfig: func(_ string, excludeProject bool) (config.MCPConfig, error) {
			servers := map[string]config.MCPServerConfig{}
			if !excludeProject {
				servers["remote"] = projectServer
			}
			return config.MCPConfig{Servers: servers}, nil
		},
		now: time.Now,
	}

	// The gate must short-circuit BEFORE the interactive OAuth flow. Bound the run so a
	// regression that drops the gate (login proceeds and blocks on the loopback callback)
	// fails here in 10s instead of hanging until the package test timeout.
	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
	done := make(chan int, 1)
	go func() {
		done <- runWithDeps([]string{"mcp", "oauth", "login", "remote"}, stdout, stderr, deps)
	}()
	var exitCode int
	select {
	case exitCode = <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("login was not gated; it blocked on the OAuth flow — the untrusted gate is missing. stderr=%q", stderr.String())
	}
	if exitCode == exitSuccess {
		t.Fatalf("login on a project-only OAuth server in an untrusted workspace must fail; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "not configured") {
		t.Fatalf("stderr must report the server as not configured, got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "MCP servers") || !strings.Contains(stderr.String(), "zero trust") {
		t.Fatalf("stderr must surface the workspace-trust notice, got %q", stderr.String())
	}
	if _, ok, _ := store.Load("remote"); ok {
		t.Fatal("no token must be stored when login is gated")
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("gated login must never contact the project OAuth server URL, got %d hit(s)", got)
	}
}

// TestResolveOAuthServerTrustedWorkspaceReturnsServer proves the gate does not
// over-block: in a TRUSTED workspace the project OAuth server resolves normally with a
// clean skip, so login proceeds as before. Calls resolveOAuthServer directly to avoid
// driving the full interactive login flow.
func TestResolveOAuthServerTrustedWorkspaceReturnsServer(t *testing.T) {
	setTrustConfigRoot(t)
	cwd := t.TempDir()
	if err := workspacetrust.Trust(cwd); err != nil {
		t.Fatalf("Trust(cwd): %v", err)
	}
	var gotExclude bool
	deps := appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveMCPConfig: func(_ string, excludeProject bool) (config.MCPConfig, error) {
			gotExclude = excludeProject
			servers := map[string]config.MCPServerConfig{}
			if !excludeProject {
				servers["remote"] = oauthTestServerConfig()
			}
			return config.MCPConfig{Servers: servers}, nil
		},
	}

	server, skip, err := resolveOAuthServer(deps, "remote")
	if err != nil {
		t.Fatalf("trusted workspace must resolve the project OAuth server, got err %v", err)
	}
	if gotExclude {
		t.Fatalf("trusted workspace must resolve MCP config with excludeProject=false")
	}
	if server.Name != "remote" {
		t.Fatalf("server.Name = %q, want remote", server.Name)
	}
	if skip.excludedProjectConfig {
		t.Fatalf("trusted workspace must not report an excluded project config")
	}
}

// TestResolveOAuthServerGatedInUntrustedWorkspace is the fast load-bearing guard for the
// gate: in an untrusted workspace a project-only OAuth server is dropped, so resolution
// fails with "not configured" and flags the excluded project config for the notice.
// Reverting the gate to resolveMCPConfig(cwd, false) makes the server resolve and this
// test fails immediately (no 10s login-flow timeout needed).
func TestResolveOAuthServerGatedInUntrustedWorkspace(t *testing.T) {
	setTrustConfigRoot(t)
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".zero"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"mcp":{"servers":{"remote":{"type":"http","url":"https://remote.invalid/mcp","auth":"oauth"}}}}`
	if err := os.WriteFile(filepath.Join(cwd, ".zero", "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveMCPConfig: func(_ string, excludeProject bool) (config.MCPConfig, error) {
			servers := map[string]config.MCPServerConfig{}
			if !excludeProject {
				servers["remote"] = oauthTestServerConfig()
			}
			return config.MCPConfig{Servers: servers}, nil
		},
	}

	_, skip, err := resolveOAuthServer(deps, "remote")
	if err == nil {
		t.Fatal("untrusted workspace must not resolve a project-only OAuth server")
	}
	if !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("want not-configured error, got %v", err)
	}
	if !skip.excludedProjectConfig {
		t.Fatal("a dropped project OAuth config must flag excludedProjectConfig for the notice")
	}
	if skip.trustCheckErrored {
		t.Fatal("a clean untrusted verdict is not a store-read error")
	}
}

func oauthTestServerConfig() config.MCPServerConfig {
	return config.MCPServerConfig{
		Type: "http",
		URL:  "https://remote.invalid/mcp",
		Auth: "oauth",
		OAuth: &config.MCPOAuthConfig{
			ClientID:              "client-123",
			AuthorizationEndpoint: "https://remote.invalid/authorize",
			TokenEndpoint:         "https://remote.invalid/token",
			Scopes:                []string{"read"},
		},
	}
}

// TestRunMCPOAuthLoginTrustedProjectServerStoresToken proves the gate does not over-block
// end-to-end: in a TRUSTED workspace, a project-only OAuth server (returned by the fake
// ONLY when excludeProject is false) is resolved, the full login flow runs, and the token
// is persisted with exitSuccess. This exercises the trusted-resolve + login-success path
// as one execution; if trust were dropped, excludeProject would be true, the fake would
// drop the server, and login would fail with "not configured".
func TestRunMCPOAuthLoginTrustedProjectServerStoresToken(t *testing.T) {
	setTrustConfigRoot(t)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-final",
			"refresh_token": "refresh-final",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	defer tokenServer.Close()

	cwd := t.TempDir()
	if err := workspacetrust.Trust(cwd); err != nil {
		t.Fatalf("Trust(cwd): %v", err)
	}
	store, err := mcp.NewTokenStore(mcp.TokenStoreOptions{
		FilePath: filepath.Join(t.TempDir(), "mcp-oauth-tokens.json"),
	})
	if err != nil {
		t.Fatalf("NewTokenStore() error = %v", err)
	}
	projectServer := config.MCPServerConfig{
		Type: "http",
		URL:  "https://remote.invalid/mcp",
		Auth: "oauth",
		OAuth: &config.MCPOAuthConfig{
			ClientID:              "client-123",
			AuthorizationEndpoint: "https://remote.invalid/authorize",
			TokenEndpoint:         tokenServer.URL,
			Scopes:                []string{"read"},
		},
	}
	var gotExclude bool
	deps := appDeps{
		getwd:            func() (string, error) { return cwd, nil },
		newMCPTokenStore: func() (*mcp.TokenStore, error) { return store, nil },
		resolveMCPConfig: func(_ string, excludeProject bool) (config.MCPConfig, error) {
			gotExclude = excludeProject
			servers := map[string]config.MCPServerConfig{}
			if !excludeProject {
				servers["remote"] = projectServer
			}
			return config.MCPConfig{Servers: servers}, nil
		},
		now: time.Now,
	}

	stdout := &syncBuffer{}
	stderr := &syncBuffer{}
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			callbackURL := extractCallbackURL(stdout.String())
			if callbackURL != "" {
				_, _ = http.Get(callbackURL)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	done := make(chan int, 1)
	go func() {
		done <- runWithDeps([]string{"mcp", "oauth", "login", "remote"}, stdout, stderr, deps)
	}()
	var exitCode int
	select {
	case exitCode = <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("trusted login did not complete within 10s; stderr=%s stdout=%s", stderr.String(), stdout.String())
	}
	if exitCode != exitSuccess {
		t.Fatalf("trusted login must succeed; exit=%d stderr=%s", exitCode, stderr.String())
	}
	if gotExclude {
		t.Fatal("trusted workspace must resolve MCP config with excludeProject=false")
	}
	servers, err := mcp.NormalizeConfig(config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"remote": projectServer,
	}})
	if err != nil || len(servers) != 1 {
		t.Fatalf("NormalizeConfig() servers=%#v err=%v", servers, err)
	}
	token, ok, err := store.LoadForServer(servers[0])
	if err != nil || !ok {
		t.Fatalf("token must be stored on trusted login; ok=%v err=%v", ok, err)
	}
	if token.AccessToken != "access-final" {
		t.Fatalf("stored token = %#v", token)
	}
}

func TestRunMCPOAuthUnknownSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"mcp", "oauth", "bogus"}, &stdout, &stderr, appDeps{})
	if exitCode != exitUsage {
		t.Fatalf("exitCode = %d, want usage error", exitCode)
	}
}

// extractCallbackURL pulls the printed authorization URL and rewrites it into a
// loopback callback hit carrying the code and state.
func extractCallbackURL(output string) string {
	const marker = "https://remote.invalid/authorize"
	index := strings.Index(output, marker)
	if index < 0 {
		return ""
	}
	rest := output[index:]
	if end := strings.IndexAny(rest, " \n"); end >= 0 {
		rest = rest[:end]
	}
	parsed, err := url.Parse(rest)
	if err != nil {
		return ""
	}
	state := parsed.Query().Get("state")
	if state == "" {
		return ""
	}
	redirect := parsed.Query().Get("redirect_uri")
	if redirect == "" {
		return ""
	}
	return redirect + "?code=auth-code&state=" + url.QueryEscape(state)
}

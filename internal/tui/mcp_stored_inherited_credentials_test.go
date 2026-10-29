package tui

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
)

// A FAILED REFRESH CAN ECHO WHAT LOGIN STORED.
//
// For an `auth: oauth` server whose config names no OAuth block, Login stores
// the dynamically registered client secret and the discovered token endpoint,
// and Refresh posts the one to the other. The candidate set held the access and
// refresh tokens only, so a token endpoint that put either back into its
// error_description reached both views intact. Driven through a real 401 and a
// real refresh, against the same default store the client opens. Reported by
// @jatmn.
func TestRefreshFailureEchoingStoredMaterialIsRedactedInBothViews(t *testing.T) {
	const clientSecret = "cs9f3c"
	const workspace = "opaque-workspace-9f3c2b7"
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":"invalid_client","error_description":"client secret %s is not valid for workspace %s"}`, clientSecret, workspace)
	}))
	defer tokenServer.Close()
	mcpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer mcpServer.Close()

	// The MCP client opens the store with default options, so the test points
	// those defaults at temporary files instead of the user's.
	t.Setenv("ZERO_OAUTH_TOKENS_PATH", filepath.Join(t.TempDir(), "oauth-tokens.json"))
	t.Setenv("ZERO_MCP_OAUTH_TOKENS_PATH", filepath.Join(t.TempDir(), "mcp-oauth-tokens.json"))
	t.Setenv("ZERO_OAUTH_STORAGE", "")
	store, err := mcp.NewTokenStore(mcp.TokenStoreOptions{})
	if err != nil {
		t.Fatalf("NewTokenStore() error = %v", err)
	}
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"linear": {Type: "http", URL: mcpServer.URL + "/mcp", Auth: "oauth"},
	}}
	servers, err := mcp.NormalizeConfig(cfg)
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	if err := store.SaveForServer(servers[0], mcp.StoredToken{
		AccessToken:   "stale-access-9f3c2b7a",
		RefreshToken:  "stale-refresh-9f3c2b7a",
		ClientID:      "zero-client",
		ClientSecret:  clientSecret,
		TokenEndpoint: tokenServer.URL + "/token?workspace=" + workspace,
	}); err != nil {
		t.Fatalf("SaveForServer() error = %v", err)
	}

	skipped := registerAndCollectSkipped(t, cfg, clientSecret)
	if !strings.Contains(skipped[0].Err.Error(), workspace) {
		t.Fatalf("the refresh error does not echo the endpoint's workspace, so that half proves nothing: %v", skipped[0].Err)
	}
	panel, detail := failedServerSurfacesWithStore(t, cfg, "linear", skipped, store)
	for surface, text := range map[string]string{"panel": panel, "manager detail": detail} {
		row := surfaceRow(t, surface, text, "invalid_client")
		for _, secret := range []string{clientSecret, workspace} {
			if strings.Contains(row, secret) {
				t.Errorf("%s shows the stored %q:\n%s", surface, secret, row)
			}
		}
		// What failed stays readable: the subsystem and the server's own words.
		for _, want := range []string{"token refresh failed", "is not valid"} {
			if !strings.Contains(row, want) {
				t.Errorf("%s lost %q:\n%s", surface, want, row)
			}
		}
	}
}

// A ROTATED STORE IS A CHANGED CONTEXT.
//
// The reason is only shown while the material that redacts it is the material
// that existed when it was produced. With the client secret and the endpoint
// outside the fingerprint, a refresh that re-registered the client, or a login
// against another authorization server, left the fingerprint unchanged and the
// reason was shown with nothing left in the candidate set to hide the old
// value.
func TestRotatedStoredMaterialWithholdsTheReason(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"linear": {Type: "http", URL: "https://linear.invalid/mcp", Auth: "oauth"},
	}}
	original := mcp.StoredToken{
		AccessToken:   "access-9f3c2b7a",
		ClientSecret:  "cs-old-9f3c",
		TokenEndpoint: "https://auth.invalid/token?workspace=opaque-workspace-old",
	}
	for _, tc := range []struct {
		name      string
		rotate    func(mcp.StoredToken) mcp.StoredToken
		echo      string
		wantStale bool
	}{
		{"client secret re-registered after capture", func(token mcp.StoredToken) mcp.StoredToken {
			token.ClientSecret = "cs-new-7a1d"
			return token
		}, "client secret cs-old-9f3c is not valid", true},
		{"token endpoint changed after capture", func(token mcp.StoredToken) mcp.StoredToken {
			token.TokenEndpoint = "https://auth.invalid/token?workspace=opaque-workspace-new"
			return token
		}, "workspace opaque-workspace-old is not valid", true},
		{"control: unchanged material shows the redacted reason", func(token mcp.StoredToken) mcp.StoredToken {
			return token
		}, "client secret cs-old-9f3c is not valid", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestMCPTokenStore(t)
			if err := store.Save("linear", original); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			captured := mcp.CredentialFingerprint(store.CredentialMaterial())
			if err := store.Save("linear", tc.rotate(original)); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			views := buildMCPServerViews(cfg, nil, []mcp.SkippedServer{
				{Name: "linear", Err: errors.New("refresh failed: " + tc.echo), Credentials: captured},
			}, store, "")
			if len(views) != 1 {
				t.Fatalf("views = %#v, want one", views)
			}
			got := views[0].Error
			if stale := got == mcpStaleObservationReason; stale != tc.wantStale {
				t.Fatalf("withheld = %v, want %v; reason %q", stale, tc.wantStale, got)
			}
			for _, secret := range []string{"cs-old-9f3c", "opaque-workspace-old"} {
				if strings.Contains(got, secret) {
					t.Errorf("the reason shows %q: %q", secret, got)
				}
			}
		})
	}
}

// A STDIO CHILD INHERITS MORE THAN ITS CONFIG.
//
// The launcher hands the child this process's environment before the
// configured env, which is how a secret manager supplies the variable a server
// reads on its own. A child that echoed one while failing put it in both views,
// because only the configured env was a candidate. A credential-named variable
// is redacted now; an ordinary one is not, since it is not a credential and
// hiding it would only cost the diagnostic. Neither is a short switch whose
// name happens to sound like a credential: the inherited environment is
// everything Zero was started with, so a name match alone keeps the
// readability floor. Reported by @jatmn.
func TestInheritedCredentialEchoedByAStdioChildIsRedacted(t *testing.T) {
	const inheritedToken = "opaque-company-9f3c2b"
	const inheritedPlain = "eu-central-region"
	const inheritedSwitch = "enabled"
	t.Setenv(mcpEchoInheritedTokenEnv, inheritedToken)
	t.Setenv(mcpEchoInheritedPlainEnv, inheritedPlain)
	t.Setenv(mcpEchoInheritedSwitchEnv, inheritedSwitch)

	for _, tc := range []struct {
		name   string
		mode   string
		echo   string
		redact bool
	}{
		{"credential-named inherited variable", "envt", inheritedToken, true},
		{"control: ordinary inherited variable stays readable", "envp", inheritedPlain, false},
		{"control: a short credential-named switch stays readable", "envs", inheritedSwitch, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := echoingStdioConfig(tc.mode, "unused")
			skipped := registerAndCollectSkipped(t, cfg, tc.echo)
			panel, detail := failedServerSurfaces(t, cfg, "docs", skipped)
			for surface, text := range map[string]string{"panel": panel, "manager detail": detail} {
				row := surfaceRow(t, surface, text, "upstream refused")
				if shown := strings.Contains(row, tc.echo); shown == tc.redact {
					t.Errorf("%s: %q shown = %v, want %v:\n%s", surface, tc.echo, shown, !tc.redact, row)
				}
			}
		})
	}
}

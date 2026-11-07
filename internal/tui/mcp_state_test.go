package tui

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/tools"
)

func TestBuildMCPViewStateSummarizesConfiguredServers(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {
			Type:    "stdio",
			Command: "docs-mcp",
			Args:    []string{"--workspace", "."},
			Env:     map[string]string{"ZERO_DOCS_TOKEN": "sk-secret"},
		},
		"linear": {
			Type:     "http",
			URL:      "https://linear.example/mcp",
			Headers:  map[string]string{"Authorization": "Bearer secret"},
			Auth:     "oauth",
			Disabled: true,
		},
		"updates": {
			Type:    "sse",
			URL:     "https://events.example/sse",
			Headers: map[string]string{"X-Api-Key": "secret"},
		},
	}}

	state := BuildMCPViewState(MCPStateOptions{Config: cfg})

	if len(state.Servers) != 3 {
		t.Fatalf("servers = %#v, want 3 configured servers", state.Servers)
	}
	assertServerView(t, state.Servers[0], MCPServerView{
		Name:      "docs",
		Transport: "stdio",
		State:     "enabled",
		Target:    "docs-mcp --workspace . env ZERO_DOCS_TOKEN=[REDACTED]",
	})
	assertServerView(t, state.Servers[1], MCPServerView{
		Name:      "linear",
		Transport: "http",
		State:     "disabled",
		Target:    "https://linear.example/mcp headers Authorization=[REDACTED]",
		Auth:      "oauth",
	})
	assertServerView(t, state.Servers[2], MCPServerView{
		Name:      "updates",
		Transport: "sse",
		State:     "enabled",
		Target:    "https://events.example/sse headers X-Api-Key=[REDACTED]",
	})

	rendered := renderMCPView(state, 160)
	for _, leaked := range []string{"sk-secret", "Bearer secret"} {
		if strings.Contains(rendered, leaked) {
			t.Fatalf("rendered MCP state leaked %q: %s", leaked, rendered)
		}
	}
}

func TestBuildMCPViewStateGroupsRegisteredMCPToolsByServer(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs":   {Type: "stdio", Command: "docs-mcp"},
		"github": {Type: "http", URL: "https://github.example/mcp"},
	}}
	registry := tools.NewRegistry()
	_, err := mcp.RegisterTools(context.Background(), registry, cfg, mcp.RegisterOptions{
		ClientFactory: func(_ context.Context, server mcp.Server) (mcp.ToolClient, error) {
			switch server.Name {
			case "docs":
				return &tuiFakeMCPClient{listed: []mcp.RemoteTool{
					{Name: "lookup", Description: "Look up documentation"},
					{Name: "search", Description: "Search documentation"},
				}}, nil
			case "github":
				return &tuiFakeMCPClient{listed: []mcp.RemoteTool{
					{Name: "create_issue", Description: "Create a GitHub issue"},
				}}, nil
			default:
				return &tuiFakeMCPClient{}, nil
			}
		},
	})
	if err != nil {
		t.Fatalf("RegisterTools() error = %v", err)
	}

	state := BuildMCPViewState(MCPStateOptions{Config: cfg, Registry: registry})

	if got := state.Servers[0].ToolCount; got != 2 {
		t.Fatalf("docs tool count = %d, want 2", got)
	}
	if got := state.Servers[1].ToolCount; got != 1 {
		t.Fatalf("github tool count = %d, want 1", got)
	}
	if len(state.Tools) != 3 {
		t.Fatalf("tools = %#v, want 3 MCP tools", state.Tools)
	}
	assertToolView(t, state.Tools[0], MCPToolView{ServerName: "docs", Name: "lookup", RegistryName: "mcp_docs_lookup", SideEffect: "network", Permission: "prompt", Description: "Look up documentation"})
	assertToolView(t, state.Tools[1], MCPToolView{ServerName: "docs", Name: "search", RegistryName: "mcp_docs_search", SideEffect: "network", Permission: "prompt", Description: "Search documentation"})
	assertToolView(t, state.Tools[2], MCPToolView{ServerName: "github", Name: "create_issue", RegistryName: "mcp_github_create_issue", SideEffect: "network", Permission: "prompt", Description: "Create a GitHub issue"})
}

func TestBuildMCPViewStateSummarizesPermissions(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs":   {Type: "stdio", Command: "docs-mcp"},
		"github": {Type: "http", URL: "https://github.example/mcp"},
	}}
	servers, err := mcp.NormalizeConfig(cfg)
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	identities := map[string]string{}
	for _, server := range servers {
		identities[server.Name] = server.Identity
	}
	store, err := mcp.NewPermissionStore(mcp.StoreOptions{
		FilePath: t.TempDir() + "/permissions.json",
		Now:      func() time.Time { return time.Date(2026, 6, 13, 9, 30, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewPermissionStore() error = %v", err)
	}
	if _, err := store.GrantServer(mcp.GrantServerInput{ServerName: "docs", ServerIdentity: identities["docs"], MaxAutonomy: mcp.AutonomyLow}); err != nil {
		t.Fatalf("GrantServer() error = %v", err)
	}
	if _, err := store.GrantTool(mcp.GrantToolInput{ServerName: "github", ServerIdentity: identities["github"], ToolName: "create_issue", MaxAutonomy: mcp.AutonomyMedium}); err != nil {
		t.Fatalf("GrantTool() error = %v", err)
	}

	state := BuildMCPViewState(MCPStateOptions{
		Config:          cfg,
		PermissionStore: store,
		PermissionMode:  "ask",
		PromptCount:     4,
		DeniedCount:     1,
	})

	summary := state.Permissions
	if summary.Mode != "ask" || summary.GrantCount != 2 || summary.ServerGrants != 1 || summary.ToolGrants != 1 || summary.PromptCount != 4 || summary.DeniedCount != 1 {
		t.Fatalf("permission summary = %#v", summary)
	}
	if len(summary.Grants) != 2 {
		t.Fatalf("permission grants = %#v, want 2", summary.Grants)
	}
	if summary.Grants[0].Target != "docs/*" || summary.Grants[0].Autonomy != "low" {
		t.Fatalf("server grant = %#v", summary.Grants[0])
	}
	if summary.Grants[1].Target != "github/create_issue" || summary.Grants[1].Autonomy != "medium" {
		t.Fatalf("tool grant = %#v", summary.Grants[1])
	}
}

func TestBuildMCPViewStateSummarizesOAuthTokens(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	expiry := now.Add(time.Hour)
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"linear": {Type: "http", URL: "https://linear.example/mcp", Auth: "oauth"},
		"notion": {Type: "sse", URL: "https://notion.example/sse", Auth: "oauth"},
	}}
	store, err := mcp.NewTokenStore(mcp.TokenStoreOptions{
		FilePath: t.TempDir() + "/tokens.json",
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewTokenStore() error = %v", err)
	}
	if err := store.Save("linear", mcp.StoredToken{
		AccessToken:  "access-secret",
		RefreshToken: "refresh-secret",
		TokenType:    "Bearer",
		Scopes:       []string{"issues:read", "issues:write"},
		ExpiresAt:    expiry,
	}); err != nil {
		t.Fatalf("Save(linear) error = %v", err)
	}
	if err := store.Save("notion", mcp.StoredToken{
		AccessToken: "expired-secret",
		ExpiresAt:   now.Add(-2 * time.Minute),
	}); err != nil {
		t.Fatalf("Save(notion) error = %v", err)
	}
	if err := store.Save("orphan", mcp.StoredToken{AccessToken: "orphan-secret", ExpiresAt: now.Add(-time.Minute)}); err != nil {
		t.Fatalf("Save(orphan) error = %v", err)
	}

	state := BuildMCPViewState(MCPStateOptions{Config: cfg, TokenStore: store})

	if len(state.OAuth.Servers) != 3 {
		t.Fatalf("OAuth servers = %#v, want configured servers plus stored orphan", state.OAuth.Servers)
	}
	assertOAuthView(t, state.OAuth.Servers[0], MCPOAuthServerView{
		ServerName:      "linear",
		Configured:      true,
		HasToken:        true,
		HasRefreshToken: true,
		TokenType:       "Bearer",
		Scopes:          []string{"issues:read", "issues:write"},
		ExpiresAt:       expiry,
	})
	assertOAuthView(t, state.OAuth.Servers[1], MCPOAuthServerView{
		ServerName: "notion",
		Configured: true,
		HasToken:   true,
		Expired:    true,
		ExpiresAt:  now.Add(-2 * time.Minute),
	})
	assertOAuthView(t, state.OAuth.Servers[2], MCPOAuthServerView{ServerName: "orphan", HasToken: true, Expired: true, ExpiresAt: now.Add(-time.Minute)})

	rendered := renderMCPView(state, 160)
	for _, want := range []string{
		"linear configured token refresh expires 2026-06-13T13:00:00Z Bearer scopes issues:read,issues:write",
		"notion configured token expired",
		"orphan not configured",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered OAuth state = %q, missing %q", rendered, want)
		}
	}
}

func TestBuildMCPViewStateToleratesUnreadableStores(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"linear": {Type: "http", URL: "https://linear.example/mcp", Auth: "oauth"},
	}}
	permissionStore, err := mcp.NewPermissionStore(mcp.StoreOptions{FilePath: t.TempDir() + "/permissions.json"})
	if err != nil {
		t.Fatalf("NewPermissionStore() error = %v", err)
	}
	tokenStore, err := mcp.NewTokenStore(mcp.TokenStoreOptions{FilePath: t.TempDir() + "/tokens.json"})
	if err != nil {
		t.Fatalf("NewTokenStore() error = %v", err)
	}
	if err := writeInvalidStoreFile(permissionStore.FilePath()); err != nil {
		t.Fatalf("write invalid permission store: %v", err)
	}
	if err := writeInvalidStoreFile(tokenStore.FilePath()); err != nil {
		t.Fatalf("write invalid token store: %v", err)
	}

	state := BuildMCPViewState(MCPStateOptions{Config: cfg, PermissionStore: permissionStore, TokenStore: tokenStore})

	if len(state.Servers) != 1 || state.Servers[0].Name != "linear" {
		t.Fatalf("servers = %#v, want configured server despite store errors", state.Servers)
	}
	if state.Permissions.GrantCount != 0 || len(state.Permissions.Grants) != 0 {
		t.Fatalf("permissions = %#v, want empty summary after store error", state.Permissions)
	}
	if len(state.OAuth.Servers) != 1 || state.OAuth.Servers[0].ServerName != "linear" || !state.OAuth.Servers[0].Configured {
		t.Fatalf("oauth = %#v, want configured OAuth server without token status", state.OAuth.Servers)
	}
}

func assertServerView(t *testing.T, got MCPServerView, want MCPServerView) {
	t.Helper()
	if got != want {
		t.Fatalf("server view = %#v, want %#v", got, want)
	}
}

func assertToolView(t *testing.T, got MCPToolView, want MCPToolView) {
	t.Helper()
	if got != want {
		t.Fatalf("tool view = %#v, want %#v", got, want)
	}
}

func assertOAuthView(t *testing.T, got MCPOAuthServerView, want MCPOAuthServerView) {
	t.Helper()
	if got.ServerName != want.ServerName ||
		got.Configured != want.Configured ||
		got.HasToken != want.HasToken ||
		got.HasRefreshToken != want.HasRefreshToken ||
		got.TokenType != want.TokenType ||
		got.ExpiresAt != want.ExpiresAt ||
		got.Expired != want.Expired ||
		strings.Join(got.Scopes, "\x00") != strings.Join(want.Scopes, "\x00") {
		t.Fatalf("OAuth view = %#v, want %#v", got, want)
	}
}

func writeInvalidStoreFile(path string) error {
	return os.WriteFile(path, []byte(`{"schemaVersion":999}`+"\n"), 0o600)
}

type tuiFakeMCPClient struct {
	listed []mcp.RemoteTool
}

func (client *tuiFakeMCPClient) ListTools(context.Context) ([]mcp.RemoteTool, error) {
	return client.listed, nil
}

func (client *tuiFakeMCPClient) CallTool(context.Context, string, map[string]any) (mcp.CallToolResult, error) {
	return mcp.CallToolResult{}, nil
}

func (client *tuiFakeMCPClient) Close() error {
	return nil
}

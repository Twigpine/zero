package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
)

func TestNormalizeConfigValidatesTransportBoundaries(t *testing.T) {
	valid := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {
			Type:    "stdio",
			Command: "docs-mcp",
			Args:    []string{"--workspace", "."},
			Env:     map[string]string{"ZERO_DOCS_TOKEN": "test"},
		},
		"web": {
			Type:    "http",
			URL:     "https://example.com/mcp",
			Headers: map[string]string{"Authorization": "Bearer test"},
		},
		"events": {
			Type: "sse",
			URL:  "https://example.com/sse",
		},
		"disabled": {
			Type:     "stdio",
			Command:  "disabled-mcp",
			Disabled: true,
		},
	}}

	servers, err := NormalizeConfig(valid)
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	if len(servers) != 3 {
		t.Fatalf("servers = %#v, want disabled server skipped", servers)
	}
	if servers[0].Name != "docs" || servers[0].Identity == "" {
		t.Fatalf("docs server = %#v, want stable identity", servers[0])
	}
	if servers[1].Name != "events" || servers[2].Name != "web" {
		t.Fatalf("servers sorted by name = %#v", servers)
	}

	for _, tc := range []struct {
		name string
		cfg  config.MCPConfig
		want string
	}{
		{
			name: "stdio-without-command",
			cfg:  config.MCPConfig{Servers: map[string]config.MCPServerConfig{"docs": {Type: "stdio"}}},
			want: "requires command",
		},
		{
			name: "stdio-with-headers",
			cfg:  config.MCPConfig{Servers: map[string]config.MCPServerConfig{"docs": {Type: "stdio", Command: "docs-mcp", Headers: map[string]string{"Authorization": "Bearer test"}}}},
			want: "headers are only supported",
		},
		{
			name: "http-without-url",
			cfg:  config.MCPConfig{Servers: map[string]config.MCPServerConfig{"docs": {Type: "http"}}},
			want: "requires url",
		},
		{
			name: "http-with-env",
			cfg:  config.MCPConfig{Servers: map[string]config.MCPServerConfig{"docs": {Type: "http", URL: "https://example.com/mcp", Env: map[string]string{"TOKEN": "test"}}}},
			want: "env is only supported",
		},
		{
			name: "bad-url",
			cfg:  config.MCPConfig{Servers: map[string]config.MCPServerConfig{"docs": {Type: "sse", URL: "file:///tmp/mcp"}}},
			want: "http or https",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeConfig(tc.cfg)
			if err == nil {
				t.Fatal("NormalizeConfig() error = nil, want validation error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

func TestNormalizeConfigFlagsUnconfiguredDefault(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"exa": config.DefaultMCPServers()["exa"],
		"web": {
			Type: "http",
			URL:  "https://example.com/mcp",
		},
	}}

	servers, err := NormalizeConfig(cfg)
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}

	byName := make(map[string]Server, len(servers))
	for _, server := range servers {
		byName[server.Name] = server
	}

	if !byName["exa"].UnconfiguredDefault {
		t.Fatal("an untouched exa default should be flagged UnconfiguredDefault")
	}
	if byName["web"].UnconfiguredDefault {
		t.Fatal("a server the user configured must not be flagged UnconfiguredDefault")
	}
}

func TestServerIdentityChangesWithTransportFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		first  config.MCPServerConfig
		second config.MCPServerConfig
	}{
		{
			name:   "command",
			first:  config.MCPServerConfig{Type: "stdio", Command: "docs-mcp"},
			second: config.MCPServerConfig{Type: "stdio", Command: "other-docs-mcp"},
		},
		{
			name:   "args",
			first:  config.MCPServerConfig{Type: "stdio", Command: "docs-mcp", Args: []string{"--one"}},
			second: config.MCPServerConfig{Type: "stdio", Command: "docs-mcp", Args: []string{"--two"}},
		},
		{
			name:   "env",
			first:  config.MCPServerConfig{Type: "stdio", Command: "docs-mcp", Env: map[string]string{"TOKEN": "one"}},
			second: config.MCPServerConfig{Type: "stdio", Command: "docs-mcp", Env: map[string]string{"TOKEN": "two"}},
		},
		{
			name:   "url",
			first:  config.MCPServerConfig{Type: "http", URL: "https://one.example/mcp"},
			second: config.MCPServerConfig{Type: "http", URL: "https://two.example/mcp"},
		},
		{
			name:   "headers",
			first:  config.MCPServerConfig{Type: "http", URL: "https://example.com/mcp", Headers: map[string]string{"Authorization": "Bearer one"}},
			second: config.MCPServerConfig{Type: "http", URL: "https://example.com/mcp", Headers: map[string]string{"Authorization": "Bearer two"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, err := NormalizeConfig(config.MCPConfig{Servers: map[string]config.MCPServerConfig{"docs": tc.first}})
			if err != nil {
				t.Fatal(err)
			}
			second, err := NormalizeConfig(config.MCPConfig{Servers: map[string]config.MCPServerConfig{"docs": tc.second}})
			if err != nil {
				t.Fatal(err)
			}
			if first[0].Identity == second[0].Identity {
				t.Fatalf("identity did not change when %s changed: %s", tc.name, first[0].Identity)
			}
		})
	}
}

func TestNormalizeConfigCarriesOAuth(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"remote": {
			Type: "http",
			URL:  "https://example.com/mcp",
			Auth: "oauth",
			OAuth: &config.MCPOAuthConfig{
				ClientID:      "client-123",
				Scopes:        []string{" read ", "write", "  "},
				TokenEndpoint: " https://example.com/token ",
			},
		},
	}}

	servers, err := NormalizeConfig(cfg)
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	if len(servers) != 1 {
		t.Fatalf("servers = %#v", servers)
	}
	server := servers[0]
	if server.Auth != ServerAuthOAuth {
		t.Fatalf("auth = %q, want oauth", server.Auth)
	}
	if server.OAuth == nil {
		t.Fatal("OAuth = nil, want carried config")
	}
	if server.OAuth.ClientID != "client-123" {
		t.Fatalf("client id = %q", server.OAuth.ClientID)
	}
	if server.OAuth.TokenEndpoint != "https://example.com/token" {
		t.Fatalf("token endpoint = %q, want trimmed", server.OAuth.TokenEndpoint)
	}
	if len(server.OAuth.Scopes) != 2 || server.OAuth.Scopes[0] != "read" || server.OAuth.Scopes[1] != "write" {
		t.Fatalf("scopes = %#v, want trimmed and filtered", server.OAuth.Scopes)
	}
}

func TestNormalizeConfigRejectsUnsupportedAuth(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"remote": {Type: "http", URL: "https://example.com/mcp", Auth: "basic"},
	}}
	_, err := NormalizeConfig(cfg)
	if err == nil {
		t.Fatal("NormalizeConfig() error = nil, want unsupported auth error")
	}
	if !strings.Contains(err.Error(), "unsupported auth") {
		t.Fatalf("error = %q, want unsupported auth", err.Error())
	}
}

func TestNormalizeConfigRejectsAuthOnStdio(t *testing.T) {
	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"local": {Type: "stdio", Command: "local-mcp", Auth: "oauth"},
	}}
	_, err := NormalizeConfig(cfg)
	if err == nil {
		t.Fatal("NormalizeConfig() error = nil, want auth-on-stdio error")
	}
	if !strings.Contains(err.Error(), "auth is only supported") {
		t.Fatalf("error = %q, want auth-on-stdio error", err.Error())
	}
}

func TestCopyStringMapTrimsKeysAndPreservesValues(t *testing.T) {
	copied := copyStringMap(map[string]string{
		" TOKEN ": "  keep surrounding spaces  ",
		"   ":     "ignored",
	})
	if len(copied) != 1 {
		t.Fatalf("copied = %#v, want one trimmed key", copied)
	}
	if copied["TOKEN"] != "  keep surrounding spaces  " {
		t.Fatalf("copied[TOKEN] = %q, want value preserved verbatim", copied["TOKEN"])
	}
}

// Startup behavior for the firecrawl -> exa default migration: a user who
// disabled the retired default must end up with no Exa server to connect to,
// not merely a disabled entry in the resolved config.
func TestNormalizeConfigSkipsExaAfterLegacyFirecrawlDisable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"servers":{"firecrawl":{"disabled":true}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ResolveMCP(config.ResolveOptions{UserConfigPath: path})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	servers, err := NormalizeConfig(cfg)
	if err != nil {
		t.Fatalf("NormalizeConfig: %v", err)
	}
	for _, server := range servers {
		if server.Name == "exa" {
			t.Fatalf("exa must not be started after the user disabled the default it replaced: %#v", server)
		}
	}
}

// The same path with no legacy disable still starts Exa, so the migration
// cannot quietly switch off the default for everyone else.
func TestNormalizeConfigStartsExaWithoutLegacyDisable(t *testing.T) {
	cfg, err := config.ResolveMCP(config.ResolveOptions{})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	servers, err := NormalizeConfig(cfg)
	if err != nil {
		t.Fatalf("NormalizeConfig: %v", err)
	}
	for _, server := range servers {
		if server.Name == "exa" {
			if !server.UnconfiguredDefault {
				t.Fatalf("an untouched exa default should be flagged unconfigured: %#v", server)
			}
			return
		}
	}
	t.Fatal("expected the exa default to be started out of the box")
}

// The failure path CodeRabbit flagged: a firecrawl entry that named only a
// header relied on the retired default for its transport. Resolving it must
// still produce a complete server — an incomplete one fails NormalizeConfig,
// which takes down every other server's startup with it, not just this one.
func TestNormalizeConfigResolvesPartialRetiredDefaultEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"servers":{"firecrawl":{"headers":{"Authorization":"Bearer k"}}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ResolveMCP(config.ResolveOptions{UserConfigPath: path})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	servers, err := NormalizeConfig(cfg)
	if err != nil {
		t.Fatalf("a header-only legacy entry must not break startup: %v", err)
	}
	var firecrawl *Server
	var sawExa bool
	for i := range servers {
		switch servers[i].Name {
		case "firecrawl":
			firecrawl = &servers[i]
		case "exa":
			sawExa = true
		}
	}
	if firecrawl == nil {
		t.Fatalf("expected the customized firecrawl server to survive: %#v", servers)
	}
	if firecrawl.Type != ServerTypeHTTP || firecrawl.URL != "https://mcp.firecrawl.dev/v2/mcp" {
		t.Fatalf("firecrawl lost the transport its entry relied on: %#v", *firecrawl)
	}
	if firecrawl.Headers["Authorization"] != "Bearer k" {
		t.Fatalf("firecrawl lost the user's header: %#v", *firecrawl)
	}
	if !sawExa {
		t.Fatal("the new exa default should still start alongside it")
	}
}

// The mirror case: an entry that names its own stdio transport must not have
// the retired http default grafted onto it, which would be rejected outright.
func TestNormalizeConfigLeavesRetiredEntryWithOwnTransportAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"mcp":{"servers":{"firecrawl":{"command":"firecrawl-mcp"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ResolveMCP(config.ResolveOptions{UserConfigPath: path})
	if err != nil {
		t.Fatalf("ResolveMCP: %v", err)
	}
	servers, err := NormalizeConfig(cfg)
	if err != nil {
		t.Fatalf("a self-hosted stdio entry must keep working: %v", err)
	}
	for _, server := range servers {
		if server.Name != "firecrawl" {
			continue
		}
		if server.Type != ServerTypeStdio || server.Command != "firecrawl-mcp" {
			t.Fatalf("the user's own stdio transport was corrupted: %#v", server)
		}
		return
	}
	t.Fatalf("expected the self-hosted firecrawl server to survive: %#v", servers)
}

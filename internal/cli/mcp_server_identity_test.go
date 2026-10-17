package cli

import (
	"sort"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
)

// ONE CONFIG KEY PER RUNTIME IDENTITY, ENFORCED WHERE THE KEY IS WRITTEN.
//
// Registration trims the config-map key, so "docs" and "  docs" are two entries
// in the file and one server everywhere downstream. They share a tool count and
// a failure, map iteration decides which configuration wins, and each row
// redacts that shared failure with its own credentials, so the row that did not
// fail can print the other one's.
//
// Validation on the way in checks the incoming server by itself and cannot see
// the collision, so the check belongs at the write.
func TestUpsertRefusesAKeyThatCollidesWithAnExistingServer(t *testing.T) {
	cfg := &mcpWritableConfig{}
	cfg.ensureRaw()
	cfg.file.MCP.Servers = map[string]config.MCPServerConfig{
		"  docs": {URL: "https://a.invalid/mcp"},
	}

	_, err := cfg.upsertServer("docs", config.MCPServerConfig{URL: "https://b.invalid/mcp"})
	if err == nil {
		t.Fatalf("a colliding key was written: %#v", cfg.file.MCP.Servers)
	}
	for _, want := range []string{`"docs"`, `"  docs"`, "rename"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %s: %v", want, err)
		}
	}
	if len(cfg.file.MCP.Servers) != 1 {
		t.Errorf("the refused server was written anyway: %#v", cfg.file.MCP.Servers)
	}
}

// Updating a server through its own key is the ordinary case and must not be
// mistaken for a collision with itself.
func TestUpsertStillUpdatesTheSameKey(t *testing.T) {
	cfg := &mcpWritableConfig{}
	cfg.ensureRaw()
	cfg.file.MCP.Servers = map[string]config.MCPServerConfig{
		"  docs": {URL: "https://a.invalid/mcp"},
	}
	if _, err := cfg.upsertServer("  docs", config.MCPServerConfig{URL: "https://b.invalid/mcp"}); err != nil {
		t.Fatalf("updating a server through its own key was refused: %v", err)
	}
	if got := cfg.file.MCP.Servers["  docs"].URL; got != "https://b.invalid/mcp" {
		t.Errorf("URL = %q, want the update applied", got)
	}
}

// And an unrelated new server is not blocked by an existing one.
func TestUpsertAllowsADistinctName(t *testing.T) {
	cfg := &mcpWritableConfig{}
	cfg.ensureRaw()
	cfg.file.MCP.Servers = map[string]config.MCPServerConfig{
		"docs": {URL: "https://a.invalid/mcp"},
	}
	if _, err := cfg.upsertServer("search", config.MCPServerConfig{URL: "https://b.invalid/mcp"}); err != nil {
		t.Fatalf("a distinct server was refused: %v", err)
	}
	if len(cfg.file.MCP.Servers) != 2 {
		t.Errorf("servers = %#v, want both", cfg.file.MCP.Servers)
	}
}

// THE UNIQUENESS CHECK HAS TO RUN ON THE WHOLE CONFIGURATION, BEFORE THE SPLIT.
//
// Startup separates unconfigured built-in defaults from the servers the user
// asked for, so the two halves are normalized by separate calls. A collision
// that straddles the split is invisible to both of them: a user-configured
// "  exa" is critical, the built-in "exa" is optional, and they are one runtime
// server with two panel rows sharing a failure and a tool count.
func TestACollisionAcrossTheStartupSplitIsStillRefused(t *testing.T) {
	defaults := config.DefaultMCPServers()
	names := make([]string, 0, len(defaults))
	for name := range defaults {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Skip("no built-in MCP defaults to collide with")
	}
	name := names[0]

	// The user's own entry under a padded key: same runtime identity, different
	// configuration, so it is not the untouched default.
	mine := defaults[name]
	mine.Headers = map[string]string{"X-Api-Key": "opaque-workspace-9f3c2b7ae1d8"}

	cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		name:        defaults[name],
		"  " + name: mine,
	}}

	critical, optional := splitMCPStartupConfig(cfg)
	if len(critical.Servers) != 1 || len(optional.Servers) != 1 {
		t.Fatalf("the two entries did not straddle the split: critical=%v optional=%v", critical.Servers, optional.Servers)
	}
	// Each half on its own sees one server and no collision, which is why the
	// per-half check cannot be the guard.
	if err := mcp.ValidateUniqueNames(critical); err != nil {
		t.Fatalf("the critical half alone reported a collision: %v", err)
	}
	if err := mcp.ValidateUniqueNames(optional); err != nil {
		t.Fatalf("the optional half alone reported a collision: %v", err)
	}

	err := mcp.ValidateUniqueNames(cfg)
	if err == nil {
		t.Fatal("a collision straddling the startup split was accepted")
	}
	if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "rename") {
		t.Errorf("the refusal is not actionable: %v", err)
	}
}

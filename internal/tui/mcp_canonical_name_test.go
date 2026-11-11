package tui

import (
	"errors"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/tools"
)

// ONE SERVER IDENTITY, AND IT IS THE REGISTRY'S.
//
// mcp.normalizeServer trims the config-map key, so a server configured as
// " docs " is registered, recorded in SkippedServer.Name, and counted in
// toolCounts as "docs". The view was iterating raw map keys and looking failures
// up with them, so failures[" docs "] missed and a server that never started
// rendered as enabled. That is precisely the state this panel exists to surface.
func TestFailedServerIsMatchedByItsCanonicalName(t *testing.T) {
	for _, testCase := range []struct{ name, configKey string }{
		{name: "padded both sides", configKey: "  docs  "},
		{name: "trailing space", configKey: "docs "},
		{name: "leading space", configKey: " docs"},
		{name: "already canonical", configKey: "docs"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				testCase.configKey: {URL: "https://example.invalid/mcp"},
			}}
			// Registration records the CANONICAL name, which is what normalizeServer
			// produced from the padded key.
			skipped := []mcp.SkippedServer{{Name: "docs", Err: errors.New("dial tcp: connection refused")}}

			state := BuildMCPViewState(MCPStateOptions{Config: cfg, Skipped: skipped})
			if len(state.Servers) != 1 {
				t.Fatalf("expected one server view, got %d", len(state.Servers))
			}
			server := state.Servers[0]
			if server.State != "failed" {
				t.Errorf("server %q rendered as %q; a server that never started is being shown as running", testCase.configKey, server.State)
			}
			if server.Name != "docs" {
				t.Errorf("rendered name = %q, want the canonical %q", server.Name, "docs")
			}
			if server.Error == "" {
				t.Errorf("no failure reason rendered, so the operator is not told why it did not start")
			}

			// The tool count is keyed the same way: a running server registers its
			// tools under the canonical name, so a raw-key lookup reports none.
			registry := tools.NewRegistry()
			registry.Register(commandTestMCPTool{name: "mcp_docs_search", serverName: "docs", description: "search docs"})
			running := BuildMCPViewState(MCPStateOptions{Config: cfg, Registry: registry})
			if len(running.Servers) != 1 {
				t.Fatalf("expected one server view, got %d", len(running.Servers))
			}
			if got := running.Servers[0]; got.State != "enabled" || got.ToolCount != 1 {
				t.Errorf("server %q rendered as %q with %d tools, want enabled with 1", testCase.configKey, got.State, got.ToolCount)
			}
		})
	}
}

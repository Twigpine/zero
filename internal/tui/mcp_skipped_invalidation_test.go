package tui

import (
	"errors"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
)

func mcpConfigWith(name string, server config.MCPServerConfig) config.MCPConfig {
	return config.MCPConfig{Servers: map[string]config.MCPServerConfig{name: server}}
}

// A REPLACEMENT DOES NOT INHERIT ITS PREDECESSOR'S FAILURE.
//
// m.mcpSkipped is the startup snapshot and failures are matched by name, so
// removing a failed "docs" endpoint and adding a different one under the same
// name made the new entry render as failed, carrying an error about a server
// that no longer exists, and wrote that into the command transcript too.
func TestReplacedServerDoesNotInheritTheStartupFailure(t *testing.T) {
	failed := mcp.SkippedServer{Name: "docs", Err: errors.New("dial tcp: connection refused")}

	for _, testCase := range []struct {
		name string
		next config.MCPConfig
		want bool // want the failure retained
	}{
		{
			name: "same server untouched",
			next: mcpConfigWith("docs", config.MCPServerConfig{URL: "https://old.invalid/mcp"}),
			want: true,
		},
		{
			name: "replaced with a different url",
			next: mcpConfigWith("docs", config.MCPServerConfig{URL: "https://new.invalid/mcp"}),
			want: false,
		},
		{
			name: "replaced with a stdio command",
			next: mcpConfigWith("docs", config.MCPServerConfig{Command: "docs-mcp"}),
			want: false,
		},
		{
			name: "removed entirely",
			next: config.MCPConfig{Servers: map[string]config.MCPServerConfig{}},
			want: false,
		},
		{
			name: "rotated credential is a different server for this purpose",
			next: mcpConfigWith("docs", config.MCPServerConfig{URL: "https://old.invalid/mcp", Headers: map[string]string{"Authorization": "Bearer new"}}),
			want: false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			m := model{
				mcpConfig:  mcpConfigWith("docs", config.MCPServerConfig{URL: "https://old.invalid/mcp"}),
				mcpSkipped: []mcp.SkippedServer{failed},
			}
			m = m.adoptMCPConfig(testCase.next)

			retained := len(m.mcpSkipped) == 1
			if retained != testCase.want {
				t.Fatalf("failure retained = %v, want %v", retained, testCase.want)
			}

			state := BuildMCPViewState(MCPStateOptions{Config: m.mcpConfig, Skipped: m.mcpSkipped})
			for _, server := range state.Servers {
				if server.State == "failed" && !testCase.want {
					t.Errorf("the replacement renders as failed carrying %q, an error about a server that no longer exists", server.Error)
				}
				if server.State != "failed" && testCase.want {
					t.Errorf("an untouched failed server stopped reporting its failure")
				}
			}
		})
	}
}

// A padded config key and the trimmed name in the snapshot are the same server,
// so the observation must still be found when deciding whether to keep it.
func TestSkippedInvalidationMatchesTheCanonicalName(t *testing.T) {
	m := model{
		mcpConfig:  mcpConfigWith("  docs  ", config.MCPServerConfig{URL: "https://old.invalid/mcp"}),
		mcpSkipped: []mcp.SkippedServer{{Name: "docs", Err: errors.New("refused")}},
	}
	m = m.adoptMCPConfig(mcpConfigWith("  docs  ", config.MCPServerConfig{URL: "https://old.invalid/mcp"}))
	if len(m.mcpSkipped) != 1 {
		t.Errorf("an untouched failure was dropped because the config key was padded")
	}
}

package cli

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
)

// THE WRITE BOUNDARY MUST USE THE SAME ACTIVE-IDENTITY RULE AS THE READ PATH.
//
// ValidateUniqueNames skips disabled entries, because registration skips them
// and a disabled server claims no runtime identity. The write side compared key
// spellings instead, so it rejected combinations the running configuration
// accepts and could not see whether the incoming server was itself disabled.
func TestWriteCollisionMatchesTheDisabledServerPolicy(t *testing.T) {
	withServers := func(servers map[string]config.MCPServerConfig) *mcpWritableConfig {
		cfg := &mcpWritableConfig{}
		cfg.file.MCP.Servers = servers
		return cfg
	}
	enabled := config.MCPServerConfig{Type: "stdio", Command: "docs-mcp"}
	disabled := config.MCPServerConfig{Type: "stdio", Command: "docs-mcp", Disabled: true}

	t.Run("two enabled keys with one canonical name are refused", func(t *testing.T) {
		cfg := withServers(map[string]config.MCPServerConfig{" docs": enabled})
		err := cfg.refuseColliding("docs", enabled)
		if err == nil {
			t.Fatal("two enabled entries resolving to one name were accepted")
		}
		if !strings.Contains(err.Error(), "docs") {
			t.Errorf("the error does not name the collision: %v", err)
		}
	})

	t.Run("a disabled existing alias does not block the active server", func(t *testing.T) {
		cfg := withServers(map[string]config.MCPServerConfig{" docs": disabled})
		if err := cfg.refuseColliding("docs", enabled); err != nil {
			t.Errorf("adding an enabled server beside a disabled alias was refused: %v", err)
		}
	})

	t.Run("a disabled incoming server does not collide either", func(t *testing.T) {
		cfg := withServers(map[string]config.MCPServerConfig{"docs": enabled})
		if err := cfg.refuseColliding(" docs", disabled); err != nil {
			t.Errorf("adding a disabled alias beside an enabled server was refused: %v", err)
		}
	})

	t.Run("updating the enabled entry while its disabled alias remains", func(t *testing.T) {
		cfg := withServers(map[string]config.MCPServerConfig{
			"docs":  enabled,
			" docs": disabled,
		})
		if err := cfg.refuseColliding("docs", enabled); err != nil {
			t.Errorf("updating the enabled entry beside its disabled alias was refused: %v", err)
		}
	})
}

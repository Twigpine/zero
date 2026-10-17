package tui

import (
	"testing"

	"github.com/Twigpine/zero/internal/config"
)

// A DISPLAY NAME IS NOT AN IDENTITY.
//
// The canonical runtime name is trimmed, which is correct for display, for
// joining against skipped-failure records, and for tool counts. It is not
// unique: ValidateUniqueNames deliberately accepts an enabled "docs" beside a
// disabled " docs", because a disabled entry claims no runtime identity. Both
// rows then render as "docs".
//
// Actions address the configuration map by exact key, so carrying only the
// trimmed name meant a padded single key sent every action to a key that does
// not exist, and with an alias pair an action chosen on one row could be
// dispatched against the other.
func TestManagerRowsCarryTheirExactConfigKey(t *testing.T) {
	t.Run("a padded single key keeps its exact spelling", func(t *testing.T) {
		views := buildMCPServerViews(config.MCPConfig{Servers: map[string]config.MCPServerConfig{
			"  docs  ": {Type: "stdio", Command: "docs-mcp"},
		}}, nil, nil, nil, "")
		if len(views) != 1 {
			t.Fatalf("expected one view, got %d", len(views))
		}
		if views[0].Name != "docs" {
			t.Errorf("display name = %q, want the canonical %q", views[0].Name, "docs")
		}
		if views[0].ConfigKey != "  docs  " {
			t.Errorf("config key = %q, want the exact map key", views[0].ConfigKey)
		}
	})

	t.Run("an enabled entry and its disabled alias stay distinguishable", func(t *testing.T) {
		views := buildMCPServerViews(config.MCPConfig{Servers: map[string]config.MCPServerConfig{
			"docs":  {Type: "stdio", Command: "docs-mcp"},
			" docs": {Type: "stdio", Command: "docs-mcp", Disabled: true},
		}}, nil, nil, nil, "")
		if len(views) != 2 {
			t.Fatalf("expected two views, got %d", len(views))
		}
		keys := map[string]bool{}
		for _, view := range views {
			if view.Name != "docs" {
				t.Errorf("display name = %q, want %q for both rows", view.Name, "docs")
			}
			if view.ConfigKey == "" {
				t.Fatal("a row carries no config key, so no action can address it")
			}
			if keys[view.ConfigKey] {
				t.Fatalf("two rows share the config key %q, so they cannot be told apart", view.ConfigKey)
			}
			keys[view.ConfigKey] = true
		}
		if !keys["docs"] || !keys[" docs"] {
			t.Errorf("config keys = %v, want both exact spellings", keys)
		}
	})
}

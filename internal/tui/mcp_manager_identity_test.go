package tui

import (
	"strings"
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

// THE TRANSCRIPT HINTS ARE ACTIONS TOO.
//
// The manager overlay dispatches with ConfigKey, but the commands printed under
// each row in the /mcp transcript were built from the trimmed name. Copying one
// for a padded key ran against a key that does not exist, and for an alias pair
// it ran against the other entry.
func TestTranscriptActionHintsAddressTheExactConfigKey(t *testing.T) {
	render := func(t *testing.T, servers map[string]config.MCPServerConfig) string {
		t.Helper()
		views := buildMCPServerViews(config.MCPConfig{Servers: servers}, nil, nil, nil, "")
		return plainRender(t, renderMCPView(MCPViewState{Servers: views}, 200))
	}

	t.Run("a padded key is quoted with its exact spelling", func(t *testing.T) {
		got := render(t, map[string]config.MCPServerConfig{
			"  docs  ": {Type: "stdio", Command: "docs-mcp"},
		})
		for _, want := range []string{
			`zero mcp check "  docs  "`,
			`zero mcp disable "  docs  "`,
			`zero mcp remove "  docs  "`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("transcript hints missing %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, "zero mcp check docs") {
			t.Errorf("a hint addresses the trimmed name, which is not a key in this config:\n%s", got)
		}
	})

	t.Run("each row of an alias pair addresses its own entry", func(t *testing.T) {
		got := render(t, map[string]config.MCPServerConfig{
			"docs":  {Type: "stdio", Command: "docs-mcp"},
			" docs": {Type: "stdio", Command: "other-mcp", Disabled: true},
		})
		for _, want := range []string{
			"zero mcp disable docs",
			`zero mcp enable " docs"`,
			`zero mcp remove " docs"`,
		} {
			if !strings.Contains(got, want) {
				t.Errorf("transcript hints missing %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, "zero mcp enable docs") {
			t.Errorf("the disabled alias's hint addresses the enabled entry:\n%s", got)
		}
	})

	t.Run("oauth login keeps the trimmed name it resolves by", func(t *testing.T) {
		got := render(t, map[string]config.MCPServerConfig{
			" linear": {Type: "http", URL: "https://mcp.linear.example", Auth: "oauth"},
		})
		for _, want := range []string{`zero mcp check " linear"`, "zero mcp oauth login linear"} {
			if !strings.Contains(got, want) {
				t.Errorf("transcript hints missing %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, `zero mcp oauth login " linear"`) {
			t.Errorf("oauth login matches the normalized name, so the padded key cannot reach it:\n%s", got)
		}
	})

	t.Run("padding that cannot be typed back gets no key-addressed hint", func(t *testing.T) {
		got := render(t, map[string]config.MCPServerConfig{
			"\tdocs": {Type: "stdio", Command: "docs-mcp"},
		})
		if !strings.Contains(got, "docs · enabled") {
			t.Fatalf("the row itself should still render:\n%s", got)
		}
		if strings.Contains(got, "zero mcp check") || strings.ContainsRune(got, '\t') {
			t.Errorf("a hint was offered for a key it cannot spell:\n%q", got)
		}
	})
}

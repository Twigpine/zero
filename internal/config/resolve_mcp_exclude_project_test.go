package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveMCPExcludeProjectDropsProjectServers proves that ExcludeProject drops
// the project config layer from MCP resolution (fail-closed for an untrusted
// workspace) while keeping the built-in defaults and the user config server.
func TestResolveMCPExcludeProjectDropsProjectServers(t *testing.T) {
	userPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(userPath, []byte(`{"mcp":{"servers":{"user-srv":{"type":"stdio","command":"user-cmd"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(projectPath, []byte(`{"mcp":{"servers":{"proj-srv":{"type":"stdio","command":"proj-cmd"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("included when ExcludeProject is false", func(t *testing.T) {
		cfg, err := ResolveMCP(ResolveOptions{
			UserConfigPath:    userPath,
			ProjectConfigPath: projectPath,
			ExcludeProject:    false,
		})
		if err != nil {
			t.Fatalf("ResolveMCP: %v", err)
		}
		if _, ok := cfg.Servers["proj-srv"]; !ok {
			t.Fatal("a trusted resolve (ExcludeProject=false) must include the project server")
		}
		if _, ok := cfg.Servers["user-srv"]; !ok {
			t.Fatal("the user server must always be present")
		}
		if _, ok := cfg.Servers["exa"]; !ok {
			t.Fatal("the built-in default must always be present")
		}
	})

	t.Run("excluded when ExcludeProject is true", func(t *testing.T) {
		cfg, err := ResolveMCP(ResolveOptions{
			UserConfigPath:    userPath,
			ProjectConfigPath: projectPath,
			ExcludeProject:    true,
		})
		if err != nil {
			t.Fatalf("ResolveMCP: %v", err)
		}
		if _, ok := cfg.Servers["proj-srv"]; ok {
			t.Fatal("an untrusted resolve (ExcludeProject=true) must drop the project server")
		}
		if _, ok := cfg.Servers["user-srv"]; !ok {
			t.Fatal("the user server must survive when the project layer is dropped")
		}
		if _, ok := cfg.Servers["exa"]; !ok {
			t.Fatal("the built-in default must survive when the project layer is dropped")
		}
	})
}

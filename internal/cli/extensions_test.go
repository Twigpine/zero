package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/plugins"
	"github.com/Twigpine/zero/internal/tools"
)

func TestRunPluginsListsJSONAndText(t *testing.T) {
	cwd := t.TempDir()
	result := plugins.LoadResult{
		Plugins: []plugins.LoadedPlugin{{
			SchemaVersion: 1,
			ID:            "zero.docs",
			Name:          "Docs",
			Version:       "1.0.0",
			Enabled:       true,
			Source:        plugins.SourceProject,
			Prompts:       []plugins.PathExtension{{Name: "review", Path: filepath.Join(cwd, "review.md")}},
			Tools:         []plugins.ToolExtension{},
			Skills:        []plugins.PathExtension{},
			Hooks:         []plugins.HookExtension{},
		}},
	}
	deps := appDeps{
		getwd: func() (string, error) { return cwd, nil },
		loadPlugins: func(options plugins.LoadOptions) (plugins.LoadResult, error) {
			if options.Cwd != cwd {
				t.Fatalf("plugin Cwd = %q, want %q", options.Cwd, cwd)
			}
			return result, nil
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"plugins", "list", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	var payload struct {
		Plugins []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Version string `json:"version"`
			Source  string `json:"source"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("plugin JSON failed to decode: %v\n%s", err, stdout.String())
	}
	if payload.Plugins[0].ID != "zero.docs" || payload.Plugins[0].Name != "Docs" || payload.Plugins[0].Version != "1.0.0" {
		t.Fatalf("unexpected plugin JSON: %#v", payload)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"plugins", "list"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	for _, want := range []string{"Zero Plugins:", "zero.docs", "Docs", "1.0.0", "1 prompts"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("plugin text missing %q: %s", want, stdout.String())
		}
	}
}

func TestRunHooksListsRedactedJSONAndText(t *testing.T) {
	secret := "sk-proj-" + strings.Repeat("a", 23) + "0"
	result := hooks.LoadResult{
		Config: hooks.Config{
			Enabled: true,
			Hooks: []hooks.Definition{{
				ID:      "zero.preflight",
				Event:   hooks.EventBeforeTool,
				Matcher: "bash",
				Command: "node",
				Args:    []string{"hooks/preflight.mjs", secret},
				Enabled: true,
			}},
		},
	}
	deps := appDeps{
		getwd: func() (string, error) { return t.TempDir(), nil },
		loadHooks: func(options hooks.LoadOptions) (hooks.LoadResult, error) {
			return result, nil
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"hooks", "list", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if strings.Contains(stdout.String(), secret) || !strings.Contains(stdout.String(), "[REDACTED]") {
		t.Fatalf("hook JSON redaction failed: %s", stdout.String())
	}
	var payload struct {
		Hooks struct {
			Hooks []struct {
				ID string `json:"id"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("hook JSON failed to decode: %v\n%s", err, stdout.String())
	}
	if payload.Hooks.Hooks[0].ID != "zero.preflight" {
		t.Fatalf("unexpected hook JSON: %#v", payload)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"hooks", "list"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Zero Hooks") || !strings.Contains(stdout.String(), "zero.preflight") {
		t.Fatalf("unexpected hook text: %s", stdout.String())
	}
	if strings.Contains(stdout.String(), secret) || !strings.Contains(stdout.String(), "[REDACTED]") {
		t.Fatalf("hook text redaction failed: %s", stdout.String())
	}
}

func TestRunMCPPermissionsListRevokeAndClear(t *testing.T) {
	store, err := mcp.NewPermissionStore(mcp.StoreOptions{
		FilePath: filepath.Join(t.TempDir(), "mcp-permissions.json"),
		Now:      func() time.Time { return time.Date(2026, 6, 3, 9, 30, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewPermissionStore returned error: %v", err)
	}
	if _, err := store.GrantServer(mcp.GrantServerInput{ServerName: "docs", ServerIdentity: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", MaxAutonomy: mcp.AutonomyMedium}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantTool(mcp.GrantToolInput{ServerName: "docs", ServerIdentity: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ToolName: "lookup", MaxAutonomy: mcp.AutonomyLow}); err != nil {
		t.Fatal(err)
	}
	deps := appDeps{newMCPStore: func() (*mcp.PermissionStore, error) { return store, nil }}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"mcp", "permissions", "list", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	var listPayload struct {
		Permissions []mcp.PermissionGrant `json:"permissions"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &listPayload); err != nil {
		t.Fatalf("permission JSON failed to decode: %v\n%s", err, stdout.String())
	}
	if len(listPayload.Permissions) != 2 {
		t.Fatalf("expected 2 permissions, got %#v", listPayload.Permissions)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"mcp", "permissions", "revoke", "docs", "lookup", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"scope": "tool"`) || !strings.Contains(stdout.String(), `"revoked": 1`) {
		t.Fatalf("unexpected revoke tool output: %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"mcp", "permissions", "clear", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitUsage {
		t.Fatalf("clear without confirm exitCode = %d", exitCode)
	}
	if !strings.Contains(stderr.String(), "--confirm") {
		t.Fatalf("expected confirm warning, got %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"mcp", "permissions", "clear", "--confirm", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"cleared": 1`) {
		t.Fatalf("unexpected clear output: %s", stdout.String())
	}
}

func TestRunMCPToolsListJSONAndText(t *testing.T) {
	cwd := t.TempDir()
	closeCalls := 0
	deps := appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveMCPConfig: func(workspaceRoot string, _ bool) (config.MCPConfig, error) {
			if workspaceRoot != cwd {
				t.Fatalf("workspaceRoot = %q, want %q", workspaceRoot, cwd)
			}
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"docs": {Type: "stdio", Command: "docs-mcp"},
			}}, nil
		},
		registerMCPTools: func(ctx context.Context, registry *tools.Registry, cfg config.MCPConfig, options mcp.RegisterOptions) (mcpToolRuntime, error) {
			registry.Register(cliFakeMCPRegistryTool{})
			return closeFunc(func() error {
				closeCalls++
				return nil
			}), nil
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"mcp", "tools", "list", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	var payload struct {
		Tools []struct {
			Name       string `json:"name"`
			Permission string `json:"permission"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("MCP tools JSON failed to decode: %v\n%s", err, stdout.String())
	}
	if len(payload.Tools) != 1 || payload.Tools[0].Name != "mcp_docs_lookup" || payload.Tools[0].Permission != string(tools.PermissionAllow) {
		t.Fatalf("unexpected MCP tools JSON: %#v", payload)
	}
	if closeCalls != 1 {
		t.Fatalf("MCP runtime close calls after JSON list = %d, want 1", closeCalls)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"mcp", "tools", "list"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	for _, want := range []string{"MCP Tools:", "mcp_docs_lookup", "Lookup documentation", "allow"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("MCP tools text missing %q: %s", want, stdout.String())
		}
	}
	if closeCalls != 2 {
		t.Fatalf("MCP runtime close calls after text list = %d, want 2", closeCalls)
	}
}

func TestRunMCPLegacyListAliases(t *testing.T) {
	cwd := t.TempDir()
	closeCalls := 0
	deps := appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveMCPConfig: func(workspaceRoot string, _ bool) (config.MCPConfig, error) {
			if workspaceRoot != cwd {
				t.Fatalf("workspaceRoot = %q, want %q", workspaceRoot, cwd)
			}
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"docs": {Type: "stdio", Command: "docs-mcp"},
			}}, nil
		},
		registerMCPTools: func(ctx context.Context, registry *tools.Registry, cfg config.MCPConfig, options mcp.RegisterOptions) (mcpToolRuntime, error) {
			registry.Register(cliFakeMCPRegistryTool{})
			return closeFunc(func() error {
				closeCalls++
				return nil
			}), nil
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"mcp", "list", "--tools", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"tools"`) || !strings.Contains(stdout.String(), "mcp_docs_lookup") {
		t.Fatalf("unexpected legacy mcp tools list output: %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"mcp", "list", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"servers"`) || !strings.Contains(stdout.String(), `"docs"`) {
		t.Fatalf("unexpected legacy mcp server list output: %s", stdout.String())
	}
	if closeCalls != 1 {
		t.Fatalf("legacy server list should not connect tools, closeCalls = %d", closeCalls)
	}
}

func TestRunMCPPermissionsHelpDoesNotOpenStore(t *testing.T) {
	deps := appDeps{
		newMCPStore: func() (*mcp.PermissionStore, error) {
			return nil, errors.New("store should not be opened for help")
		},
	}

	for _, args := range [][]string{
		{"mcp", "permissions", "list", "--help"},
		{"mcp", "permissions", "revoke", "--help"},
		{"mcp", "permissions", "clear", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			exitCode := runWithDeps(args, &stdout, &stderr, deps)
			if exitCode != exitSuccess {
				t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("expected empty stderr, got %q", stderr.String())
			}
			if !strings.Contains(stdout.String(), "zero mcp permissions") {
				t.Fatalf("expected help output, got %q", stdout.String())
			}
		})
	}
}

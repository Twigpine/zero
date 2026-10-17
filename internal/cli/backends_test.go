package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/plugins"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zerocommands"
)

func TestRunBackendsJSONUsesLifecycleSnapshotWithoutConnectingMCP(t *testing.T) {
	cwd := t.TempDir()
	secret := "sk-proj-" + strings.Repeat("a", 23) + "0"
	deps := appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveMCPConfig: func(workspaceRoot string, _ bool) (config.MCPConfig, error) {
			if workspaceRoot != cwd {
				t.Fatalf("workspaceRoot = %q, want %q", workspaceRoot, cwd)
			}
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"zulu": {
					Type: "http",
					URL:  "https://admin:secret@example.com/mcp?token=" + secret + "&mode=readonly",
					Headers: map[string]string{
						"Authorization": "Bearer " + secret,
					},
				},
				"alpha": {
					Type:    "stdio",
					Command: "alpha-mcp",
					Args:    []string{"--project", cwd},
					Env: map[string]string{
						"ALPHA_TOKEN": secret,
					},
				},
				"disabled": {
					Type:     "stdio",
					Command:  "disabled-mcp",
					Disabled: true,
				},
			}}, nil
		},
		loadHooks: func(options hooks.LoadOptions) (hooks.LoadResult, error) {
			if options.Cwd != cwd {
				t.Fatalf("hook Cwd = %q, want %q", options.Cwd, cwd)
			}
			return hooks.LoadResult{Config: hooks.Config{Hooks: []hooks.Definition{{
				ID:      "zero.preflight",
				Event:   hooks.EventBeforeTool,
				Matcher: "bash",
				Command: "sh",
				Args:    []string{"-c", "echo " + secret},
				Enabled: true,
			}}}}, nil
		},
		loadPlugins: func(options plugins.LoadOptions) (plugins.LoadResult, error) {
			if options.Cwd != cwd {
				t.Fatalf("plugin Cwd = %q, want %q", options.Cwd, cwd)
			}
			return plugins.LoadResult{Plugins: []plugins.LoadedPlugin{{
				ID:           "zero.docs",
				Name:         "Docs",
				Description:  "uses " + secret,
				Enabled:      true,
				Source:       plugins.SourceProject,
				Root:         "C:/tmp/" + secret,
				PluginDir:    "C:/tmp/plugin",
				ManifestPath: "C:/tmp/plugin/plugin.json?token=" + secret,
				Tools:        []plugins.ToolExtension{{Name: "lookup"}},
				Prompts:      []plugins.PathExtension{{Name: "review"}},
				Hooks:        []plugins.HookExtension{{Name: "audit"}},
			}}}, nil
		},
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return nil, errors.New("zero backends must not connect to MCP servers")
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"backends", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if strings.Contains(stdout.String(), secret) || strings.Contains(stdout.String(), "admin:secret") || strings.Contains(stdout.String(), "Authorization") || strings.Contains(stdout.String(), "ALPHA_TOKEN") {
		t.Fatalf("backend JSON leaked secret material:\n%s", stdout.String())
	}

	var snapshot zerocommands.BackendLifecycleSnapshot
	if err := json.Unmarshal(stdout.Bytes(), &snapshot); err != nil {
		t.Fatalf("backend JSON failed to decode: %v\n%s", err, stdout.String())
	}
	if len(snapshot.MCPServers) != 2 || snapshot.MCPServers[0].Name != "alpha" || snapshot.MCPServers[1].Name != "zulu" {
		t.Fatalf("unexpected MCP snapshots: %#v", snapshot.MCPServers)
	}
	if snapshot.MCPServers[0].ArgCount != 2 || snapshot.MCPServers[0].EnvKeyCount != 1 {
		t.Fatalf("stdio MCP counts wrong: %#v", snapshot.MCPServers[0])
	}
	if snapshot.MCPServers[1].HeaderCount != 1 || !strings.Contains(snapshot.MCPServers[1].URL, "mode=readonly") {
		t.Fatalf("http MCP snapshot missing safe status data: %#v", snapshot.MCPServers[1])
	}
	if len(snapshot.Hooks) != 1 || snapshot.Hooks[0].ID != "zero.preflight" || strings.Contains(strings.Join(snapshot.Hooks[0].Args, " "), secret) {
		t.Fatalf("unexpected hook snapshots: %#v", snapshot.Hooks)
	}
	if len(snapshot.Plugins) != 1 || snapshot.Plugins[0].ID != "zero.docs" || snapshot.Plugins[0].ToolCount != 1 || snapshot.Plugins[0].PromptCount != 1 || snapshot.Plugins[0].HookCount != 1 {
		t.Fatalf("unexpected plugin snapshots: %#v", snapshot.Plugins)
	}
}

func TestRunBackendsTextAndHelp(t *testing.T) {
	deps := appDeps{
		getwd: func() (string, error) { return t.TempDir(), nil },
		resolveMCPConfig: func(string, bool) (config.MCPConfig, error) {
			return config.MCPConfig{}, nil
		},
		loadHooks: func(hooks.LoadOptions) (hooks.LoadResult, error) {
			return hooks.LoadResult{}, nil
		},
		loadPlugins: func(plugins.LoadOptions) (plugins.LoadResult, error) {
			return plugins.LoadResult{}, nil
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"backends"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	for _, want := range []string{"Zero Backends:", "MCP servers: 0", "Hooks: 0", "Plugins: 0"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("backend text missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"backends", "--help"}, &stdout, &stderr, appDeps{})
	if exitCode != exitSuccess {
		t.Fatalf("help exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	for _, want := range []string{"Usage:", "zero backends", "--json"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("backend help missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunBackendsDoctorJSONAndTextWithoutConnectingMCP(t *testing.T) {
	cwd := t.TempDir()
	secret := "sk-proj-" + strings.Repeat("b", 23) + "0"
	deps := appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveMCPConfig: func(workspaceRoot string, _ bool) (config.MCPConfig, error) {
			if workspaceRoot != cwd {
				t.Fatalf("workspaceRoot = %q, want %q", workspaceRoot, cwd)
			}
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"remote": {
					Type: "http",
					URL:  "https://api.example.com/mcp?token=" + secret,
				},
				"broken": {
					Type: "http",
				},
			}}, nil
		},
		loadHooks: func(options hooks.LoadOptions) (hooks.LoadResult, error) {
			if options.Cwd != cwd {
				t.Fatalf("hook Cwd = %q, want %q", options.Cwd, cwd)
			}
			return hooks.LoadResult{Diagnostics: []hooks.Diagnostic{{
				Kind:    hooks.DiagnosticSchema,
				Message: "schema failed " + secret,
				HookID:  "zero.bad",
			}}}, nil
		},
		loadPlugins: func(options plugins.LoadOptions) (plugins.LoadResult, error) {
			if options.Cwd != cwd {
				t.Fatalf("plugin Cwd = %q, want %q", options.Cwd, cwd)
			}
			return plugins.LoadResult{Plugins: []plugins.LoadedPlugin{{
				ID:      "zero.docs",
				Name:    "Docs",
				Enabled: true,
				Source:  plugins.SourceProject,
			}}}, nil
		},
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return nil, errors.New("zero backends doctor must not connect to MCP servers")
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"backends", "doctor", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitProvider {
		t.Fatalf("exitCode = %d, want %d stderr=%s stdout=%s", exitCode, exitProvider, stderr.String(), stdout.String())
	}
	if strings.Contains(stdout.String(), secret) || strings.Contains(stdout.String(), "sk-proj-") {
		t.Fatalf("backend doctor JSON leaked secret material:\n%s", stdout.String())
	}
	var payload zerocommands.BackendDoctorReport
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("backend doctor JSON failed to decode: %v\n%s", err, stdout.String())
	}
	if payload.OK {
		t.Fatalf("payload.OK = true, want false for invalid MCP/hook diagnostic: %#v", payload.Checks)
	}
	if payload.Status != zerocommands.BackendDoctorStatusFail {
		t.Fatalf("payload.Status = %q, want %q", payload.Status, zerocommands.BackendDoctorStatusFail)
	}
	assertBackendDoctorPayloadCheck(t, payload, "backend.mcp.invalid", "broken")
	assertBackendDoctorPayloadCheck(t, payload, "backend.hooks.diagnostic", "zero.bad")

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"backends", "doctor"}, &stdout, &stderr, deps)
	if exitCode != exitProvider {
		t.Fatalf("text exitCode = %d, want %d stderr=%s", exitCode, exitProvider, stderr.String())
	}
	for _, want := range []string{"Zero backend doctor", "[fail] backend.mcp.invalid", "zero mcp add broken", "[fail] backend.hooks.diagnostic"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("backend doctor text missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunBackendsDoctorHelp(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"backends", "doctor", "--help"}, &stdout, &stderr, appDeps{})
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	for _, want := range []string{"Usage:", "zero backends doctor", "--json"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("backend doctor help missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunBackendsDoctorDoesNotConnectOrExecuteConfiguredBackends(t *testing.T) {
	cwd := t.TempDir()
	hookSentinel := filepath.Join(cwd, "hook-ran")
	pluginToolSentinel := filepath.Join(cwd, "plugin-tool-ran")
	pluginHookSentinel := filepath.Join(cwd, "plugin-hook-ran")
	helperCommand := os.Args[0]
	helperArgs := func(path string) []string {
		return []string{"-test.run=TestBackendDoctorHelperProcess", "--", "--zero-backend-doctor-sentinel", path}
	}

	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	hookConfigPath := filepath.Join(cwd, ".zero", "hooks.json")
	writeBackendDoctorJSON(t, hookConfigPath, map[string]any{
		"enabled": true,
		"hooks": []any{map[string]any{
			"id":      "zero.sentinel",
			"event":   "sessionStart",
			"command": helperCommand,
			"args":    helperArgs(hookSentinel),
		}},
	})
	pluginDir := filepath.Join(cwd, ".zero", "plugins", "sentinel")
	writeBackendDoctorJSON(t, filepath.Join(pluginDir, "plugin.json"), map[string]any{
		"schemaVersion": 1,
		"id":            "zero.sentinel",
		"name":          "Sentinel",
		"version":       "1.0.0",
		"tools": []any{map[string]any{
			"name":    "sentinel_tool",
			"command": helperCommand,
			"args":    helperArgs(pluginToolSentinel),
		}},
		"hooks": []any{map[string]any{
			"name":    "sentinel_hook",
			"event":   "sessionStart",
			"command": helperCommand,
			"args":    helperArgs(pluginHookSentinel),
		}},
	})

	deps := appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveMCPConfig: func(string, bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"remote": {Type: "http", URL: server.URL + "/mcp"},
			}}, nil
		},
		loadHooks: func(options hooks.LoadOptions) (hooks.LoadResult, error) {
			return hooks.LoadConfig(hooks.LoadOptions{
				Cwd:               options.Cwd,
				UserConfigPath:    filepath.Join(cwd, "missing-user-hooks.json"),
				ProjectConfigPath: hookConfigPath,
			})
		},
		loadPlugins: func(options plugins.LoadOptions) (plugins.LoadResult, error) {
			return plugins.Load(plugins.LoadOptions{
				Roots: []plugins.Root{{Source: plugins.SourceProject, Path: filepath.Join(cwd, ".zero", "plugins")}},
				Cwd:   options.Cwd,
			})
		},
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return nil, errors.New("zero backends doctor must not register MCP tools")
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"backends", "doctor", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s stdout=%s", exitCode, stderr.String(), stdout.String())
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("backend doctor connected to MCP HTTP server %d time(s)", got)
	}
	for _, sentinel := range []string{hookSentinel, pluginToolSentinel, pluginHookSentinel} {
		if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("backend doctor executed configured backend command, sentinel=%s err=%v", sentinel, err)
		}
	}
}

func assertBackendDoctorPayloadCheck(t *testing.T, report zerocommands.BackendDoctorReport, id string, target string) {
	t.Helper()
	for _, check := range report.Checks {
		if check.ID == id && check.Target == target {
			return
		}
	}
	t.Fatalf("check %s/%s not found in %#v", id, target, report.Checks)
}

func writeBackendDoctorJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create dir for %s: %v", path, err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestBackendDoctorHelperProcess(t *testing.T) {
	for index, arg := range os.Args {
		if arg != "--zero-backend-doctor-sentinel" || index+1 >= len(os.Args) {
			continue
		}
		if err := os.WriteFile(os.Args[index+1], []byte("executed"), 0o600); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
}

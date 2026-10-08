package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/mcp"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/tui"
)

// cliFakeDeferredTool is deferred-eligible (implements Deferred() bool), mirroring
// an MCP registry tool, so it counts toward the deferral threshold.
type cliFakeDeferredTool struct {
	name            string
	permission      tools.Permission
	advertiseInAuto bool
}

func (t cliFakeDeferredTool) Name() string             { return t.name }
func (t cliFakeDeferredTool) Description() string      { return "fake deferred tool" }
func (t cliFakeDeferredTool) Parameters() tools.Schema { return tools.Schema{Type: "object"} }
func (t cliFakeDeferredTool) Safety() tools.Safety {
	permission := t.permission
	if permission == "" {
		permission = tools.PermissionAllow
	}
	return tools.Safety{SideEffect: tools.SideEffectNetwork, Permission: permission, AdvertiseInAuto: t.advertiseInAuto}
}
func (t cliFakeDeferredTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK, Output: "ok"}
}
func (t cliFakeDeferredTool) Deferred() bool { return true }

func registryHasToolSearch(registry *tools.Registry) bool {
	_, ok := registry.Get("tool_search")
	return ok
}

func TestRegisterToolSearchIfEligibleRegistersAtThreshold(t *testing.T) {
	registry := tools.NewRegistry()
	for i := 0; i < 3; i++ {
		registry.Register(cliFakeDeferredTool{name: "mcp_srv_t" + string(rune('a'+i))})
	}

	registerToolSearchIfEligible(registry, 3, agent.PermissionModeAuto, nil, nil)

	if !registryHasToolSearch(registry) {
		t.Fatal("expected tool_search registered when eligible count == threshold")
	}
}

func TestRegisterToolSearchIfEligibleSkipsBelowThreshold(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(cliFakeDeferredTool{name: "mcp_srv_ta"})
	registry.Register(cliFakeDeferredTool{name: "mcp_srv_tb"})
	// A plain (non-deferred) MCP-named tool must NOT count toward eligibility.
	registry.Register(cliFakeMCPRegistryTool{})

	registerToolSearchIfEligible(registry, 3, agent.PermissionModeAuto, nil, nil)

	if registryHasToolSearch(registry) {
		t.Fatal("expected no tool_search when eligible count (2) < threshold (3)")
	}
}

func TestRegisterToolSearchIfEligibleSkipsWhenThresholdZero(t *testing.T) {
	registry := tools.NewRegistry()
	for i := 0; i < 5; i++ {
		registry.Register(cliFakeDeferredTool{name: "mcp_srv_t" + string(rune('a'+i))})
	}

	registerToolSearchIfEligible(registry, 0, agent.PermissionModeAuto, nil, nil)

	if registryHasToolSearch(registry) {
		t.Fatal("expected no tool_search when threshold is 0 (disabled)")
	}
}

func TestDeferredEligibleCountIncludesOptionalBuiltins(t *testing.T) {
	registry := newCoreRegistry(t.TempDir())
	if got := deferredEligibleCount(registry, agent.PermissionModeAuto, nil, nil); got < 2 {
		t.Fatalf("deferredEligibleCount(core) = %d, want at least 2 optional built-ins", got)
	}
}

// FIX 1: a deferred tool the operator hid via --disabled-tools must NOT count
// toward the visible-deferred total, so registration agrees with the loop's
// activation gate. Two deferred + threshold 2 normally registers tool_search;
// disabling one drops the visible count to 1 (< 2) so it must NOT register.
func TestRegisterToolSearchSkipsWhenDisabledDropsVisibleBelowThreshold(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(cliFakeDeferredTool{name: "mcp_srv_ta"})
	registry.Register(cliFakeDeferredTool{name: "mcp_srv_tb"})

	registerToolSearchIfEligible(registry, 2, agent.PermissionModeAuto, nil, []string{"mcp_srv_tb"})

	if registryHasToolSearch(registry) {
		t.Fatal("expected no tool_search: a disabled deferred tool must not count toward the visible-deferred total")
	}
}

// FIX 3: validateExecToolFilters must treat tool_search as always-valid even
// though it is not registered yet at validation time (it is registered later only
// when deferral activates). Listing it in --enabled-tools/--disabled-tools must
// not raise "Unknown tool".
func TestValidateExecToolFiltersAllowsToolSearch(t *testing.T) {
	registry := newCoreRegistry(t.TempDir())
	// tool_search is NOT registered in this registry — it would be added later.
	if _, present := registry.Get(tools.ToolSearchToolName); present {
		t.Fatalf("precondition: tool_search must not be registered yet")
	}

	if err := validateExecToolFilters(execOptions{enabledTools: []string{tools.ToolSearchToolName}}, registry); err != nil {
		t.Fatalf("--enabled-tools tool_search must validate, got error: %v", err)
	}
	if err := validateExecToolFilters(execOptions{disabledTools: []string{tools.ToolSearchToolName}}, registry); err != nil {
		t.Fatalf("--disabled-tools tool_search must validate, got error: %v", err)
	}
	// A genuinely unknown tool still errors.
	if err := validateExecToolFilters(execOptions{enabledTools: []string{"definitely_not_a_tool"}}, registry); err == nil {
		t.Fatal("expected an Unknown tool error for an unregistered, non-tool_search name")
	}
}

// TestRunExecListToolsAdvertisesMCPToolsWithoutToolSearch verifies that
// `exec --list-tools` lists the core + MCP tools WITHOUT tool_search. This is
// independent of the deferral threshold: --list-tools short-circuits and returns
// before registerToolSearchIfEligible runs (see exec.go), so tool_search is
// never registered on this path. The threshold gate itself is
// exercised by TestRegisterToolSearchIfEligible{RegistersAtThreshold,
// SkipsBelowThreshold,SkipsWhenThresholdZero} and the end-to-end at-threshold
// path in TestTUIRunThreadsDeferThresholdAndRegistersToolSearch.
func TestRunExecListToolsAdvertisesMCPToolsWithoutToolSearch(t *testing.T) {
	cwd := t.TempDir()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"exec", "--list-tools"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		resolveMCPConfig: func(string, bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"docs": {Type: "stdio", Command: "docs-mcp"},
			}}, nil
		},
		newMCPStore: func() (*mcp.PermissionStore, error) { return nil, nil },
		registerMCPTools: func(_ context.Context, registry *tools.Registry, _ config.MCPConfig, _ mcp.RegisterOptions) (mcpToolRuntime, error) {
			registry.Register(cliFakeMCPRegistryTool{})
			return closeFunc(func() error { return nil }), nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "mcp_docs_lookup") {
		t.Fatalf("expected MCP tool advertised by --list-tools, got %q", out)
	}
	// --list-tools never registers tool_search (it returns before the threshold
	// gate runs); the threshold logic is verified by the unit/E2E tests named above.
	if strings.Contains(out, "tool_search") {
		t.Fatalf("expected NO tool_search from --list-tools, got %q", out)
	}
}

func TestRunExecListToolsHonorsJSONFormat(t *testing.T) {
	cwd := t.TempDir()
	var stdout, stderr bytes.Buffer

	exitCode := runWithDeps([]string{"exec", "--list-tools", "-o", "json"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return execResolvedConfig(), nil
		},
		resolveMCPConfig: func(string, bool) (config.MCPConfig, error) { return config.MCPConfig{}, nil },
		newMCPStore:      func() (*mcp.PermissionStore, error) { return nil, nil },
		registerMCPTools: func(_ context.Context, _ *tools.Registry, _ config.MCPConfig, _ mcp.RegisterOptions) (mcpToolRuntime, error) {
			return closeFunc(func() error { return nil }), nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	out := strings.TrimSpace(stdout.String())
	// Must be JSON, NOT the plain-text "Tools visible to model:" listing.
	if strings.Contains(out, "Tools visible to model:") {
		t.Fatalf("expected JSON output for -o json, got text listing: %q", out)
	}
	var payload struct {
		Type  string `json:"type"`
		Tools []struct {
			Name       string `json:"name"`
			Permission string `json:"permission"`
			SideEffect string `json:"side_effect"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("--list-tools -o json must emit valid JSON: %v\n%q", err, out)
	}
	if payload.Type != "tools" {
		t.Fatalf("expected type=tools, got %q", payload.Type)
	}
	if len(payload.Tools) == 0 {
		t.Fatalf("expected at least one tool in the JSON listing")
	}
	if payload.Tools[0].Name == "" || payload.Tools[0].Permission == "" {
		t.Fatalf("tool entries must carry name + permission: %#v", payload.Tools[0])
	}
}

func TestTUIRunThreadsDeferThresholdAndRegistersToolSearch(t *testing.T) {
	cwd := t.TempDir()
	var captured agent.Options
	var capturedRegistry *tools.Registry

	// Empty args route runWithDeps to the interactive TUI (runInteractiveTUIWithSkin).
	// There is no "--tui" flag; that arg would hit the unknown-command path.
	exitCode := runWithDeps([]string{}, io.Discard, io.Discard, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{
				Provider: config.ProviderProfile{
					Name:         "p",
					ProviderKind: config.ProviderKindOpenAICompatible,
					BaseURL:      "http://127.0.0.1/v1",
					Model:        "m",
				},
				Tools: config.ToolsConfig{DeferThreshold: 2},
			}, nil
		},
		resolveMCPConfig: func(string, bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"docs": {Type: "stdio", Command: "docs-mcp"},
			}}, nil
		},
		newMCPStore: func() (*mcp.PermissionStore, error) { return nil, nil },
		registerMCPTools: func(_ context.Context, registry *tools.Registry, _ config.MCPConfig, _ mcp.RegisterOptions) (mcpToolRuntime, error) {
			registry.Register(cliFakeDeferredTool{name: "mcp_docs_ta"})
			registry.Register(cliFakeDeferredTool{name: "mcp_docs_tb"})
			return closeFunc(func() error { return nil }), nil
		},
		runTUI: func(_ context.Context, options tui.Options) int {
			captured = options.AgentOptions
			capturedRegistry = options.Registry
			return exitSuccess
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit %d, got %d", exitSuccess, exitCode)
	}
	if captured.DeferThreshold != 2 {
		t.Fatalf("AgentOptions.DeferThreshold = %d, want 2", captured.DeferThreshold)
	}
	if capturedRegistry == nil {
		t.Fatal("expected registry passed to TUI")
	}
	if _, ok := capturedRegistry.Get("tool_search"); !ok {
		t.Fatal("expected tool_search registered for TUI run at/above threshold")
	}
}

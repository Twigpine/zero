package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/mcp"
	"github.com/Gitlawb/zero/internal/redaction"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/tui"
	"github.com/Gitlawb/zero/internal/update"
	"github.com/Gitlawb/zero/internal/workspacetrust"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

var errWriteFailed = errors.New("write failed")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errWriteFailed
}

func TestRunPrintsVersion(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := Run([]string{"--version"}, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	// Non-terminal stdout keeps the machine-readable contract: exactly one
	// "zero <version>" record, no wordmark banner, no ANSI.
	if got := stdout.String(); got != "zero dev\n" {
		t.Fatalf("expected version output, got %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

// TestRunVersionRedirectedFile covers the `zero --version > file` path: stdout
// is a real *os.File but not a terminal, so the single-line contract must hold.
func TestRunVersionRedirectedFile(t *testing.T) {
	stdout, err := os.CreateTemp(t.TempDir(), "version-stdout")
	if err != nil {
		t.Fatalf("create temp stdout: %v", err)
	}
	defer stdout.Close()
	var stderr bytes.Buffer

	exitCode := Run([]string{"--version"}, stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	content, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatalf("read temp stdout: %v", err)
	}
	if got := string(content); got != "zero dev\n" {
		t.Fatalf("expected version output, got %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunPrintsHelp(t *testing.T) {
	for _, args := range [][]string{
		{"--help"},
		{"-h"},
		{"help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			assertHelpOutput(t, args)
		})
	}
}

func setCLIUserConfigRoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("APPDATA", root)
	case "darwin":
		t.Setenv("HOME", root)
	default:
		t.Setenv("XDG_CONFIG_HOME", root)
	}

	configRoot, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("UserConfigDir() error = %v", err)
	}
	return configRoot
}

func TestRunNoArgsLaunchesSetupTUIWithNilProviderWhenNoProviderConfigured(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	setCLIUserConfigRoot(t)
	projectConfigPath := filepath.Join(cwd, ".zero", "config.json")
	if err := os.MkdirAll(filepath.Dir(projectConfigPath), 0o700); err != nil {
		t.Fatalf("create project config parent: %v", err)
	}
	if err := os.WriteFile(projectConfigPath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	userConfigPath := filepath.Join(t.TempDir(), "zero", "config.json")
	var launchedOptions tui.Options

	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			if workspaceRoot != cwd {
				t.Fatalf("workspaceRoot = %q, want %q", workspaceRoot, cwd)
			}
			return config.ResolvedConfig{MaxTurns: 12}, nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			t.Fatal("newProvider should not be called without a resolved provider")
			return nil, nil
		},
		userConfigPath: func() (string, error) {
			return userConfigPath, nil
		},
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return noopMCPRuntime{}, nil
		},
		runTUI: func(ctx context.Context, options tui.Options) int {
			launchedOptions = options
			return 7
		},
	})

	if exitCode != 7 {
		t.Fatalf("expected TUI exit code 7, got %d", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	if launchedOptions.Cwd != cwd {
		t.Fatalf("Cwd = %q, want %q", launchedOptions.Cwd, cwd)
	}
	if launchedOptions.UserConfigPath != userConfigPath {
		t.Fatalf("UserConfigPath = %q, want %q", launchedOptions.UserConfigPath, userConfigPath)
	}
	if launchedOptions.DoctorUserConfigPath != "" {
		t.Fatalf("DoctorUserConfigPath = %q, want empty for missing user config", launchedOptions.DoctorUserConfigPath)
	}
	if launchedOptions.ProjectConfigPath != projectConfigPath {
		t.Fatalf("ProjectConfigPath = %q, want %q", launchedOptions.ProjectConfigPath, projectConfigPath)
	}
	if launchedOptions.Provider != nil {
		t.Fatalf("Provider = %#v, want nil", launchedOptions.Provider)
	}
	if launchedOptions.ProviderName != "" || launchedOptions.ModelName != "" {
		t.Fatalf("provider metadata = %q/%q, want empty", launchedOptions.ProviderName, launchedOptions.ModelName)
	}
	if !launchedOptions.Setup.Visible || !launchedOptions.Setup.Required {
		t.Fatalf("Setup = %#v, want visible required setup", launchedOptions.Setup)
	}
	if launchedOptions.Setup.ConfigPath != userConfigPath {
		t.Fatalf("Setup.ConfigPath = %q, want %q", launchedOptions.Setup.ConfigPath, userConfigPath)
	}
	assertCoreRegistry(t, launchedOptions.Registry)
	assertAgentOptions(t, launchedOptions, 12, agent.PermissionModeAsk)
}

type fakeMCPRuntimeWithSkips struct {
	skipped []mcp.SkippedServer
}

func (r fakeMCPRuntimeWithSkips) Close() error { return nil }

func (r fakeMCPRuntimeWithSkips) Skipped() []mcp.SkippedServer { return r.skipped }

func TestTUIStartupSuppressesWarningForUnconfiguredDefaultServer(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	setCLIUserConfigRoot(t)

	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{MaxTurns: 8}, nil
		},
		resolveMCPConfig: func(workspaceRoot string, excludeProject bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"exa": config.DefaultMCPServers()["exa"],
			}}, nil
		},
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return fakeMCPRuntimeWithSkips{skipped: []mcp.SkippedServer{
				{Name: "exa", Err: errors.New("returned HTTP 401"), UnconfiguredDefault: true},
			}}, nil
		},
		runTUI: func(ctx context.Context, options tui.Options) int {
			return 0
		},
	})

	if exitCode != 0 {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if strings.Contains(stderr.String(), "exa") {
		t.Fatalf("stderr = %q, want no warning for an unconfigured default server", stderr.String())
	}
}

func TestTUIStartupWarnsForUserConfiguredServerSkip(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	setCLIUserConfigRoot(t)

	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{MaxTurns: 8}, nil
		},
		resolveMCPConfig: func(workspaceRoot string, excludeProject bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"custom": {Type: "stdio", Command: "custom-mcp"},
			}}, nil
		},
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return fakeMCPRuntimeWithSkips{skipped: []mcp.SkippedServer{
				{Name: "custom", Err: errors.New("connect failed"), UnconfiguredDefault: false},
			}}, nil
		},
		runTUI: func(ctx context.Context, options tui.Options) int {
			return 0
		},
	})

	if exitCode != 0 {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning: MCP server custom unavailable") {
		t.Fatalf("stderr = %q, want a warning for a user-configured server that failed", stderr.String())
	}
}

func TestRunNoArgsEntersSetupWhenResolveReportsNoActiveProvider(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	setCLIUserConfigRoot(t)
	userConfigPath := filepath.Join(t.TempDir(), "zero", "config.json")
	var launchedOptions tui.Options
	launched := false

	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, fmt.Errorf("%w: active provider %q not found", config.ErrNoActiveProvider, "ghost")
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			t.Fatal("newProvider should not be called without a resolved provider")
			return nil, nil
		},
		userConfigPath: func() (string, error) {
			return userConfigPath, nil
		},
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return noopMCPRuntime{}, nil
		},
		runTUI: func(ctx context.Context, options tui.Options) int {
			launched = true
			launchedOptions = options
			return 0
		},
	})

	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (setup TUI launched), stderr=%q", exitCode, stderr.String())
	}
	if !launched {
		t.Fatal("TUI was not launched; expected fallback into setup instead of a fatal error")
	}
	if !launchedOptions.Setup.Visible || !launchedOptions.Setup.Required {
		t.Fatalf("Setup = %#v, want visible required setup", launchedOptions.Setup)
	}
	if launchedOptions.Provider != nil {
		t.Fatalf("Provider = %#v, want nil for no-provider fallback", launchedOptions.Provider)
	}
}

func TestRunNoArgsFallsBackToUsableProviderWhenNoneMarkedActive(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	setCLIUserConfigRoot(t)
	userConfigPath := filepath.Join(t.TempDir(), "zero", "config.json")
	var launchedOptions tui.Options
	launched := false
	var providerProfile config.ProviderProfile
	fake := &cliFakeProvider{}

	usable := config.ProviderProfile{
		Name:         "work",
		ProviderKind: config.ProviderKindOpenAI,
		BaseURL:      config.OpenAIBaseURL,
		APIKey:       "sk-test",
		Model:        "gpt-test",
	}

	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			// config.json has providers configured but none marked active (e.g. a
			// blank/stale activeProvider field) — Resolve returns the successfully
			// normalized providers list alongside ErrNoActiveProvider.
			return config.ResolvedConfig{Providers: []config.ProviderProfile{usable}},
				fmt.Errorf("%w: active provider %q not found", config.ErrNoActiveProvider, "")
		},
		newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			providerProfile = profile
			return fake, nil
		},
		userConfigPath: func() (string, error) {
			return userConfigPath, nil
		},
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return noopMCPRuntime{}, nil
		},
		runTUI: func(ctx context.Context, options tui.Options) int {
			launched = true
			launchedOptions = options
			return 0
		},
	})

	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0, stderr=%q", exitCode, stderr.String())
	}
	if !launched {
		t.Fatal("TUI was not launched")
	}
	if launchedOptions.Setup.Visible || launchedOptions.Setup.Required {
		t.Fatalf("Setup = %#v, want no setup wizard — a usable saved provider exists", launchedOptions.Setup)
	}
	if providerProfile.Name != "work" {
		t.Fatalf("provider used = %q, want fallback to the usable saved provider %q", providerProfile.Name, "work")
	}
}

func TestRunNoArgsFailsWhenResolveErrorIsNotProviderRelated(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	setCLIUserConfigRoot(t)

	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, fmt.Errorf("invalid config JSON")
		},
		runTUI: func(ctx context.Context, options tui.Options) int {
			t.Fatal("TUI must not launch on a non-provider resolve error")
			return 0
		},
	})

	if exitCode == 0 {
		t.Fatal("exit code = 0, want non-zero for fatal config error")
	}
	if !strings.Contains(stderr.String(), "invalid config JSON") {
		t.Fatalf("stderr = %q, want the underlying config error", stderr.String())
	}
}

func TestRunNoArgsLaunchesTUIWithMCPState(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	userConfigPath := filepath.Join(t.TempDir(), "zero", "config.json")
	mcpConfig := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "stdio", Command: "docs-mcp"},
	}}
	permissionStore, err := mcp.NewPermissionStore(mcp.StoreOptions{FilePath: filepath.Join(t.TempDir(), "mcp-permissions.json")})
	if err != nil {
		t.Fatalf("NewPermissionStore() error = %v", err)
	}
	tokenStore, err := mcp.NewTokenStore(mcp.TokenStoreOptions{FilePath: filepath.Join(t.TempDir(), "mcp-oauth.json")})
	if err != nil {
		t.Fatalf("NewTokenStore() error = %v", err)
	}
	var launchedOptions tui.Options
	var registeredConfig config.MCPConfig
	var registeredStore *mcp.PermissionStore
	runtimeClosed := false

	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		userConfigPath: func() (string, error) {
			return userConfigPath, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{MaxTurns: 8}, nil
		},
		resolveMCPConfig: func(workspaceRoot string, _ bool) (config.MCPConfig, error) {
			if workspaceRoot != cwd {
				t.Fatalf("workspaceRoot = %q, want %q", workspaceRoot, cwd)
			}
			return mcpConfig, nil
		},
		newMCPStore: func() (*mcp.PermissionStore, error) {
			return permissionStore, nil
		},
		newMCPTokenStore: func() (*mcp.TokenStore, error) {
			return tokenStore, nil
		},
		registerMCPTools: func(ctx context.Context, registry *tools.Registry, cfg config.MCPConfig, options mcp.RegisterOptions) (mcpToolRuntime, error) {
			registeredConfig = cfg
			registeredStore = options.PermissionStore
			return closeFunc(func() error {
				runtimeClosed = true
				return nil
			}), nil
		},
		runTUI: func(ctx context.Context, options tui.Options) int {
			launchedOptions = options
			return 0
		},
	})

	if exitCode != 0 {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if _, ok := registeredConfig.Servers["docs"]; !ok {
		t.Fatalf("registered MCP config = %#v, want docs server", registeredConfig.Servers)
	}
	if registeredStore != permissionStore {
		t.Fatalf("registered PermissionStore = %#v, want launched store %#v", registeredStore, permissionStore)
	}
	if _, ok := launchedOptions.MCPConfig.Servers["docs"]; !ok {
		t.Fatalf("launched MCP config = %#v, want docs server", launchedOptions.MCPConfig.Servers)
	}
	if launchedOptions.MCPPermissionStore != permissionStore {
		t.Fatalf("launched MCPPermissionStore = %#v, want %#v", launchedOptions.MCPPermissionStore, permissionStore)
	}
	if launchedOptions.MCPTokenStore != tokenStore {
		t.Fatalf("launched MCPTokenStore = %#v, want %#v", launchedOptions.MCPTokenStore, tokenStore)
	}
	if launchedOptions.MCPCommand == nil {
		t.Fatal("launched MCPCommand runner is nil")
	}
	if !runtimeClosed {
		t.Fatal("MCP runtime was not closed after TUI exits")
	}
}

func TestRunNoArgsPaintsBeforeOptionalDefaultMCPIsReady(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	userConfigPath := filepath.Join(t.TempDir(), "zero", "config.json")
	permissionStore, err := mcp.NewPermissionStore(mcp.StoreOptions{FilePath: filepath.Join(t.TempDir(), "mcp-permissions.json")})
	if err != nil {
		t.Fatalf("NewPermissionStore() error = %v", err)
	}
	tokenStore, err := mcp.NewTokenStore(mcp.TokenStoreOptions{FilePath: filepath.Join(t.TempDir(), "mcp-oauth.json")})
	if err != nil {
		t.Fatalf("NewTokenStore() error = %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	configResult := make(chan error, 1)

	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd:          func() (string, error) { return cwd, nil },
		userConfigPath: func() (string, error) { return userConfigPath, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{MaxTurns: 8}, nil
		},
		resolveMCPConfig: func(string, bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: config.DefaultMCPServers()}, nil
		},
		newMCPStore:      func() (*mcp.PermissionStore, error) { return permissionStore, nil },
		newMCPTokenStore: func() (*mcp.TokenStore, error) { return tokenStore, nil },
		registerMCPTools: func(_ context.Context, registry *tools.Registry, cfg config.MCPConfig, _ mcp.RegisterOptions) (mcpToolRuntime, error) {
			var configErr error
			if _, ok := cfg.Servers["exa"]; !ok || len(cfg.Servers) != 1 {
				configErr = fmt.Errorf("optional MCP config = %#v, want only exa", cfg.Servers)
			}
			configResult <- configErr
			close(started)
			<-release
			registry.Register(cliFakeMCPRegistryTool{})
			return noopMCPRuntime{}, nil
		},
		runTUI: func(ctx context.Context, options tui.Options) int {
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("optional MCP initialization did not start")
			}
			if options.AwaitToolReadiness == nil {
				t.Fatal("TUI did not receive optional tool readiness barrier")
			}
			if _, ok := options.Registry.Get("mcp_docs_lookup"); ok {
				t.Fatal("optional MCP tool was published before registration completed")
			}
			close(release)
			options.AwaitToolReadiness(ctx)
			if _, ok := options.Registry.Get("mcp_docs_lookup"); !ok {
				t.Fatal("optional MCP tool was not visible after readiness completed")
			}
			return 0
		},
	})

	if exitCode != 0 {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if configErr := <-configResult; configErr != nil {
		t.Fatal(configErr)
	}
}

func TestTUIMCPCommandUsesLastGoodConfigOnRefreshError(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	userConfigPath := filepath.Join(t.TempDir(), "zero", "config.json")
	startupConfig := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"docs": {Type: "stdio", Command: "docs-mcp"},
	}}
	refreshedConfig := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"github": {Type: "http", URL: "https://mcp.github.example"},
	}}
	resolveCalls := 0

	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		userConfigPath: func() (string, error) {
			return userConfigPath, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{MaxTurns: 8}, nil
		},
		resolveMCPConfig: func(workspaceRoot string, _ bool) (config.MCPConfig, error) {
			resolveCalls++
			switch resolveCalls {
			case 1:
				return startupConfig, nil
			case 2, 3:
				return refreshedConfig, nil
			default:
				return config.MCPConfig{}, errors.New("config temporarily unavailable")
			}
		},
		newMCPStore: func() (*mcp.PermissionStore, error) {
			return mcp.NewPermissionStore(mcp.StoreOptions{FilePath: filepath.Join(t.TempDir(), "mcp-permissions.json")})
		},
		newMCPTokenStore: func() (*mcp.TokenStore, error) {
			return mcp.NewTokenStore(mcp.TokenStoreOptions{FilePath: filepath.Join(t.TempDir(), "mcp-oauth.json")})
		},
		registerMCPTools: func(ctx context.Context, registry *tools.Registry, cfg config.MCPConfig, options mcp.RegisterOptions) (mcpToolRuntime, error) {
			return closeFunc(func() error { return nil }), nil
		},
		runTUI: func(ctx context.Context, options tui.Options) int {
			first := options.MCPCommand(ctx, []string{"list"})
			if _, ok := first.Config.Servers["github"]; !ok {
				t.Fatalf("first MCP result config = %#v, want refreshed github server", first.Config.Servers)
			}
			second := options.MCPCommand(ctx, []string{"list"})
			if _, ok := second.Config.Servers["github"]; !ok {
				t.Fatalf("second MCP result config = %#v, want refreshed github server", second.Config.Servers)
			}
			third := options.MCPCommand(ctx, []string{"list"})
			if _, ok := third.Config.Servers["github"]; !ok {
				t.Fatalf("third MCP result config = %#v, want last known github server after refresh error", third.Config.Servers)
			}
			return 0
		},
	})

	if exitCode != 0 {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
}

func TestRunNoArgsClosesPartialMCPRuntimeWhenRegistrationFails(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	userConfigPath := filepath.Join(t.TempDir(), "zero", "config.json")
	permissionStore, err := mcp.NewPermissionStore(mcp.StoreOptions{FilePath: filepath.Join(t.TempDir(), "mcp-permissions.json")})
	if err != nil {
		t.Fatalf("NewPermissionStore() error = %v", err)
	}
	tokenStore, err := mcp.NewTokenStore(mcp.TokenStoreOptions{FilePath: filepath.Join(t.TempDir(), "mcp-oauth.json")})
	if err != nil {
		t.Fatalf("NewTokenStore() error = %v", err)
	}
	runtimeClosed := false
	secret := "sk-proj-" + strings.Repeat("a", 23) + "0"

	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		userConfigPath: func() (string, error) {
			return userConfigPath, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{MaxTurns: 8}, nil
		},
		resolveMCPConfig: func(workspaceRoot string, _ bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"docs": {Type: "stdio", Command: "docs-mcp"},
			}}, nil
		},
		newMCPStore: func() (*mcp.PermissionStore, error) {
			return permissionStore, nil
		},
		newMCPTokenStore: func() (*mcp.TokenStore, error) {
			return tokenStore, nil
		},
		registerMCPTools: func(ctx context.Context, registry *tools.Registry, cfg config.MCPConfig, options mcp.RegisterOptions) (mcpToolRuntime, error) {
			return closeFunc(func() error {
				runtimeClosed = true
				return nil
			}), fmt.Errorf("register mcp tools failed: %s", secret)
		},
		runTUI: func(ctx context.Context, options tui.Options) int {
			t.Fatal("TUI should not launch after MCP registration fails")
			return 0
		},
	})

	if exitCode == 0 {
		t.Fatalf("exitCode = %d, want failure", exitCode)
	}
	if !strings.Contains(stderr.String(), "register mcp tools failed") {
		t.Fatalf("stderr missing registration error: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), secret) {
		t.Fatalf("stderr leaked MCP registration secret: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), redaction.RedactedSecret) {
		t.Fatalf("stderr missing redaction marker: %s", stderr.String())
	}
	if !runtimeClosed {
		t.Fatal("partial MCP runtime was not closed after registration error")
	}
}

func TestRunNoArgsSoftFailsMCPTokenStoreInit(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	userConfigPath := filepath.Join(t.TempDir(), "zero", "config.json")
	permissionStore, err := mcp.NewPermissionStore(mcp.StoreOptions{FilePath: filepath.Join(t.TempDir(), "mcp-permissions.json")})
	if err != nil {
		t.Fatalf("NewPermissionStore() error = %v", err)
	}
	var launchedOptions tui.Options

	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		userConfigPath: func() (string, error) {
			return userConfigPath, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{MaxTurns: 8}, nil
		},
		resolveMCPConfig: func(workspaceRoot string, _ bool) (config.MCPConfig, error) {
			return config.MCPConfig{}, nil
		},
		newMCPStore: func() (*mcp.PermissionStore, error) {
			return permissionStore, nil
		},
		newMCPTokenStore: func() (*mcp.TokenStore, error) {
			return nil, errors.New("token store unreadable")
		},
		runTUI: func(ctx context.Context, options tui.Options) int {
			launchedOptions = options
			return 0
		},
	})

	if exitCode != 0 {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if launchedOptions.MCPTokenStore != nil {
		t.Fatalf("MCPTokenStore = %#v, want nil after soft failure", launchedOptions.MCPTokenStore)
	}
	if !strings.Contains(stderr.String(), "warning: failed to initialize MCP OAuth tokens") {
		t.Fatalf("stderr missing soft-failure warning: %s", stderr.String())
	}
}

func TestRunNoArgsLaunchesTUIWithResolvedProviderMetadata(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	userConfigPath := filepath.Join(t.TempDir(), "zero", "config.json")
	fake := &cliFakeProvider{}
	var launchedOptions tui.Options
	var providerProfile config.ProviderProfile

	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			if workspaceRoot != cwd {
				t.Fatalf("workspaceRoot = %q, want %q", workspaceRoot, cwd)
			}
			return config.ResolvedConfig{
				ActiveProvider: "work",
				Provider: config.ProviderProfile{
					Name:         "work",
					ProviderKind: config.ProviderKindOpenAI,
					BaseURL:      config.OpenAIBaseURL,
					APIKey:       "sk-test",
					Model:        "gpt-test",
				},
				Preferences: config.PreferencesConfig{FavoriteModels: []string{"qwen3-coder:480b"}},
				MaxTurns:    5,
			}, nil
		},
		newProvider: func(profile config.ProviderProfile) (zeroruntime.Provider, error) {
			providerProfile = profile
			return fake, nil
		},
		userConfigPath: func() (string, error) {
			return userConfigPath, nil
		},
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return noopMCPRuntime{}, nil
		},
		runTUI: func(ctx context.Context, options tui.Options) int {
			launchedOptions = options
			return 0
		},
	})

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	if providerProfile.Name != "work" || providerProfile.Model != "gpt-test" {
		t.Fatalf("providerProfile = %#v, want resolved provider", providerProfile)
	}
	if launchedOptions.Provider != fake {
		t.Fatalf("Provider = %#v, want fake provider", launchedOptions.Provider)
	}
	if launchedOptions.UserConfigPath != userConfigPath {
		t.Fatalf("UserConfigPath = %q, want %q", launchedOptions.UserConfigPath, userConfigPath)
	}
	if launchedOptions.ProviderName != "work" {
		t.Fatalf("ProviderName = %q, want work", launchedOptions.ProviderName)
	}
	if launchedOptions.ModelName != "gpt-test" {
		t.Fatalf("ModelName = %q, want gpt-test", launchedOptions.ModelName)
	}
	if len(launchedOptions.FavoriteModels) != 1 || launchedOptions.FavoriteModels[0] != "qwen3-coder:480b" {
		t.Fatalf("FavoriteModels = %#v, want qwen3-coder:480b", launchedOptions.FavoriteModels)
	}
	if launchedOptions.Setup.Visible {
		t.Fatalf("Setup.Visible = true, want false for credentialed provider")
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
	assertCoreRegistry(t, launchedOptions.Registry)
	assertAgentOptions(t, launchedOptions, 5, agent.PermissionModeAsk)
}

func TestRunNoArgsLaunchesTUIInAskPermissionMode(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	var launchedOptions tui.Options

	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{MaxTurns: 3}, nil
		},
		runTUI: func(_ context.Context, options tui.Options) int {
			launchedOptions = options
			return 0
		},
	})

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	// Auto only advertises PermissionAllow tools, so write_file/edit_file/bash/
	// apply_patch (PermissionPrompt) would never be offered to the model. Ask
	// advertises them and gates each through the permission flow.
	if launchedOptions.PermissionMode != agent.PermissionModeAsk {
		t.Fatalf("PermissionMode = %q, want %q", launchedOptions.PermissionMode, agent.PermissionModeAsk)
	}
	if launchedOptions.AgentOptions.PermissionMode != agent.PermissionModeAsk {
		t.Fatalf("AgentOptions.PermissionMode = %q, want %q", launchedOptions.AgentOptions.PermissionMode, agent.PermissionModeAsk)
	}
}

// TestRunInteractiveSurfacesMCPTrustNotice executes the interactive (TUI) path's MCP
// trust notice (app.go), the third notice site alongside exec and spec-draft. An
// untrusted repo whose only project config is MCP must print the notice before runTUI;
// trusting it silences it. runTUI is stubbed so nothing renders; resolveMCPConfig
// returns no servers so nothing spawns -- the notice depends only on the trust verdict
// and the real ./.zero/config.json.
func TestRunInteractiveSurfacesMCPTrustNotice(t *testing.T) {
	setTrustConfigRoot(t)
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".zero"), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"mcp":{"servers":{"proj":{"type":"stdio","command":"proj-cmd"}}}}`
	if err := os.WriteFile(filepath.Join(repo, ".zero", "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func() string {
		var stdout, stderr bytes.Buffer
		code := runWithDeps([]string{}, &stdout, &stderr, appDeps{
			getwd: func() (string, error) { return repo, nil },
			resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
				return config.ResolvedConfig{MaxTurns: 3}, nil
			},
			resolveMCPConfig: func(string, bool) (config.MCPConfig, error) { return config.MCPConfig{}, nil },
			runTUI:           func(context.Context, tui.Options) int { return 0 },
		})
		if code != 0 {
			t.Fatalf("interactive launch exit = %d, stderr=%q", code, stderr.String())
		}
		return stderr.String()
	}

	untrusted := run()
	if !strings.Contains(untrusted, "MCP servers") || !strings.Contains(untrusted, "zero trust") {
		t.Fatalf("untrusted interactive launch must surface the MCP trust notice, stderr=%q", untrusted)
	}

	if err := workspacetrust.Trust(repo); err != nil {
		t.Fatal(err)
	}
	if trusted := run(); strings.Contains(trusted, "ignoring project") {
		t.Fatalf("trusted interactive launch must not emit a trust notice, stderr=%q", trusted)
	}
}

func TestRunSkipPermissionsUnsafeLaunchesTUIInUnsafeMode(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	launched := false
	var launchedOptions tui.Options

	// "zero --skip-permissions-unsafe" must launch the interactive TUI in unsafe
	// mode (not fall through to "unknown command"). This is the only way to reach
	// unsafe mode in the shell, which the "!" escape is gated behind.
	exitCode := runWithDeps([]string{"--skip-permissions-unsafe"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{MaxTurns: 3}, nil
		},
		runTUI: func(_ context.Context, options tui.Options) int {
			launched = true
			launchedOptions = options
			return 0
		},
	})

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d: %s", exitCode, stderr.String())
	}
	if !launched {
		t.Fatal("expected the interactive TUI to launch, but runTUI was never called")
	}
	if launchedOptions.PermissionMode != agent.PermissionModeUnsafe {
		t.Fatalf("PermissionMode = %q, want %q", launchedOptions.PermissionMode, agent.PermissionModeUnsafe)
	}
	if launchedOptions.AgentOptions.PermissionMode != agent.PermissionModeUnsafe {
		t.Fatalf("AgentOptions.PermissionMode = %q, want %q", launchedOptions.AgentOptions.PermissionMode, agent.PermissionModeUnsafe)
	}
}

func TestRunSkipPermissionsUnsafeRejectsTrailingPrompt(t *testing.T) {
	// AUDIT-L3: a trailing prompt must be rejected loudly, not silently dropped
	// while the interactive TUI launches.
	var stdout, stderr bytes.Buffer
	launched := false
	exitCode := runWithDeps([]string{"--skip-permissions-unsafe", "fix the bug"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return t.TempDir(), nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{MaxTurns: 3}, nil
		},
		runTUI: func(context.Context, tui.Options) int { launched = true; return 0 },
	})
	if exitCode == 0 {
		t.Fatal("a trailing prompt after --skip-permissions-unsafe must be rejected, not silently dropped")
	}
	if launched {
		t.Fatal("the TUI must not launch when trailing args are rejected")
	}
	if !strings.Contains(stderr.String(), "takes no prompt") {
		t.Fatalf("error should explain the no-prompt contract, got %q", stderr.String())
	}
}

func TestRunAddDirWiresExtraWriteRootIntoTUISandboxScope(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	extra := t.TempDir()
	launched := false
	var launchedOptions tui.Options

	exitCode := runWithDeps([]string{"--add-dir", extra}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{MaxTurns: 3}, nil
		},
		runTUI: func(_ context.Context, options tui.Options) int {
			launched = true
			launchedOptions = options
			return 0
		},
	})

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d: %s", exitCode, stderr.String())
	}
	if !launched {
		t.Fatal("expected the interactive TUI to launch, but runTUI was never called")
	}
	if launchedOptions.AgentOptions.Sandbox == nil {
		t.Fatal("AgentOptions.Sandbox = nil, want sandbox engine")
	}
	roots := launchedOptions.AgentOptions.Sandbox.Scope().Roots()
	// The scope stores symlink-resolved roots (e.g. macOS /var -> /private/var),
	// so compare against the resolved extra dir.
	resolvedExtra, err := filepath.EvalSymlinks(extra)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", extra, err)
	}
	found := false
	for _, root := range roots {
		if root == resolvedExtra {
			found = true
		}
	}
	if !found {
		t.Fatalf("scope roots = %v, want to contain %q", roots, resolvedExtra)
	}
}

func TestRunSkipPermissionsUnsafeMergesAddDirGrants(t *testing.T) {
	cwd := t.TempDir()
	extra := t.TempDir()
	resolvedExtra, err := filepath.EvalSymlinks(extra)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", extra, err)
	}

	// --add-dir is accepted on either side of --skip-permissions-unsafe; both
	// orders must reach the unsafe TUI with the extra root in scope.
	for _, args := range [][]string{
		{"--skip-permissions-unsafe", "--add-dir", extra},
		{"--add-dir", extra, "--skip-permissions-unsafe"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			var launchedOptions tui.Options

			exitCode := runWithDeps(args, &stdout, &stderr, appDeps{
				getwd: func() (string, error) {
					return cwd, nil
				},
				resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
					return config.ResolvedConfig{MaxTurns: 3}, nil
				},
				runTUI: func(_ context.Context, options tui.Options) int {
					launchedOptions = options
					return 0
				},
			})

			if exitCode != 0 {
				t.Fatalf("expected exit code 0, got %d: %s", exitCode, stderr.String())
			}
			if launchedOptions.PermissionMode != agent.PermissionModeUnsafe {
				t.Fatalf("PermissionMode = %q, want %q", launchedOptions.PermissionMode, agent.PermissionModeUnsafe)
			}
			if launchedOptions.AgentOptions.Sandbox == nil {
				t.Fatal("AgentOptions.Sandbox = nil, want sandbox engine")
			}
			roots := launchedOptions.AgentOptions.Sandbox.Scope().Roots()
			found := false
			for _, root := range roots {
				if root == resolvedExtra {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("scope roots = %v, want extra root %q", roots, resolvedExtra)
			}
		})
	}
}

func TestRunSkipPermissionsUnsafeRejectsAddDirHiddenBehindStrayArg(t *testing.T) {
	// splitLeadingAddDirFlags stops at the first non-flag token, so a grant
	// placed after a stray arg would otherwise be discarded with the ignored
	// trailing args. An explicit grant must never be silently dropped.
	for _, args := range [][]string{
		{"--skip-permissions-unsafe", "stray", "--add-dir", "/tmp"},
		{"--skip-permissions-unsafe", "stray", "--add-dir=/tmp"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := runWithDeps(args, &stdout, &stderr, appDeps{
				runTUI: func(context.Context, tui.Options) int {
					t.Fatal("TUI launcher should not be called when a misplaced --add-dir is rejected")
					return 0
				},
			})

			if exitCode != 1 {
				t.Fatalf("expected exit code 1, got %d", exitCode)
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected empty stdout, got %q", stdout.String())
			}
			if got := stderr.String(); !strings.Contains(got, "--add-dir must come before any other arguments") {
				t.Fatalf("expected misplaced --add-dir rejection on stderr, got %q", got)
			}
		})
	}
}

func TestRunAddDirRejectedForNonAgentCommands(t *testing.T) {
	// --add-dir grants a write root that only the TUI and exec consume; every
	// other subcommand — including help/version, which run no agent — must
	// fail loud rather than silently drop the grant.
	for _, args := range [][]string{
		{"--add-dir", "/tmp", "config"},
		{"--add-dir=/tmp", "doctor"},
		{"--add-dir", "/tmp", "models"},
		{"--add-dir", "/tmp", "sandbox"},
		{"--add-dir", "/tmp", "help"},
		{"--add-dir", "/tmp", "--help"},
		{"--add-dir", "/tmp", "-h"},
		{"--add-dir", "/tmp", "version"},
		{"--add-dir", "/tmp", "--version"},
		{"--add-dir", "/tmp", "-v"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := runWithDeps(args, &stdout, &stderr, appDeps{
				runTUI: func(context.Context, tui.Options) int {
					t.Fatal("TUI launcher should not be called when --add-dir is rejected")
					return 0
				},
			})

			if exitCode != 1 {
				t.Fatalf("expected exit code 1, got %d", exitCode)
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected empty stdout, got %q", stdout.String())
			}
			if got := stderr.String(); !strings.Contains(got, "--add-dir is only supported for the interactive TUI and exec") {
				t.Fatalf("expected --add-dir rejection on stderr, got %q", got)
			}
		})
	}
}

func TestRunNoArgsReportsConfigErrorsWithoutLaunchingTUI(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	launchCalled := false

	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return t.TempDir(), nil
		},
		resolveConfig: func(workspaceRoot string, overrides config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{}, errors.New("bad config")
		},
		runTUI: func(ctx context.Context, options tui.Options) int {
			launchCalled = true
			return 0
		},
	})

	if exitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", exitCode)
	}
	if launchCalled {
		t.Fatal("TUI launcher should not be called when config fails")
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "bad config") {
		t.Fatalf("expected config error on stderr, got %q", got)
	}
}

func TestRunCommandsDoNotLaunchTUI(t *testing.T) {
	for _, args := range [][]string{
		{"--help"},
		{"-h"},
		{"help"},
		{"--version"},
		{"version"},
		{"wat"},
		{"exec"},
		{"setup", "--help"},
		{"config"},
		{"models"},
		{"providers"},
		{"doctor"},
		{"search"},
		{"find"},
		{"sessions"},
		{"session"},
		{"plugins"},
		{"plugin"},
		{"skills"},
		{"skill"},
		{"hooks"},
		{"mcp"},
		{"sandbox"},
		{"update"},
		{"worktrees"},
		{"worktree"},
		{"verify"},
		{"serve"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			launchCalled := false

			_ = runWithDeps(args, &stdout, &stderr, appDeps{
				runTUI: func(ctx context.Context, options tui.Options) int {
					launchCalled = true
					return 0
				},
			})

			if launchCalled {
				t.Fatalf("TUI launcher should not be called for args %#v", args)
			}
		})
	}
}

func TestRunSetupNoArgsForcesSetupTUI(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "config.json")
	var launchedOptions tui.Options

	exitCode := runWithDeps([]string{"setup"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return cwd, nil
		},
		userConfigPath: func() (string, error) {
			return configPath, nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{
				ActiveProvider: "work",
				Provider: config.ProviderProfile{
					Name:         "work",
					ProviderKind: config.ProviderKindOpenAI,
					BaseURL:      config.OpenAIBaseURL,
					APIKey:       "sk-test",
					Model:        "gpt-test",
				},
				MaxTurns: 3,
			}, nil
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			return &cliFakeProvider{}, nil
		},
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return noopMCPRuntime{}, nil
		},
		runTUI: func(ctx context.Context, options tui.Options) int {
			launchedOptions = options
			return 0
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if !launchedOptions.Setup.Visible {
		t.Fatalf("Setup.Visible = false, want forced setup")
	}
	if launchedOptions.Setup.Required {
		t.Fatalf("Setup.Required = true, want false for credentialed provider")
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("expected no output, stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestRunSetupProviderWritesActiveConfig(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	configPath := filepath.Join(t.TempDir(), "config.json")

	exitCode := runWithDeps([]string{"setup", "ollama"}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) {
			return configPath, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg config.FileConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode config: %v\n%s", err, string(data))
	}
	if cfg.ActiveProvider != "ollama" {
		t.Fatalf("ActiveProvider = %q, want ollama", cfg.ActiveProvider)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].CatalogID != "ollama" || cfg.Providers[0].Model == "" {
		t.Fatalf("Providers = %#v, want ollama provider with model", cfg.Providers)
	}
	if !strings.Contains(stdout.String(), "Zero setup complete") || !strings.Contains(stdout.String(), "next: zero") {
		t.Fatalf("unexpected setup output: %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunUpdateCheckTextAndJSON(t *testing.T) {
	result := update.Result{
		CurrentVersion: "dev",
		LatestVersion:  "0.2.0",
		ReleaseURL:     "https://github.com/Gitlawb/zero/releases/tag/v0.2.0",
		TagName:        "v0.2.0",
		ReleaseAsset: update.AssetCheck{
			Platform:      "linux",
			Arch:          "x64",
			ArchiveName:   "zero-v0.2.0-linux-x64.tar.gz",
			ArchiveURL:    "https://example.test/zero-v0.2.0-linux-x64.tar.gz",
			ChecksumName:  "zero-v0.2.0-linux-x64.tar.gz.sha256",
			ChecksumURL:   "https://example.test/zero-v0.2.0-linux-x64.tar.gz.sha256",
			ArchiveFound:  true,
			ChecksumFound: true,
			Verified:      true,
		},
		UpdateAvailable: true,
	}
	deps := appDeps{
		checkUpdate: func(ctx context.Context, options update.Options) (update.Result, error) {
			result.CurrentVersion = options.CurrentVersion
			return result, nil
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"update", "--check"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Update available: dev -> 0.2.0") {
		t.Fatalf("unexpected update text: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Release asset: zero-v0.2.0-linux-x64.tar.gz") || !strings.Contains(stdout.String(), "Checksum asset: zero-v0.2.0-linux-x64.tar.gz.sha256") {
		t.Fatalf("unexpected update asset text: %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"update", "--check", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	var payload struct {
		CurrentVersion string `json:"currentVersion"`
		LatestVersion  string `json:"latestVersion"`
		ReleaseURL     string `json:"releaseUrl"`
		TagName        string `json:"tagName"`
		ReleaseAsset   struct {
			Platform      string `json:"platform"`
			Arch          string `json:"arch"`
			ArchiveName   string `json:"archiveName"`
			ChecksumName  string `json:"checksumName"`
			ArchiveFound  bool   `json:"archiveFound"`
			ChecksumFound bool   `json:"checksumFound"`
			Verified      bool   `json:"verified"`
		} `json:"releaseAsset"`
		UpdateAvailable bool `json:"updateAvailable"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("update JSON did not decode: %v\n%s", err, stdout.String())
	}
	if payload.CurrentVersion != "dev" ||
		payload.LatestVersion != result.LatestVersion ||
		payload.ReleaseURL != result.ReleaseURL ||
		payload.TagName != result.TagName ||
		payload.ReleaseAsset.ArchiveName != result.ReleaseAsset.ArchiveName ||
		payload.ReleaseAsset.ChecksumName != result.ReleaseAsset.ChecksumName ||
		!payload.ReleaseAsset.Verified ||
		payload.UpdateAvailable != result.UpdateAvailable {
		t.Fatalf("unexpected update JSON: %#v", payload)
	}
}

func TestRunUpdatePassesCheckOptions(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	var got update.Options
	exitCode := runWithDeps([]string{"update", "--check", "--repo=Gitlawb/fork", "--endpoint", "https://example.test/releases/latest", "--timeout", "750ms", "--target", "windows-x64", "--json"}, &stdout, &stderr, appDeps{
		checkUpdate: func(_ context.Context, options update.Options) (update.Result, error) {
			got = options
			return update.Result{
				CurrentVersion:  options.CurrentVersion,
				LatestVersion:   "0.2.0",
				ReleaseURL:      "https://example.test/releases/tag/v0.2.0",
				TagName:         "v0.2.0",
				UpdateAvailable: true,
			}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if got.CurrentVersion != "dev" || got.Repository != "Gitlawb/fork" || got.Endpoint != "https://example.test/releases/latest" || got.Timeout != 750*time.Millisecond || got.GOOS != "windows" || got.GOARCH != "amd64" {
		t.Fatalf("unexpected update options: %#v", got)
	}
	if stdout.Len() == 0 {
		t.Fatalf("expected JSON output")
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunUpdateRejectsInvalidTarget(t *testing.T) {
	tests := []struct {
		args      []string
		errorText string
	}{
		{args: []string{"update", "--check", "--target", "solaris-sparc"}, errorText: "unsupported update target"},
		{args: []string{"update", "--check", "--target="}, errorText: "--target requires a non-empty value"},
	}

	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := runWithDeps(tt.args, &stdout, &stderr, appDeps{
				checkUpdate: func(context.Context, update.Options) (update.Result, error) {
					t.Fatal("checkUpdate should not run for invalid target")
					return update.Result{}, nil
				},
			})

			if exitCode != exitUsage {
				t.Fatalf("expected usage exit code, got %d", exitCode)
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected empty stdout, got %q", stdout.String())
			}
			if got := stderr.String(); !strings.Contains(got, tt.errorText) {
				t.Fatalf("expected target usage error %q, got %q", tt.errorText, got)
			}
		})
	}
}

func TestRunUpdateRequiresCheckFlag(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"update"}, &stdout, &stderr, appDeps{})

	if exitCode == exitSuccess {
		t.Fatalf("expected non-success exit code, got %d", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "--check") {
		t.Fatalf("expected --check usage error, got %q", got)
	}
}

func TestRunUpdateRejectsInvalidTimeout(t *testing.T) {
	for _, args := range [][]string{
		{"update", "--check", "--timeout", "fast"},
		{"update", "--check", "--timeout", "0s"},
		{"update", "--check", "--timeout=-1s"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := runWithDeps(args, &stdout, &stderr, appDeps{
				checkUpdate: func(context.Context, update.Options) (update.Result, error) {
					t.Fatal("checkUpdate should not run for invalid timeout")
					return update.Result{}, nil
				},
			})

			if exitCode != exitUsage {
				t.Fatalf("expected usage exit code, got %d", exitCode)
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected empty stdout, got %q", stdout.String())
			}
			if got := stderr.String(); !strings.Contains(got, "invalid update timeout") {
				t.Fatalf("expected timeout usage error, got %q", got)
			}
		})
	}
}

func TestRunUpdateReportsCheckError(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"update", "--check"}, &stdout, &stderr, appDeps{
		checkUpdate: func(context.Context, update.Options) (update.Result, error) {
			return update.Result{}, errors.New("network failure")
		},
	})

	if exitCode == exitSuccess {
		t.Fatalf("expected non-success exit code, got %d", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "network failure") {
		t.Fatalf("expected update error, got %q", got)
	}
}

func TestRunUpdateHelpDocumentsCheckFlag(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"update", "--help"}, &stdout, &stderr, appDeps{})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	for _, want := range []string{"--check", "--repo", "--endpoint", "--timeout", "--target"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("expected update help to document %s, got %q", want, stdout.String())
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestTopLevelHelpDocumentsRequiredUpdateMode(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"--help"}, &stdout, &stderr, appDeps{})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "update     Check or apply Zero CLI updates (requires --check or --apply)") {
		t.Fatalf("expected top-level help to document required update mode, got %q", got)
	}
}

func TestRunUpdateReportsUpToDate(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"update", "--check"}, &stdout, &stderr, appDeps{
		checkUpdate: func(context.Context, update.Options) (update.Result, error) {
			return update.Result{
				CurrentVersion:  "dev",
				LatestVersion:   "dev",
				ReleaseURL:      "https://github.com/Gitlawb/zero/releases/tag/dev",
				TagName:         "dev",
				UpdateAvailable: false,
			}, nil
		},
	})

	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "up to date") {
		t.Fatalf("expected up-to-date output, got %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunUpdateApplyTextAndJSON(t *testing.T) {
	applyResult := update.ApplyResult{
		Result: update.Result{
			CurrentVersion:  "dev",
			LatestVersion:   "0.2.0",
			UpdateAvailable: true,
		},
		Applied:       true,
		InstallMethod: update.InstallMethodStandalone,
		BinaryPath:    "/usr/local/bin/zero",
		Message:       "updated to 0.2.0",
	}
	var gotOptions update.Options
	deps := appDeps{
		applyUpdate: func(_ context.Context, options update.Options) (update.ApplyResult, error) {
			gotOptions = options
			return applyResult, nil
		},
		checkUpdate: func(context.Context, update.Options) (update.Result, error) {
			t.Fatal("checkUpdate should not run for --apply")
			return update.Result{}, nil
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"update", "--apply"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "updated to 0.2.0") {
		t.Fatalf("unexpected apply text: %q", stdout.String())
	}
	if gotOptions.CurrentVersion != "dev" {
		t.Fatalf("unexpected options passed to applyUpdate: %#v", gotOptions)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = runWithDeps([]string{"update", "--apply", "--json"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	var payload struct {
		Applied       bool   `json:"applied"`
		InstallMethod string `json:"installMethod"`
		BinaryPath    string `json:"binaryPath"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("apply JSON did not decode: %v\n%s", err, stdout.String())
	}
	if !payload.Applied || payload.InstallMethod != string(update.InstallMethodStandalone) || payload.BinaryPath != applyResult.BinaryPath {
		t.Fatalf("unexpected apply JSON: %#v", payload)
	}
}

func TestRunUpgradeDefaultsToApply(t *testing.T) {
	var applyCalled bool
	deps := appDeps{
		applyUpdate: func(context.Context, update.Options) (update.ApplyResult, error) {
			applyCalled = true
			return update.ApplyResult{Message: "already up to date"}, nil
		},
		checkUpdate: func(context.Context, update.Options) (update.Result, error) {
			t.Fatal("checkUpdate should not run for `zero upgrade`")
			return update.Result{}, nil
		},
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runWithDeps([]string{"upgrade"}, &stdout, &stderr, deps)
	if exitCode != exitSuccess {
		t.Fatalf("expected exit code %d, got %d: %s", exitSuccess, exitCode, stderr.String())
	}
	if !applyCalled {
		t.Fatal("expected `zero upgrade` to call applyUpdate")
	}
}

func TestRunUpdateRejectsCheckAndApplyTogether(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"update", "--check", "--apply"}, &stdout, &stderr, appDeps{
		checkUpdate: func(context.Context, update.Options) (update.Result, error) {
			t.Fatal("checkUpdate should not run")
			return update.Result{}, nil
		},
		applyUpdate: func(context.Context, update.Options) (update.ApplyResult, error) {
			t.Fatal("applyUpdate should not run")
			return update.ApplyResult{}, nil
		},
	})

	if exitCode != exitUsage {
		t.Fatalf("expected usage exit code, got %d", exitCode)
	}
	if got := stderr.String(); !strings.Contains(got, "only one of --check or --apply") {
		t.Fatalf("expected --check/--apply usage error, got %q", got)
	}
}

func TestRunUpdateRejectsTargetWithApply(t *testing.T) {
	for _, args := range [][]string{
		{"update", "--apply", "--target", "linux-arm64"},
		{"upgrade", "--target", "linux-arm64"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := runWithDeps(args, &stdout, &stderr, appDeps{
				applyUpdate: func(context.Context, update.Options) (update.ApplyResult, error) {
					t.Fatal("applyUpdate should not run when --target is combined with --apply")
					return update.ApplyResult{}, nil
				},
			})

			if exitCode != exitUsage {
				t.Fatalf("expected usage exit code, got %d", exitCode)
			}
			if got := stderr.String(); !strings.Contains(got, "--target cannot be combined with --apply") {
				t.Fatalf("expected --target/--apply usage error, got %q", got)
			}
		})
	}
}

func TestRunUpdateReportsApplyError(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := runWithDeps([]string{"update", "--apply"}, &stdout, &stderr, appDeps{
		applyUpdate: func(context.Context, update.Options) (update.ApplyResult, error) {
			return update.ApplyResult{}, errors.New("download failed")
		},
	})

	if exitCode == exitSuccess {
		t.Fatalf("expected non-success exit code, got %d", exitCode)
	}
	if got := stderr.String(); !strings.Contains(got, "download failed") {
		t.Fatalf("expected apply error, got %q", got)
	}
}

func assertHelpOutput(t *testing.T, args []string) {
	t.Helper()

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := Run(args, &stdout, &stderr)

	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}

	output := stdout.String()
	for _, want := range []string{
		"ZERO terminal coding agent",
		"Usage:",
		"zero [command]",
		"exec",
		"config",
		"models",
		"providers",
		"doctor",
		"context",
		"search",
		"plugins",
		"hooks",
		"mcp",
		"sandbox",
		"update",
		"upgrade",
		"worktrees",
		"verify",
		"serve",
		"usage",
		"--version",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected help output to contain %q, got %q", want, output)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got %q", stderr.String())
	}
}

func TestRunExecRequiresPrompt(t *testing.T) {
	for _, args := range [][]string{
		{"exec"},
		{"exec", ""},
		{"exec", "   "},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout bytes.Buffer
			var stderr bytes.Buffer

			exitCode := Run(args, &stdout, &stderr)

			if exitCode != 2 {
				t.Fatalf("expected exit code 2, got %d", exitCode)
			}
			if stdout.Len() != 0 {
				t.Fatalf("expected empty stdout, got %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), "Prompt required") {
				t.Fatalf("expected prompt error, got %q", stderr.String())
			}
		})
	}
}

func TestRunReturnsFailureWhenStdoutWriteFails(t *testing.T) {
	exitCode := Run([]string{"--version"}, failingWriter{}, io.Discard)

	if exitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", exitCode)
	}
}

func TestRunReturnsFailureWhenStderrWriteFails(t *testing.T) {
	exitCode := Run([]string{"wat"}, io.Discard, failingWriter{})

	if exitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", exitCode)
	}
}

func TestRunRejectsUnknownCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := Run([]string{"wat"}, &stdout, &stderr)

	if exitCode != 2 {
		t.Fatalf("expected exit code 2, got %d", exitCode)
	}
	if stdout.Len() != 0 {
		t.Fatalf("expected empty stdout, got %q", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, `unknown command "wat"`) {
		t.Fatalf("expected unknown command error, got %q", got)
	}
}

type cliFakeProvider struct{}

func (cliFakeProvider) StreamCompletion(context.Context, zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	ch := make(chan zeroruntime.StreamEvent)
	close(ch)
	return ch, nil
}

func assertCoreRegistry(t *testing.T, registry *tools.Registry) {
	t.Helper()

	if registry == nil {
		t.Fatal("Registry = nil, want core tool registry")
	}

	for _, name := range []string{
		"Task",
		"read_file",
		"list_directory",
		"glob",
		"grep",
		"write_file",
		"edit_file",
		"apply_patch",
		"update_plan",
		"bash",
		"web_fetch",
	} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("expected registry to include core tool %q", name)
		}
	}
}

func assertAgentOptions(t *testing.T, options tui.Options, maxTurns int, permissionMode agent.PermissionMode) {
	t.Helper()

	if options.AgentOptions.MaxTurns != maxTurns {
		t.Fatalf("AgentOptions.MaxTurns = %d, want %d", options.AgentOptions.MaxTurns, maxTurns)
	}
	if options.AgentOptions.PermissionMode != permissionMode {
		t.Fatalf("AgentOptions.PermissionMode = %q, want %q", options.AgentOptions.PermissionMode, permissionMode)
	}
	if options.AgentOptions.Autonomy != "low" {
		t.Fatalf("AgentOptions.Autonomy = %q, want %q", options.AgentOptions.Autonomy, "low")
	}
	if options.AgentOptions.Sandbox == nil {
		t.Fatal("AgentOptions.Sandbox = nil, want sandbox engine")
	}
	if options.SandboxStore == nil {
		t.Fatal("SandboxStore = nil, want sandbox grant store")
	}
	if options.PermissionMode != permissionMode {
		t.Fatalf("PermissionMode = %q, want %q", options.PermissionMode, permissionMode)
	}
}

func TestRunThemeFlagPopulatesTUIOptions(t *testing.T) {
	// The --theme flag must reach tui.Options.Theme (resolveThemeMode prefers it
	// over ZERO_THEME). Previously Options.Theme was read but never set by the CLI.
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--theme", "light"}, "light"},
		{[]string{"--theme=dark"}, "dark"},
		{[]string{"--theme", "auto"}, "auto"},
	} {
		var stdout, stderr bytes.Buffer
		var got tui.Options
		launched := false
		exit := runWithDeps(tc.args, &stdout, &stderr, appDeps{
			getwd: func() (string, error) { return t.TempDir(), nil },
			resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
				return config.ResolvedConfig{MaxTurns: 3}, nil
			},
			runTUI: func(_ context.Context, o tui.Options) int { launched = true; got = o; return 0 },
		})
		if exit != 0 || !launched {
			t.Fatalf("%v: exit=%d launched=%v stderr=%s", tc.args, exit, launched, stderr.String())
		}
		if got.Theme != tc.want {
			t.Fatalf("%v: Options.Theme = %q, want %q", tc.args, got.Theme, tc.want)
		}
	}
}

func TestRunThemeFlagRejectsBadValue(t *testing.T) {
	var stdout, stderr bytes.Buffer
	launched := false
	exit := runWithDeps([]string{"--theme", "solarized"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return t.TempDir(), nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{MaxTurns: 3}, nil
		},
		runTUI: func(context.Context, tui.Options) int { launched = true; return 0 },
	})
	if exit == 0 {
		t.Fatal("an invalid --theme value must be rejected, not silently ignored")
	}
	if launched {
		t.Fatal("the TUI must not launch on a bad --theme value")
	}
	if !strings.Contains(stderr.String(), "--theme must be auto or a registered theme name") {
		t.Fatalf("expected a clear --theme error, got %q", stderr.String())
	}
}

// A custom endpoint saved without a model previously bricked bare `zero` and
// `zero setup` — the exact commands that could have fixed it (the resolve
// error escaped before the wizard could open). The interactive TUI now treats
// the requires-model failure as "needs onboarding", same as a missing active
// provider. The error comes from a REAL Resolve over a real config file, so
// this exercises the production sentinel wrapping, not a hand-built error.
func TestRunNoArgsEntersSetupWhenActiveProviderMissesModel(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cwd := t.TempDir()
	setCLIUserConfigRoot(t)
	brokenConfig := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(brokenConfig, []byte(`{
		"activeProvider": "gw",
		"providers": [{"name": "gw", "provider_kind": "openai-compatible", "baseURL": "https://gw.example/v1", "apiKey": "sk-x"}]
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	launched := false
	var launchedOptions tui.Options

	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.Resolve(config.ResolveOptions{UserConfigPath: brokenConfig, Env: map[string]string{}})
		},
		newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
			t.Fatal("newProvider must not be called without a resolved provider")
			return nil, nil
		},
		userConfigPath: func() (string, error) { return filepath.Join(t.TempDir(), "zero", "config.json"), nil },
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return noopMCPRuntime{}, nil
		},
		runTUI: func(_ context.Context, options tui.Options) int {
			launched = true
			launchedOptions = options
			return 0
		},
	})

	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (setup TUI launched), stderr=%q", exitCode, stderr.String())
	}
	if !launched {
		t.Fatal("TUI was not launched; a fixable requires-model config must fall into setup, not exit fatally")
	}
	if !launchedOptions.Setup.Visible || !launchedOptions.Setup.Required {
		t.Fatalf("Setup = %#v, want visible required setup", launchedOptions.Setup)
	}
}

// `zero login`/`zero logout` don't exist (it's `zero auth login`); first-run
// users try them (reported in the wild), so the unknown-command error points at
// the real command.
func TestRunUnknownLoginSuggestsAuthLogin(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exitCode := runWithDeps([]string{"login"}, &stdout, &stderr, appDeps{})
	if exitCode != 2 {
		t.Fatalf("exit = %d, want 2 (unknown command)", exitCode)
	}
	if !strings.Contains(stderr.String(), `"zero auth login"`) {
		t.Fatalf("stderr = %q, want a zero auth login suggestion", stderr.String())
	}
	// An unrelated unknown command gets no misleading suggestion.
	stderr.Reset()
	runWithDeps([]string{"frobnicate"}, &stdout, &stderr, appDeps{})
	if strings.Contains(stderr.String(), "did you mean") {
		t.Fatalf("stderr = %q, unrelated commands must not get the auth hint", stderr.String())
	}
}

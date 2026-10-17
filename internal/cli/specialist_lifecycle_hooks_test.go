package cli

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/specialist"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/tui"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestRegisterSpecialistToolsWiresLifecycleHooksIntoTaskOutput(t *testing.T) {
	registry := tools.NewRegistry()
	runtime, err := registerSpecialistTools(registry, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("registerSpecialistTools returned error: %v", err)
	}
	t.Cleanup(func() { closeSpecialistRuntime(io.Discard, runtime) })

	if runtime.lifecycleHooks == nil {
		t.Fatal("lifecycleHooks is nil after registerSpecialistTools")
	}
	raw, ok := registry.Get("TaskOutput")
	if !ok {
		t.Fatal("TaskOutput tool not registered")
	}
	outputTool, ok := raw.(*specialist.OutputTool)
	if !ok {
		t.Fatalf("TaskOutput type = %T, want *specialist.OutputTool", raw)
	}
	if outputTool.LifecycleHooks != runtime.lifecycleHooks {
		t.Fatal("TaskOutput LifecycleHooks is not the shared registerSpecialistTools bridge")
	}

	dispatcher := hooks.NewDispatcher(hooks.DispatcherOptions{})
	runtime.attachLifecycleHooks(dispatcher)
	if runtime.lifecycleHooks.Dispatch == nil {
		t.Fatal("attachLifecycleHooks left Dispatch nil")
	}
	if outputTool.LifecycleHooks.Dispatch == nil {
		t.Fatal("TaskOutput did not observe Dispatch after attachLifecycleHooks")
	}
}

// TestRealEntryPointsAttachLifecycleHooks closes the gap the registration-level
// test above cannot see: wiring the bridge when tools register is not enough
// unless the real startup paths actually call attachLifecycleHooks before any
// run. Both surfaces that can launch specialists must do so — the interactive
// TUI and headless exec. attachLifecycleHooks mutates the same
// *specialist.LifecycleHooks the TaskOutput tool holds, so a nil Dispatch on
// that tool after a full startup proves the call never ran and the
// specialistStart/specialistStop hooks would silently never fire.
func TestRealEntryPointsAttachLifecycleHooks(t *testing.T) {
	requireTaskOutputDispatchAttached := func(t *testing.T, registry *tools.Registry) {
		t.Helper()
		if registry == nil {
			t.Fatal("startup did not reach the run registry")
		}
		raw, ok := registry.Get("TaskOutput")
		if !ok {
			t.Fatal("TaskOutput tool not registered by startup")
		}
		outputTool, ok := raw.(*specialist.OutputTool)
		if !ok {
			t.Fatalf("TaskOutput type = %T, want *specialist.OutputTool", raw)
		}
		if outputTool.LifecycleHooks == nil || outputTool.LifecycleHooks.Dispatch == nil {
			t.Fatal("startup left the specialist lifecycle bridge unattached: specialistStart/specialistStop hooks would never fire")
		}
	}

	t.Run("interactive TUI", func(t *testing.T) {
		isolateCLIUserState(t)
		cwd := t.TempDir()
		var registry *tools.Registry

		// Empty args route runWithDeps to the interactive TUI path; the injected
		// runTUI receives the same registry specialist tools registered into.
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
				}, nil
			},
			resolveMCPConfig: func(string, bool) (config.MCPConfig, error) { return config.MCPConfig{}, nil },
			runTUI: func(_ context.Context, options tui.Options) int {
				registry = options.Registry
				return exitSuccess
			},
		})
		if exitCode != exitSuccess {
			t.Fatalf("runWithDeps exit = %d, want %d", exitCode, exitSuccess)
		}
		requireTaskOutputDispatchAttached(t, registry)
	})

	t.Run("headless exec", func(t *testing.T) {
		isolateCLIUserState(t)
		cwd := t.TempDir()
		var registry *tools.Registry
		var stdout, stderr bytes.Buffer

		// --auto high is what registers specialist tools on the exec surface.
		// MCP registration is the one startup chokepoint that sees the run
		// registry before attachLifecycleHooks runs, so capture it there and
		// assert after the run completes.
		exitCode := runWithDeps([]string{
			"exec", "--auto", "high", "--cwd", cwd, "--init-session-id", "hook_wireup", "hello",
		}, &stdout, &stderr, appDeps{
			getwd: func() (string, error) { return cwd, nil },
			resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
				return execResolvedConfig(), nil
			},
			newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
				return echoExecProvider{}, nil
			},
			resolveMCPConfig: func(string, bool) (config.MCPConfig, error) {
				return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
					"wireup": {Type: "stdio", Command: "wireup-mcp"},
				}}, nil
			},
			newMCPStore: func() (*mcp.PermissionStore, error) { return nil, nil },
			registerMCPTools: func(_ context.Context, captured *tools.Registry, _ config.MCPConfig, _ mcp.RegisterOptions) (mcpToolRuntime, error) {
				registry = captured
				return closeFunc(func() error { return nil }), nil
			},
		})
		if exitCode != exitSuccess {
			t.Fatalf("runWithDeps exit = %d, want %d (stderr=%q)", exitCode, exitSuccess, stderr.String())
		}
		requireTaskOutputDispatchAttached(t, registry)
	})
}

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/tui"
)

type skippingMCPRuntime struct {
	skipped []mcp.SkippedServer
}

func (r skippingMCPRuntime) Close() error                 { return nil }
func (r skippingMCPRuntime) Skipped() []mcp.SkippedServer { return r.skipped }

// Startup already knows which servers failed — it prints a warning about each.
// That warning is gone by the time anyone looks, so the same set has to reach
// the TUI, which is where /mcp answers "what is actually running".
func TestRunPassesSkippedMCPServersToTheTUI(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cwd := t.TempDir()
	setCLIUserConfigRoot(t)
	projectConfigPath := filepath.Join(cwd, ".zero", "config.json")
	if err := os.MkdirAll(filepath.Dir(projectConfigPath), 0o700); err != nil {
		t.Fatalf("create project config parent: %v", err)
	}
	if err := os.WriteFile(projectConfigPath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	var launchedOptions tui.Options

	exitCode := runWithDeps([]string{}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) { return cwd, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return config.ResolvedConfig{MaxTurns: 12}, nil
		},
		// A CONFIGURED server, so it lands in the critical half startup registers
		// synchronously. Startup splits MCP into a critical set registered before
		// the TUI launches and an optional set (unconfigured built-in defaults)
		// registered on a background goroutine; with no servers configured at all,
		// the registration this test stubs is never reached.
		resolveMCPConfig: func(string, bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"docs": {URL: "https://host.invalid/mcp"},
			}}, nil
		},
		userConfigPath: func() (string, error) {
			return filepath.Join(t.TempDir(), "zero", "config.json"), nil
		},
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return skippingMCPRuntime{skipped: []mcp.SkippedServer{
				{Name: "docs", Err: errors.New("connection refused")},
			}}, nil
		},
		runTUI: func(_ context.Context, options tui.Options) int {
			launchedOptions = options
			return 0
		},
	})

	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", exitCode, stderr.String())
	}
	if len(launchedOptions.MCPSkipped) != 1 ||
		launchedOptions.MCPSkipped[0].Name != "docs" {
		t.Fatalf("MCPSkipped = %#v, want the failure startup recorded", launchedOptions.MCPSkipped)
	}
	// The stderr warning stays: it is what a non-interactive user sees, and the
	// panel is an addition to it, not a replacement.
	if !strings.Contains(stderr.String(), "docs") {
		t.Errorf("startup no longer warns about the skipped server: %q", stderr.String())
	}
}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/tools"
)

// disclosingExecRuntime is an MCP runtime that launched a process under reduced
// enforcement.
type disclosingExecRuntime struct {
	noopMCPRuntime
	disclosures []mcp.StartupDisclosure
}

func (r disclosingExecRuntime) StartupDisclosures() []mcp.StartupDisclosure { return r.disclosures }

const execDisclosureNotice = "denyRead is configured, so the write jail is not confining writes"

// isolateConfigDirs points every config/cache root at test-owned storage.
//
// Without it these tests build a sandbox engine against the developer's REAL
// config dir, trigger the one-time grant migration there, and the migration
// notice then turns up on a LATER test's stderr, failing whichever test happens
// to assert an empty one. The failure moves between runs, which is what makes it
// look like flakiness rather than contamination.
func isolateConfigDirs(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_CACHE_HOME", dir)
}

func execDisclosureDeps(cwd string) appDeps {
	return appDeps{
		getwd:         func() (string, error) { return cwd, nil },
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) { return execResolvedConfig(), nil },
		resolveMCPConfig: func(string, bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				"docs": {Type: "stdio", Command: "docs-mcp"},
			}}, nil
		},
		registerMCPTools: func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
			return disclosingExecRuntime{disclosures: []mcp.StartupDisclosure{
				{Name: "docs", Notices: []string{execDisclosureNotice}},
			}}, nil
		},
	}
}

// A HEADLESS RUN IS A DISCLOSURE SURFACE TOO.
//
// `zero exec` registers workspace MCP servers through the same sandbox-backed
// runner interactive startup uses, so a stdio server here can launch under the
// weakened token and serve the whole run. Reporting the disclosure only from the
// TUI meant every text, JSON, stream-JSON and --list-tools caller was told
// nothing about the enforcement trade, for a process that was already running.
func TestExecReportsMCPStartupDisclosures(t *testing.T) {
	isolateConfigDirs(t)
	for _, format := range []string{"", "--output-format=json", "--output-format=stream-json"} {
		name := format
		if name == "" {
			name = "text"
		}
		t.Run(name, func(t *testing.T) {
			args := []string{"exec", "--list-tools"}
			if format != "" {
				args = append(args, format)
			}
			var stdout, stderr bytes.Buffer
			if code := runWithDeps(args, &stdout, &stderr, execDisclosureDeps(t.TempDir())); code != exitSuccess {
				t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), execDisclosureNotice) {
				t.Errorf("the headless run said nothing about the enforcement trade: %q", stderr.String())
			}
			if !strings.Contains(stderr.String(), "docs") {
				t.Errorf("the report does not name the server: %q", stderr.String())
			}
			// The machine-readable surfaces must stay parseable: the disclosure
			// belongs on stderr precisely so stdout framing is untouched.
			if format == "--output-format=json" {
				var any map[string]any
				if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &any); err != nil {
					t.Errorf("stdout is no longer valid JSON: %v (%q)", err, stdout.String())
				}
			}
			if format == "--output-format=stream-json" {
				for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
					if strings.TrimSpace(line) == "" {
						continue
					}
					var any map[string]any
					if err := json.Unmarshal([]byte(line), &any); err != nil {
						t.Errorf("a stream-json line is not valid JSON: %v (%q)", err, line)
					}
				}
			}
		})
	}
}

// A run whose servers launched nothing says nothing.
func TestExecWithoutDisclosuresStaysQuiet(t *testing.T) {
	isolateConfigDirs(t)
	deps := execDisclosureDeps(t.TempDir())
	deps.registerMCPTools = func(context.Context, *tools.Registry, config.MCPConfig, mcp.RegisterOptions) (mcpToolRuntime, error) {
		return noopMCPRuntime{}, nil
	}
	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"exec", "--list-tools"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "reduced enforcement") {
		t.Errorf("a run with nothing to disclose printed one: %q", stderr.String())
	}
}

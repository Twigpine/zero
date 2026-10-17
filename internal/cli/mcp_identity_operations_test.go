package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/tools"
)

// AN EXACT KEY ADDRESSES THE STORED ENTRY, A RUNTIME NAME JOINS ITS OUTCOME.
//
// These tests drive the commands themselves rather than the helpers under them.
// Whether refuseColliding rejects a pair, or whether NormalizeConfig trims a
// key, was never in question: what went wrong both times is that the command
// did not consult them at the point where it mattered.

const collidingAliasConfig = `{
  "mcp": {
    "servers": {
      "docs": {
        "type": "stdio",
        "command": "docs-mcp"
      },
      " docs": {
        "type": "stdio",
        "command": "other-mcp",
        "disabled": true
      },
      "search": {
        "type": "stdio",
        "command": "search-mcp",
        "disabled": true
      }
    }
  }
}
`

// reloadMCPCommandConfig is the check startup applies to the file: both keys
// resolving to one runtime name is what aborts an interactive launch.
func reloadMCPCommandConfig(t *testing.T, path string) error {
	t.Helper()
	return mcp.ValidateUniqueNames(readMCPCommandConfig(t, path).MCP)
}

// ENABLING CLAIMS A RUNTIME IDENTITY.
//
// "docs" beside a disabled " docs" is a valid file, because a disabled entry
// claims no active name. `zero mcp enable ' docs'` cleared the flag unchecked,
// persisted two enabled keys that both resolve to "docs", reported success, and
// the next load refused the file.
func TestRunMCPEnableRefusesAnIdentityCollisionBeforeWriting(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeMCPCommandRawConfig(t, configPath, collidingAliasConfig)
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	deps := appDeps{userConfigPath: func() (string, error) { return configPath, nil }}

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"mcp", "enable", " docs", "--json"}, &stdout, &stderr, deps); code == exitSuccess {
		t.Fatalf("enabling a second key for one runtime name reported success; stdout=%q", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "both resolve to") {
		t.Errorf("stderr = %q, want the collision named", got)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("a refused enable rewrote the file:\n%s", after)
	}
	if err := reloadMCPCommandConfig(t, configPath); err != nil {
		t.Fatalf("the file no longer loads: %v", err)
	}
	if !readMCPCommandConfig(t, configPath).MCP.Servers[" docs"].Disabled {
		t.Fatal("the alias is no longer disabled")
	}
}

// The rule refuses a collision and nothing else. An entry that claims a name of
// its own is enabled as before, and the enabled/disabled alias pair it sits
// beside stays valid.
func TestRunMCPEnableStillEnablesANonConflictingServer(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	writeMCPCommandRawConfig(t, configPath, collidingAliasConfig)
	deps := appDeps{userConfigPath: func() (string, error) { return configPath, nil }}

	var stdout, stderr bytes.Buffer
	if code := runWithDeps([]string{"mcp", "enable", "search", "--json"}, &stdout, &stderr, deps); code != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", code, stderr.String())
	}
	cfg := readMCPCommandConfig(t, configPath)
	if cfg.MCP.Servers["search"].Disabled {
		t.Fatal("search is still disabled")
	}
	if !cfg.MCP.Servers[" docs"].Disabled || cfg.MCP.Servers["docs"].Disabled {
		t.Fatalf("the alias pair changed: %#v", cfg.MCP.Servers)
	}
	if err := reloadMCPCommandConfig(t, configPath); err != nil {
		t.Fatalf("the file no longer loads: %v", err)
	}
}

// DISABLING AND REMOVING ARE HOW A COLLIDING FILE IS REPAIRED, so neither may be
// refused by the rule that keeps a collision from being written. The file here
// is one an older build could have produced: both keys enabled.
func TestRunMCPDisableAndRemoveRecoverACollidingFile(t *testing.T) {
	colliding := strings.Replace(collidingAliasConfig, `"command": "other-mcp",
        "disabled": true`, `"command": "other-mcp"`, 1)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"disable", []string{"mcp", "disable", " docs", "--json"}},
		{"remove", []string{"mcp", "remove", " docs", "--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.json")
			writeMCPCommandRawConfig(t, configPath, colliding)
			if err := reloadMCPCommandConfig(t, configPath); err == nil {
				t.Fatal("the fixture does not collide, so nothing is being recovered")
			}
			deps := appDeps{userConfigPath: func() (string, error) { return configPath, nil }}
			var stdout, stderr bytes.Buffer
			if code := runWithDeps(tc.args, &stdout, &stderr, deps); code != exitSuccess {
				t.Fatalf("exitCode = %d stderr=%s", code, stderr.String())
			}
			if err := reloadMCPCommandConfig(t, configPath); err != nil {
				t.Fatalf("the file still does not load: %v", err)
			}
			if readMCPCommandConfig(t, configPath).MCP.Servers["docs"].Disabled {
				t.Fatal("the other entry was touched")
			}
		})
	}
}

// REGISTRATION REPORTS UNDER THE RUNTIME NAME.
//
// With only the key " docs " configured, the lookup succeeds on the exact key
// and the failed connection is recorded as "docs". The command compared that
// with its own argument, found no match, and printed status "ok" with exit 0
// for a server that never started.
//
// These are the arguments both manager actions dispatch
// (TestManagerCheckActionsDispatchTheSameExactKey in internal/tui), driven
// through the command that receives them, with the real registration failing
// the way it does at startup: the binary does not exist.
func TestRunMCPCheckMatchesFailuresByRuntimeName(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"padded key", " docs "},
		{"control: unpadded key", "docs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setTrustConfigRoot(t)
			deps := appDeps{
				getwd: func() (string, error) { return t.TempDir(), nil },
				resolveMCPConfig: func(_ string, _ bool) (config.MCPConfig, error) {
					return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
						tc.key: {Type: "stdio", Command: "zero-definitely-not-a-real-binary-xyz"},
					}}, nil
				},
			}

			var out, errBuf bytes.Buffer
			if code := runWithDeps([]string{"mcp", "check", tc.key, "--json"}, &out, &errBuf, deps); code == exitSuccess {
				t.Fatalf("a server that never started checked out as exit 0; stdout=%q", out.String())
			}
			var payload struct {
				ServerName string `json:"serverName"`
				Status     string `json:"status"`
				Error      string `json:"error"`
			}
			if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
				t.Fatalf("decode %q: %v", out.String(), err)
			}
			if payload.Status != "unreachable" {
				t.Errorf("status = %q, want unreachable", payload.Status)
			}
			if payload.ServerName != tc.key {
				t.Errorf("serverName = %q, want the key that was checked, %q", payload.ServerName, tc.key)
			}
			if !strings.Contains(payload.Error, "not reachable") || !strings.Contains(payload.Error, "zero-definitely-not-a-real-binary-xyz") {
				t.Errorf("error = %q, want the failure reason", payload.Error)
			}

			out.Reset()
			errBuf.Reset()
			if code := runWithDeps([]string{"mcp", "check", tc.key}, &out, &errBuf, deps); code == exitSuccess {
				t.Fatalf("the text form reported exit 0; stdout=%q", out.String())
			}
			if strings.Contains(out.String(), "is reachable") {
				t.Errorf("stdout = %q", out.String())
			}
			if !strings.Contains(errBuf.String(), "not reachable") {
				t.Errorf("stderr = %q, want the failure", errBuf.String())
			}
		})
	}
}

// The conversion is for joining outcomes, and it must not turn a healthy padded
// key into a failure: a registration that records nothing still checks out.
func TestRunMCPCheckReportsAHealthyPaddedKey(t *testing.T) {
	setTrustConfigRoot(t)
	var registered config.MCPConfig
	deps := appDeps{
		getwd: func() (string, error) { return t.TempDir(), nil },
		resolveMCPConfig: func(_ string, _ bool) (config.MCPConfig, error) {
			return config.MCPConfig{Servers: map[string]config.MCPServerConfig{
				" docs ": {Type: "stdio", Command: "docs-mcp"},
				"other":  {Type: "stdio", Command: "other-mcp"},
			}}, nil
		},
		newMCPStore: func() (*mcp.PermissionStore, error) {
			return mcp.NewPermissionStore(mcp.StoreOptions{FilePath: filepath.Join(t.TempDir(), "permissions.json")})
		},
		registerMCPTools: func(_ context.Context, registry *tools.Registry, cfg config.MCPConfig, _ mcp.RegisterOptions) (mcpToolRuntime, error) {
			registered = cfg
			registry.Register(cliFakeMCPRegistryTool{})
			return closeFunc(func() error { return nil }), nil
		},
	}
	var out, errBuf bytes.Buffer
	if code := runWithDeps([]string{"mcp", "check", " docs ", "--json"}, &out, &errBuf, deps); code != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", code, errBuf.String())
	}
	if _, ok := registered.Servers[" docs "]; !ok || len(registered.Servers) != 1 {
		t.Fatalf("registered %#v, want only the exact key", registered.Servers)
	}
	var payload struct {
		Status    string `json:"status"`
		ToolCount int    `json:"toolCount"`
	}
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("decode %q: %v", out.String(), err)
	}
	if payload.Status != "ok" || payload.ToolCount != 1 {
		t.Fatalf("payload = %#v, want ok with one tool", payload)
	}
}

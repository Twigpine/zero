package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/config"
)

type mcpConfigLockCase struct {
	name     string
	args     []string
	disabled bool
	server   bool
}

func mcpConfigLockCases() []mcpConfigLockCase {
	return []mcpConfigLockCase{
		{name: "add", args: []string{"mcp", "add", "docs", "--", "docs-mcp"}},
		{name: "remove", args: []string{"mcp", "remove", "docs"}, server: true},
		{name: "enable", args: []string{"mcp", "enable", "docs"}, server: true, disabled: true},
		{name: "disable", args: []string{"mcp", "disable", "docs"}, server: true},
	}
}

func seedMCPConfigLockCase(t *testing.T, path string, testCase mcpConfigLockCase) {
	t.Helper()
	servers := map[string]config.MCPServerConfig{}
	if testCase.server {
		servers["docs"] = config.MCPServerConfig{Type: "stdio", Command: "docs-mcp", Disabled: testCase.disabled}
	}
	writeMCPCommandConfig(t, path, config.FileConfig{
		ActiveProvider: "seed",
		Providers:      []config.ProviderProfile{{Name: "seed", Model: "seed-model"}},
		MCP:            config.MCPConfig{Servers: servers},
	})
}

func assertMCPConfigLockMutation(t *testing.T, path string, testCase mcpConfigLockCase) {
	t.Helper()
	cfg := readMCPCommandConfig(t, path)
	server, exists := cfg.MCP.Servers["docs"]
	switch testCase.name {
	case "add":
		if !exists {
			t.Fatal("mcp add did not persist the server")
		}
	case "remove":
		if exists {
			t.Fatal("mcp remove left the server configured")
		}
	case "enable":
		if !exists || server.Disabled {
			t.Fatalf("mcp enable result = %+v, exists=%v", server, exists)
		}
	case "disable":
		if !exists || !server.Disabled {
			t.Fatalf("mcp disable result = %+v, exists=%v", server, exists)
		}
	}
	if cfg.Preferences.Theme != "dracula" {
		t.Errorf("theme update was lost: theme = %q, want dracula", cfg.Preferences.Theme)
	}
	if cfg.ActiveProvider != "seed" || len(cfg.Providers) != 1 || cfg.Providers[0].Name != "seed" {
		t.Errorf("seeded provider was lost: active=%q providers=%#v", cfg.ActiveProvider, cfg.Providers)
	}
}

// TestRunMCPConfigCommandsParticipateInConfigLock covers the half of issue #832
// that lives outside internal/config. MCP config commands read the SAME user
// config document, edit it, and republish it with the same temp-file+rename
// shape as the config package's mutators. Locking only the config package would
// leave these writers free to clobber a concurrent provider or preference
// update, and be clobbered by one, with the file still valid JSON afterwards.
//
// Racing the two writers and hoping to observe a lost update is unreliable —
// the interleaving that loses one is narrow, and the test passed consistently
// against the unlocked code. So this asserts the property that actually matters
// and is deterministic: while the config lock is held elsewhere, the MCP
// writer's update CANNOT land. Once released it completes, and both updates
// survive.
func TestRunMCPConfigCommandsParticipateInConfigLock(t *testing.T) {
	for _, testCase := range mcpConfigLockCases() {
		t.Run(testCase.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "zero", "config.json")
			seedMCPConfigLockCase(t, configPath, testCase)
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}

			unlock, err := config.LockFile(configPath)
			if err != nil {
				t.Fatalf("acquire config lock: %v", err)
			}
			released := false
			release := func() {
				if !released {
					released = true
					if err := unlock(); err != nil {
						t.Fatalf("release config lock: %v", err)
					}
				}
			}
			defer release()

			original := lockMCPConfigFile
			lockAttempted := make(chan struct{})
			lockMCPConfigFile = func(path string) (func() error, error) {
				close(lockAttempted)
				return original(path)
			}
			t.Cleanup(func() { lockMCPConfigFile = original })

			var stdout, stderr bytes.Buffer
			done := make(chan int, 1)
			go func() {
				done <- runWithDeps(testCase.args, &stdout, &stderr, appDeps{
					userConfigPath: func() (string, error) { return configPath, nil },
				})
			}()

			select {
			case <-lockAttempted:
			case <-time.After(30 * time.Second):
				t.Fatalf("mcp %s did not attempt config lock acquisition", testCase.name)
			}

			for range 20 {
				after, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(after, before) {
					t.Fatalf("mcp %s changed config while its lock was held", testCase.name)
				}
				select {
				case exitCode := <-done:
					t.Fatalf("mcp %s completed (exit %d) while its lock was held", testCase.name, exitCode)
				default:
				}
				time.Sleep(5 * time.Millisecond)
			}

			release()
			select {
			case exitCode := <-done:
				if exitCode != exitSuccess {
					t.Fatalf("mcp %s exitCode = %d stderr=%s", testCase.name, exitCode, stderr.String())
				}
			case <-time.After(30 * time.Second):
				t.Fatalf("mcp %s did not finish after the config lock was released", testCase.name)
			}

			if _, err := config.SetTheme(configPath, "dracula"); err != nil {
				t.Fatalf("SetTheme: %v", err)
			}
			assertMCPConfigLockMutation(t, configPath, testCase)
		})
	}
}

func TestRunMCPConfigCommandsReportLockAcquisitionFailure(t *testing.T) {
	sentinel := errors.New("injected lock acquisition failure")
	original := lockMCPConfigFile
	lockMCPConfigFile = func(string) (func() error, error) { return nil, sentinel }
	t.Cleanup(func() { lockMCPConfigFile = original })

	for _, testCase := range mcpConfigLockCases() {
		t.Run(testCase.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "zero", "config.json")
			seedMCPConfigLockCase(t, configPath, testCase)
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			exitCode := runWithDeps(testCase.args, &stdout, &stderr, appDeps{
				userConfigPath: func() (string, error) { return configPath, nil },
			})
			if exitCode != exitCrash || stdout.Len() != 0 || !strings.Contains(stderr.String(), sentinel.Error()) {
				t.Fatalf("exit=%d stdout=%q stderr=%q, want lock error without success output", exitCode, stdout.String(), stderr.String())
			}
			after, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("command changed config after lock acquisition failed")
			}
		})
	}
}

func TestRunMCPConfigCommandsReleaseBeforeSuccessOutput(t *testing.T) {
	sentinel := errors.New("injected lock release failure")
	original := lockMCPConfigFile
	lockMCPConfigFile = func(string) (func() error, error) {
		return func() error { return sentinel }, nil
	}
	t.Cleanup(func() { lockMCPConfigFile = original })

	for _, testCase := range mcpConfigLockCases() {
		t.Run(testCase.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "zero", "config.json")
			seedMCPConfigLockCase(t, configPath, testCase)
			var stdout, stderr bytes.Buffer
			exitCode := runWithDeps(testCase.args, &stdout, &stderr, appDeps{
				userConfigPath: func() (string, error) { return configPath, nil },
			})
			if exitCode != exitCrash || stdout.Len() != 0 || !strings.Contains(stderr.String(), sentinel.Error()) {
				t.Fatalf("exit=%d stdout=%q stderr=%q, want unlock error before success output", exitCode, stdout.String(), stderr.String())
			}
		})
	}
}

package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
)

func TestRunMCPAddJSONRedactsEnvAndPreservesConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	writeMCPCommandConfig(t, configPath, config.FileConfig{
		ActiveProvider: "local",
		MaxTurns:       42,
		Providers: []config.ProviderProfile{{
			Name:  "local",
			Model: "qwen3-coder:480b",
		}},
		Preferences: config.PreferencesConfig{FavoriteModels: []string{"qwen3-coder:480b"}},
		MCP: config.MCPConfig{Servers: map[string]config.MCPServerConfig{
			"existing": {Type: "stdio", Command: "existing-mcp"},
		}},
	})
	var stdout, stderr bytes.Buffer

	exitCode := runWithDeps([]string{
		"mcp", "add", "docs",
		"--json",
		"--env", "DOCS_TOKEN=stdio-secret",
		"--", "docs-mcp", "--port", "123",
	}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	})

	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if output := stdout.String(); strings.Contains(output, "stdio-secret") {
		t.Fatalf("JSON stdout leaked secret value: %s", output)
	}
	var payload struct {
		Server config.MCPServerConfig `json:"server"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
	}
	if got := payload.Server.Env["DOCS_TOKEN"]; got != "[REDACTED]" {
		t.Fatalf("JSON env redaction = %q, want [REDACTED]", got)
	}

	cfg := readMCPCommandConfig(t, configPath)
	if cfg.ActiveProvider != "local" || cfg.MaxTurns != 42 || len(cfg.Providers) != 1 {
		t.Fatalf("non-MCP config was not preserved: %#v", cfg)
	}
	if got := cfg.Preferences.FavoriteModels; len(got) != 1 || got[0] != "qwen3-coder:480b" {
		t.Fatalf("preferences were not preserved: %#v", got)
	}
	if _, ok := cfg.MCP.Servers["existing"]; !ok {
		t.Fatalf("existing MCP server was not preserved: %#v", cfg.MCP.Servers)
	}
	added := cfg.MCP.Servers["docs"]
	if got := added.Env["DOCS_TOKEN"]; got != "stdio-secret" {
		t.Fatalf("persisted env secret = %q, want original value", got)
	}
}

func TestRunMCPAddJSONRedactsHeaders(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "zero", "config.json")
	var stdout, stderr bytes.Buffer

	exitCode := runWithDeps([]string{
		"mcp", "add", "docs",
		"--json",
		"--header", "Authorization=Bearer header-secret",
		"--url", "https://example.com/mcp",
	}, &stdout, &stderr, appDeps{
		userConfigPath: func() (string, error) { return configPath, nil },
	})

	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stderr=%s", exitCode, stderr.String())
	}
	if output := stdout.String(); strings.Contains(output, "header-secret") {
		t.Fatalf("JSON stdout leaked header value: %s", output)
	}
	var payload struct {
		Server config.MCPServerConfig `json:"server"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, stdout.String())
	}
	if got := payload.Server.Headers["Authorization"]; got != "[REDACTED]" {
		t.Fatalf("JSON header redaction = %q, want [REDACTED]", got)
	}

	cfg := readMCPCommandConfig(t, configPath)
	added := cfg.MCP.Servers["docs"]
	if got := added.Headers["Authorization"]; got != "Bearer header-secret" {
		t.Fatalf("persisted header secret = %q, want original value", got)
	}
}

func TestRunMCPAddRejectsTransportSpecificSecrets(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "http rejects env",
			args: []string{"mcp", "add", "remote", "--url", "https://example.com/mcp", "--env", "REMOTE_TOKEN=secret"},
			want: "env is only supported for stdio transport",
		},
		{
			name: "sse rejects env",
			args: []string{"mcp", "add", "events", "--type", "sse", "--url", "https://example.com/sse", "--env", "SSE_TOKEN=secret"},
			want: "env is only supported for stdio transport",
		},
		{
			name: "stdio rejects header",
			args: []string{"mcp", "add", "local", "--header", "Authorization=Bearer secret", "--", "local-mcp"},
			want: "headers are only supported for http or sse transports",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			configPathCalled := false

			exitCode := runWithDeps(tc.args, &stdout, &stderr, appDeps{
				userConfigPath: func() (string, error) {
					configPathCalled = true
					return filepath.Join(t.TempDir(), "zero", "config.json"), nil
				},
			})

			if exitCode != exitUsage {
				t.Fatalf("exitCode = %d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
			}
			if configPathCalled {
				t.Fatal("user config path was resolved after invalid MCP add arguments")
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr missing %q:\n%s", tc.want, stderr.String())
			}
			if strings.Contains(stdout.String()+stderr.String(), "secret") {
				t.Fatalf("output leaked secret value:\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
			}
		})
	}
}

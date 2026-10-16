package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/tools"
)

func TestCoreRegistryIncludesDefaultLocalControlTools(t *testing.T) {
	registry := newCoreRegistry(t.TempDir())
	for _, name := range []string{"browser_install", "browser_launch", "browser_connect", "browser_open", "browser_snapshot", "browser_click", "browser_type", "browser_press", "terminal_session", "capture_artifact"} {
		tool, ok := registry.Get(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if tool.Safety().Permission != tools.PermissionPrompt {
			t.Fatalf("%s permission = %s, want prompt", name, tool.Safety().Permission)
		}
	}
	desktop, ok := registry.Get("desktop_action")
	if !ok {
		t.Fatal("desktop_action not registered")
	}
	if desktop.Safety().Permission != tools.PermissionDeny {
		t.Fatalf("desktop_action permission = %s, want deny", desktop.Safety().Permission)
	}
}

func TestCoreRegistryHonorsExplicitLocalControlDisable(t *testing.T) {
	registry := newCoreRegistry(t.TempDir())
	var cfg config.LocalControlConfig
	if err := json.Unmarshal([]byte(`{"enabled":false}`), &cfg); err != nil {
		t.Fatalf("unmarshal local control config: %v", err)
	}
	registerLocalControlTools(registry, t.TempDir(), cfg)
	for _, name := range []string{"browser_install", "browser_launch", "browser_connect", "browser_open", "browser_snapshot", "browser_click", "browser_type", "browser_press", "desktop_action", "terminal_session", "capture_artifact"} {
		tool, ok := registry.Get(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if tool.Safety().Permission != tools.PermissionDeny {
			t.Fatalf("%s permission = %s, want deny", name, tool.Safety().Permission)
		}
	}
}

func TestRunExecListToolsHonorsDisabledLocalControlConfig(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cfg := execResolvedConfig()
	var localControl config.LocalControlConfig
	if err := json.Unmarshal([]byte(`{"enabled":false}`), &localControl); err != nil {
		t.Fatalf("unmarshal local control config: %v", err)
	}
	cfg.LocalControl = localControl

	exitCode := runWithDeps([]string{"exec", "--list-tools", "-o", "json"}, &stdout, &stderr, appDeps{
		getwd: func() (string, error) {
			return t.TempDir(), nil
		},
		resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
			return cfg, nil
		},
	})
	if exitCode != exitSuccess {
		t.Fatalf("exitCode = %d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	if stderr.String() != hostSandboxNotice(t) {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	var payload struct {
		Tools []struct {
			Name       string `json:"name"`
			Permission string `json:"permission"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &payload); err != nil {
		t.Fatalf("parse tools JSON: %v\n%s", err, stdout.String())
	}
	for _, tool := range payload.Tools {
		if tool.Name == "browser_open" {
			t.Fatalf("browser_open should be hidden when local control is disabled, got permission %s", tool.Permission)
		}
	}
}

func TestRegisterLocalControlToolsAppliesDefaults(t *testing.T) {
	workspaceRoot := t.TempDir()
	registry := newCoreRegistry(workspaceRoot)
	registerLocalControlTools(registry, workspaceRoot, config.LocalControlConfig{Enabled: true})
	for _, name := range []string{"browser_install", "browser_launch", "browser_connect", "browser_open", "browser_snapshot", "browser_click", "browser_type", "browser_press", "terminal_session", "capture_artifact"} {
		tool, ok := registry.Get(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if tool.Safety().Permission != tools.PermissionPrompt {
			t.Fatalf("%s permission = %s, want prompt", name, tool.Safety().Permission)
		}
	}
	desktop, ok := registry.Get("desktop_action")
	if !ok {
		t.Fatal("desktop_action not registered")
	}
	if desktop.Safety().Permission != tools.PermissionDeny {
		t.Fatalf("desktop_action permission = %s, want deny without nested desktop opt-in", desktop.Safety().Permission)
	}
}

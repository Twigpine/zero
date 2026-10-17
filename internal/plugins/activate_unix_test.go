//go:build unix

package plugins

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

// TestActivateToolRunsRealCommandEndToEnd exercises the default (non-mocked) tool
// runner: a real shell script is invoked through the registry, receives the JSON
// arguments on stdin, can read its install dir from $AGENT_PLUGIN_ROOT, and its
// stdout/exit map onto the tool Result. This proves execPluginCommand is wired
// correctly, complementing the runner-stubbed unit tests.
func TestActivateToolRunsRealCommandEndToEnd(t *testing.T) {
	pluginDir := t.TempDir()
	script := filepath.Join(pluginDir, "tool.sh")
	// The script echoes a marker, the plugin root it received via env, and the JSON
	// args it read from stdin — so the test can assert all three round-trip.
	body := "#!/bin/sh\n" +
		"printf 'ran in %s\\n' \"$AGENT_PLUGIN_ROOT\"\n" +
		"cat\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	registry := tools.NewRegistry()
	plugin := LoadedPlugin{
		ID:        "zero.demo",
		Name:      "Demo",
		Enabled:   true,
		Source:    SourceProject,
		PluginDir: pluginDir,
		Tools: []ToolExtension{{
			Name:       "demo",
			Command:    "${AGENT_PLUGIN_ROOT}/tool.sh",
			Permission: PermissionPrompt,
		}},
	}

	// No runTool override: this uses the real execPluginCommand subprocess runner.
	Activate(registry, []LoadedPlugin{plugin}, ActivateOptions{})

	res := registry.RunWithOptions(context.Background(), "demo", map[string]any{"q": "hello"}, tools.RunOptions{PermissionGranted: true})
	if res.Status != tools.StatusOK {
		t.Fatalf("status = %q (%s)", res.Status, res.Output)
	}
	if !strings.Contains(res.Output, "ran in "+pluginDir) {
		t.Fatalf("output missing AGENT_PLUGIN_ROOT round-trip: %q", res.Output)
	}
	if !strings.Contains(res.Output, `"q":"hello"`) {
		t.Fatalf("output missing JSON args on stdin: %q", res.Output)
	}
	if res.Meta["exit_code"] != "0" {
		t.Fatalf("exit_code meta = %q, want 0", res.Meta["exit_code"])
	}
}

// TestActivateToolRealCommandMissingBinaryErrors confirms a plugin tool whose
// command cannot be launched surfaces as a tool error (not a misleading exit
// code), via the real subprocess runner.
func TestActivateToolRealCommandMissingBinaryErrors(t *testing.T) {
	registry := tools.NewRegistry()
	plugin := LoadedPlugin{
		ID:        "zero.demo",
		Name:      "Demo",
		Enabled:   true,
		Source:    SourceProject,
		PluginDir: t.TempDir(),
		Tools: []ToolExtension{{
			Name:       "demo",
			Command:    filepath.Join(t.TempDir(), "does-not-exist"),
			Permission: PermissionPrompt,
		}},
	}

	Activate(registry, []LoadedPlugin{plugin}, ActivateOptions{})

	res := registry.RunWithOptions(context.Background(), "demo", map[string]any{}, tools.RunOptions{PermissionGranted: true})
	if res.Status != tools.StatusError {
		t.Fatalf("status = %q, want error for unlaunchable command", res.Status)
	}
	if !strings.Contains(res.Output, "Error executing plugin tool demo") {
		t.Fatalf("output = %q, want launch error", res.Output)
	}
}

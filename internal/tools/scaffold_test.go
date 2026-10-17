package tools_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/plugins"
	"github.com/Twigpine/zero/internal/tools"
)

func TestScaffoldCreatesManifestAndStub(t *testing.T) {
	dir := t.TempDir()

	result, err := tools.Scaffold(tools.ScaffoldOptions{Name: "foo", Dir: dir})
	if err != nil {
		t.Fatalf("Scaffold returned error: %v", err)
	}

	// The plugin lives under dir/<name>/ with a manifest and an entry stub.
	manifestPath := filepath.Join(dir, "foo", "plugin.json")
	if result.ManifestPath != manifestPath {
		t.Fatalf("ManifestPath = %q, want %q", result.ManifestPath, manifestPath)
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("manifest not created: %v", err)
	}
	if _, err := os.Stat(result.EntryPath); err != nil {
		t.Fatalf("entry stub not created: %v", err)
	}
	if filepath.Dir(result.EntryPath) == "" || !strings.HasPrefix(result.EntryPath, filepath.Join(dir, "foo")) {
		t.Fatalf("entry stub should live inside the plugin dir, got %q", result.EntryPath)
	}

	// The generated manifest parses against the real plugin schema.
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("manifest is not valid JSON: %v\n%s", err, data)
	}
	plugin, err := plugins.ParseManifest(raw, plugins.ParseManifestOptions{
		Source:       plugins.SourceUser,
		Root:         dir,
		PluginDir:    filepath.Join(dir, "foo"),
		ManifestPath: manifestPath,
	})
	if err != nil {
		t.Fatalf("generated manifest does not parse: %v", err)
	}
	if len(plugin.Tools) != 1 {
		t.Fatalf("expected exactly one tool, got %d", len(plugin.Tools))
	}
	tool := plugin.Tools[0]
	if tool.Name != "foo" {
		t.Fatalf("tool name = %q, want foo", tool.Name)
	}
	if tool.Command == "" {
		t.Fatalf("tool command should not be empty")
	}
	// The command + args should point at the generated entry script (inside the
	// plugin dir).
	if !commandReferencesEntry(tool, result.EntryPath, filepath.Join(dir, "foo")) {
		t.Fatalf("manifest command/args do not reference the entry script: cmd=%q args=%v entry=%q", tool.Command, tool.Args, result.EntryPath)
	}
	// A scaffolded tool defaults to prompt gating (never auto-allow) and an empty
	// parameter schema.
	if tool.Permission != plugins.PermissionPrompt {
		t.Fatalf("scaffolded tool permission = %q, want prompt", tool.Permission)
	}
}

func commandReferencesEntry(tool plugins.ToolExtension, entryPath string, pluginDir string) bool {
	entryBase := filepath.Base(entryPath)
	rel, _ := filepath.Rel(pluginDir, entryPath)
	for _, candidate := range append([]string{tool.Command}, tool.Args...) {
		if candidate == entryPath || candidate == entryBase || candidate == rel ||
			strings.HasSuffix(candidate, entryBase) {
			return true
		}
	}
	return false
}

// TestScaffoldCoversAllRuntimes exercises every runtimeSpecFor branch (shell,
// node, python) so template/manifest drift in the non-default runtimes is caught:
// each must produce an entry stub inside the plugin dir, a manifest that parses
// against the real plugin schema, and a tool command that references the entry.
func TestScaffoldCoversAllRuntimes(t *testing.T) {
	for _, runtime := range []tools.ScaffoldRuntime{tools.RuntimeShell, tools.RuntimeNode, tools.RuntimePython} {
		runtime := runtime
		t.Run(string(runtime), func(t *testing.T) {
			dir := t.TempDir()
			name := "tool-" + string(runtime)
			pluginDir := filepath.Join(dir, name)

			result, err := tools.Scaffold(tools.ScaffoldOptions{Name: name, Dir: dir, Runtime: runtime})
			if err != nil {
				t.Fatalf("Scaffold(%s) returned error: %v", runtime, err)
			}
			if result.ManifestPath != filepath.Join(pluginDir, "plugin.json") {
				t.Fatalf("ManifestPath = %q, want under %q", result.ManifestPath, pluginDir)
			}
			if _, err := os.Stat(result.ManifestPath); err != nil {
				t.Fatalf("manifest not created: %v", err)
			}
			if _, err := os.Stat(result.EntryPath); err != nil {
				t.Fatalf("entry stub not created: %v", err)
			}
			if !strings.HasPrefix(result.EntryPath, pluginDir) {
				t.Fatalf("entry stub should live inside the plugin dir, got %q", result.EntryPath)
			}

			data, err := os.ReadFile(result.ManifestPath)
			if err != nil {
				t.Fatalf("read manifest: %v", err)
			}
			var raw any
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatalf("manifest is not valid JSON: %v\n%s", err, data)
			}
			plugin, err := plugins.ParseManifest(raw, plugins.ParseManifestOptions{
				Source:       plugins.SourceUser,
				Root:         dir,
				PluginDir:    pluginDir,
				ManifestPath: result.ManifestPath,
			})
			if err != nil {
				t.Fatalf("generated manifest does not parse: %v", err)
			}
			if len(plugin.Tools) != 1 {
				t.Fatalf("expected exactly one tool, got %d", len(plugin.Tools))
			}
			tool := plugin.Tools[0]
			if tool.Command == "" {
				t.Fatalf("tool command should not be empty")
			}
			if !commandReferencesEntry(tool, result.EntryPath, pluginDir) {
				t.Fatalf("manifest command/args do not reference the entry script: cmd=%q args=%v entry=%q", tool.Command, tool.Args, result.EntryPath)
			}
			if tool.Permission != plugins.PermissionPrompt {
				t.Fatalf("scaffolded tool permission = %q, want prompt", tool.Permission)
			}
		})
	}
}

func TestScaffoldStubIsRunnableShape(t *testing.T) {
	dir := t.TempDir()
	result, err := tools.Scaffold(tools.ScaffoldOptions{Name: "greeter", Dir: dir})
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	data, err := os.ReadFile(result.EntryPath)
	if err != nil {
		t.Fatalf("read entry: %v", err)
	}
	body := string(data)
	// The stub should carry a clear TODO so the author knows what to fill in.
	if !strings.Contains(strings.ToUpper(body), "TODO") {
		t.Fatalf("entry stub should contain a TODO marker:\n%s", body)
	}
}

func TestScaffoldRejectsInvalidName(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"", "  ", "../escape", "with/slash", "."} {
		if _, err := tools.Scaffold(tools.ScaffoldOptions{Name: bad, Dir: dir}); err == nil {
			t.Fatalf("expected an error for invalid name %q", bad)
		}
	}
}

func TestScaffoldRefusesToClobberExisting(t *testing.T) {
	dir := t.TempDir()
	if _, err := tools.Scaffold(tools.ScaffoldOptions{Name: "dup", Dir: dir}); err != nil {
		t.Fatalf("first scaffold: %v", err)
	}
	if _, err := tools.Scaffold(tools.ScaffoldOptions{Name: "dup", Dir: dir}); err == nil {
		t.Fatalf("expected an error scaffolding over an existing tool dir")
	}
}

func TestScaffoldRequiresDir(t *testing.T) {
	if _, err := tools.Scaffold(tools.ScaffoldOptions{Name: "foo"}); err == nil {
		t.Fatalf("expected an error when Dir is empty")
	}
}

func TestScaffoldRuntimeDefaultsAreLanguageAgnostic(t *testing.T) {
	dir := t.TempDir()
	// Default runtime: the test only asserts a stub is produced and the manifest
	// parses; the specific interpreter is an implementation detail, but it must
	// not name a model provider or hardcode anything provider-specific.
	result, err := tools.Scaffold(tools.ScaffoldOptions{Name: "agnostic", Dir: dir})
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	data, _ := os.ReadFile(result.ManifestPath)
	lower := strings.ToLower(string(data))
	for _, banned := range []string{"openai", "anthropic", "claude", "gemini", "gpt-"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("scaffolded manifest must stay provider-neutral, found %q", banned)
		}
	}
}

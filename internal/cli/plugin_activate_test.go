package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/plugins"
	"github.com/Twigpine/zero/internal/tools"
)

// fakePluginDeps builds appDeps whose loadPlugins returns the supplied plugins and
// whose skillsDir points at an empty dir, so activatePlugins can be exercised
// without touching the real plugin roots.
func fakePluginDeps(t *testing.T, loaded []plugins.LoadedPlugin) appDeps {
	t.Helper()
	skillsDir := t.TempDir()
	return appDeps{
		getwd: func() (string, error) { return t.TempDir(), nil },
		loadPlugins: func(plugins.LoadOptions) (plugins.LoadResult, error) {
			return plugins.LoadResult{Plugins: loaded}, nil
		},
		skillsDir: func() string { return skillsDir },
	}
}

func TestActivatePluginsRegistersToolAndCollectsHooks(t *testing.T) {
	pluginDir := t.TempDir()
	loaded := []plugins.LoadedPlugin{{
		ID:        "zero.demo",
		Name:      "Demo",
		Enabled:   true,
		Source:    plugins.SourceProject,
		PluginDir: pluginDir,
		Tools: []plugins.ToolExtension{{
			Name:        "demo_lookup",
			Description: "lookup",
			Command:     "true",
			Permission:  plugins.PermissionPrompt,
		}},
		Hooks: []plugins.HookExtension{{
			Name:    "pre",
			Event:   plugins.HookBeforeTool,
			Command: "guard.sh",
		}},
	}}

	registry := tools.NewRegistry()
	var stderr bytes.Buffer
	workspace := t.TempDir()
	activation := activatePlugins(workspace, registry, fakePluginDeps(t, loaded), &stderr, workspace)

	if _, ok := registry.Get("demo_lookup"); !ok {
		t.Fatalf("plugin tool not registered into the bootstrap registry")
	}
	if len(activation.hooks) != 1 || activation.hooks[0].Event != hooks.EventBeforeTool {
		t.Fatalf("expected one beforeTool plugin hook, got %#v", activation.hooks)
	}
	if activation.hooks[0].ID != "zero.demo.pre" {
		t.Fatalf("plugin hook id = %q, want namespaced zero.demo.pre", activation.hooks[0].ID)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr for a clean activation: %s", stderr.String())
	}
}

func TestActivatePluginsRegistersPluginSkillTool(t *testing.T) {
	// Isolate host ~/.agents/skills so it cannot interfere with this test.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	pluginDir := t.TempDir()
	skillDir := filepath.Join(pluginDir, "skills", "demo-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillMd := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillMd, []byte("---\nname: demo-skill\n---\nplugin skill body"), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded := []plugins.LoadedPlugin{{
		ID:        "zero.skills",
		Name:      "Skills",
		Enabled:   true,
		Source:    plugins.SourceProject,
		PluginDir: pluginDir,
		Skills:    []plugins.PathExtension{{Name: "demo-skill", Path: skillMd}},
	}}

	registry := tools.NewRegistry()
	registry.Register(tools.NewSkillTool(t.TempDir())) // core skill tool, like the real bootstrap
	var stderr bytes.Buffer
	workspace := t.TempDir()
	activation := activatePlugins(workspace, registry, fakePluginDeps(t, loaded), &stderr, workspace)

	if len(activation.skillRoots) != 1 {
		t.Fatalf("expected one plugin skill root, got %#v", activation.skillRoots)
	}
	skillTool, ok := registry.Get("skill")
	if !ok {
		t.Fatal("skill tool missing after activation")
	}
	// The re-registered skill tool resolves the plugin skill by name.
	res := skillTool.Run(context.Background(), map[string]any{"name": "demo-skill"})
	if res.Status != tools.StatusOK {
		t.Fatalf("plugin skill not resolvable via the agent skill tool: %q (%s)", res.Status, res.Output)
	}
}

func TestActivatePluginsAlwaysRegistersMultiRootSkillTool(t *testing.T) {
	// Even with zero plugins / zero plugin skill roots, activation must overlay
	// the multi-root skill tool so ~/.agents/skills is loadable on bare runs.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	agents := filepath.Join(home, ".agents", "skills")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(agents, "agents-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: agents-skill\n---\nagents body"), 0o644); err != nil {
		t.Fatal(err)
	}

	registry := tools.NewRegistry()
	registry.Register(tools.NewSkillTool(t.TempDir())) // core single-dir tool
	var stderr bytes.Buffer
	workspace := t.TempDir()
	activation := activatePlugins(workspace, registry, fakePluginDeps(t, nil), &stderr, workspace)
	if len(activation.skillRoots) != 0 {
		t.Fatalf("expected no plugin skill roots, got %#v", activation.skillRoots)
	}
	skillTool, ok := registry.Get("skill")
	if !ok {
		t.Fatal("skill tool missing after activation")
	}
	res := skillTool.Run(context.Background(), map[string]any{"name": "agents-skill"})
	if res.Status != tools.StatusOK || !bytes.Contains([]byte(res.Output), []byte("agents body")) {
		t.Fatalf("agents skill must load with zero plugin roots: %q (%s)", res.Status, res.Output)
	}
}

func TestActivatePluginsSurfacesLoadDiagnostics(t *testing.T) {
	// Load succeeds (no top-level error) but reports a per-plugin diagnostic for a
	// plugin it had to skip. activatePlugins must forward that diagnostic to stderr
	// rather than dropping it, so a skipped plugin is never silent.
	deps := appDeps{
		getwd: func() (string, error) { return t.TempDir(), nil },
		loadPlugins: func(plugins.LoadOptions) (plugins.LoadResult, error) {
			return plugins.LoadResult{
				Diagnostics: []plugins.Diagnostic{{
					Kind:    plugins.DiagnosticSchema,
					Message: "schemaVersion: Expected schemaVersion 1.",
				}},
			}, nil
		},
		skillsDir: func() string { return t.TempDir() },
	}
	registry := tools.NewRegistry()
	var stderr bytes.Buffer
	workspace := t.TempDir()
	activatePlugins(workspace, registry, deps, &stderr, workspace)

	if stderr.Len() == 0 {
		t.Fatal("a load diagnostic must be surfaced on stderr")
	}
	if !bytes.Contains(stderr.Bytes(), []byte("Expected schemaVersion 1")) {
		t.Fatalf("stderr should carry the diagnostic message, got %q", stderr.String())
	}
}

func TestActivatePluginsFailsOpenOnLoadError(t *testing.T) {
	deps := appDeps{
		loadPlugins: func(plugins.LoadOptions) (plugins.LoadResult, error) {
			return plugins.LoadResult{}, errors.New("boom")
		},
		skillsDir: func() string { return t.TempDir() },
	}
	registry := tools.NewRegistry()
	var stderr bytes.Buffer
	workspace := t.TempDir()
	activation := activatePlugins(workspace, registry, deps, &stderr, workspace)

	if len(activation.hooks) != 0 || len(activation.skillRoots) != 0 {
		t.Fatalf("a load error must yield an inert activation, got %#v", activation)
	}
	if stderr.Len() == 0 {
		t.Fatalf("a load error should be surfaced as a warning on stderr")
	}
}

func TestActivatePluginsWarnsOnMalformedPluginButKeepsGood(t *testing.T) {
	good := plugins.LoadedPlugin{
		ID:        "zero.good",
		Name:      "Good",
		Enabled:   true,
		Source:    plugins.SourceProject,
		PluginDir: t.TempDir(),
		Tools:     []plugins.ToolExtension{{Name: "good_tool", Command: "true", Permission: plugins.PermissionPrompt}},
	}
	bad := plugins.LoadedPlugin{
		ID:        "zero.bad",
		Name:      "Bad",
		Enabled:   true,
		Source:    plugins.SourceProject,
		PluginDir: t.TempDir(),
		Tools:     []plugins.ToolExtension{{Name: "bad_tool", Command: "", Permission: plugins.PermissionPrompt}},
	}

	registry := tools.NewRegistry()
	var stderr bytes.Buffer
	workspace := t.TempDir()
	activatePlugins(workspace, registry, fakePluginDeps(t, []plugins.LoadedPlugin{bad, good}), &stderr, workspace)

	if _, ok := registry.Get("good_tool"); !ok {
		t.Fatal("the good plugin tool must still register despite a malformed sibling")
	}
	if _, ok := registry.Get("bad_tool"); ok {
		t.Fatal("the malformed plugin tool must not register")
	}
	if stderr.Len() == 0 {
		t.Fatal("a malformed plugin should produce a warning on stderr")
	}
}

func TestNewHookDispatcherWithExtraFoldsPluginHooks(t *testing.T) {
	// Keep the hook audit store inside a temp dir rather than the user's real data
	// directory (the audit path is derived from XDG_DATA_HOME).
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	workspace := t.TempDir()
	extra := []hooks.Definition{{
		ID:      "zero.demo.pre",
		Event:   hooks.EventBeforeTool,
		Command: "guard.sh",
		Enabled: true,
	}}

	dispatcher, _ := newHookDispatcherWithExtra(workspace, extra, workspace)
	if dispatcher == nil {
		t.Fatal("dispatcher should never be nil for a clean workspace")
	}

	// Drive a beforeTool dispatch: the plugin hook runs, proving it is part of the
	// active hook set (not merely returned as data).
	outcome := dispatcher.Dispatch(context.Background(), hooks.DispatchInput{
		Event:    hooks.EventBeforeTool,
		ToolName: "bash",
	})
	// With no real guard.sh on PATH the command fails to launch, which for a
	// beforeTool hook is advisory (fails OPEN), so the call is recorded as having
	// run exactly one hook.
	if outcome.Ran != 1 {
		t.Fatalf("expected the plugin beforeTool hook to run, Ran=%d", outcome.Ran)
	}
}

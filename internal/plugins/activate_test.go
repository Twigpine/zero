package plugins

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/hooks"
	"github.com/Gitlawb/zero/internal/skills"
	"github.com/Gitlawb/zero/internal/tools"
)

type pluginExecutionPreparer struct {
	request execution.Request
}

func (preparer *pluginExecutionPreparer) PrepareExecution(_ context.Context, request execution.Request) (execution.PreparedCommand, error) {
	preparer.request = request
	return execution.PreparedCommand{Command: exec.Command(request.Command.Name, request.Command.Args...)}, nil
}

// fakeToolRunner records the invocation and returns a canned result so tool
// activation can be exercised without spawning a real process.
type fakeToolRunner struct {
	calls []pluginCommand
	out   commandOutput
}

func (runner *fakeToolRunner) run(_ context.Context, command pluginCommand) commandOutput {
	runner.calls = append(runner.calls, command)
	return runner.out
}

// fakeRegisteredTool is a minimal tools.Tool used to pre-occupy a name in the
// registry so activation's collision handling can be exercised.
type fakeRegisteredTool struct {
	name string
}

func (tool *fakeRegisteredTool) Name() string             { return tool.name }
func (tool *fakeRegisteredTool) Description() string      { return "pre-registered" }
func (tool *fakeRegisteredTool) Parameters() tools.Schema { return tools.Schema{Type: "object"} }
func (tool *fakeRegisteredTool) Safety() tools.Safety     { return tools.Safety{} }
func (tool *fakeRegisteredTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK}
}

func toolPlugin(pluginDir string, tool ToolExtension) LoadedPlugin {
	return LoadedPlugin{
		SchemaVersion: 1,
		ID:            "zero.demo",
		Name:          "Zero Demo",
		Version:       "0.1.0",
		Enabled:       true,
		Source:        SourceProject,
		PluginDir:     pluginDir,
		Tools:         []ToolExtension{tool},
	}
}

func TestActivateRegistersToolThatInvokesCommand(t *testing.T) {
	pluginDir := t.TempDir()
	registry := tools.NewRegistry()
	runner := &fakeToolRunner{out: commandOutput{Stdout: "lookup result", ExitCode: 0}}

	plugin := toolPlugin(pluginDir, ToolExtension{
		Name:        "lookup",
		Description: "Lookup docs",
		Command:     "${AGENT_PLUGIN_ROOT}/bin/lookup",
		Args:        []string{"--root", "${AGENT_PLUGIN_ROOT}", "docs"},
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}},
		Permission:  PermissionPrompt,
	})

	result := Activate(registry, []LoadedPlugin{plugin}, ActivateOptions{runTool: runner.run})

	tool, ok := registry.Get("lookup")
	if !ok {
		t.Fatalf("expected tool %q registered, registry has %d tools", "lookup", len(registry.All()))
	}
	if tool.Description() != "Lookup docs" {
		t.Fatalf("description = %q", tool.Description())
	}
	if tool.Parameters().Type != "object" {
		t.Fatalf("schema type = %q, want object", tool.Parameters().Type)
	}
	if _, ok := tool.Parameters().Properties["query"]; !ok {
		t.Fatalf("schema properties missing query: %#v", tool.Parameters().Properties)
	}

	// Running through the registry with permission granted invokes the plugin
	// command and maps stdout/exit onto the tool Result.
	runResult := registry.RunWithOptions(context.Background(), "lookup", map[string]any{"query": "hello"}, tools.RunOptions{PermissionGranted: true})
	if runResult.Status != tools.StatusOK {
		t.Fatalf("status = %q (%s)", runResult.Status, runResult.Output)
	}
	if !strings.Contains(runResult.Output, "lookup result") {
		t.Fatalf("output = %q, want plugin stdout", runResult.Output)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("expected exactly one command invocation, got %d", len(runner.calls))
	}
	call := runner.calls[0]
	// ${AGENT_PLUGIN_ROOT} is expanded in both command and args.
	if call.Command != filepath.Join(pluginDir, "bin", "lookup") {
		t.Fatalf("command = %q, want expanded plugin root", call.Command)
	}
	if len(call.Args) != 3 || call.Args[1] != pluginDir {
		t.Fatalf("args = %#v, want ${AGENT_PLUGIN_ROOT} expanded", call.Args)
	}
	// The plugin dir is also exported as an env var the command can read.
	if !envContains(call.Env, "AGENT_PLUGIN_ROOT="+pluginDir) {
		t.Fatalf("env missing AGENT_PLUGIN_ROOT=%s: %#v", pluginDir, call.Env)
	}
	if result.Tools[0].ToolName != "lookup" || result.Tools[0].PluginID != "zero.demo" {
		t.Fatalf("provenance = %#v", result.Tools)
	}
}

func TestActivateToolMapsNonZeroExitToError(t *testing.T) {
	registry := tools.NewRegistry()
	runner := &fakeToolRunner{out: commandOutput{Stderr: "boom", ExitCode: 2}}
	plugin := toolPlugin(t.TempDir(), ToolExtension{Name: "lookup", Command: "lookup", Permission: PermissionPrompt})

	Activate(registry, []LoadedPlugin{plugin}, ActivateOptions{runTool: runner.run})

	res := registry.RunWithOptions(context.Background(), "lookup", map[string]any{}, tools.RunOptions{PermissionGranted: true})
	if res.Status != tools.StatusError {
		t.Fatalf("status = %q, want error on non-zero exit", res.Status)
	}
	if !strings.Contains(res.Output, "boom") {
		t.Fatalf("output = %q, want stderr surfaced", res.Output)
	}
	if res.Meta["exit_code"] != "2" {
		t.Fatalf("meta exit_code = %q, want 2", res.Meta["exit_code"])
	}
}

func TestActivatePromptPermissionMapsToPromptSafety(t *testing.T) {
	registry := tools.NewRegistry()
	plugin := toolPlugin(t.TempDir(), ToolExtension{Name: "edit", Command: "edit", Permission: PermissionPrompt})

	Activate(registry, []LoadedPlugin{plugin}, ActivateOptions{runTool: (&fakeToolRunner{}).run})

	tool, ok := registry.Get("edit")
	if !ok {
		t.Fatal("tool edit not registered")
	}
	safety := tool.Safety()
	if safety.Permission != tools.PermissionPrompt {
		t.Fatalf("permission = %q, want prompt", safety.Permission)
	}
	// A prompt-gated plugin tool is not auto-runnable: without granted permission
	// the registry refuses to execute it.
	res := registry.Run(context.Background(), "edit", map[string]any{})
	if res.Status != tools.StatusError || !strings.Contains(res.Output, "Permission required") {
		t.Fatalf("expected permission-required error, got %q (%s)", res.Status, res.Output)
	}
}

func TestActivateAllowPermissionMapsToAllowSafety(t *testing.T) {
	registry := tools.NewRegistry()
	runner := &fakeToolRunner{out: commandOutput{Stdout: "ok"}}
	// permission=allow only survives parsing when AllowManifestToolAutoApproval is
	// set, so an allow-permission tool here represents an explicitly auto-approved
	// plugin tool and must map to allow-level Safety (runnable without a prompt).
	plugin := toolPlugin(t.TempDir(), ToolExtension{Name: "report", Command: "report", Permission: PermissionAllow})

	Activate(registry, []LoadedPlugin{plugin}, ActivateOptions{runTool: runner.run})

	tool, _ := registry.Get("report")
	if tool.Safety().Permission != tools.PermissionAllow {
		t.Fatalf("permission = %q, want allow", tool.Safety().Permission)
	}
	res := registry.Run(context.Background(), "report", map[string]any{})
	if res.Status != tools.StatusOK {
		t.Fatalf("allow tool should run without granted permission, got %q (%s)", res.Status, res.Output)
	}
}

func TestActivateDenyPermissionRegistersDenySafety(t *testing.T) {
	registry := tools.NewRegistry()
	runner := &fakeToolRunner{out: commandOutput{Stdout: "should not run"}}
	plugin := toolPlugin(t.TempDir(), ToolExtension{Name: "danger", Command: "danger", Permission: PermissionDeny})

	Activate(registry, []LoadedPlugin{plugin}, ActivateOptions{runTool: runner.run})

	tool, ok := registry.Get("danger")
	if !ok {
		t.Fatal("deny tool not registered")
	}
	if tool.Safety().Permission != tools.PermissionDeny {
		t.Fatalf("permission = %q, want deny", tool.Safety().Permission)
	}
	res := registry.RunWithOptions(context.Background(), "danger", map[string]any{}, tools.RunOptions{PermissionGranted: true})
	if res.Status != tools.StatusError {
		t.Fatalf("deny tool must never execute, got %q (%s)", res.Status, res.Output)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("deny tool command must not be invoked, got %d calls", len(runner.calls))
	}
}

func TestActivateSkipsToolWhoseNameCollidesWithRegisteredTool(t *testing.T) {
	registry := tools.NewRegistry()
	// A tool already occupies the "bash" name (mimicking a core tool). The plugin
	// tool must not silently overwrite it; the existing registration must survive.
	existing := &fakeRegisteredTool{name: "bash"}
	registry.Register(existing)

	runner := &fakeToolRunner{out: commandOutput{Stdout: "should not replace bash"}}
	plugin := toolPlugin(t.TempDir(), ToolExtension{Name: "bash", Command: "evil", Permission: PermissionPrompt})

	result := Activate(registry, []LoadedPlugin{plugin}, ActivateOptions{runTool: runner.run})

	got, ok := registry.Get("bash")
	if !ok {
		t.Fatal("the pre-existing bash tool must remain registered")
	}
	if got != tools.Tool(existing) {
		t.Fatalf("plugin tool overwrote the existing %q registration", "bash")
	}
	if len(result.Tools) != 0 {
		t.Fatalf("a skipped colliding tool must not be recorded as provenance: %#v", result.Tools)
	}
	if !containsSubstring(result.Warnings, "bash") {
		t.Fatalf("expected a warning naming the skipped colliding tool, got %#v", result.Warnings)
	}
}

func TestActivateBuildsHookDefinitionsForMappedEvent(t *testing.T) {
	registry := tools.NewRegistry()
	plugin := LoadedPlugin{
		ID:        "zero.guard",
		Name:      "Guard",
		Enabled:   true,
		Source:    SourceProject,
		PluginDir: t.TempDir(),
		Hooks: []HookExtension{
			{Name: "pre", Event: HookBeforeTool, Command: "${AGENT_PLUGIN_ROOT}/guard.sh", Args: []string{"--strict"}},
			{Name: "post", Event: HookAfterTool, Command: "log.sh"},
		},
	}

	result := Activate(registry, []LoadedPlugin{plugin}, ActivateOptions{})

	if len(result.Hooks) != 2 {
		t.Fatalf("expected 2 hook definitions, got %d: %#v", len(result.Hooks), result.Hooks)
	}
	// Plugin HookEvent maps onto the hooks package Event, and the command's
	// ${AGENT_PLUGIN_ROOT} is expanded against the plugin dir.
	var before hooks.Definition
	found := false
	for _, def := range result.Hooks {
		if def.Event == hooks.EventBeforeTool {
			before = def
			found = true
		}
	}
	if !found {
		t.Fatalf("no beforeTool hook in %#v", result.Hooks)
	}
	if before.Command != filepath.Join(plugin.PluginDir, "guard.sh") {
		t.Fatalf("hook command = %q, want expanded plugin root", before.Command)
	}
	if len(before.Args) != 1 || before.Args[0] != "--strict" {
		t.Fatalf("hook args = %#v", before.Args)
	}
	if !before.Enabled {
		t.Fatalf("plugin hook should be enabled")
	}

	// The dispatcher built from these definitions selects the plugin hook for the
	// mapped event, proving the hook is live (not merely parsed).
	cfg := hooks.Config{Enabled: true, Hooks: result.Hooks}
	selected := hooks.Select(cfg, hooks.SelectInput{Event: hooks.EventBeforeTool, ToolName: "bash"})
	if len(selected) != 1 {
		t.Fatalf("expected the plugin beforeTool hook selected, got %d", len(selected))
	}
}

func TestActivateExposesSkillDirInLoaderRoots(t *testing.T) {
	pluginDir := t.TempDir()
	// A plugin skill is a directory containing SKILL.md, nested under the plugin's
	// skills/ directory: skills/ts-review/SKILL.md.
	skillDir := filepath.Join(pluginDir, "skills", "ts-review")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillMd := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillMd, []byte("---\nname: ts-review\ndescription: review TS\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}

	registry := tools.NewRegistry()
	plugin := LoadedPlugin{
		ID:        "zero.review",
		Name:      "Review",
		Enabled:   true,
		Source:    SourceProject,
		PluginDir: pluginDir,
		Skills:    []PathExtension{{Name: "ts-review", Path: skillMd}},
	}

	result := Activate(registry, []LoadedPlugin{plugin}, ActivateOptions{})

	if len(result.SkillRoots) != 1 {
		t.Fatalf("expected 1 skill root, got %#v", result.SkillRoots)
	}
	// The root is the directory that internal/skills.Load scans (parent of the
	// per-skill directory), so the skill becomes discoverable through the loader.
	wantRoot := filepath.Join(pluginDir, "skills")
	if result.SkillRoots[0] != wantRoot {
		t.Fatalf("skill root = %q, want %q", result.SkillRoots[0], wantRoot)
	}
	loaded, err := skills.Load(result.SkillRoots[0])
	if err != nil {
		t.Fatalf("skills.Load: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Name != "ts-review" {
		t.Fatalf("plugin skill not discoverable via loader: %#v", loaded)
	}
}

func TestActivateResolvesManifestRelativeSkillRoot(t *testing.T) {
	// A LoadedPlugin handed to Activate directly may carry a manifest-relative skill
	// path (e.g. "skills/ts-review/SKILL.md") or one using the ${AGENT_PLUGIN_ROOT}
	// placeholder. Either must resolve against the plugin dir so SkillRoots is the
	// real <pluginDir>/skills directory, not a bare "skills".
	cases := map[string]string{
		"relative":    filepath.Join("skills", "ts-review", "SKILL.md"),
		"placeholder": pluginRootPlaceholder + "/skills/ts-review/SKILL.md",
	}
	for name, skillPath := range cases {
		t.Run(name, func(t *testing.T) {
			pluginDir := t.TempDir()
			plugin := LoadedPlugin{
				ID:        "zero.review",
				Name:      "Review",
				Enabled:   true,
				Source:    SourceProject,
				PluginDir: pluginDir,
				Skills:    []PathExtension{{Name: "ts-review", Path: skillPath}},
			}

			result := Activate(tools.NewRegistry(), []LoadedPlugin{plugin}, ActivateOptions{})

			if len(result.SkillRoots) != 1 {
				t.Fatalf("expected 1 skill root, got %#v", result.SkillRoots)
			}
			wantRoot := filepath.Join(pluginDir, "skills")
			if result.SkillRoots[0] != wantRoot {
				t.Fatalf("skill root = %q, want %q (resolved against plugin dir)", result.SkillRoots[0], wantRoot)
			}
		})
	}
}

func TestNewSkillToolSurfacesPluginSkillInList(t *testing.T) {
	isolateAgentsHome(t)
	defaultDir := t.TempDir()
	writeTestSkill(t, defaultDir, "core-skill", "core body")
	pluginRoot := t.TempDir()
	writeTestSkill(t, pluginRoot, "plugin-skill", "plugin body")

	tool := NewSkillTool(defaultDir, []string{pluginRoot})
	if tool.Name() != "skill" {
		t.Fatalf("plugin skill tool must reuse the skill tool name, got %q", tool.Name())
	}
	if tool.Safety().Permission != tools.PermissionAllow || tool.Safety().SideEffect != tools.SideEffectRead {
		t.Fatalf("skill tool must stay read-only/allow, got %#v", tool.Safety())
	}

	// Loading the plugin skill by name returns its body.
	got := tool.Run(context.Background(), map[string]any{"name": "plugin-skill"})
	if got.Status != tools.StatusOK || !strings.Contains(got.Output, "plugin body") {
		t.Fatalf("expected plugin skill body, got %q (%s)", got.Status, got.Output)
	}
	// A core skill is still reachable through the same tool.
	core := tool.Run(context.Background(), map[string]any{"name": "core-skill"})
	if core.Status != tools.StatusOK || !strings.Contains(core.Output, "core body") {
		t.Fatalf("expected core skill body, got %q (%s)", core.Status, core.Output)
	}
	// An unknown name lists the available skills (both core and plugin).
	unknown := tool.Run(context.Background(), map[string]any{"name": "nope"})
	if unknown.Status != tools.StatusError {
		t.Fatalf("unknown skill must error, got %q", unknown.Status)
	}
	if !strings.Contains(unknown.Output, "core-skill") || !strings.Contains(unknown.Output, "plugin-skill") {
		t.Fatalf("unknown-skill error should list both skills, got %q", unknown.Output)
	}
}

func TestMergeSkillRootsListsPluginSkillInAgentList(t *testing.T) {
	isolateAgentsHome(t)
	defaultDir := t.TempDir()
	writeTestSkill(t, defaultDir, "core-skill", "core")

	pluginRoot := t.TempDir()
	writeTestSkill(t, pluginRoot, "plugin-skill", "from plugin")

	listed, dups := MergedSkills(defaultDir, []string{pluginRoot})
	if len(dups) != 0 {
		t.Fatalf("unexpected duplicates: %#v", dups)
	}
	names := map[string]bool{}
	for _, skill := range listed {
		names[skill.Name] = true
	}
	if !names["core-skill"] || !names["plugin-skill"] {
		t.Fatalf("agent skill list missing entries: %#v", listed)
	}
}

func TestMergedSkillsRecordsDuplicatesWithoutCrashing(t *testing.T) {
	// Isolate AgentsDir so a host ~/.agents/skills cannot add extra collisions.
	isolateAgentsHome(t)

	defaultDir := t.TempDir()
	writeTestSkill(t, defaultDir, "shared", "default copy")

	pluginRoot := t.TempDir()
	writeTestSkill(t, pluginRoot, "shared", "plugin copy")

	listed, dups := MergedSkills(defaultDir, []string{pluginRoot})
	// Name clash must be recorded, not crash; exactly one survivor named "shared".
	count := 0
	for _, skill := range listed {
		if skill.Name == "shared" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected one surviving 'shared' skill, got %d", count)
	}
	if len(dups) != 1 || dups[0].Name != "shared" {
		t.Fatalf("expected one recorded duplicate for 'shared', got %#v", dups)
	}
}

// isolateAgentsHome points HOME/USERPROFILE at an empty temp home so AgentsDir
// does not pick up the developer's real ~/.agents/skills during unit tests.
func isolateAgentsHome(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

func TestMergedSkillsIncludesAgentsWithoutPlugins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	agents := filepath.Join(home, ".agents", "skills")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestSkill(t, agents, "agents-skill", "from agents")

	defaultDir := t.TempDir()
	listed, dups := MergedSkills(defaultDir, nil)
	if len(dups) != 0 {
		t.Fatalf("unexpected dups: %#v", dups)
	}
	if len(listed) != 1 || listed[0].Name != "agents-skill" {
		t.Fatalf("agents skill should surface with no plugins, got %#v", listed)
	}
}

func TestMergedSkillsPriorityPrimaryAgentsPlugins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	agents := filepath.Join(home, ".agents", "skills")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}

	primary := t.TempDir()
	pluginRoot := t.TempDir()
	writeTestSkill(t, primary, "shared", "primary body")
	writeTestSkill(t, agents, "shared", "agents body")
	writeTestSkill(t, agents, "agents-plugin", "agents body")
	writeTestSkill(t, pluginRoot, "shared", "plugin body")
	writeTestSkill(t, pluginRoot, "agents-plugin", "plugin body")
	writeTestSkill(t, pluginRoot, "plugin-only", "plugin only")

	listed, dups := MergedSkills(primary, []string{pluginRoot})
	byName := map[string]string{}
	for _, skill := range listed {
		// Content is stripped by MergedSkills; use path origin instead.
		byName[skill.Name] = skill.Path
	}
	if !strings.Contains(byName["shared"], primary) {
		t.Fatalf("primary should win shared, path=%q", byName["shared"])
	}
	if !strings.Contains(byName["agents-plugin"], agents) {
		t.Fatalf("agents should win over plugin for agents-plugin, path=%q", byName["agents-plugin"])
	}
	if !strings.Contains(byName["plugin-only"], pluginRoot) {
		t.Fatalf("plugin-only missing, path=%q", byName["plugin-only"])
	}
	if len(dups) < 2 {
		t.Fatalf("expected cross-layer dups, got %#v", dups)
	}
}

func TestNewSkillToolLoadsAgentsSkillWithoutPluginRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	agents := filepath.Join(home, ".agents", "skills")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestSkill(t, agents, "agents-skill", "agents body")

	tool := NewSkillTool(t.TempDir(), nil)
	got := tool.Run(context.Background(), map[string]any{"name": "agents-skill"})
	if got.Status != tools.StatusOK || !strings.Contains(got.Output, "agents body") {
		t.Fatalf("expected agents skill body with zero plugin roots, got %q (%s)", got.Status, got.Output)
	}
}

func TestActivateSkipsMalformedPluginAndContinues(t *testing.T) {
	registry := tools.NewRegistry()
	good := toolPlugin(t.TempDir(), ToolExtension{Name: "good", Command: "good", Permission: PermissionPrompt})
	// A tool extension with an empty command is malformed: it can never be invoked,
	// so activation must skip it with a warning rather than register a broken tool.
	bad := toolPlugin(t.TempDir(), ToolExtension{Name: "bad", Command: "   ", Permission: PermissionPrompt})
	bad.ID = "zero.bad"

	result := Activate(registry, []LoadedPlugin{bad, good}, ActivateOptions{runTool: (&fakeToolRunner{}).run})

	if _, ok := registry.Get("good"); !ok {
		t.Fatal("good plugin tool must still activate after a malformed plugin")
	}
	if _, ok := registry.Get("bad"); ok {
		t.Fatal("malformed plugin tool must not be registered")
	}
	if len(result.Warnings) == 0 {
		t.Fatalf("expected a warning for the malformed plugin")
	}
	if !containsSubstring(result.Warnings, "bad") && !containsSubstring(result.Warnings, "zero.bad") {
		t.Fatalf("warning should identify the offending plugin/tool: %#v", result.Warnings)
	}
}

func TestActivateSkipsDisabledPlugin(t *testing.T) {
	registry := tools.NewRegistry()
	plugin := toolPlugin(t.TempDir(), ToolExtension{Name: "lookup", Command: "lookup", Permission: PermissionPrompt})
	plugin.Enabled = false

	result := Activate(registry, []LoadedPlugin{plugin}, ActivateOptions{runTool: (&fakeToolRunner{}).run})

	if _, ok := registry.Get("lookup"); ok {
		t.Fatal("a disabled plugin's tools must not be registered")
	}
	if len(result.Tools) != 0 || len(result.Hooks) != 0 {
		t.Fatalf("disabled plugin should contribute nothing, got %#v", result)
	}
}

func TestPluginCommandUsesTypedPluginExecutionOrigin(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a subprocess")
	}
	preparer := &pluginExecutionPreparer{}
	command := pluginCommand{Command: os.Args[0], Args: []string{"-test.run=^$"}, Cwd: t.TempDir()}
	// This checks origin wiring, not deadline enforcement. Allow startup and
	// the race runtime's one-second exit delay now that even a plain exec.Command
	// returned by the preparer is subject to the captured execution deadline.
	output := execPluginCommandWithExecution(context.Background(), execution.NewRunner(preparer), command, 15*time.Second)
	if output.Err != nil || output.ExitCode != 0 {
		t.Fatalf("plugin execution output = %#v", output)
	}
	if preparer.request.Origin != execution.OriginPlugin || preparer.request.Mode != execution.ModeCaptured {
		t.Fatalf("execution request = %#v", preparer.request)
	}
}

func TestActivateIsDeterministic(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	plugins := []LoadedPlugin{
		{ID: "zero.b", Name: "B", Enabled: true, PluginDir: dirB, Hooks: []HookExtension{{Name: "hb", Event: HookSessionStart, Command: "b.sh"}}},
		{ID: "zero.a", Name: "A", Enabled: true, PluginDir: dirA, Hooks: []HookExtension{{Name: "ha", Event: HookSessionStart, Command: "a.sh"}}},
	}

	first := Activate(tools.NewRegistry(), plugins, ActivateOptions{})
	second := Activate(tools.NewRegistry(), plugins, ActivateOptions{})
	if len(first.Hooks) != 2 || len(second.Hooks) != 2 {
		t.Fatalf("expected 2 hooks each, got %d/%d", len(first.Hooks), len(second.Hooks))
	}
	for i := range first.Hooks {
		if first.Hooks[i].ID != second.Hooks[i].ID {
			t.Fatalf("hook order not deterministic at %d: %q vs %q", i, first.Hooks[i].ID, second.Hooks[i].ID)
		}
	}
	// Deterministic order is by plugin ID then extension name: zero.a before zero.b.
	if first.Hooks[0].ID != "zero.a.ha" {
		t.Fatalf("first hook id = %q, want zero.a.ha (deterministic by plugin then name)", first.Hooks[0].ID)
	}
}

func TestActivateHookEventMappingCoversAllEvents(t *testing.T) {
	cases := map[HookEvent]hooks.Event{
		HookBeforeTool:   hooks.EventBeforeTool,
		HookAfterTool:    hooks.EventAfterTool,
		HookSessionStart: hooks.EventSessionStart,
		HookSessionEnd:   hooks.EventSessionEnd,
	}
	for pluginEvent, want := range cases {
		got, ok := mapHookEvent(pluginEvent)
		if !ok || got != want {
			t.Fatalf("mapHookEvent(%q) = %q,%v want %q", pluginEvent, got, ok, want)
		}
	}
	if _, ok := mapHookEvent(HookEvent("bogus")); ok {
		t.Fatal("unknown hook event must not map")
	}
}

func writeTestSkill(t *testing.T, dir string, name string, body string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func envContains(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

func TestExpandPluginRootPathRelative(t *testing.T) {
	pluginDir := "/etc/zero/plugin"
	cases := []struct {
		input string
		want  string
	}{
		{"./bin/tool.sh", filepath.Join(pluginDir, "./bin/tool.sh")},
		{"../outside/tool", filepath.Join(pluginDir, "../outside/tool")},
		{"tool.sh", "tool.sh"},             // bare binary name
		{"/usr/bin/tool", "/usr/bin/tool"}, // absolute Unix
		{`C:\bin\tool`, `C:\bin\tool`},     // absolute Windows
		{"${AGENT_PLUGIN_ROOT}/bin/tool.sh", filepath.FromSlash(pluginDir + "/bin/tool.sh")},
	}
	for _, tc := range cases {
		got := expandPluginRootPath(tc.input, pluginDir)
		if got != tc.want {
			t.Errorf("expandPluginRootPath(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

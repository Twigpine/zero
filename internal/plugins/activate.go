package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/execution"
	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/secrets"
	"github.com/Twigpine/zero/internal/skills"
	"github.com/Twigpine/zero/internal/tools"
)

// pluginRootPlaceholder is the manifest path placeholder a plugin may use in a
// tool/hook command (or its args) to refer to its own install directory. It is
// expanded to the plugin's PluginDir at activation time and also exported into
// the command's environment as AGENT_PLUGIN_ROOT.
const pluginRootPlaceholder = "${AGENT_PLUGIN_ROOT}"

// pluginRootEnvVar is the environment variable carrying the plugin's root, set on
// every plugin tool/hook command so a script can resolve sibling files even when
// it does not use the ${AGENT_PLUGIN_ROOT} placeholder in its argv.
const pluginRootEnvVar = "AGENT_PLUGIN_ROOT"

// defaultPluginToolTimeout bounds a single plugin tool command so a hung or slow
// plugin process cannot stall a tool call indefinitely (mirrors the hook
// dispatcher's per-command timeout).
const defaultPluginToolTimeout = 120 * time.Second

// pluginCommand is the fully-resolved invocation handed to the tool runner: the
// expanded command + args, the working directory, the JSON-encoded tool args fed
// on stdin, and the environment (process env plus AGENT_PLUGIN_ROOT).
type pluginCommand struct {
	Command string
	Args    []string
	Cwd     string
	Stdin   []byte
	Env     []string
}

// commandOutput is the captured result of running a pluginCommand. ExitCode is 0
// on success and -1 when the command could not be launched (Err set).
type commandOutput struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

// toolRunner executes a resolved plugin tool command. It is injectable so
// activation can be tested without spawning processes.
type toolRunner func(ctx context.Context, command pluginCommand) commandOutput

// ToolProvenance records which plugin a registered tool originated from, for
// `zero plugin list` and debugging.
type ToolProvenance struct {
	ToolName string `json:"toolName"`
	PluginID string `json:"pluginId"`
}

// HookProvenance records which plugin an activated hook originated from.
type HookProvenance struct {
	HookID   string `json:"hookId"`
	PluginID string `json:"pluginId"`
}

// SkillProvenance records which plugin contributed a skill search root.
type SkillProvenance struct {
	SkillName string `json:"skillName"`
	Root      string `json:"root"`
	PluginID  string `json:"pluginId"`
}

// ActivationResult reports what a single Activate call wired up. Tools are
// registered into the supplied registry directly; the remaining fields are
// returned for the caller (the agent bootstrap) to merge into the hook dispatcher
// and skills loader, and to surface provenance/warnings.
type ActivationResult struct {
	// Hooks are the live hook definitions built from plugin manifests, ready to be
	// merged into the dispatcher's hook set. Order is deterministic.
	Hooks []hooks.Definition
	// SkillRoots are directories internal/skills.Load can scan to discover plugin
	// skills, deduplicated and deterministically ordered.
	SkillRoots []string
	// Tools/HookProv/Skills carry provenance so a listing can attribute each live
	// extension to its originating plugin.
	Tools    []ToolProvenance
	HookProv []HookProvenance
	Skills   []SkillProvenance
	// Warnings collects per-plugin/per-extension issues that caused an extension to
	// be skipped. A single malformed plugin never aborts activation.
	Warnings []string
}

// ActivateOptions configures activation. runTool is injectable for tests; when
// nil it defaults to executing the plugin command as a subprocess.
type ActivateOptions struct {
	// Cwd is the working directory plugin tool commands run in. Empty falls back to
	// the plugin's own directory.
	Cwd string
	// Timeout bounds each plugin tool command. <= 0 uses defaultPluginToolTimeout.
	Timeout time.Duration
	// Env supplies the base environment for plugin tool commands. nil uses the
	// current process environment (os.Environ()).
	Env []string
	// Execution routes plugin tool subprocesses through the shared sandbox and
	// process execution seam. Production callers provide it before any tool can
	// be invoked; tests may continue to inject runTool directly.
	Execution *execution.Runner

	runTool toolRunner
}

// Activate turns the resolved plugin extensions into live registrations: it
// registers each plugin tool into registry, and returns the hook definitions and
// skill roots for the caller to wire into the dispatcher and skills loader.
//
// Activation is isolation-first: a malformed plugin or extension is skipped with
// a recorded warning rather than aborting the whole activation, mirroring how
// skills.Load skips a bad skill. Plugins are processed in a deterministic order
// (sorted by ID) so the resulting registrations are stable across runs.
func Activate(registry *tools.Registry, loaded []LoadedPlugin, options ActivateOptions) ActivationResult {
	result := ActivationResult{
		Hooks:      []hooks.Definition{},
		SkillRoots: []string{},
		Tools:      []ToolProvenance{},
		HookProv:   []HookProvenance{},
		Skills:     []SkillProvenance{},
		Warnings:   []string{},
	}

	runTool := options.runTool
	if runTool == nil {
		timeout := options.Timeout
		if timeout <= 0 {
			timeout = defaultPluginToolTimeout
		}
		if options.Execution != nil {
			runTool = func(ctx context.Context, command pluginCommand) commandOutput {
				return execPluginCommandWithExecution(ctx, options.Execution, command, timeout)
			}
		} else {
			runTool = func(ctx context.Context, command pluginCommand) commandOutput {
				return execPluginCommand(ctx, command, timeout)
			}
		}
	}

	// Process plugins in a deterministic order so the registry/hook/skill
	// registrations are stable regardless of discovery order.
	ordered := append([]LoadedPlugin{}, loaded...)
	sort.Slice(ordered, func(left int, right int) bool {
		return ordered[left].ID < ordered[right].ID
	})

	seenSkillRoot := map[string]bool{}
	for _, plugin := range ordered {
		if !plugin.Enabled {
			continue
		}

		if registry != nil {
			activateTools(registry, plugin, options, runTool, &result)
		}
		activateHooks(plugin, &result)
		activateSkills(plugin, seenSkillRoot, &result)
	}

	return result
}

// activateTools registers every well-formed tool extension of a plugin and
// records a warning for any it has to skip.
func activateTools(registry *tools.Registry, plugin LoadedPlugin, options ActivateOptions, runTool toolRunner, result *ActivationResult) {
	for _, ext := range plugin.Tools {
		if strings.TrimSpace(ext.Name) == "" {
			result.Warnings = append(result.Warnings, fmt.Sprintf("plugin %q: skipped a tool with no name", plugin.ID))
			continue
		}
		if strings.TrimSpace(ext.Command) == "" {
			result.Warnings = append(result.Warnings, fmt.Sprintf("plugin %q: skipped tool %q with no command", plugin.ID, ext.Name))
			continue
		}
		tool := newPluginTool(plugin, ext, options, runTool)
		// registry.Register is last-wins, so a plugin tool sharing a name with a core
		// tool (or another plugin's tool) would silently replace it and change its
		// safety semantics. Skip the colliding tool with a warning instead, keeping
		// the existing registration intact (isolation-first, like a malformed plugin).
		if _, exists := registry.Get(tool.Name()); exists {
			result.Warnings = append(result.Warnings, fmt.Sprintf("plugin %q: skipped tool %q because that name is already registered", plugin.ID, ext.Name))
			continue
		}
		registry.Register(tool)
		result.Tools = append(result.Tools, ToolProvenance{ToolName: ext.Name, PluginID: plugin.ID})
	}
}

// activateHooks builds a hooks.Definition for each well-formed hook extension and
// appends it (with provenance) to the result.
func activateHooks(plugin LoadedPlugin, result *ActivationResult) {
	for _, ext := range plugin.Hooks {
		if strings.TrimSpace(ext.Command) == "" {
			result.Warnings = append(result.Warnings, fmt.Sprintf("plugin %q: skipped hook %q with no command", plugin.ID, ext.Name))
			continue
		}
		event, ok := mapHookEvent(ext.Event)
		if !ok {
			result.Warnings = append(result.Warnings, fmt.Sprintf("plugin %q: skipped hook %q with unsupported event %q", plugin.ID, ext.Name, ext.Event))
			continue
		}
		id := hookID(plugin.ID, ext.Name)
		result.Hooks = append(result.Hooks, hooks.Definition{
			ID:          id,
			Name:        ext.Name,
			Description: ext.Description,
			Event:       event,
			Command:     expandPluginRootPath(ext.Command, plugin.PluginDir),
			Args:        expandPluginRootAll(ext.Args, plugin.PluginDir),
			Enabled:     true,
		})
		result.HookProv = append(result.HookProv, HookProvenance{HookID: id, PluginID: plugin.ID})
	}
}

// activateSkills records each plugin skill's search root (the directory
// internal/skills.Load scans) so the caller can merge it into the skills loader.
// Roots are deduplicated; the original discovery order within the plugin is kept.
func activateSkills(plugin LoadedPlugin, seenRoot map[string]bool, result *ActivationResult) {
	for _, ext := range plugin.Skills {
		// Resolve the manifest path against the plugin dir first: expand any
		// ${AGENT_PLUGIN_ROOT} placeholder, then anchor a still-relative path (e.g.
		// "skills/foo/SKILL.md") under PluginDir so the derived root is the real
		// filesystem directory skills.Load can scan rather than a bare "skills".
		// (Paths loaded via ParseManifest are already absolute; this only hardens
		// the case where a caller hands Activate a relative path directly.)
		path := expandPluginRoot(ext.Path, plugin.PluginDir)
		if trimmed := strings.TrimSpace(path); trimmed != "" && !filepath.IsAbs(trimmed) {
			path = filepath.Join(plugin.PluginDir, trimmed)
		}
		root := skillSearchRoot(path)
		if root == "" {
			result.Warnings = append(result.Warnings, fmt.Sprintf("plugin %q: skipped skill %q with no path", plugin.ID, ext.Name))
			continue
		}
		if !seenRoot[root] {
			seenRoot[root] = true
			result.SkillRoots = append(result.SkillRoots, root)
		}
		result.Skills = append(result.Skills, SkillProvenance{SkillName: ext.Name, Root: root, PluginID: plugin.ID})
	}
}

// skillSearchRoot maps a plugin skill's SKILL.md path to the directory
// internal/skills.Load scans for `*/SKILL.md`: the parent of the per-skill
// directory (i.e. the grandparent of the SKILL.md file). A path that already
// names a directory (no SKILL.md leaf) is treated as the per-skill directory and
// its parent is returned.
func skillSearchRoot(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	skillDir := path
	if strings.EqualFold(filepath.Base(path), skillFileName) {
		skillDir = filepath.Dir(path)
	}
	return filepath.Dir(skillDir)
}

// skillFileName mirrors internal/skills' SKILL.md filename so skillSearchRoot can
// recognize a manifest path that points at the file rather than the directory.
const skillFileName = "SKILL.md"

// MergedSkills loads the default skills directory, the shared ~/.agents/skills
// root when present, plus the supplied plugin skill roots and returns one
// merged, name-deduplicated list (Content stripped, like skills.ListFromRoots) alongside
// the duplicate-name collisions across all roots. Earlier roots win a name
// clash, matching skills.Load's first-wins rule; the default dir is always
// considered first so a user skill shadows agents and plugins. A bad root simply
// yields no skills rather than failing the merge.
func MergedSkills(defaultDir string, pluginRoots []string) ([]skills.Skill, []skills.DuplicateName) {
	return mergeSkills(defaultDir, pluginRoots, false)
}

// mergeSkills merges the primary skills dir, optional ~/.agents/skills, and
// plugin roots into one sorted, name-deduplicated list via skills.LoadFromRoots /
// ListFromRoots. keepContent retains each skill's body versus stripping it.
// Roots are considered in order with the default dir first, so an earlier root
// wins a name clash; collisions are recorded rather than crashing.
func mergeSkills(defaultDir string, pluginRoots []string, keepContent bool) ([]skills.Skill, []skills.DuplicateName) {
	// Keep defaultDir injectable for tests rather than always calling DefaultDir.
	roots := skills.GlobalRoots(defaultDir)
	// GlobalRoots already includes AgentsDir; append plugin roots only.
	for _, root := range pluginRoots {
		if root = strings.TrimSpace(root); root != "" {
			roots = append(roots, root)
		}
	}
	if keepContent {
		merged, dups, _ := skills.LoadFromRoots(roots)
		return merged, dups
	}
	merged, dups, _ := skills.ListFromRoots(roots)
	return merged, dups
}

// skillTool is a drop-in replacement for the core skill tool that resolves a
// named skill across the default skills directory, the shared ~/.agents/skills
// root, and plugin skill roots, so agents- and plugin-declared skills surface in
// the agent's skill list. It keeps the core tool's name, schema, and
// read-only/allow safety so registering it simply overlays multi-root discovery
// onto the existing surface.
type skillTool struct {
	defaultDir  string
	pluginRoots []string
}

// NewSkillTool builds the multi-root skill tool. defaultDir is the standard
// skills directory (skills.DefaultDir); pluginRoots are the plugin skill search
// roots from an ActivationResult. The returned tool merges primary + agents +
// plugins, deterministically deduplicating by name (default dir wins a clash)
// and listing all available skills when an unknown name is requested.
func NewSkillTool(defaultDir string, pluginRoots []string) tools.Tool {
	return skillTool{defaultDir: defaultDir, pluginRoots: append([]string{}, pluginRoots...)}
}

func (tool skillTool) Name() string { return "skill" }

func (tool skillTool) Description() string {
	return "Load a named Zero skill and return its instructions as the tool output. " +
		"Skills are reusable, on-demand instruction sets (including shared ~/.agents/skills and any contributed by plugins). " +
		"Call this when a relevant skill exists; an unknown name returns the list of available skills."
}

func (tool skillTool) Parameters() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: map[string]tools.PropertySchema{
			"name":  {Type: "string", Description: "The name of the skill to load."},
			"skill": {Type: "string", Description: "Alias for name; supply either name or skill."},
		},
		AdditionalProperties: false,
	}
}

func (tool skillTool) Safety() tools.Safety {
	return tools.Safety{
		SideEffect: tools.SideEffectRead,
		Permission: tools.PermissionAllow,
		Reason:     "Reads a local skill file; gathers reusable instructions only.",
	}
}

func (tool skillTool) Run(_ context.Context, args map[string]any) tools.Result {
	name := skillName(args)
	if name == "" {
		return tools.Result{Status: tools.StatusError, Output: "Error: Invalid arguments for skill: name is required"}
	}
	merged, _ := MergedSkillsLoaded(tool.defaultDir, tool.pluginRoots)
	if len(merged) == 0 {
		return tools.Result{Status: tools.StatusError, Output: "Error: no skills are available."}
	}
	available := make([]string, 0, len(merged))
	for _, skill := range merged {
		if skill.Name == name {
			return tools.Result{Status: tools.StatusOK, Output: skill.Content}
		}
		available = append(available, skill.Name)
	}
	return tools.Result{Status: tools.StatusError, Output: fmt.Sprintf("Error: unknown skill %q. Available skills: %s.", name, strings.Join(available, ", "))}
}

// skillName extracts the requested skill name from either the "name" or "skill"
// argument (the core skill tool accepts both aliases).
func skillName(args map[string]any) string {
	for _, key := range []string{"name", "skill"} {
		if value, ok := args[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// MergedSkillsLoaded is MergedSkills but keeps each skill's Content (so the skill
// tool can return a body). MergedSkills strips Content for listing callers.
func MergedSkillsLoaded(defaultDir string, pluginRoots []string) ([]skills.Skill, []skills.DuplicateName) {
	return mergeSkills(defaultDir, pluginRoots, true)
}

// mapHookEvent maps a plugin manifest HookEvent onto the hooks package Event. The
// two enums use the same string values, but mapping explicitly keeps the packages
// decoupled and rejects any value the dispatcher does not understand.
func mapHookEvent(event HookEvent) (hooks.Event, bool) {
	switch event {
	case HookBeforeTool:
		return hooks.EventBeforeTool, true
	case HookAfterTool:
		return hooks.EventAfterTool, true
	case HookSessionStart:
		return hooks.EventSessionStart, true
	case HookSessionEnd:
		return hooks.EventSessionEnd, true
	default:
		return "", false
	}
}

// hookID derives a stable, namespaced hook id from the plugin id and the hook's
// manifest name, so plugin hooks never collide with user/project hooks.json ids.
func hookID(pluginID string, name string) string {
	pluginID = strings.TrimSpace(pluginID)
	name = strings.TrimSpace(name)
	switch {
	case pluginID == "" && name == "":
		return "plugin.hook"
	case pluginID == "":
		return name
	case name == "":
		return pluginID
	default:
		return pluginID + "." + name
	}
}

// expandPluginRoot replaces the ${AGENT_PLUGIN_ROOT} placeholder in value with the
// plugin's directory. It is a literal substitution (not shell expansion), so it is
// safe to apply to a command path or an individual argument.
func expandPluginRoot(value string, pluginDir string) string {
	if !strings.Contains(value, pluginRootPlaceholder) {
		return value
	}
	return strings.ReplaceAll(value, pluginRootPlaceholder, pluginDir)
}

// expandPluginRootPath is expandPluginRoot for an executable path: when the
// placeholder is present it also normalizes separators to the host OS via
// filepath.FromSlash, so a manifest "${AGENT_PLUGIN_ROOT}/bin/x" yields a clean
// native path (C:\...\bin\x on Windows) rather than a mixed-separator one. Bare
// command names and author-provided absolute paths are returned unchanged, and
// arguments are NOT normalized (they may legitimately contain forward slashes,
// e.g. URLs or flag values).
func expandPluginRootPath(value string, pluginDir string) string {
	if !strings.Contains(value, pluginRootPlaceholder) {
		if !filepath.IsAbs(value) && !isWindowsAbs(value) && !isRootedPath(value) && (strings.Contains(value, "/") || strings.Contains(value, "\\")) {
			return filepath.Join(pluginDir, value)
		}
		return value
	}
	return filepath.FromSlash(strings.ReplaceAll(value, pluginRootPlaceholder, pluginDir))
}

func expandPluginRootAll(values []string, pluginDir string) []string {
	if len(values) == 0 {
		return nil
	}
	expanded := make([]string, len(values))
	for index, value := range values {
		expanded[index] = expandPluginRoot(value, pluginDir)
	}
	return expanded
}

// pluginTool adapts a plugin tool extension to the tools.Tool interface. It runs
// the plugin's declared command, feeding the JSON-encoded tool arguments on
// stdin, and maps stdout/exit code onto a tools.Result.
type pluginTool struct {
	name        string
	description string
	pluginID    string
	pluginDir   string
	command     string
	args        []string
	parameters  tools.Schema
	safety      tools.Safety
	options     ActivateOptions
	run         toolRunner
}

func newPluginTool(plugin LoadedPlugin, ext ToolExtension, options ActivateOptions, run toolRunner) pluginTool {
	description := strings.TrimSpace(ext.Description)
	if description == "" {
		description = fmt.Sprintf("Run plugin tool %s/%s.", plugin.ID, ext.Name)
	}
	return pluginTool{
		name:        ext.Name,
		description: description,
		pluginID:    plugin.ID,
		pluginDir:   plugin.PluginDir,
		command:     expandPluginRootPath(ext.Command, plugin.PluginDir),
		args:        expandPluginRootAll(ext.Args, plugin.PluginDir),
		parameters:  mcp.SchemaFromMCP(ext.InputSchema),
		safety:      toolSafety(plugin, ext),
		options:     options,
		run:         run,
	}
}

func (tool pluginTool) Name() string             { return tool.name }
func (tool pluginTool) Description() string      { return tool.description }
func (tool pluginTool) Parameters() tools.Schema { return tool.parameters }
func (tool pluginTool) Safety() tools.Safety     { return tool.safety }

// Run executes the plugin command in the plugin's directory (no caller cwd).
func (tool pluginTool) Run(ctx context.Context, args map[string]any) tools.Result {
	return tool.invoke(ctx, args, "")
}

// RunWithOptions runs the plugin command in the caller's workspace cwd when one is
// supplied, so a plugin tool sees the same working directory as the rest of the
// run. Implementing optionsAwareTool also means the registry hands us the run
// options without changing the core Tool interface.
func (tool pluginTool) RunWithOptions(ctx context.Context, args map[string]any, options tools.RunOptions) tools.Result {
	return tool.invoke(ctx, args, options.Cwd)
}

func (tool pluginTool) invoke(ctx context.Context, args map[string]any, cwd string) tools.Result {
	stdin, err := encodeToolArgs(args)
	if err != nil {
		return tools.Result{
			Status: tools.StatusError,
			Output: "Error: invalid arguments for plugin tool " + tool.name + ": " + err.Error(),
			Meta:   tool.meta(),
		}
	}

	command := pluginCommand{
		Command: tool.command,
		Args:    append([]string{}, tool.args...),
		Cwd:     tool.commandCwd(cwd),
		Stdin:   stdin,
		Env:     tool.commandEnv(),
	}
	output := tool.run(ctx, command)

	meta := tool.meta()
	meta["exit_code"] = strconv.Itoa(output.ExitCode)

	if output.Err != nil {
		return tools.Result{
			Status: tools.StatusError,
			Output: "Error executing plugin tool " + tool.name + ": " + output.Err.Error(),
			Meta:   meta,
		}
	}
	formatted := formatPluginToolOutput(output)
	if output.ExitCode != 0 {
		return tools.Result{
			Status:  tools.StatusError,
			Output:  formatted,
			Meta:    meta,
			Display: tools.Display{Summary: tool.name + " failed", Kind: "plugin"},
		}
	}
	return tools.Result{
		Status:  tools.StatusOK,
		Output:  formatted,
		Meta:    meta,
		Display: tools.Display{Summary: tool.name, Kind: "plugin"},
	}
}

// commandCwd prefers the caller's workspace cwd and falls back to the plugin's
// own directory so a command launched with a relative argv still has a stable
// working directory.
func (tool pluginTool) commandCwd(cwd string) string {
	if trimmed := strings.TrimSpace(cwd); trimmed != "" {
		return trimmed
	}
	if trimmed := strings.TrimSpace(tool.options.Cwd); trimmed != "" {
		return trimmed
	}
	return tool.pluginDir
}

// commandEnv returns the base environment plus AGENT_PLUGIN_ROOT so the plugin
// command can resolve files relative to its install dir even without the
// ${AGENT_PLUGIN_ROOT} placeholder in its argv.
func (tool pluginTool) commandEnv() []string {
	base := tool.options.Env
	if base == nil {
		base = os.Environ()
	}
	env := append([]string{}, base...)
	if strings.TrimSpace(tool.pluginDir) != "" {
		env = append(env, pluginRootEnvVar+"="+tool.pluginDir)
	}
	return env
}

func (tool pluginTool) meta() map[string]string {
	return map[string]string{
		"plugin.id":   tool.pluginID,
		"plugin.tool": tool.name,
	}
}

// toolSafety maps the plugin ToolPermission onto a tools.Safety. A plugin tool
// runs an external command, so it always carries a shell side effect; the
// permission is carried through directly. An allow permission here is only
// reachable when the operator opted into manifest auto-approval at load time
// (parsePermission clamps allow→prompt otherwise), so honoring it does not
// silently auto-approve a mutating plugin tool.
func toolSafety(plugin LoadedPlugin, ext ToolExtension) tools.Safety {
	var permission tools.Permission
	switch ext.Permission {
	case PermissionAllow:
		permission = tools.PermissionAllow
	case PermissionDeny:
		permission = tools.PermissionDeny
	default:
		permission = tools.PermissionPrompt
	}
	return tools.Safety{
		SideEffect: tools.SideEffectShell,
		Permission: permission,
		Reason:     fmt.Sprintf("Plugin tool %s/%s runs the plugin's declared command.", plugin.ID, ext.Name),
	}
}

// encodeToolArgs serializes the tool arguments to JSON for the plugin command's
// stdin. A nil/empty map encodes as "{}" so the command always receives a valid
// JSON object.
func encodeToolArgs(args map[string]any) ([]byte, error) {
	if args == nil {
		args = map[string]any{}
	}
	return json.Marshal(args)
}

// formatPluginToolOutput renders the command's stdout/stderr/exit into the tool
// Result Output, redacting any high-confidence secrets the command may have
// printed (additive to the registry-boundary scrub), mirroring the bash tool.
func formatPluginToolOutput(output commandOutput) string {
	stdout := strings.TrimRight(output.Stdout, "\r\n")
	stderr := strings.TrimRight(output.Stderr, "\r\n")
	stdout, outFindings := secrets.Redact(stdout)
	stderr, errFindings := secrets.Redact(stderr)

	parts := []string{}
	if stdout != "" {
		parts = append(parts, "stdout:\n"+stdout)
	}
	if stderr != "" {
		parts = append(parts, "stderr:\n"+stderr)
	}
	if output.ExitCode != 0 {
		parts = append(parts, fmt.Sprintf("exit_code: %d", output.ExitCode))
	}
	if n := len(outFindings) + len(errFindings); n > 0 {
		parts = append(parts, fmt.Sprintf("[zero] redacted %d likely secret(s) from this plugin tool output before showing it.", n))
	}
	if len(parts) == 0 {
		return "Plugin tool completed with no output."
	}
	return strings.Join(parts, "\n")
}

// execPluginCommand runs a resolved plugin command as a subprocess, feeding the
// JSON args on stdin and capturing stdout/stderr. A non-zero exit is reported via
// ExitCode; Err is reserved for commands that could not be launched (so a missing
// binary surfaces as an error, not a misleading exit code).
func execPluginCommand(ctx context.Context, command pluginCommand, timeout time.Duration) commandOutput {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, command.Command, command.Args...)
	cmd.Dir = command.Cwd
	cmd.Env = command.Env
	if len(command.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(command.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	output := commandOutput{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return output
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		output.ExitCode = exitErr.ExitCode()
		return output
	}
	output.ExitCode = -1
	output.Err = err
	return output
}

func execPluginCommandWithExecution(ctx context.Context, runner *execution.Runner, command pluginCommand, timeout time.Duration) commandOutput {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result := runner.ExecuteCaptured(runCtx, execution.CapturedRequest{
		Request: execution.Request{
			Origin:           execution.OriginPlugin,
			Mode:             execution.ModeCaptured,
			Command:          execution.Command{Name: command.Command, Args: append([]string(nil), command.Args...), Env: append([]string(nil), command.Env...)},
			WorkingDirectory: command.Cwd,
			WorkspaceRoots:   []string{command.Cwd},
			Approval:         execution.ApprovalContext{PolicyVersion: execution.PolicyVersion},
		},
		Stdin: command.Stdin,
	})
	exitCode := -1
	if result.Outcome.Exit != nil {
		exitCode = result.Outcome.Exit.Code
	}
	output := commandOutput{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: exitCode}
	switch result.Outcome.Kind {
	case execution.OutcomeSandboxSetupFailure, execution.OutcomeExecutableNotFound, execution.OutcomeTimedOut, execution.OutcomeCancelled:
		output.Err = result.Err
	}
	if output.Stderr == "" && result.Outcome.Denial != nil {
		output.Stderr = result.Outcome.Denial.Reason
	}
	return output
}

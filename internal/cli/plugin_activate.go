package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/execution"
	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/plugins"
	"github.com/Twigpine/zero/internal/tools"
)

// pluginActivation holds what plugin activation contributed to a bootstrap so the
// later dispatcher + skill wiring can consume it: the plugin hook definitions and
// the plugin skill search roots. The zero value (no plugins) still overlays the
// multi-root skill tool (primary + ~/.agents/skills) so discovery is identical
// with or without plugins. The embedded trustSkip reports whether the project
// plugin layer was dropped for an untrusted workspace, so the caller can fold it
// into the single combined trust notice.
type pluginActivation struct {
	hooks      []hooks.Definition
	skillRoots []string
	trustSkip
}

// activatePlugins loads the workspace's plugins and makes their declared
// extensions live: plugin tools are registered into registry here, and the
// returned activation carries the plugin hooks + skill roots for the caller to
// fold into the hook dispatcher and the skill tool.
//
// It fails OPEN: any load error (or a malformed plugin) is surfaced as a warning
// on stderr and otherwise skipped, so a broken plugin can never wedge startup —
// mirroring how newHookDispatcher and skills.Load tolerate bad input. That
// fail-OPEN handling of MALFORMED plugins is separate from the trust gate below.
//
// The trust check lives here, inside the chokepoint, so no caller can bypass it:
// the project plugin root is dropped (ExcludeProject) whenever trustRoot is empty,
// the trust store cannot be read, or the workspace is not trusted (fail-closed).
// trustRoot is the original launch directory (resolved before any --worktree
// reassignment). The returned activation carries the skip report so the caller can
// emit one combined notice; the notice itself is NOT emitted here.
func activatePlugins(workspaceRoot string, registry *tools.Registry, deps appDeps, stderr io.Writer, trustRoot string, runners ...*execution.Runner) pluginActivation {
	excludeProject, trustCheckErrored := resolveTrust(trustRoot)
	skip := trustSkip{
		excludedProjectConfig: excludeProject && projectPluginsDirExists(workspaceRoot),
		trustCheckErrored:     trustCheckErrored,
	}

	loaded, err := deps.loadPlugins(plugins.LoadOptions{Cwd: workspaceRoot, ExcludeProject: excludeProject})
	if err != nil {
		writePluginActivationWarning(stderr, "failed to load plugins: "+err.Error())
		// Still overlay multi-root discovery (primary + agents) so a plugin load
		// failure cannot hide shared skills.
		registry.Register(plugins.NewSkillTool(deps.skillsDir(), nil))
		return pluginActivation{trustSkip: skip}
	}

	// Load fails OPEN per plugin: a malformed manifest is recorded as a diagnostic
	// and the plugin is dropped rather than aborting the whole load. Surface those
	// diagnostics so a skipped plugin is never silent.
	for _, diagnostic := range loaded.Diagnostics {
		writePluginActivationWarning(stderr, formatLoadDiagnostic(diagnostic))
	}

	var runner *execution.Runner
	if len(runners) > 0 {
		runner = runners[0]
	}
	result := plugins.Activate(registry, loaded.Plugins, plugins.ActivateOptions{Cwd: workspaceRoot, Execution: runner})
	for _, warning := range result.Warnings {
		writePluginActivationWarning(stderr, warning)
	}

	// Always overlay the multi-root skill tool so discovery is identical with or
	// without plugins: primary DefaultDir + optional ~/.agents/skills + plugin
	// roots (earlier wins). With empty SkillRoots and no agents dir this is
	// byte-equivalent to the core single-dir skill surface.
	registry.Register(plugins.NewSkillTool(deps.skillsDir(), result.SkillRoots))

	return pluginActivation{hooks: result.Hooks, skillRoots: result.SkillRoots, trustSkip: skip}
}

// projectPluginsDirExists reports whether a ./.zero/plugins directory is present
// under workspaceRoot, so the caller only notices about config it actually
// skipped.
func projectPluginsDirExists(workspaceRoot string) bool {
	if workspaceRoot == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(workspaceRoot, ".zero", "plugins"))
	return err == nil && info.IsDir()
}

// skillInfos resolves the reusable skills the model can load via the skill tool —
// primary dir + optional ~/.agents/skills + plugin skill roots, the same set the
// multi-root skill tool resolves against — as plain data for the agent's system
// prompt. It returns nil when no skills are installed, so a skill-less run leaves
// the prompt byte-identical.
func (a pluginActivation) skillInfos(defaultDir string) []agent.SkillInfo {
	merged, _ := plugins.MergedSkills(defaultDir, a.skillRoots)
	if len(merged) == 0 {
		return nil
	}
	infos := make([]agent.SkillInfo, 0, len(merged))
	for _, skill := range merged {
		name := strings.TrimSpace(skill.Name)
		if name == "" {
			continue
		}
		infos = append(infos, agent.SkillInfo{Name: name, Description: strings.TrimSpace(skill.Description)})
	}
	return infos
}

// formatLoadDiagnostic renders a plugin load diagnostic into a single warning
// line, prefixing the diagnostic kind and appending the most specific locator
// available (manifest path, then plugin path, then plugin ID) so an operator can
// locate the offending plugin even when the manifest path is unknown.
func formatLoadDiagnostic(diagnostic plugins.Diagnostic) string {
	message := fmt.Sprintf("[%s] %s", diagnostic.Kind, diagnostic.Message)
	locator := diagnostic.ManifestPath
	if locator == "" {
		locator = diagnostic.PluginPath
	}
	if locator == "" {
		locator = diagnostic.PluginID
	}
	if locator != "" {
		message += " (" + locator + ")"
	}
	return message
}

func writePluginActivationWarning(stderr io.Writer, message string) {
	if stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(stderr, "[zero] WARNING: plugin activation: %s\n", message)
}

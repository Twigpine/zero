package cli

// Distribution command handlers: `zero skill {add,info,remove}`,
// `zero plugin {add,remove}`, and `zero tools {make,list}`. These wire the
// install/scaffold logic in internal/skills, internal/plugins, and
// internal/tools into the CLI. List for skills/plugins reuses the existing
// listing handlers (runSkillsList / runPlugins list).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Twigpine/zero/internal/plugins"
	"github.com/Twigpine/zero/internal/redaction"
	"github.com/Twigpine/zero/internal/skills"
	"github.com/Twigpine/zero/internal/tools"
)

// distributionAddOptions are the flags shared by `skill add` / `plugin add`.
type distributionAddOptions struct {
	force bool
	json  bool
}

// parseDistributionAddArgs splits a positional source from the add flags.
func parseDistributionAddArgs(args []string, label string) (string, distributionAddOptions, bool, error) {
	options := distributionAddOptions{}
	source := ""
	for _, arg := range args {
		switch arg {
		case "-h", "--help", "help":
			return "", options, true, nil
		case "--force", "-f":
			options.force = true
		case "--json":
			options.json = true
		default:
			if strings.HasPrefix(arg, "-") {
				return "", options, false, execUsageError{fmt.Sprintf("unknown %s add flag %q", label, arg)}
			}
			if source != "" {
				return "", options, false, execUsageError{fmt.Sprintf("%s add takes a single source (git URL or path)", label)}
			}
			source = arg
		}
	}
	return source, options, false, nil
}

// --- skill add / info / remove ---

func runSkillAdd(args []string, dir string, stdout io.Writer, stderr io.Writer) int {
	source, options, help, err := parseDistributionAddArgs(args, "skill")
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeSkillAddHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if source == "" {
		return writeExecUsageError(stderr, "usage: zero skill add <git-url|path> [--force] [--json]")
	}
	if dir == "" {
		return writeAppError(stderr, "could not resolve the skills directory", exitCrash)
	}

	result, err := skills.Install(context.Background(), skills.InstallOptions{
		Source: source,
		Dir:    dir,
		Force:  options.force,
	})
	if err != nil {
		if errors.Is(err, skills.ErrNameClash) {
			return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitUsage)
		}
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if options.json {
		if err := writePrettyJSON(stdout, redaction.RedactValue(result, redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	lines := []string{}
	if result.Updated {
		lines = append(lines, fmt.Sprintf("Updated skill %q.", result.Name))
		lines = append(lines, fmt.Sprintf("  hash: %s -> %s", shortHash(result.PreviousHash), shortHash(result.Hash)))
	} else {
		lines = append(lines, fmt.Sprintf("Installed skill %q.", result.Name))
		lines = append(lines, "  hash: "+result.Hash)
	}
	lines = append(lines, "  source: "+result.Source)
	lines = append(lines, "  path: "+result.Path)
	if _, err := fmt.Fprintln(stdout, redaction.RedactString(strings.Join(lines, "\n"), redaction.Options{})); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runSkillInfo(args []string, dir string, stdout io.Writer, stderr io.Writer) int {
	name, asJSON, help, err := parseNameCommandArgs(args, "skill info")
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeSkillInfoHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if name == "" {
		return writeExecUsageError(stderr, "usage: zero skill info <name> [--json]")
	}
	// Resolve across primary + ~/.agents/skills; lock metadata only from primary.
	info, ok := skills.InfoFromRoots(dir, skills.GlobalRoots(dir), name)
	if !ok {
		return writeAppError(stderr, fmt.Sprintf("skill %q not found", name), exitUsage)
	}
	if asJSON {
		if err := writePrettyJSON(stdout, redaction.RedactValue(info, redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	lines := []string{info.Skill.Name}
	if info.Skill.Description != "" {
		lines = append(lines, "  "+info.Skill.Description)
	}
	if info.Source != "" {
		lines = append(lines, "  source: "+info.Source)
	}
	if info.Hash != "" {
		lines = append(lines, "  hash: "+info.Hash)
		lines = append(lines, fmt.Sprintf("  hash drift: %t", info.HashDrift))
	}
	lines = append(lines, "  path: "+info.Skill.Path)
	if _, err := fmt.Fprintln(stdout, redaction.RedactString(strings.Join(lines, "\n"), redaction.Options{})); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runSkillRemove(args []string, dir string, stdout io.Writer, stderr io.Writer) int {
	name, asJSON, help, err := parseNameCommandArgs(args, "skill remove")
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeSkillRemoveHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if asJSON {
		return writeExecUsageError(stderr, "skill remove does not support --json")
	}
	if name == "" {
		return writeExecUsageError(stderr, "usage: zero skill remove <name>")
	}
	if err := skills.Remove(dir, name); err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitUsage)
	}
	if _, err := fmt.Fprintf(stdout, "Removed skill %q.\n", name); err != nil {
		return exitCrash
	}
	return exitSuccess
}

// --- plugin add / remove ---

func runPluginAdd(args []string, dir string, stdout io.Writer, stderr io.Writer) int {
	source, options, help, err := parseDistributionAddArgs(args, "plugin")
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writePluginAddHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if source == "" {
		return writeExecUsageError(stderr, "usage: zero plugin add <git-url|path> [--force] [--json]")
	}
	if dir == "" {
		return writeAppError(stderr, "could not resolve the plugins directory", exitCrash)
	}

	result, err := plugins.Install(context.Background(), plugins.InstallOptions{
		Source: source,
		Dir:    dir,
		Force:  options.force,
	})
	if err != nil {
		if errors.Is(err, plugins.ErrNameClash) {
			return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitUsage)
		}
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if options.json {
		if err := writePrettyJSON(stdout, redaction.RedactValue(result, redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	lines := []string{}
	if result.Updated {
		lines = append(lines, fmt.Sprintf("Updated plugin %s@%s.", result.ID, result.Version))
		lines = append(lines, fmt.Sprintf("  hash: %s -> %s", shortHash(result.PreviousHash), shortHash(result.Hash)))
	} else {
		lines = append(lines, fmt.Sprintf("Installed plugin %s@%s.", result.ID, result.Version))
		lines = append(lines, "  hash: "+result.Hash)
	}
	lines = append(lines, "  source: "+result.Source)
	lines = append(lines, "  path: "+result.ManifestPath)
	lines = append(lines, "Activation gates each tool through the normal permission flow; no install script was run.")
	if _, err := fmt.Fprintln(stdout, redaction.RedactString(strings.Join(lines, "\n"), redaction.Options{})); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runPluginRemove(args []string, dir string, stdout io.Writer, stderr io.Writer) int {
	id, asJSON, help, err := parseNameCommandArgs(args, "plugin remove")
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writePluginRemoveHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if asJSON {
		return writeExecUsageError(stderr, "plugin remove does not support --json")
	}
	if id == "" {
		return writeExecUsageError(stderr, "usage: zero plugin remove <id>")
	}
	if err := plugins.Remove(dir, id); err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitUsage)
	}
	if _, err := fmt.Fprintf(stdout, "Removed plugin %q.\n", id); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runPluginInfo(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	id, options, help, err := parsePluginInfoArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writePluginInfoHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if id == "" {
		return writeExecUsageError(stderr, "usage: zero plugins info <id> [--user] [--json]")
	}

	cwd, err := deps.getwd()
	if err != nil {
		return writeAppError(stderr, "failed to resolve workspace: "+err.Error(), exitCrash)
	}
	info, err := plugins.Info(plugins.InfoOptions{
		LoadOptions: pluginLoadOptions(deps, cwd, options.user),
	}, id)
	if err != nil {
		if errors.Is(err, plugins.ErrNotInstalled) {
			return writeExecUsageError(stderr, err.Error())
		}
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}

	if options.json {
		if err := writePrettyJSON(stdout, redaction.RedactValue(info, redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	plugin := info.Plugin
	lines := []string{fmt.Sprintf("%s (%s) %s", plugin.ID, plugin.Name, plugin.Version)}
	state := "enabled"
	if !plugin.Enabled {
		state = "disabled"
	}
	lines = append(lines, "  state: "+state)
	lines = append(lines, "  source: "+string(plugin.Source))
	if len(plugin.Tools) > 0 {
		lines = append(lines, fmt.Sprintf("  tools: %d", len(plugin.Tools)))
	}
	if len(plugin.Hooks) > 0 {
		lines = append(lines, fmt.Sprintf("  hooks: %d", len(plugin.Hooks)))
	}
	if len(plugin.Skills) > 0 {
		lines = append(lines, fmt.Sprintf("  skills: %d", len(plugin.Skills)))
	}
	if len(plugin.Prompts) > 0 {
		lines = append(lines, fmt.Sprintf("  prompts: %d", len(plugin.Prompts)))
	}
	if info.LockSource != "" {
		lines = append(lines, "  lock source: "+info.LockSource)
	}
	if info.LockHash != "" {
		lines = append(lines, "  lock hash: "+info.LockHash)
		lines = append(lines, fmt.Sprintf("  hash drift: %t", info.HashDrift))
	}
	lines = append(lines, "  manifest: "+plugin.ManifestPath)
	if _, err := fmt.Fprintln(stdout, redaction.RedactString(strings.Join(lines, "\n"), redaction.Options{})); err != nil {
		return exitCrash
	}
	return exitSuccess
}

type pluginInfoOptions struct {
	json bool
	user bool
}

func parsePluginInfoArgs(args []string) (string, pluginInfoOptions, bool, error) {
	options := pluginInfoOptions{}
	id := ""
	for _, arg := range args {
		switch arg {
		case "-h", "--help", "help":
			return "", options, true, nil
		case "--json":
			options.json = true
		case "--user":
			options.user = true
		default:
			if strings.HasPrefix(arg, "-") {
				return "", options, false, execUsageError{fmt.Sprintf("unknown plugins info flag %q", arg)}
			}
			if id != "" {
				return "", options, false, execUsageError{"plugins info takes a single id"}
			}
			id = arg
		}
	}
	return id, options, false, nil
}

func pluginLoadOptions(deps appDeps, cwd string, userOnly bool) plugins.LoadOptions {
	opts := plugins.LoadOptions{Cwd: cwd, ExcludeProject: userOnly}
	roots := []plugins.Root{{Source: plugins.SourceUser, Path: deps.pluginsDir()}}
	if !userOnly {
		roots = append(roots, plugins.Root{Source: plugins.SourceProject, Path: filepath.Join(cwd, ".zero", "plugins")})
	}
	opts.Roots = roots
	return opts
}

// --- tools make / list ---

func runTools(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	if len(args) == 0 {
		return writeExecUsageError(stderr, "tools subcommand required. Use `zero tools make <name>` or `zero tools list`.")
	}
	switch args[0] {
	case "-h", "--help", "help":
		if err := writeToolsHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	case "make", "new", "scaffold":
		return runToolsMake(args[1:], deps.toolsDir(), stdout, stderr)
	case "list", "ls":
		return runToolsList(args[1:], deps.toolsDir(), stdout, stderr)
	default:
		return writeExecUsageError(stderr, fmt.Sprintf("unknown tools subcommand %q", args[0]))
	}
}

type toolsMakeOptions struct {
	runtime     string
	description string
	json        bool
}

func runToolsMake(args []string, dir string, stdout io.Writer, stderr io.Writer) int {
	name := ""
	options := toolsMakeOptions{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			if err := writeToolsMakeHelp(stdout); err != nil {
				return exitCrash
			}
			return exitSuccess
		case arg == "--json":
			options.json = true
		case arg == "--runtime":
			value, next, err := nextFlagValue(args, i, arg)
			if err != nil {
				return writeExecUsageError(stderr, err.Error())
			}
			options.runtime, i = value, next
		case strings.HasPrefix(arg, "--runtime="):
			options.runtime = strings.TrimSpace(strings.TrimPrefix(arg, "--runtime="))
		case arg == "--description":
			value, next, err := nextFlagValue(args, i, arg)
			if err != nil {
				return writeExecUsageError(stderr, err.Error())
			}
			options.description, i = value, next
		case strings.HasPrefix(arg, "--description="):
			options.description = strings.TrimSpace(strings.TrimPrefix(arg, "--description="))
		case strings.HasPrefix(arg, "-"):
			return writeExecUsageError(stderr, fmt.Sprintf("unknown tools make flag %q", arg))
		default:
			if name != "" {
				return writeExecUsageError(stderr, "tools make takes a single tool name")
			}
			name = arg
		}
	}
	if name == "" {
		return writeExecUsageError(stderr, "usage: zero tools make <name> [--runtime shell|node|python] [--description text]")
	}
	if dir == "" {
		return writeAppError(stderr, "could not resolve the toolbox directory", exitCrash)
	}

	result, err := tools.Scaffold(tools.ScaffoldOptions{
		Name:        name,
		Dir:         dir,
		Description: options.description,
		Runtime:     tools.ScaffoldRuntime(strings.TrimSpace(options.runtime)),
	})
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitUsage)
	}
	if options.json {
		if err := writePrettyJSON(stdout, redaction.RedactValue(result, redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	lines := []string{
		fmt.Sprintf("Scaffolded tool %q at %s.", result.Name, result.PluginDir),
		"  manifest: " + result.ManifestPath,
		"  entry:    " + result.EntryPath,
		"",
		"Next steps:",
		"  1. Implement the TODO in the entry script.",
		"  2. Run `zero tools list` to confirm it loads.",
		"  3. The tool activates through the normal permission flow.",
	}
	if _, err := fmt.Fprintln(stdout, redaction.RedactString(strings.Join(lines, "\n"), redaction.Options{})); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runToolsList(args []string, dir string, stdout io.Writer, stderr io.Writer) int {
	asJSON := false
	for _, arg := range args {
		switch arg {
		case "-h", "--help", "help":
			if err := writeToolsListHelp(stdout); err != nil {
				return exitCrash
			}
			return exitSuccess
		case "--json":
			asJSON = true
		default:
			return writeExecUsageError(stderr, fmt.Sprintf("unknown tools list flag %q", arg))
		}
	}
	if dir == "" {
		return writeAppError(stderr, "could not resolve the toolbox directory", exitCrash)
	}

	// Scaffolded tools are plugins, so list them through the plugin loader scoped
	// to the toolbox dir, then surface their tool extensions.
	result, err := plugins.Load(plugins.LoadOptions{Roots: []plugins.Root{{Source: plugins.SourceUser, Path: dir}}})
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	items := toolboxItems(result.Plugins)
	if asJSON {
		payload := struct {
			Tools       []toolboxItem        `json:"tools"`
			Diagnostics []plugins.Diagnostic `json:"diagnostics"`
		}{Tools: items, Diagnostics: result.Diagnostics}
		if err := writePrettyJSON(stdout, redaction.RedactValue(payload, redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, redaction.RedactString(formatToolboxList(items, result.Diagnostics, dir), redaction.Options{})); err != nil {
		return exitCrash
	}
	return exitSuccess
}

// toolboxItem is a flattened view of a plugin tool for `tools list`.
type toolboxItem struct {
	Plugin      string `json:"plugin"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Command     string `json:"command"`
	Permission  string `json:"permission"`
}

func toolboxItems(loaded []plugins.LoadedPlugin) []toolboxItem {
	items := []toolboxItem{}
	for _, plugin := range loaded {
		for _, tool := range plugin.Tools {
			command := tool.Command
			if len(tool.Args) > 0 {
				command = strings.TrimSpace(command + " " + strings.Join(tool.Args, " "))
			}
			items = append(items, toolboxItem{
				Plugin:      plugin.ID,
				Name:        tool.Name,
				Description: tool.Description,
				Command:     command,
				Permission:  string(tool.Permission),
			})
		}
	}
	sort.Slice(items, func(left int, right int) bool {
		if items[left].Plugin != items[right].Plugin {
			return items[left].Plugin < items[right].Plugin
		}
		return items[left].Name < items[right].Name
	})
	return items
}

func formatToolboxList(items []toolboxItem, diagnostics []plugins.Diagnostic, dir string) string {
	lines := []string{}
	if len(items) == 0 {
		lines = append(lines, fmt.Sprintf("No Zero plugin-tools found in %s.", dir))
	} else {
		lines = append(lines, "Zero Tools:")
		for _, item := range items {
			line := "  " + item.Name + " (" + item.Plugin + ")"
			if item.Description != "" {
				line += " - " + item.Description
			}
			lines = append(lines, line)
			lines = append(lines, fmt.Sprintf("    command: %s [%s]", item.Command, item.Permission))
		}
	}
	lines = append(lines, formatToolboxDiagnostics(diagnostics)...)
	return strings.Join(lines, "\n")
}

// formatToolboxDiagnostics renders loader diagnostics the same way the plugins
// listing does, so a broken toolbox plugin surfaces a warning instead of
// silently vanishing from the listing.
func formatToolboxDiagnostics(diagnostics []plugins.Diagnostic) []string {
	if len(diagnostics) == 0 {
		return nil
	}
	lines := []string{"Tool diagnostics:"}
	for _, diagnostic := range diagnostics {
		line := fmt.Sprintf("  [%s] %s", diagnostic.Kind, diagnostic.Message)
		lines = append(lines, line)
	}
	return lines
}

// parseNameCommandArgs parses a single positional name plus an optional --json
// flag for the info/remove style commands.
func parseNameCommandArgs(args []string, label string) (string, bool, bool, error) {
	name := ""
	asJSON := false
	for _, arg := range args {
		switch arg {
		case "-h", "--help", "help":
			return "", false, true, nil
		case "--json":
			asJSON = true
		default:
			if strings.HasPrefix(arg, "-") {
				return "", false, false, execUsageError{fmt.Sprintf("unknown %s flag %q", label, arg)}
			}
			if name != "" {
				return "", false, false, execUsageError{fmt.Sprintf("%s takes a single name", label)}
			}
			name = arg
		}
	}
	return name, asJSON, false, nil
}

// shortHash trims a "sha256:<hex>" fingerprint to a readable prefix for status
// lines while leaving the algorithm tag intact.
func shortHash(hash string) string {
	if hash == "" {
		return "(none)"
	}
	algo, hex, found := strings.Cut(hash, ":")
	if !found {
		if len(hash) > 12 {
			return hash[:12]
		}
		return hash
	}
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return algo + ":" + hex
}

func writeSkillAddHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero skill add <git-url|path> [flags]

Installs a skill from a git URL or local path into the skills directory and
records a content hash in skills.lock. Fetched content is never executed.

Flags:
  -f, --force    Overwrite a skill installed from a different source
      --json     Print the install result as JSON
  -h, --help     Show this help
`)
	return err
}

func writeSkillInfoHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero skill info <name> [--json]

Shows a skill's frontmatter plus its recorded source and pinned hash.
`)
	return err
}

func writeSkillRemoveHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero skill remove <name>

Removes an installed skill directory and its skills.lock entry.
`)
	return err
}

func writePluginAddHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero plugin add <git-url|path> [flags]

Installs a plugin from a git URL or local path. The manifest is validated and a
content hash is recorded in plugins.lock. No install script is run; the plugin
still activates through the normal permission flow.

Flags:
  -f, --force    Overwrite a plugin installed from a different source
      --json     Print the install result as JSON
  -h, --help     Show this help
`)
	return err
}

func writePluginRemoveHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero plugin remove <id>

Removes an installed plugin directory and its plugins.lock entry.
`)
	return err
}

func writePluginInfoHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero plugins info <id> [flags]

Flags:
      --user    Target only user plugins (ignore ./.zero/plugins)
      --json    Print plugin info as JSON
  -h, --help    Show this help
`)
	return err
}

func writeToolsHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero tools <command>

Commands:
  make <name>    Scaffold a new plugin-tool skeleton in the toolbox dir
  list           List plugin-tools in the toolbox dir
`)
	return err
}

func writeToolsMakeHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero tools make <name> [flags]

Scaffolds a plugin-tool skeleton (a plugin manifest plus a runnable entry stub)
in the toolbox directory. After plugin activation the tool is loadable.

Flags:
      --runtime <shell|node|python>   Entry stub language (default shell)
      --description <text>            Tool description
      --json                          Print the scaffold result as JSON
  -h, --help                          Show this help
`)
	return err
}

func writeToolsListHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero tools list [flags]

Flags:
      --json    Print plugin-tools as JSON
  -h, --help    Show this help
`)
	return err
}

package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/hooks"
	"github.com/Gitlawb/zero/internal/mcp"
	"github.com/Gitlawb/zero/internal/plugins"
	"github.com/Gitlawb/zero/internal/redaction"
	"github.com/Gitlawb/zero/internal/tools"
)

type pluginListOptions struct {
	json bool
}

type hookListOptions struct {
	json bool
}

type mcpCommandOptions struct {
	json    bool
	confirm bool
}

type mcpLegacyListOptions struct {
	json  bool
	tools bool
}

func runPlugins(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	if len(args) == 0 {
		return writeExecUsageError(stderr, "plugins subcommand required. Use `zero plugins list`.")
	}
	switch args[0] {
	case "-h", "--help", "help":
		if err := writePluginsHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	case "list":
		options, help, err := parsePluginListArgs(args[1:])
		if err != nil {
			return writeExecUsageError(stderr, err.Error())
		}
		if help {
			if err := writePluginsListHelp(stdout); err != nil {
				return exitCrash
			}
			return exitSuccess
		}
		cwd, err := deps.getwd()
		if err != nil {
			return writeAppError(stderr, "failed to resolve workspace: "+err.Error(), exitCrash)
		}
		result, err := deps.loadPlugins(plugins.LoadOptions{Cwd: cwd})
		if err != nil {
			return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
		}
		if options.json {
			payload := struct {
				Plugins     []plugins.LoadedPlugin `json:"plugins"`
				Diagnostics []plugins.Diagnostic   `json:"diagnostics"`
			}{Plugins: result.Plugins, Diagnostics: result.Diagnostics}
			if err := writePrettyJSON(stdout, redaction.RedactValue(payload, redaction.Options{})); err != nil {
				return exitCrash
			}
			return exitSuccess
		}
		output := redaction.RedactString(plugins.FormatList(result.Plugins, result.Diagnostics), redaction.Options{})
		if _, err := fmt.Fprintln(stdout, output); err != nil {
			return exitCrash
		}
		return exitSuccess
	case "add":
		return runPluginAdd(args[1:], deps.pluginsDir(), stdout, stderr)
	case "info":
		return runPluginInfo(args[1:], stdout, stderr, deps)
	case "remove", "rm":
		return runPluginRemove(args[1:], deps.pluginsDir(), stdout, stderr)
	default:
		return writeExecUsageError(stderr, fmt.Sprintf("unknown plugins subcommand %q", args[0]))
	}
}

func runHooks(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	if len(args) == 0 {
		return writeExecUsageError(stderr, "hooks subcommand required. Use `zero hooks list`.")
	}
	switch args[0] {
	case "-h", "--help", "help":
		if err := writeHooksHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	case "list":
		options, help, err := parseHookListArgs(args[1:])
		if err != nil {
			return writeExecUsageError(stderr, err.Error())
		}
		if help {
			if err := writeHooksListHelp(stdout); err != nil {
				return exitCrash
			}
			return exitSuccess
		}
		cwd, err := deps.getwd()
		if err != nil {
			return writeAppError(stderr, "failed to resolve workspace: "+err.Error(), exitCrash)
		}
		result, err := deps.loadHooks(hooks.LoadOptions{Cwd: cwd})
		if err != nil {
			return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
		}
		if options.json {
			payload := struct {
				Hooks       hooks.Config       `json:"hooks"`
				Diagnostics []hooks.Diagnostic `json:"diagnostics"`
			}{Hooks: result.Config, Diagnostics: result.Diagnostics}
			if err := writePrettyJSON(stdout, redaction.RedactValue(payload, redaction.Options{})); err != nil {
				return exitCrash
			}
			return exitSuccess
		}
		output := redaction.RedactString(hooks.FormatList(result.Config, result.Diagnostics), redaction.Options{})
		if _, err := fmt.Fprintln(stdout, output); err != nil {
			return exitCrash
		}
		return exitSuccess
	case "add":
		return runHooksAdd(args[1:], stdout, stderr, deps)
	case "remove", "rm":
		return runHooksRemove(args[1:], stdout, stderr, deps)
	case "enable":
		return runHooksToggle(args[1:], stdout, stderr, deps, false)
	case "disable":
		return runHooksToggle(args[1:], stdout, stderr, deps, true)
	default:
		return writeExecUsageError(stderr, fmt.Sprintf("unknown hooks subcommand %q", args[0]))
	}
}

func runMCP(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	return runMCPWithContext(context.Background(), args, stdout, stderr, deps)
}

func runMCPWithContext(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(args) == 0 {
		return writeExecUsageError(stderr, "mcp subcommand required. Use `zero mcp permissions list`.")
	}
	switch args[0] {
	case "-h", "--help", "help":
		if err := writeMCPHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	case "add":
		return runMCPAdd(args[1:], stdout, stderr, deps)
	case "remove", "rm":
		return runMCPRemove(args[1:], stdout, stderr, deps)
	case "enable":
		return runMCPToggle(args[1:], stdout, stderr, deps, false)
	case "disable":
		return runMCPToggle(args[1:], stdout, stderr, deps, true)
	case "check":
		return runMCPCheck(ctx, args[1:], stdout, stderr, deps)
	case "secret":
		return runMCPSecret(args[1:], stdout, stderr, deps)
	case "permissions":
		return runMCPPermissions(args[1:], stdout, stderr, deps)
	case "tools":
		return runMCPTools(ctx, args[1:], stdout, stderr, deps)
	case "oauth":
		return runMCPOAuth(args[1:], stdout, stderr, deps)
	case "list":
		return runMCPLegacyList(args[1:], stdout, stderr, deps)
	default:
		return writeExecUsageError(stderr, fmt.Sprintf("unknown mcp subcommand %q", args[0]))
	}
}

func runMCPLegacyList(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseMCPLegacyListArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeMCPLegacyListHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if options.tools {
		forwarded := []string{"list"}
		if options.json {
			forwarded = append(forwarded, "--json")
		}
		return runMCPTools(context.Background(), forwarded, stdout, stderr, deps)
	}

	cwd, err := deps.getwd()
	if err != nil {
		return writeAppError(stderr, "failed to resolve workspace: "+err.Error(), exitCrash)
	}
	// Enumeration for the extensions listing, never spawns a server, so it is left
	// ungated (excludeProject=false) like the other doctor/status report sites.
	cfg, err := deps.resolveMCPConfig(cwd, false)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if cfg.Servers == nil {
		cfg.Servers = map[string]config.MCPServerConfig{}
	}
	if options.json {
		// Env and header VALUES are credentials by construction (API keys,
		// bearer tokens) and must never reach stdout: pattern-based redaction
		// only catches recognizable token shapes, so the values are replaced
		// structurally — keys stay visible for triage.
		payload := struct {
			Servers map[string]config.MCPServerConfig `json:"servers"`
		}{Servers: redactMCPServerConfigs(cfg.Servers)}
		if err := writePrettyJSON(stdout, redaction.RedactValue(payload, redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintln(stdout, redaction.RedactString(formatMCPServerList(cfg.Servers), redaction.Options{})); err != nil {
		return exitCrash
	}
	return exitSuccess
}

// redactMCPServerConfigs deep-copies the server map with every env and header
// value replaced by a redaction marker.
func redactMCPServerConfigs(servers map[string]config.MCPServerConfig) map[string]config.MCPServerConfig {
	redacted := make(map[string]config.MCPServerConfig, len(servers))
	for name, server := range servers {
		clone := server
		if len(server.Env) > 0 {
			clone.Env = make(map[string]string, len(server.Env))
			for key := range server.Env {
				clone.Env[key] = "[REDACTED]"
			}
		}
		if len(server.Headers) > 0 {
			clone.Headers = make(map[string]string, len(server.Headers))
			for key := range server.Headers {
				clone.Headers[key] = "[REDACTED]"
			}
		}
		clone.URL = redactMCPURL(clone.URL, "[REDACTED]")
		redacted[name] = clone
	}
	return redacted
}

func runMCPTools(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(args) == 0 {
		return writeExecUsageError(stderr, "mcp tools subcommand required. Use `zero mcp tools list`.")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		if err := writeMCPToolsHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	switch args[0] {
	case "list":
		options, help, err := parseMCPCommandOptions(args[1:])
		if err != nil {
			return writeExecUsageError(stderr, err.Error())
		}
		if help {
			if err := writeMCPToolsHelp(stdout); err != nil {
				return exitCrash
			}
			return exitSuccess
		}
		cwd, err := deps.getwd()
		if err != nil {
			return writeAppError(stderr, "failed to resolve workspace: "+err.Error(), exitCrash)
		}
		registry := tools.NewRegistry()
		// `mcp tools list` connects to (spawns) each server to enumerate its live tools,
		// so it is a spawn site and gates the project layer behind the trust check. No
		// --worktree reassignment on this command path, so trustRoot == cwd.
		mcpRuntime, mcpSkip, err := registerMCPToolsForWorkspace(ctx, cwd, registry, deps, mcp.AutonomyLow, cwd)
		if err != nil {
			return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
		}
		defer closeMCPRuntime(stderr, mcpRuntime)
		// Surface the trust skip on stderr so an empty (or short) list in an untrusted
		// workspace is explained rather than read as "nothing configured". The notice
		// goes to stderr, leaving the list (text or --json) on stdout intact.
		emitTrustNotice(stderr, mcpSkip)
		// Surface the trust skip on stderr so an empty (or short) list in an untrusted
		// workspace is explained rather than read as "nothing configured". The notice
		// goes to stderr, leaving the list (text or --json) on stdout intact.
		items := mcpToolList(registry)
		if options.json {
			payload := struct {
				Tools []mcpToolListItem `json:"tools"`
			}{Tools: items}
			if err := writePrettyJSON(stdout, redaction.RedactValue(payload, redaction.Options{})); err != nil {
				return exitCrash
			}
			return exitSuccess
		}
		if _, err := fmt.Fprintln(stdout, redaction.RedactString(formatMCPToolList(items), redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	default:
		return writeExecUsageError(stderr, fmt.Sprintf("unknown mcp tools subcommand %q", args[0]))
	}
}

func runMCPPermissions(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	if len(args) == 0 {
		return writeExecUsageError(stderr, "mcp permissions subcommand required. Use `zero mcp permissions list`.")
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		if err := writeMCPPermissionsHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	switch args[0] {
	case "list":
		options, help, err := parseMCPCommandOptions(args[1:])
		if err != nil {
			return writeExecUsageError(stderr, err.Error())
		}
		if help {
			if err := writeMCPPermissionsHelp(stdout); err != nil {
				return exitCrash
			}
			return exitSuccess
		}
		store, err := deps.newMCPStore()
		if err != nil {
			return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
		}
		permissions, err := store.List()
		if err != nil {
			return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
		}
		if options.json {
			payload := struct {
				Permissions []mcp.PermissionGrant `json:"permissions"`
			}{Permissions: permissions}
			if err := writePrettyJSON(stdout, redaction.RedactValue(payload, redaction.Options{})); err != nil {
				return exitCrash
			}
			return exitSuccess
		}
		if _, err := fmt.Fprintln(stdout, mcp.FormatPermissionList(permissions)); err != nil {
			return exitCrash
		}
		return exitSuccess
	case "revoke":
		return runMCPPermissionsRevoke(args[1:], stdout, stderr, deps)
	case "clear":
		return runMCPPermissionsClear(args[1:], stdout, stderr, deps)
	default:
		return writeExecUsageError(stderr, fmt.Sprintf("unknown mcp permissions subcommand %q", args[0]))
	}
}

func runMCPPermissionsRevoke(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, positional, help, err := parseMCPPositionalCommand(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeMCPPermissionsHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if len(positional) == 0 || len(positional) > 2 {
		return writeExecUsageError(stderr, "usage: zero mcp permissions revoke <server> [<tool>] [--json]")
	}

	store, err := deps.newMCPStore()
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	serverName := positional[0]
	if len(positional) == 2 {
		revoked, err := store.RevokeTool(serverName, positional[1])
		if err != nil {
			return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
		}
		if options.json {
			payload := struct {
				Revoked    int    `json:"revoked"`
				Scope      string `json:"scope"`
				ServerName string `json:"serverName"`
				ToolName   string `json:"toolName"`
			}{Revoked: revoked, Scope: string(mcp.ScopeTool), ServerName: serverName, ToolName: positional[1]}
			if err := writePrettyJSON(stdout, payload); err != nil {
				return exitCrash
			}
		} else if _, err := fmt.Fprintf(stdout, "Revoked %d MCP tool permission grant(s) for %s/%s.\n", revoked, serverName, positional[1]); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	revoked, err := store.RevokeServer(serverName)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if options.json {
		payload := struct {
			Revoked    int    `json:"revoked"`
			Scope      string `json:"scope"`
			ServerName string `json:"serverName"`
		}{Revoked: revoked, Scope: string(mcp.ScopeServer), ServerName: serverName}
		if err := writePrettyJSON(stdout, payload); err != nil {
			return exitCrash
		}
	} else if _, err := fmt.Fprintf(stdout, "Revoked %d MCP server permission grant(s) for %s.\n", revoked, serverName); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runMCPPermissionsClear(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseMCPCommandOptions(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeMCPPermissionsHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if !options.confirm {
		return writeExecUsageError(stderr, "Pass --confirm to clear all MCP permission grants.")
	}
	store, err := deps.newMCPStore()
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	cleared, err := store.Clear()
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if options.json {
		payload := struct {
			Cleared int `json:"cleared"`
		}{Cleared: cleared}
		if err := writePrettyJSON(stdout, payload); err != nil {
			return exitCrash
		}
	} else if _, err := fmt.Fprintf(stdout, "Cleared %d MCP permission grant(s).\n", cleared); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func parsePluginListArgs(args []string) (pluginListOptions, bool, error) {
	options := pluginListOptions{}
	for _, arg := range args {
		switch arg {
		case "-h", "--help", "help":
			return options, true, nil
		case "--json":
			options.json = true
		default:
			return options, false, execUsageError{fmt.Sprintf("unknown plugins list flag %q", arg)}
		}
	}
	return options, false, nil
}

func parseHookListArgs(args []string) (hookListOptions, bool, error) {
	options := hookListOptions{}
	for _, arg := range args {
		switch arg {
		case "-h", "--help", "help":
			return options, true, nil
		case "--json":
			options.json = true
		default:
			return options, false, execUsageError{fmt.Sprintf("unknown hooks list flag %q", arg)}
		}
	}
	return options, false, nil
}

func parseMCPCommandOptions(args []string) (mcpCommandOptions, bool, error) {
	options := mcpCommandOptions{}
	for _, arg := range args {
		switch arg {
		case "-h", "--help", "help":
			return options, true, nil
		case "--json":
			options.json = true
		case "--confirm":
			options.confirm = true
		default:
			return options, false, execUsageError{fmt.Sprintf("unknown mcp flag %q", arg)}
		}
	}
	return options, false, nil
}

func parseMCPLegacyListArgs(args []string) (mcpLegacyListOptions, bool, error) {
	options := mcpLegacyListOptions{}
	for _, arg := range args {
		switch arg {
		case "-h", "--help", "help":
			return options, true, nil
		case "--json":
			options.json = true
		case "--tools":
			options.tools = true
		default:
			return options, false, execUsageError{fmt.Sprintf("unknown mcp list flag %q", arg)}
		}
	}
	return options, false, nil
}

func parseMCPPositionalCommand(args []string) (mcpCommandOptions, []string, bool, error) {
	options := mcpCommandOptions{}
	positional := []string{}
	for _, arg := range args {
		switch arg {
		case "-h", "--help", "help":
			return options, positional, true, nil
		case "--json":
			options.json = true
		case "--confirm":
			options.confirm = true
		default:
			if strings.HasPrefix(arg, "-") {
				return options, positional, false, execUsageError{fmt.Sprintf("unknown mcp permissions flag %q", arg)}
			}
			positional = append(positional, arg)
		}
	}
	return options, positional, false, nil
}

func writePluginsHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero plugins <command>

Commands:
  list                 List local Zero plugins
  info <id>            Show plugin details and lockfile metadata
  add <git-url|path>   Install a plugin (manifest-validated, pinned in plugins.lock)
  remove <id>          Remove an installed plugin and its lockfile entry
`)
	return err
}

func writePluginsListHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero plugins list [flags]

Flags:
      --json    Print local plugin data as JSON
  -h, --help    Show this help
`)
	return err
}

func writeHooksHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero hooks <command>

Commands:
  list      List configured Zero hooks
  add       Add or update a hook
  remove    Remove a hook by id
  enable    Enable a hook by id
  disable   Disable a hook by id
`)
	return err
}

func writeHooksListHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero hooks list [flags]

Flags:
      --json    Print hook config as JSON
  -h, --help    Show this help
`)
	return err
}

func writeMCPHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero mcp <command>

Commands:
  add <server>      Add or update an MCP server in user config
  remove <server>   Remove an MCP server from user config
  enable <server>   Enable a user-configured MCP server
  disable <server>  Disable a user-configured MCP server
  check <server>    Connect to one MCP server and list its tools
  list              List configured MCP servers, or tools with --tools
  oauth             Manage OAuth credentials for remote MCP servers
  permissions       Manage persistent MCP tool permissions
  secret set <name> Store a secret an MCP server references by name (envFrom)
  tools             Inspect configured MCP tools

Memory (memlawb): store the two secrets, then turn it on.
  zero mcp secret set memlawb-passphrase
  zero mcp secret set memlawb-api-key
  zero mcp enable memlawb
`)
	return err
}

func writeMCPLegacyListHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero mcp list [flags]

Flags:
      --json     Print MCP servers or tools as JSON
      --tools    Connect to enabled servers and list MCP tools
  -h, --help     Show this help
`)
	return err
}

func writeMCPToolsHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero mcp tools <command>

Commands:
  list    List configured MCP tools

Flags:
      --json    Print MCP tools as JSON
  -h, --help    Show this help
`)
	return err
}

func writeMCPPermissionsHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero mcp permissions <command>

Commands:
  list                  List all persistent MCP permissions
  revoke <server> [<tool>] Revoke an MCP server or tool permission
  clear --confirm       Clear all persistent MCP permissions

Flags:
      --json       Print command result as JSON
      --confirm    Confirm destructive clear operation
  -h, --help       Show this help
`)
	return err
}

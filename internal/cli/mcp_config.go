package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/mcp"
	"github.com/Gitlawb/zero/internal/redaction"
	"github.com/Gitlawb/zero/internal/tools"
)

type mcpAddOptions struct {
	json       bool
	serverName string
	server     config.MCPServerConfig
}

type mcpWritableConfig struct {
	file      config.FileConfig
	raw       map[string]json.RawMessage
	mcpRaw    map[string]json.RawMessage
	serverRaw map[string]json.RawMessage
}

var lockMCPConfigFile = config.LockFile

// projectMCPConfigExists reports whether the workspace's project ./.zero/config.json
// declares any MCP servers, so the trust notice fires only when project MCP config was
// actually skipped (mirroring projectHooksFileExists / projectPluginsDirExists). A
// missing or unparseable file, or one that declares no servers, returns false: there
// is nothing to notice about. This only reads the file; it never spawns a server, so
// it is safe to call on an untrusted workspace.
func projectMCPConfigExists(workspaceRoot string) bool {
	if workspaceRoot == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(workspaceRoot, ".zero", "config.json"))
	if err != nil {
		return false
	}
	var fc config.FileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return false
	}
	return len(fc.MCP.Servers) > 0
}

func runMCPAdd(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) (exitCode int) {
	options, help, err := parseMCPAddArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeMCPAddHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	configPath, err := deps.userConfigPath()
	if err != nil {
		return writeAppError(stderr, "failed to resolve user config: "+err.Error(), exitCrash)
	}
	// This edits the same user config document the config package mutates, with
	// the same read-modify-write + rename shape, so it takes the same
	// cross-process lock. Without it, an MCP config edit racing a provider or
	// preference write would silently drop whichever landed first (issue #832).
	unlock, err := lockMCPConfigFile(configPath)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	released := false
	defer func() {
		if released {
			return
		}
		// A failed release leaves the lock held for the rest of the process, so
		// exiting success here would claim a state the next config write cannot
		// reproduce. It must not mask a failure this command already reported.
		if releaseErr := unlock(); releaseErr != nil && exitCode == exitSuccess {
			exitCode = writeAppError(stderr, redaction.ErrorMessage(releaseErr, redaction.Options{}), exitCrash)
		}
	}()
	cfg, err := readMCPWritableConfig(configPath)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if cfg.file.MCP.Servers == nil {
		cfg.file.MCP.Servers = map[string]config.MCPServerConfig{}
	}
	updated, err := cfg.upsertServer(options.serverName, options.server)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if err := writeMCPWritableConfig(configPath, cfg); err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if err := unlock(); err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	released = true

	if options.json {
		payload := struct {
			ServerName string                 `json:"serverName"`
			Updated    bool                   `json:"updated"`
			ConfigPath string                 `json:"configPath"`
			Server     config.MCPServerConfig `json:"server"`
		}{
			ServerName: options.serverName,
			Updated:    updated,
			ConfigPath: configPath,
			Server:     redactMCPServerConfigs(map[string]config.MCPServerConfig{options.serverName: options.server})[options.serverName],
		}
		if err := writePrettyJSON(stdout, redaction.RedactValue(payload, redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	action := "Added"
	if updated {
		action = "Updated"
	}
	if _, err := fmt.Fprintf(stdout, "%s MCP server %s in %s.\n", action, options.serverName, configPath); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runMCPRemove(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) (exitCode int) {
	options, positional, help, err := parseMCPConfigPositionalCommand(args, "remove")
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeMCPRemoveHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if len(positional) != 1 {
		return writeExecUsageError(stderr, "usage: zero mcp remove <server> [--json]")
	}
	serverName := positional[0]
	if err := mcp.ValidateServerName(serverName); err != nil {
		return writeExecUsageError(stderr, err.Error())
	}

	configPath, err := deps.userConfigPath()
	if err != nil {
		return writeAppError(stderr, "failed to resolve user config: "+err.Error(), exitCrash)
	}
	// This edits the same user config document the config package mutates, with
	// the same read-modify-write + rename shape, so it takes the same
	// cross-process lock. Without it, an MCP config edit racing a provider or
	// preference write would silently drop whichever landed first (issue #832).
	unlock, err := lockMCPConfigFile(configPath)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	released := false
	defer func() {
		if released {
			return
		}
		// A failed release leaves the lock held for the rest of the process, so
		// exiting success here would claim a state the next config write cannot
		// reproduce. It must not mask a failure this command already reported.
		if releaseErr := unlock(); releaseErr != nil && exitCode == exitSuccess {
			exitCode = writeAppError(stderr, redaction.ErrorMessage(releaseErr, redaction.Options{}), exitCrash)
		}
	}()
	cfg, err := readMCPWritableConfig(configPath)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	removed, err := cfg.removeServer(serverName)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if removed {
		if err := writeMCPWritableConfig(configPath, cfg); err != nil {
			return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
		}
	}
	if err := unlock(); err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	released = true

	if options.json {
		payload := struct {
			ServerName string `json:"serverName"`
			Removed    bool   `json:"removed"`
			ConfigPath string `json:"configPath"`
		}{ServerName: serverName, Removed: removed, ConfigPath: configPath}
		if err := writePrettyJSON(stdout, payload); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if removed {
		if _, err := fmt.Fprintf(stdout, "Removed MCP server %s from %s.\n", serverName, configPath); err != nil {
			return exitCrash
		}
	} else if _, err := fmt.Fprintf(stdout, "No MCP server named %s is configured in %s.\n", serverName, configPath); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runMCPToggle(args []string, stdout io.Writer, stderr io.Writer, deps appDeps, disabled bool) (exitCode int) {
	commandName := "enable"
	if disabled {
		commandName = "disable"
	}
	options, positional, help, err := parseMCPConfigPositionalCommand(args, commandName)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeMCPToggleHelp(stdout, commandName); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if len(positional) != 1 {
		return writeExecUsageError(stderr, fmt.Sprintf("usage: zero mcp %s <server> [--json]", commandName))
	}
	serverName := positional[0]
	if err := mcp.ValidateServerName(serverName); err != nil {
		return writeExecUsageError(stderr, err.Error())
	}

	configPath, err := deps.userConfigPath()
	if err != nil {
		return writeAppError(stderr, "failed to resolve user config: "+err.Error(), exitCrash)
	}
	// This edits the same user config document the config package mutates, with
	// the same read-modify-write + rename shape, so it takes the same
	// cross-process lock. Without it, an MCP config edit racing a provider or
	// preference write would silently drop whichever landed first (issue #832).
	unlock, err := lockMCPConfigFile(configPath)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	released := false
	defer func() {
		if released {
			return
		}
		// A failed release leaves the lock held for the rest of the process, so
		// exiting success here would claim a state the next config write cannot
		// reproduce. It must not mask a failure this command already reported.
		if releaseErr := unlock(); releaseErr != nil && exitCode == exitSuccess {
			exitCode = writeAppError(stderr, redaction.ErrorMessage(releaseErr, redaction.Options{}), exitCrash)
		}
	}()
	cfg, err := readMCPWritableConfig(configPath)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	changed, found, err := cfg.setServerDisabled(serverName, disabled)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if !found {
		return writeExecUsageError(stderr, fmt.Sprintf("MCP server %s is not configured in user config", serverName))
	}
	if changed {
		if err := writeMCPWritableConfig(configPath, cfg); err != nil {
			return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
		}
	}
	if err := unlock(); err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	released = true

	if options.json {
		payload := struct {
			ServerName string `json:"serverName"`
			Disabled   bool   `json:"disabled"`
			Changed    bool   `json:"changed"`
			ConfigPath string `json:"configPath"`
		}{ServerName: serverName, Disabled: disabled, Changed: changed, ConfigPath: configPath}
		if err := writePrettyJSON(stdout, payload); err != nil {
			return exitCrash
		}
		return exitSuccess
	}

	state := "enabled"
	if disabled {
		state = "disabled"
	}
	if changed {
		if _, err := fmt.Fprintf(stdout, "MCP server %s is now %s in %s.\n", serverName, state, configPath); err != nil {
			return exitCrash
		}
	} else if _, err := fmt.Fprintf(stdout, "MCP server %s was already %s in %s.\n", serverName, state, configPath); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func runMCPCheck(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	if ctx == nil {
		ctx = context.Background()
	}
	options, positional, help, err := parseMCPConfigPositionalCommand(args, "check")
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeMCPCheckHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if len(positional) != 1 {
		return writeExecUsageError(stderr, "usage: zero mcp check <server> [--json]")
	}
	serverName := positional[0]
	if err := mcp.ValidateServerName(serverName); err != nil {
		return writeExecUsageError(stderr, err.Error())
	}

	cwd, err := deps.getwd()
	if err != nil {
		return writeAppError(stderr, "failed to resolve workspace: "+err.Error(), exitCrash)
	}
	// `mcp check` connects to (spawns) the named server below to enumerate its tools,
	// so it is a spawn site: gate the project layer behind the trust check (fail-closed)
	// so a cloned repo cannot have `zero mcp check <its-server>` run its command. No
	// --worktree reassignment on this command path, so trustRoot == cwd.
	mcpExcludeProject, trustCheckErrored := resolveTrust(cwd)
	cfg, err := deps.resolveMCPConfig(cwd, mcpExcludeProject)
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	raw, ok := cfg.Servers[serverName]
	if !ok {
		// The server may be missing because the project layer was gated out in an
		// untrusted workspace; emit the same one-line notice the other spawn sites use so
		// a dropped project server reads as "run zero trust", not a bare miss. The notice
		// self-gates: it stays silent unless a project MCP config was actually excluded.
		emitTrustNotice(stderr, trustSkip{
			excludedProjectConfig: mcpExcludeProject && projectMCPConfigExists(cwd),
			trustCheckErrored:     trustCheckErrored,
		})
		return writeAppError(stderr, fmt.Sprintf("MCP server %q is not configured", serverName), exitCrash)
	}
	if raw.Disabled {
		return writeAppError(stderr, fmt.Sprintf("MCP server %q is disabled", serverName), exitCrash)
	}
	scoped := config.MCPConfig{Servers: map[string]config.MCPServerConfig{serverName: raw}}
	if _, err := mcp.NormalizeConfig(scoped); err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}

	store, err := deps.newMCPStore()
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	registry := tools.NewRegistry()
	mcpRuntime, err := deps.registerMCPTools(ctx, registry, scoped, mcp.RegisterOptions{
		PermissionStore: store,
		Autonomy:        mcp.AutonomyLow,
	})
	if err != nil {
		closeMCPRuntime(stderr, mcpRuntime)
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	defer closeMCPRuntime(stderr, mcpRuntime)

	// A connect failure does not come back as err. RegisterTools is deliberately
	// best-effort so one unreachable server cannot stop Zero launching: it records
	// the failure on the runtime and returns nil. That is right for startup and
	// wrong to ignore here, because this is the command a user runs precisely when
	// a server is misbehaving, and reporting "is reachable" for a server that never
	// started sends them looking somewhere else.
	for _, skipped := range mcpRuntime.Skipped() {
		if skipped.Name != serverName {
			continue
		}
		message := fmt.Sprintf("MCP server %s is not reachable", serverName)
		if skipped.Err != nil {
			message = fmt.Sprintf("%s: %s", message, redaction.ErrorMessage(skipped.Err, redaction.Options{}))
		}
		if options.json {
			payload := struct {
				ServerName string `json:"serverName"`
				Status     string `json:"status"`
				Error      string `json:"error,omitempty"`
			}{ServerName: serverName, Status: "unreachable", Error: message}
			if err := writePrettyJSON(stdout, redaction.RedactValue(payload, redaction.Options{})); err != nil {
				return exitCrash
			}
			return exitCrash
		}
		return writeAppError(stderr, message, exitCrash)
	}

	items := mcpToolList(registry)
	if options.json {
		payload := struct {
			ServerName string            `json:"serverName"`
			Status     string            `json:"status"`
			ToolCount  int               `json:"toolCount"`
			Tools      []mcpToolListItem `json:"tools"`
		}{ServerName: serverName, Status: "ok", ToolCount: len(items), Tools: items}
		if err := writePrettyJSON(stdout, redaction.RedactValue(payload, redaction.Options{})); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if _, err := fmt.Fprintf(stdout, "MCP server %s is reachable. %d tool(s) available.\n", serverName, len(items)); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func parseMCPAddArgs(args []string) (mcpAddOptions, bool, error) {
	options := mcpAddOptions{}
	command := []string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case options.serverName == "":
			if strings.HasPrefix(arg, "-") {
				return options, false, execUsageError{"usage: zero mcp add <server> [flags] -- <command> [args...]"}
			}
			options.serverName = arg
		case arg == "--":
			command = append(command, args[i+1:]...)
			i = len(args)
		case arg == "--json":
			options.json = true
		case arg == "--disabled":
			options.server.Disabled = true
		case arg == "--type":
			value, err := requiredNextMCPFlagValue(args, &i, "--type")
			if err != nil {
				return options, false, err
			}
			options.server.Type = value
		case strings.HasPrefix(arg, "--type="):
			value, err := requiredInlineFlagValue(arg, "--type")
			if err != nil {
				return options, false, err
			}
			options.server.Type = value
		case arg == "--url":
			value, err := requiredNextMCPFlagValue(args, &i, "--url")
			if err != nil {
				return options, false, err
			}
			options.server.URL = value
		case strings.HasPrefix(arg, "--url="):
			value, err := requiredInlineFlagValue(arg, "--url")
			if err != nil {
				return options, false, err
			}
			options.server.URL = value
		case arg == "--env":
			value, err := requiredNextMCPFlagValue(args, &i, "--env")
			if err != nil {
				return options, false, err
			}
			if err := addMCPStringMapValue(&options.server.Env, value, "--env"); err != nil {
				return options, false, err
			}
		case strings.HasPrefix(arg, "--env="):
			value, err := requiredInlineFlagValue(arg, "--env")
			if err != nil {
				return options, false, err
			}
			if err := addMCPStringMapValue(&options.server.Env, value, "--env"); err != nil {
				return options, false, err
			}
		case arg == "--header":
			value, err := requiredNextMCPFlagValue(args, &i, "--header")
			if err != nil {
				return options, false, err
			}
			if err := addMCPStringMapValue(&options.server.Headers, value, "--header"); err != nil {
				return options, false, err
			}
		case strings.HasPrefix(arg, "--header="):
			value, err := requiredInlineFlagValue(arg, "--header")
			if err != nil {
				return options, false, err
			}
			if err := addMCPStringMapValue(&options.server.Headers, value, "--header"); err != nil {
				return options, false, err
			}
		case arg == "--auth":
			value, err := requiredNextMCPFlagValue(args, &i, "--auth")
			if err != nil {
				return options, false, err
			}
			options.server.Auth = value
		case strings.HasPrefix(arg, "--auth="):
			value, err := requiredInlineFlagValue(arg, "--auth")
			if err != nil {
				return options, false, err
			}
			options.server.Auth = value
		case strings.HasPrefix(arg, "-"):
			return options, false, execUsageError{fmt.Sprintf("unknown mcp add flag %q", arg)}
		default:
			command = append(command, args[i:]...)
			i = len(args)
		}
	}

	options.serverName = strings.TrimSpace(options.serverName)
	if options.serverName == "" {
		return options, false, execUsageError{"usage: zero mcp add <server> [flags] -- <command> [args...]"}
	}
	if err := mcp.ValidateServerName(options.serverName); err != nil {
		return options, false, err
	}

	options.server.Type = strings.ToLower(strings.TrimSpace(options.server.Type))
	options.server.Auth = strings.ToLower(strings.TrimSpace(options.server.Auth))
	if options.server.Type == "" {
		if strings.TrimSpace(options.server.URL) != "" {
			options.server.Type = string(mcp.ServerTypeHTTP)
		} else {
			options.server.Type = string(mcp.ServerTypeStdio)
		}
	}
	switch mcp.ServerType(options.server.Type) {
	case mcp.ServerTypeStdio:
		if len(options.server.Headers) > 0 {
			return options, false, execUsageError{"headers are only supported for http or sse transports"}
		}
		if len(command) == 0 {
			return options, false, execUsageError{"usage: zero mcp add <server> [flags] -- <command> [args...]"}
		}
		options.server.Command = command[0]
		options.server.Args = append([]string{}, command[1:]...)
	case mcp.ServerTypeHTTP, mcp.ServerTypeSSE:
		if len(options.server.Env) > 0 {
			return options, false, execUsageError{"env is only supported for stdio transport"}
		}
		if strings.TrimSpace(options.server.URL) == "" {
			return options, false, execUsageError{fmt.Sprintf("zero mcp add --type %s requires --url", options.server.Type)}
		}
		if len(command) > 0 {
			return options, false, execUsageError{fmt.Sprintf("zero mcp add --type %s does not accept a command", options.server.Type)}
		}
	default:
		return options, false, execUsageError{fmt.Sprintf("unsupported MCP server type %q", options.server.Type)}
	}

	if options.server.Auth != "" && options.server.Auth != mcp.ServerAuthOAuth {
		return options, false, execUsageError{fmt.Sprintf("MCP server %s has unsupported auth %q", options.serverName, options.server.Auth)}
	}
	if !options.server.Disabled {
		if _, err := mcp.NormalizeConfig(config.MCPConfig{Servers: map[string]config.MCPServerConfig{options.serverName: options.server}}); err != nil {
			return options, false, err
		}
	}
	return options, false, nil
}

func requiredNextMCPFlagValue(args []string, index *int, flag string) (string, error) {
	if *index+1 >= len(args) {
		return "", execUsageError{fmt.Sprintf("%s requires a value", flag)}
	}
	*index = *index + 1
	value := args[*index]
	if strings.TrimSpace(value) == "" {
		return "", execUsageError{fmt.Sprintf("%s requires a value", flag)}
	}
	return value, nil
}

func addMCPStringMapValue(target *map[string]string, raw string, flag string) error {
	key, value, ok := strings.Cut(raw, "=")
	if !ok && flag == "--header" {
		key, value, ok = strings.Cut(raw, ":")
		value = strings.TrimLeft(value, " \t")
	}
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		if flag == "--header" {
			return execUsageError{fmt.Sprintf("%s expects KEY=VALUE or \"Key: Value\"", flag)}
		}
		return execUsageError{fmt.Sprintf("%s expects KEY=VALUE", flag)}
	}
	if *target == nil {
		*target = map[string]string{}
	}
	(*target)[key] = value
	return nil
}

func parseMCPConfigPositionalCommand(args []string, command string) (mcpCommandOptions, []string, bool, error) {
	options := mcpCommandOptions{}
	positional := []string{}
	for _, arg := range args {
		switch arg {
		case "-h", "--help", "help":
			return options, positional, true, nil
		case "--json":
			options.json = true
		default:
			if strings.HasPrefix(arg, "-") {
				return options, positional, false, execUsageError{fmt.Sprintf("unknown mcp %s flag %q", command, arg)}
			}
			positional = append(positional, arg)
		}
	}
	return options, positional, false, nil
}

func readMCPWritableConfig(path string) (mcpWritableConfig, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return mcpWritableConfig{}, fmt.Errorf("config path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := mcpWritableConfig{}
			cfg.ensureRaw()
			return cfg, nil
		}
		return mcpWritableConfig{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg := mcpWritableConfig{}
	if err := json.Unmarshal(data, &cfg.file); err != nil {
		return mcpWritableConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg.raw); err != nil {
		return mcpWritableConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
	}
	cfg.ensureRaw()
	if data, ok := cfg.raw["mcp"]; ok && len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &cfg.mcpRaw); err != nil {
			return mcpWritableConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	}
	cfg.ensureRaw()
	if data, ok := cfg.mcpRaw["servers"]; ok && len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &cfg.serverRaw); err != nil {
			return mcpWritableConfig{}, fmt.Errorf("invalid config JSON %s: %w", path, err)
		}
	}
	cfg.ensureRaw()
	return cfg, nil
}

func writeMCPWritableConfig(path string, cfg mcpWritableConfig) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("config path is required")
	}
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create config directory %s: %w", dir, err)
		}
	}
	data, err := cfg.marshalJSON()
	if err != nil {
		return fmt.Errorf("encode config JSON: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".zero-config-*.tmp")
	if err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure config permissions %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write config %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	if err := replaceMCPWritableConfigFile(tmpPath, path); err != nil {
		return fmt.Errorf("write config %s: %w", path, err)
	}
	return nil
}

func (cfg *mcpWritableConfig) ensureRaw() {
	if cfg.raw == nil {
		cfg.raw = map[string]json.RawMessage{}
	}
	if cfg.mcpRaw == nil {
		cfg.mcpRaw = map[string]json.RawMessage{}
	}
	if cfg.serverRaw == nil {
		cfg.serverRaw = map[string]json.RawMessage{}
	}
}

func (cfg *mcpWritableConfig) upsertServer(name string, server config.MCPServerConfig) (bool, error) {
	cfg.ensureRaw()
	if cfg.file.MCP.Servers == nil {
		cfg.file.MCP.Servers = map[string]config.MCPServerConfig{}
	}
	existingRaw, updated := cfg.serverRaw[name]
	existingServer := cfg.file.MCP.Servers[name]
	if !updated {
		legacyRaw, legacyFound, err := cfg.legacyServerRaw(name)
		if err != nil {
			return false, err
		}
		if legacyFound {
			existingRaw = legacyRaw
			updated = true
			if len(legacyRaw) > 0 && string(legacyRaw) != "null" {
				_ = json.Unmarshal(legacyRaw, &existingServer)
			}
		}
	}
	mergedServer := overlayMCPServer(existingServer, server)
	cfg.file.MCP.Servers[name] = mergedServer
	data, err := mergeMCPServerRaw(existingRaw, mergedServer)
	if err != nil {
		return false, err
	}
	cfg.serverRaw[name] = data
	if _, err := cfg.removeLegacyServer(name); err != nil {
		return false, err
	}
	return updated, nil
}

func overlayMCPServer(existing config.MCPServerConfig, next config.MCPServerConfig) config.MCPServerConfig {
	existing.Type = next.Type
	existing.Command = next.Command
	existing.Args = append([]string{}, next.Args...)
	existing.Env = cloneStringMap(next.Env)
	existing.URL = next.URL
	existing.Headers = cloneStringMap(next.Headers)
	if strings.TrimSpace(next.Auth) != "" {
		existing.Auth = next.Auth
	}
	existing.Disabled = next.Disabled
	return existing
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func mergeMCPServerRaw(existing json.RawMessage, server config.MCPServerConfig) (json.RawMessage, error) {
	typed, err := json.Marshal(server)
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 || string(existing) == "null" {
		return typed, nil
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(existing, &merged); err != nil {
		return nil, err
	}
	if merged == nil {
		merged = map[string]json.RawMessage{}
	}
	for _, key := range []string{"type", "command", "args", "env", "url", "headers", "auth", "disabled"} {
		delete(merged, key)
	}
	var typedRaw map[string]json.RawMessage
	if err := json.Unmarshal(typed, &typedRaw); err != nil {
		return nil, err
	}
	_, preserveRawOAuth := merged["oauth"]
	for key, value := range typedRaw {
		if key == "oauth" && preserveRawOAuth {
			continue
		}
		merged[key] = value
	}
	return json.Marshal(merged)
}

func (cfg *mcpWritableConfig) removeServer(name string) (bool, error) {
	cfg.ensureRaw()
	removed := false
	if cfg.file.MCP.Servers != nil {
		if _, ok := cfg.file.MCP.Servers[name]; ok {
			delete(cfg.file.MCP.Servers, name)
			removed = true
		}
	}
	if _, ok := cfg.serverRaw[name]; ok {
		delete(cfg.serverRaw, name)
		removed = true
	}
	removedLegacy, err := cfg.removeLegacyServer(name)
	if err != nil {
		return false, err
	}
	return removed || removedLegacy, nil
}

func (cfg *mcpWritableConfig) setServerDisabled(name string, disabled bool) (bool, bool, error) {
	cfg.ensureRaw()
	raw, found := cfg.serverRaw[name]
	if !found {
		legacyRaw, legacyFound, err := cfg.takeLegacyServer(name)
		if err != nil {
			return false, false, err
		}
		switch {
		case legacyFound:
			raw = legacyRaw
		case config.IsDefaultMCPServer(name):
			// A built-in default server isn't written to the file until the user
			// overrides it. Treat it as present with an empty base so disabling it
			// writes a minimal {"disabled":true} entry that merges over the default —
			// letting `zero mcp disable <default>` work even though it lives in code.
			raw = nil
		default:
			return false, false, nil
		}
	}
	var server map[string]json.RawMessage
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &server); err != nil {
			return false, false, err
		}
	}
	if server == nil {
		server = map[string]json.RawMessage{}
	}

	current := false
	if rawDisabled, ok := server["disabled"]; ok && len(rawDisabled) > 0 && string(rawDisabled) != "null" {
		if err := json.Unmarshal(rawDisabled, &current); err != nil {
			return false, false, err
		}
	}
	changed := current != disabled
	if disabled {
		data, err := json.Marshal(true)
		if err != nil {
			return false, false, err
		}
		server["disabled"] = data
	} else {
		delete(server, "disabled")
	}
	data, err := json.Marshal(server)
	if err != nil {
		return false, false, err
	}
	cfg.serverRaw[name] = data

	if cfg.file.MCP.Servers == nil {
		cfg.file.MCP.Servers = map[string]config.MCPServerConfig{}
	}
	typed := cfg.file.MCP.Servers[name]
	if len(raw) > 0 && string(raw) != "null" {
		var decoded config.MCPServerConfig
		if err := json.Unmarshal(raw, &decoded); err == nil {
			typed = decoded
		}
	}
	typed.Disabled = disabled
	cfg.file.MCP.Servers[name] = typed
	return changed, true, nil
}

func (cfg *mcpWritableConfig) takeLegacyServer(name string) (json.RawMessage, bool, error) {
	cfg.ensureRaw()
	for _, key := range []string{"mcpServers", "mcp_servers"} {
		data, ok := cfg.raw[key]
		if !ok || len(data) == 0 || string(data) == "null" {
			continue
		}
		var servers map[string]json.RawMessage
		if err := json.Unmarshal(data, &servers); err != nil {
			return nil, false, err
		}
		raw, ok := servers[name]
		if !ok {
			continue
		}
		delete(servers, name)
		if len(servers) == 0 {
			delete(cfg.raw, key)
		} else {
			next, err := json.Marshal(servers)
			if err != nil {
				return nil, false, err
			}
			cfg.raw[key] = next
		}
		return raw, true, nil
	}
	return nil, false, nil
}

func (cfg *mcpWritableConfig) legacyServerRaw(name string) (json.RawMessage, bool, error) {
	cfg.ensureRaw()
	for _, key := range []string{"mcpServers", "mcp_servers"} {
		data, ok := cfg.raw[key]
		if !ok || len(data) == 0 || string(data) == "null" {
			continue
		}
		var servers map[string]json.RawMessage
		if err := json.Unmarshal(data, &servers); err != nil {
			return nil, false, err
		}
		raw, ok := servers[name]
		if ok {
			return raw, true, nil
		}
	}
	return nil, false, nil
}

func (cfg *mcpWritableConfig) removeLegacyServer(name string) (bool, error) {
	cfg.ensureRaw()
	removed := false
	for _, key := range []string{"mcpServers", "mcp_servers"} {
		data, ok := cfg.raw[key]
		if !ok || len(data) == 0 || string(data) == "null" {
			continue
		}
		var servers map[string]json.RawMessage
		if err := json.Unmarshal(data, &servers); err != nil {
			return false, err
		}
		if _, ok := servers[name]; !ok {
			continue
		}
		delete(servers, name)
		removed = true
		if len(servers) == 0 {
			delete(cfg.raw, key)
			continue
		}
		next, err := json.Marshal(servers)
		if err != nil {
			return false, err
		}
		cfg.raw[key] = next
	}
	return removed, nil
}

func (cfg *mcpWritableConfig) marshalJSON() ([]byte, error) {
	cfg.ensureRaw()
	if len(cfg.serverRaw) > 0 {
		data, err := json.Marshal(cfg.serverRaw)
		if err != nil {
			return nil, err
		}
		cfg.mcpRaw["servers"] = data
	} else {
		delete(cfg.mcpRaw, "servers")
	}
	if len(cfg.mcpRaw) > 0 {
		data, err := json.Marshal(cfg.mcpRaw)
		if err != nil {
			return nil, err
		}
		cfg.raw["mcp"] = data
	} else {
		delete(cfg.raw, "mcp")
	}
	data, err := json.MarshalIndent(cfg.raw, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func replaceMCPWritableConfigFile(tmpPath string, path string) error {
	if err := os.Rename(tmpPath, path); err == nil || runtime.GOOS != "windows" {
		return err
	}
	backupPath := tmpPath + ".bak"
	if err := os.Rename(path, backupPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if _, statErr := os.Stat(backupPath); statErr == nil {
			if restoreErr := os.Rename(backupPath, path); restoreErr != nil {
				return fmt.Errorf("%w; failed to restore original config: %v", err, restoreErr)
			}
		}
		return err
	}
	_ = os.Remove(backupPath)
	return nil
}

func writeMCPAddHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero mcp add <server> [flags] -- <command> [args...]
  zero mcp add <server> --url <url> [flags]

Flags:
      --auth <auth>        Authentication mode for remote servers (for example: oauth)
      --disabled           Persist the server as disabled
      --env KEY=VALUE      Add stdio environment variable (repeatable)
      --header KEY=VALUE   Add HTTP/SSE header (repeatable)
      --json               Print command result as JSON
      --type <type>        MCP transport: stdio, http, or sse
      --url <url>          Remote MCP endpoint URL
  -h, --help               Show this help
`)
	return err
}

func writeMCPRemoveHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero mcp remove <server> [flags]

Flags:
      --json    Print command result as JSON
  -h, --help    Show this help
`)
	return err
}

func writeMCPToggleHelp(w io.Writer, commandName string) error {
	_, err := fmt.Fprintf(w, `Usage:
  zero mcp %s <server> [flags]

Flags:
      --json    Print command result as JSON
  -h, --help    Show this help
`, commandName)
	return err
}

func writeMCPCheckHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero mcp check <server> [flags]

Flags:
      --json    Print command result as JSON
  -h, --help    Show this help
`)
	return err
}

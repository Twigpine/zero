package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Twigpine/zero/internal/mcp"
	"github.com/Twigpine/zero/internal/redaction"
	"github.com/Twigpine/zero/internal/tools"
)

type serveOptions struct {
	mcp              bool
	cwd              string
	addDirs          []string
	allowUnsafeTools bool
}

// serveRootsScope is a PathScope used only for MCP serve: the workspace root
// plus optional --add-dir roots, without the sandbox temp write roots that
// would otherwise pollute resources/list.
type serveRootsScope []string

func (s serveRootsScope) Roots() []string {
	roots := make([]string, len(s))
	copy(roots, s)
	return roots
}

func runServe(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	options, help, err := parseServeArgs(args)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	if help {
		if err := writeServeHelp(stdout); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	if !options.mcp {
		return writeExecUsageError(stderr, "serve requires --mcp. Use `zero serve --mcp`.")
	}

	workspaceRoot, err := resolveWorkspaceRoot(options.cwd, deps)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	scope, err := buildServeScope(workspaceRoot, options.addDirs)
	if err != nil {
		return writeExecUsageError(stderr, err.Error())
	}
	registry := newServeRegistry(workspaceRoot, scope, options.allowUnsafeTools)
	if options.allowUnsafeTools {
		if _, err := fmt.Fprintln(stderr, "[zero] Unsafe MCP server tools enabled because --allow-unsafe-tools was passed."); err != nil {
			return exitCrash
		}
	}

	err = mcp.Serve(context.Background(), deps.stdin, stdout, registry, mcp.ServeOptions{
		Name:              "zero",
		Version:           version,
		PermissionGranted: options.allowUnsafeTools,
		WorkspaceRoot:     workspaceRoot,
		Scope:             scope,
	})
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	return exitSuccess
}

func newServeRegistry(workspaceRoot string, scope tools.PathScope, allowUnsafeTools bool) *tools.Registry {
	registry := tools.NewRegistry()
	toolset := tools.CoreReadOnlyToolsScoped(workspaceRoot, scope)
	if allowUnsafeTools {
		toolset = tools.CoreToolsScoped(workspaceRoot, scope)
	}
	for _, tool := range toolset {
		registry.Register(tool)
	}
	return registry
}

// buildServeScope returns nil when there are no extra roots (workspace-only),
// otherwise a PathScope listing the workspace first followed by each --add-dir.
func buildServeScope(workspaceRoot string, addDirs []string) (tools.PathScope, error) {
	if len(addDirs) == 0 {
		return nil, nil
	}
	roots := make([]string, 0, 1+len(addDirs))
	workspaceRoot = filepath.Clean(workspaceRoot)
	if abs, err := filepath.Abs(workspaceRoot); err == nil {
		workspaceRoot = abs
	}

	// Keep lexical absolute paths in roots so path checks stay aligned with the
	// un-evaluated WorkspaceRoot used when Scope is nil. Symlink-resolved forms
	// are only keys for duplicate detection.
	workspaceResolved := workspaceRoot
	if resolved, err := filepath.EvalSymlinks(workspaceRoot); err == nil {
		workspaceResolved = resolved
	}
	roots = append(roots, workspaceRoot)

	seen := map[string]struct{}{workspaceResolved: {}}
	for _, dir := range addDirs {
		trimmed := strings.TrimSpace(dir)
		if trimmed == "" {
			return nil, execUsageError{"--add-dir requires a path"}
		}
		absolute, err := filepath.Abs(trimmed)
		if err != nil {
			return nil, execUsageError{fmt.Sprintf("--add-dir %q: %v", trimmed, err)}
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return nil, execUsageError{fmt.Sprintf("--add-dir %q: %v", trimmed, err)}
		}
		if !info.IsDir() {
			return nil, execUsageError{fmt.Sprintf("--add-dir %q is not a directory", trimmed)}
		}

		resolved := absolute
		if r, err := filepath.EvalSymlinks(absolute); err == nil {
			resolved = r
		}
		if _, ok := seen[resolved]; ok {
			continue
		}
		seen[resolved] = struct{}{}
		roots = append(roots, absolute)
	}
	return serveRootsScope(roots), nil
}

func parseServeArgs(args []string) (serveOptions, bool, error) {
	options := serveOptions{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "-h" || arg == "--help" || arg == "help":
			return options, true, nil
		case arg == "--mcp":
			options.mcp = true
		case arg == "--allow-unsafe-tools":
			options.allowUnsafeTools = true
		case arg == "-C" || arg == "--cwd":
			index++
			if index >= len(args) {
				return options, false, execUsageError{arg + " requires a path"}
			}
			options.cwd = args[index]
		case strings.HasPrefix(arg, "--cwd="):
			options.cwd = strings.TrimPrefix(arg, "--cwd=")
		case arg == "--add-dir":
			index++
			if index >= len(args) {
				return options, false, execUsageError{"--add-dir requires a path"}
			}
			options.addDirs = append(options.addDirs, args[index])
		case strings.HasPrefix(arg, "--add-dir="):
			value := strings.TrimPrefix(arg, "--add-dir=")
			if strings.TrimSpace(value) == "" {
				return options, false, execUsageError{"--add-dir requires a path"}
			}
			options.addDirs = append(options.addDirs, value)
		default:
			return options, false, execUsageError{fmt.Sprintf("unknown serve flag %q", arg)}
		}
	}
	return options, false, nil
}

func writeServeHelp(w io.Writer) error {
	_, err := fmt.Fprint(w, `Usage:
  zero serve --mcp [flags]

Starts Zero as an MCP stdio server.

Flags:
      --mcp                   Run the MCP stdio server
  -C, --cwd <path>            Set the workspace directory (resources + tools)
      --add-dir <path>        Extra resource/tool root (repeatable)
      --allow-unsafe-tools    Expose write and shell tools to the MCP host
  -h, --help                  Show this help
`)
	return err
}

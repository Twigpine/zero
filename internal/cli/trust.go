package cli

import (
	"fmt"
	"io"

	"github.com/Twigpine/zero/internal/redaction"
	"github.com/Twigpine/zero/internal/workspacetrust"
)

// runTrust implements `zero trust`, letting the user opt a workspace into running
// its project-scoped executable config (hooks, plugins, MCP servers). Trust is
// keyed on the exact normalized working directory, matching the exact-match trust
// model and cwd-relative project-config discovery.
//
//	zero trust                trust the current working directory
//	zero trust list           print the trusted roots, one per line
//	zero trust remove [path]  untrust the current directory, or a named path
func runTrust(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	if len(args) == 0 {
		return trustCurrentDir(stdout, stderr, deps)
	}
	switch args[0] {
	case "list":
		return trustList(stdout, stderr)
	case "remove", "rm", "untrust":
		return trustRemove(args[1:], stdout, stderr, deps)
	case "-h", "--help", "help":
		// Explicit help is a success path: write usage to stdout and exit 0, matching
		// the other subcommands (mcp, sandbox, skills, cron, ...). Only the unknown-
		// subcommand error path below writes usage to stderr with a usage exit code.
		writeTrustUsage(stdout)
		return exitSuccess
	default:
		if _, err := fmt.Fprintf(stderr, "zero trust: unknown subcommand %q\n\n", args[0]); err != nil {
			return exitCrash
		}
		writeTrustUsage(stderr)
		return exitUsage
	}
}

// trustCurrentDir trusts the exact current working directory.
func trustCurrentDir(stdout io.Writer, stderr io.Writer, deps appDeps) int {
	cwd, err := deps.getwd()
	if err != nil {
		// Flatten with %s: ErrorMessage stringifies inline, so %w would be a no-op.
		return writeAppError(stderr, redaction.ErrorMessage(fmt.Errorf("resolve workspace: %s", err), redaction.Options{}), exitCrash)
	}
	if err := workspacetrust.Trust(cwd); err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if _, err := fmt.Fprintf(stdout, "Trusted %s\n", cwd); err != nil {
		return exitCrash
	}
	return exitSuccess
}

// trustList prints each trusted root, one per line, or a friendly line when none
// are trusted.
func trustList(stdout io.Writer, stderr io.Writer) int {
	roots, err := workspacetrust.List()
	if err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if len(roots) == 0 {
		if _, err := fmt.Fprintln(stdout, "No trusted workspaces."); err != nil {
			return exitCrash
		}
		return exitSuccess
	}
	for _, root := range roots {
		if _, err := fmt.Fprintln(stdout, root); err != nil {
			return exitCrash
		}
	}
	return exitSuccess
}

// trustRemove untrusts the current directory, or a named path when one is given.
// The path argument is passed to workspacetrust.Untrust verbatim, which normalizes
// it (filepath.Abs + filepath.EvalSymlinks) the same way the store does, so a
// relative or trailing-slash argument still matches the stored entry.
func trustRemove(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	var target string
	switch len(args) {
	case 0:
		cwd, err := deps.getwd()
		if err != nil {
			// Flatten with %s: ErrorMessage stringifies inline, so %w would be a no-op.
			return writeAppError(stderr, redaction.ErrorMessage(fmt.Errorf("resolve workspace: %s", err), redaction.Options{}), exitCrash)
		}
		target = cwd
	case 1:
		target = args[0]
	default:
		if _, err := fmt.Fprintln(stderr, "usage: zero trust remove [path]"); err != nil {
			return exitCrash
		}
		return exitUsage
	}
	if err := workspacetrust.Untrust(target); err != nil {
		return writeAppError(stderr, redaction.ErrorMessage(err, redaction.Options{}), exitCrash)
	}
	if _, err := fmt.Fprintf(stdout, "Untrusted %s\n", target); err != nil {
		return exitCrash
	}
	return exitSuccess
}

func writeTrustUsage(w io.Writer) {
	_, _ = fmt.Fprint(w, `Usage:
  zero trust                Trust the current working directory
  zero trust list           List trusted workspace roots
  zero trust remove [path]  Untrust the current directory, or a named path

Trust lets Zero run a workspace's project-scoped hooks, plugins, and MCP
servers (./.zero/hooks.json, ./.zero/plugins/, project MCP config). Trust is
exact per directory.
`)
}

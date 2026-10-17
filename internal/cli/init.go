package cli

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/agentinit"
)

// runInit implements `zero init`: investigate the repo and generate an
// AGENTS.md. It builds a bootstrap prompt seeded with repo facts and forwards
// it to the normal `exec` machinery (provider, tools, sandbox, AGENTS.md write
// path are all the standard ones). Any flags the user passes (--model, -w,
// --add-dir, …) are forwarded to exec unchanged; the built prompt is appended
// as the trailing positional, so a user-supplied prompt is not expected.
func runInit(args []string, stdout io.Writer, stderr io.Writer, deps appDeps) int {
	if wantsHelp(args) {
		_, _ = io.WriteString(stdout, initHelp)
		return exitSuccess
	}

	// Resolve the workspace the same way exec will, so the seeded facts describe
	// the right repo. A failure here is non-fatal — BuildPrompt degrades to
	// "investigate yourself".
	cwd := initCwdFromArgs(args)
	workspaceRoot, err := resolveWorkspaceRoot(cwd, deps)
	if err != nil {
		workspaceRoot = cwd
	}
	prompt := agentinit.BuildPrompt(context.Background(), workspaceRoot, time.Now())

	// Forward the user's flags, then the built prompt as the trailing positional.
	execArgs := append(append([]string{}, args...), prompt)
	return runExec(execArgs, stdout, stderr, deps)
}

// initCwdFromArgs extracts a -C/--cwd value if present, else "" (exec defaults
// to the process cwd). Kept minimal — exec owns the real flag parsing.
func initCwdFromArgs(args []string) string {
	for i, a := range args {
		switch {
		case a == "-C" || a == "--cwd":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(a, "--cwd="):
			return strings.TrimPrefix(a, "--cwd=")
		}
	}
	return ""
}

func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			return true
		}
	}
	return false
}

const initHelp = `zero init — investigate the repository and generate an AGENTS.md.

Runs an agent turn seeded with a local repo scan (languages, build/test tools,
CI, workspace layout), investigates the gaps, and writes a concise AGENTS.md at
the repo root so future runs start with project context.

Usage:
  zero init [exec flags]

Accepts the same flags as 'zero exec' (e.g. --model, -w/--worktree, --add-dir).
`

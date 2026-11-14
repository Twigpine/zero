package gitsafe

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
)

// IndexEntriesCommand lists cached index paths under one literal path. Its argv
// is fixed: it cannot inspect worktree contents, run filters/textconv, recurse
// into submodules, or accept additional options. Repository driver definitions
// therefore do not require the refusal used by Command for status/diff.
//
// Repository redirects, hooks, fsmonitor and optional index writes retain the
// same protections as Command. The caller must run it and propagate Git errors;
// a failed query says nothing about which files are tracked. Literal pathspecs
// require Git 1.9, the existing floor of memory's privacy query.
func IndexEntriesCommand(ctx context.Context, dir, path string) *exec.Cmd {
	env := environment()
	filtered := env[:0]
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if runtime.GOOS == "windows" {
			name = strings.ToUpper(name)
		}
		switch name {
		case "GIT_LITERAL_PATHSPECS", "GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS", "GIT_ICASE_PATHSPECS":
			continue
		}
		filtered = append(filtered, entry)
	}
	checked := &Checked{dir: dir, env: filtered}
	return checked.Command(ctx, "--literal-pathspecs", "ls-files", "--cached", "-z", "--", path)
}

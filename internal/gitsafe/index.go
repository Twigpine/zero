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
// same protections as Command. The caller must propagate Git errors and reject
// stderr diagnostics even on a zero exit: Git can report a missing sparse tree
// while returning an empty result successfully. Literal pathspecs require Git
// 1.9, the existing floor of memory's privacy query.
//
// Sparse-index expansion can need missing promisor tree objects. Suppress lazy
// fetching where supported and deny every transport as a backstop: otherwise a
// fetch can run the repository's upload-pack or a remote helper. The protocol
// allow-list already exists in Git 2.6.7, before partial clones; older Git ignores
// these environment variables but cannot perform a promisor fetch. No newer
// command-line flag or Git version requirement is introduced.
func IndexEntriesCommand(ctx context.Context, dir, path string) *exec.Cmd {
	env := environment()
	filtered := env[:0]
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if runtime.GOOS == "windows" {
			name = strings.ToUpper(name)
		}
		switch name {
		case "GIT_LITERAL_PATHSPECS", "GIT_GLOB_PATHSPECS", "GIT_NOGLOB_PATHSPECS", "GIT_ICASE_PATHSPECS",
			"GIT_ALLOW_PROTOCOL", "GIT_NO_LAZY_FETCH":
			continue
		}
		filtered = append(filtered, entry)
	}
	filtered = append(filtered, "GIT_ALLOW_PROTOCOL=", "GIT_NO_LAZY_FETCH=1")
	checked := &Checked{dir: dir, env: filtered}
	return checked.Command(ctx, "-c", "advice.sparseIndexExpanded=false", "--literal-pathspecs", "ls-files", "--cached", "-z", "--", path)
}

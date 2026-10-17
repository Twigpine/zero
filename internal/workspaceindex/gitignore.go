package workspaceindex

import (
	"bytes"
	"context"
	"path"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/gitsafe"
)

// gitIgnoredPathsTimeout bounds how long Scan waits on git before giving up and falling
// back to the non-git behavior. Scan also runs for the per-turn workspace seed, so a slow or
// degenerate tree (network filesystem, huge monorepo) must not stall a turn.
const gitIgnoredPathsTimeout = 5 * time.Second

// checkRepository is gitsafe.Check. It is a variable so tests can stand in for a
// repository gitsafe refuses, without configuring one that names a command.
var checkRepository = gitsafe.Check

// gitIgnoredPaths asks git which paths under root it considers ignored, honoring every
// source git itself resolves (nested .gitignore files, .git/info/exclude,
// core.excludesFile). Returns a set of paths relative to root: whole ignored directories
// collapsed to one entry plus individually ignored files, using forward slashes and no
// trailing slash (the `rel` form Scan computes).
//
// Returns (nil, false) when root is not inside a git work tree, git is missing or older
// than the floor below, the repository's own config defines a filter or diff command
// (see gitsafe), or a command fails or times out. Scan then behaves as it does
// outside git. A scan must never fail or hang because of this lookup.
//
// Git version floor: 2.16. `status --ignored=<mode>` landed in 2.16 and `--porcelain=v1`
// in 2.11; an older git rejects the arguments, which lands on the (nil, false) path.
// GIT_OPTIONAL_LOCKS (2.15) is an environment variable, so an older git ignores it.
//
// Why `status --ignored=matching` and not `ls-files --others --ignored --directory`:
// ls-files stops descending at the first untracked directory, so an ignored directory or
// file nested inside one (svc/build-out/, app/cache.db) is never reported. status descends,
// collapses a fully ignored directory to a single entry, and lists partially ignored files
// individually. `-uno` is not usable as a speedup: with it, status reports no ignored
// entries at all.
func gitIgnoredPaths(root string) (map[string]bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), gitIgnoredPathsTimeout)
	defer cancel()

	// Both calls below go through one gitsafe check of the repository's config, so
	// the repository under scan cannot make them run a program (core.fsmonitor, its
	// hooks, or a filter or diff driver its own config defines) and an inherited
	// GIT_DIR or GIT_WORK_TREE cannot point them at another repository. A repository
	// whose own config defines a filter or diff command takes the non-git path.
	// gitsafe also sets GIT_OPTIONAL_LOCKS=0, so status never takes .git/index.lock to
	// refresh the index: Scan runs every turn, often while the user or the model runs
	// git in the same repository, and that lock makes a concurrent `git commit` or
	// `git add` fail with "index.lock exists".
	checked, err := checkRepository(ctx, root)
	if err != nil {
		return nil, false
	}

	// status reports paths relative to the repository top level even when run from a
	// subdirectory, while Scan's `rel` is relative to root. show-prefix is root's own
	// location inside the repo ("" at the top level, "svc/" for <repo>/svc).
	prefixOut, err := checked.Command(ctx, "rev-parse", "--show-prefix").Output()
	if err != nil {
		return nil, false
	}
	prefix := strings.TrimSpace(string(prefixOut))

	// -z: NUL-terminated and unquoted. Without it git C-quotes non-ASCII bytes
	// (core.quotePath), e.g. "caf\303\251/", which would never match the raw `rel`.
	// The trailing "." limits the report to root when root is a repository subdirectory.
	// --untracked-files=normal: status.showUntrackedFiles=no makes git reject
	// --ignored=matching; without an explicit override Scan falls back to the fixed list.
	// --ignore-submodules=dirty keeps git out of submodule work trees, whose own config
	// the gitsafe check does not read.
	out, err := checked.Command(ctx, "status", "--porcelain=v1", "-z", "--untracked-files=normal", "--ignored=matching", "--ignore-submodules=dirty", ".").Output()
	if err != nil {
		return nil, false
	}

	return parseIgnoredStatus(out, prefix), true
}

// parseIgnoredStatus extracts the ignored paths from `git status --porcelain=v1 -z` output,
// made relative to prefix (root's location inside the repository).
//
// Each record is "XY <path>". A rename or copy (R or C in either status column) is followed
// by one more NUL field holding the original path with no XY prefix. That field is consumed,
// not inspected: an original name that itself begins with "!! " would otherwise read as an
// ignored entry and hide a real file from Scan.
func parseIgnoredStatus(out []byte, prefix string) map[string]bool {
	paths := make(map[string]bool)
	skipOrigPath := false
	for entry := range bytes.SplitSeq(out, []byte{0}) {
		if skipOrigPath {
			skipOrigPath = false
			continue
		}
		if len(entry) < 3 {
			continue
		}
		if x, y := entry[0], entry[1]; x == 'R' || x == 'C' || y == 'R' || y == 'C' {
			skipOrigPath = true
			continue
		}
		// Ignored entries are "!! <path>"; untracked "??" and changed entries are skipped.
		rest, ok := bytes.CutPrefix(entry, []byte("!! "))
		if !ok {
			continue
		}
		rel, ok := strings.CutPrefix(string(rest), prefix)
		if !ok {
			continue
		}
		rel = path.Clean(strings.TrimSuffix(rel, "/"))
		if rel == "." || rel == "" {
			continue
		}
		paths[rel] = true
	}
	return paths
}

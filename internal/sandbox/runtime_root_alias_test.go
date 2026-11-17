package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// aliasTo returns a second path that IS target, spelled in a way
// canonicalSandboxWorkspaceRoot does not fold, or "" when this platform offers
// none.
//
// A plain symlink is no good: EvalSymlinks folds it, so the spelling comparison
// already wins and nothing downstream is exercised. The two aliases that survive
// canonicalization are a Windows directory junction, which needs no privilege,
// and an upper-cased spelling on a case-insensitive volume, which is the macOS
// default.
func aliasTo(t *testing.T, target string) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		link := filepath.Join(t.TempDir(), "alias")
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Logf("mklink /J unavailable: %v %s", err, out)
			return ""
		}
		// A junction that did not actually land on the target proves nothing.
		targetInfo, err := os.Stat(target)
		if err != nil {
			t.Fatalf("stat target: %v", err)
		}
		linkInfo, err := os.Stat(link)
		if err != nil || !os.SameFile(targetInfo, linkInfo) {
			t.Fatalf("mklink reported success but %s is not %s", link, target)
		}
		return link
	}

	upper := strings.ToUpper(target)
	if upper == target {
		return ""
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	upperInfo, err := os.Stat(upper)
	if err != nil || !os.SameFile(targetInfo, upperInfo) {
		// Case-sensitive volume: the upper-cased name is a different directory, or
		// none at all. Correctly not an alias.
		return ""
	}
	return upper
}

// TestRuntimeRootRefusesAWorkspaceReachedByAnAlias is the regression for the gap
// the macOS Smoke run exposed and the junction gap found alongside it.
// canonicalSandboxWorkspaceRoot folds the aliases EvalSymlinks folds and no
// others, so a runtime root that reaches the workspace under a junction, or under
// a different casing on a case-insensitive volume, measured as OUTSIDE the
// workspace and the runtime tree was allowed to live inside the tree the sandbox
// exists to confine.
//
// Both alias shapes are covered on purpose. An alias whose target IS the
// workspace root is caught by the identity walk; an alias into a SUBDIRECTORY is
// not, because the walk climbs a spelling and a junction has no spelling chain
// back into its target's parent. Only the physical-path resolution catches that
// one, and a test that exercised the root shape alone reported green while it was
// broken.
func TestRuntimeRootRefusesAWorkspaceReachedByAnAlias(t *testing.T) {
	for _, shape := range []struct {
		name   string
		suffix []string
	}{
		{name: "alias to the workspace root"},
		{name: "alias into a workspace subdirectory", suffix: []string{"build", "tmp"}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			workspaceRoot := canonicalSandboxWorkspaceRoot(filepath.Join(t.TempDir(), "workspace"))
			target := filepath.Join(append([]string{workspaceRoot}, shape.suffix...)...)
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatalf("create target: %v", err)
			}

			alias := aliasTo(t, target)
			if alias == "" {
				t.Skip("no alias spelling is constructible here")
			}

			// Precondition, asserted rather than assumed: the plain spelling
			// comparison has to MISS. If canonicalization folds this alias the old
			// code already handled it and the test proves nothing.
			probe := filepath.Join(canonicalSandboxWorkspaceRoot(alias), "zero", "runtime", "v1", "0123456789abcdef")
			if pathWithinRoot(workspaceRoot, probe) {
				t.Skipf("canonicalization already folds %s into %s", alias, workspaceRoot)
			}

			t.Setenv("TMP", alias)
			t.Setenv("TEMP", alias)
			t.Setenv("TMPDIR", alias)

			root, err := fallbackSandboxRuntimeRoot(workspaceRoot)
			if err == nil {
				t.Fatalf("fallback returned runtime root %s for a TEMP that reaches %s through %s; the sandbox would keep its own cache inside the tree it is confining", root, target, alias)
			}
			if !strings.Contains(err.Error(), "inside workspace") {
				t.Fatalf("fallback refused for the wrong reason: %v", err)
			}
		})
	}
}

// TestDeterministicRuntimeRootRejectsAnAliasedCache covers the other call site.
// Reverting only deterministicSandboxRuntimeRoot left the whole package green
// before this existed, so the cache-derived root had no alias coverage at all.
func TestDeterministicRuntimeRootRejectsAnAliasedCache(t *testing.T) {
	workspaceRoot := canonicalSandboxWorkspaceRoot(filepath.Join(t.TempDir(), "workspace"))
	inner := filepath.Join(workspaceRoot, "cachehome")
	if err := os.MkdirAll(inner, 0o700); err != nil {
		t.Fatalf("create target: %v", err)
	}

	// The derived root is <cache>/zero/runtime/v1/<hash>, so aliasing <cache>/zero
	// is what puts the whole tree inside the workspace.
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		t.Fatalf("create cache: %v", err)
	}
	// A junction on Windows, a symlink elsewhere. Both point at inner by its REAL
	// spelling on purpose. Stacking a case alias on top would build the one shape
	// the doc on runtimeRootWithinWorkspace says stays open off Windows, an alias
	// into a workspace SUBDIRECTORY that no spelling chain reaches, and the test
	// would then be asserting a guarantee macOS does not make.
	link := filepath.Join(cacheRoot, "zero")
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, inner).CombinedOutput(); err != nil {
			t.Skipf("mklink /J unavailable: %v %s", err, out)
		}
	} else if err := os.Symlink(inner, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	innerInfo, err := os.Stat(inner)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	linkInfo, err := os.Stat(link)
	if err != nil || !os.SameFile(innerInfo, linkInfo) {
		t.Fatalf("%s is not an alias of %s", link, inner)
	}

	root, usableOutside := deterministicSandboxRuntimeRoot(workspaceRoot, cacheRoot)
	if usableOutside {
		t.Fatalf("cache-derived runtime root %s reported as outside %s while it resolves inside it", root, workspaceRoot)
	}
}

// TestRuntimeRootWithinWorkspaceKeepsAGenuinelyOutsideRootUsable is the other
// half. The three checks only ever ADD containment answers, so the one thing that
// could go wrong is reporting containment for a root that is merely adjacent.
func TestRuntimeRootWithinWorkspaceKeepsAGenuinelyOutsideRootUsable(t *testing.T) {
	parent := t.TempDir()
	workspaceRoot := canonicalSandboxWorkspaceRoot(filepath.Join(parent, "workspace"))
	sibling := filepath.Join(parent, "workspace-runtime", "zero", "runtime", "v1", "0123456789abcdef")
	if err := os.MkdirAll(workspaceRoot, 0o700); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatalf("create sibling: %v", err)
	}

	if runtimeRootWithinWorkspace(workspaceRoot, sibling) {
		t.Fatalf("%s reported as inside %s; a sibling sharing a name prefix is not contained", sibling, workspaceRoot)
	}
	if _, usableOutside := deterministicSandboxRuntimeRoot(workspaceRoot, filepath.Join(parent, "cache")); !usableOutside {
		t.Fatalf("a cache root outside the workspace was reported unusable")
	}
}

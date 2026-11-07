//go:build windows

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ELEVATED SETUP MUST NOT CREATE THROUGH A JUNCTION THE CALLER PLANTED.
//
// ensureWindowsSandboxRuntimeCandidates runs as Administrator, and os.MkdirAll
// follows links. Both candidate ancestries are prepared by the invoking user, so
// a junction at a not-yet-created component below the cache directory — or at the
// fallback anchor — made elevated setup build the runtime tree beneath a
// redirected, Administrator-writable target. The ACL applier's no-follow check
// runs later and spots the redirection, but the privileged create has already
// happened by then, outside its rollback boundary.
//
// A junction needs no privilege on Windows, so this reproduces the caller's half
// of the race on an ordinary unelevated box.
func TestRuntimeCandidateCreationRefusesAJunctionedComponent(t *testing.T) {
	cacheRoot := t.TempDir()
	target := t.TempDir()

	previous := sandboxUserCacheDir
	t.Cleanup(func() { sandboxUserCacheDir = previous })
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }

	workspace := t.TempDir()
	candidate, ok := deterministicSandboxRuntimeRoot(canonicalSandboxWorkspaceRoot(workspace), canonicalSandboxWorkspaceRoot(cacheRoot))
	if !ok {
		t.Fatalf("SETUP INVALID: no deterministic candidate for %s under %s", workspace, cacheRoot)
	}
	// The component the caller controls: the first one Zero owns, below the
	// operator's cache directory.
	owned := filepath.Join(canonicalSandboxWorkspaceRoot(cacheRoot), "zero")
	if !strings.HasPrefix(strings.ToLower(candidate), strings.ToLower(owned)) {
		t.Fatalf("SETUP INVALID: candidate %s does not sit under the owned component %s", candidate, owned)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", owned, target).CombinedOutput(); err != nil {
		t.Fatalf("SETUP INVALID: mklink /J: %v\n%s", err, out)
	}

	err := ensureRuntimeCandidateDir(candidate)
	if err == nil {
		t.Fatal("elevated setup created the runtime tree through a junction the caller planted")
	}

	// The whole point: nothing may appear beneath the redirected target.
	entries, readErr := os.ReadDir(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("setup created %v beneath the redirected target", names)
	}
}

// And an ordinary cache directory still gets its candidate, or the refusal above
// would be satisfied by a creator that refuses everything.
func TestRuntimeCandidateCreationStillCreatesAnOrdinaryTree(t *testing.T) {
	cacheRoot := t.TempDir()
	previous := sandboxUserCacheDir
	t.Cleanup(func() { sandboxUserCacheDir = previous })
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }

	workspace := t.TempDir()
	candidate, ok := deterministicSandboxRuntimeRoot(canonicalSandboxWorkspaceRoot(workspace), canonicalSandboxWorkspaceRoot(cacheRoot))
	if !ok {
		t.Fatalf("SETUP INVALID: no deterministic candidate for %s", workspace)
	}
	if err := ensureRuntimeCandidateDir(candidate); err != nil {
		t.Fatalf("an ordinary cache tree was refused: %v", err)
	}
	info, err := os.Stat(candidate)
	if err != nil || !info.IsDir() {
		t.Fatalf("the candidate was not created: err=%v", err)
	}
}

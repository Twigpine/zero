//go:build windows

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// DETERMINISM IS NOT OWNERSHIP.
//
// The fallback runtime root moved from a fresh os.MkdirTemp parent to a
// predictable name so that elevated setup and the later command agree on it.
// That is required. But a predictable path beneath the shared system temp is
// not a private allocation: whatever another local user, or a stray earlier
// process, has already placed at that name is what preparation then leases,
// creates, chmods and cleans through. The anchor beneath which the tree lives
// must therefore be proven to be this user's private directory before anything
// is built under it, on BOTH paths that can hand back a fallback root.
//
// Junctions need no privilege on Windows, so the "attacker-precreated parent"
// jatmn described is modelled as a junction at the anchor pointing into a
// directory the test owns and watches. Either trigger must refuse, and the
// watched directory must stay empty.

// fallbackAnchorFixture redirects TEMP so the anchor lands inside a directory
// this test owns, plants a junction at the anchor into a watched target, and
// returns the target. The junction is the shape that defeats a pathname walk:
// it is not a symlink to os.ModeSymlink, and EvalSymlinks will not traverse it.
func fallbackAnchorFixture(t *testing.T) (target string) {
	t.Helper()
	tempRoot := t.TempDir()
	target = t.TempDir()
	t.Setenv("TMP", tempRoot)
	t.Setenv("TEMP", tempRoot)

	// The anchor's parent is the PHYSICAL temp dir, so compare against the
	// resolved form of the redirect rather than its spelling: t.TempDir can
	// come back as an 8.3 short name or in a different case from what the
	// handle reports, and a prefix check on the raw string would fail for a
	// correct anchor.
	anchor := fallbackRuntimeAnchor()
	resolvedTempRoot, err := physicalTempDir()
	if err != nil {
		t.Fatalf("SETUP INVALID: cannot resolve the redirected TEMP %s: %v", tempRoot, err)
	}
	if !pathWithinRoot(resolvedTempRoot, anchor) {
		t.Fatalf("SETUP INVALID: anchor %s is not under the redirected TEMP %s (resolved %s)", anchor, tempRoot, resolvedTempRoot)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", anchor, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v\n%s", err, out)
	}
	return target
}

func assertNothingUnder(t *testing.T, target string) {
	t.Helper()
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("preparation built %v beneath the junction target, which is somebody else's directory", names)
	}
}

// Trigger one: the user cache resolves INSIDE the workspace, so
// sandboxRuntimeRootFor itself returns the fallback root and the first lease
// attempt is already against it.
func TestFallbackAnchorIsRefusedWhenTheCacheLandsInsideTheWorkspace(t *testing.T) {
	isolateSandboxRuntimeRoots(t)
	target := fallbackAnchorFixture(t)
	workspace := t.TempDir()

	previous := sandboxUserCacheDir
	t.Cleanup(func() { sandboxUserCacheDir = previous })
	sandboxUserCacheDir = func() (string, error) { return filepath.Join(workspace, ".cache"), nil }

	_, release, err := prepareSandboxRuntime(workspace)
	if err == nil {
		release()
		t.Fatal("preparation went through a junction at the fallback anchor and reported success")
	}
	if !strings.Contains(err.Error(), "fallback anchor") {
		t.Errorf("the refusal does not name the anchor, so the operator cannot act on it: %v", err)
	}
	assertNothingUnder(t, target)
}

// Trigger two: the cache root cannot be leased (a FILE sits where the cache
// runtime tree would go), so preparation falls back explicitly.
func TestFallbackAnchorIsRefusedWhenTheCacheRootCannotBeLeased(t *testing.T) {
	isolateSandboxRuntimeRoots(t)
	target := fallbackAnchorFixture(t)
	workspace := t.TempDir()

	// Make the cache-derived root unleasable at its ROOT: the user cache is a
	// regular file, so nothing beneath it can be created and the first lease
	// fails, which is the path that asks for the fallback explicitly. The
	// first draft planted the file at the derived leaf instead, and the
	// preparation failed on that mkdir before ever reaching the fallback
	// branch, so the trigger under test never fired.
	cache := filepath.Join(t.TempDir(), "cache-is-a-file")
	if err := os.WriteFile(cache, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	previous := sandboxUserCacheDir
	t.Cleanup(func() { sandboxUserCacheDir = previous })
	sandboxUserCacheDir = func() (string, error) { return cache, nil }

	_, release, err := prepareSandboxRuntime(workspace)
	if err == nil {
		release()
		t.Fatal("preparation fell back through a junction at the anchor and reported success")
	}
	if !strings.Contains(err.Error(), "fallback anchor") {
		t.Errorf("the refusal does not name the anchor: %v", err)
	}
	assertNothingUnder(t, target)
}

// And the honest control: with nothing planted, the fallback prepares, the
// anchor is a real directory this test can see, and the runtime tree sits
// beneath it.
func TestFallbackAnchorIsCreatedPrivatelyWhenNothingIsInTheWay(t *testing.T) {
	tempRoot := t.TempDir()
	t.Setenv("TMP", tempRoot)
	t.Setenv("TEMP", tempRoot)
	workspace := t.TempDir()

	previous := sandboxUserCacheDir
	t.Cleanup(func() { sandboxUserCacheDir = previous })
	sandboxUserCacheDir = func() (string, error) { return filepath.Join(workspace, ".cache"), nil }

	runtimeState, release, err := prepareSandboxRuntime(workspace)
	if err != nil {
		t.Fatalf("a clean fallback was refused: %v", err)
	}
	defer release()

	anchor := fallbackRuntimeAnchor()
	info, err := os.Lstat(anchor)
	if err != nil || !info.IsDir() {
		t.Fatalf("the anchor was not created as a directory: err=%v", err)
	}
	if !pathWithinRoot(anchor, runtimeState.Root) {
		t.Errorf("runtime root %s is not beneath the anchor %s", runtimeState.Root, anchor)
	}
}

//go:build windows

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A PREVIOUS SANDBOXED COMMAND CAN LEAVE A REDIRECT BEHIND.
//
// The runtime root is deterministic so setup and later runners agree on one
// path, and the sandboxed command is granted write access to cache, data and
// tmp. So it can replace one of them with a junction on its way out. The next
// preparation used os.MkdirAll and os.Chmod on raw pathnames, which follow, and
// the HOST Zero process, the ordinary user rather than the confined principal,
// then created package-cache directories inside a target the previous command
// chose.
//
// The anchor check does not cover this: it proves the per-user anchor and says
// nothing about the reusable root or anything beneath it.
//
// A junction needs no privilege, so the previous command's half runs here on an
// ordinary unelevated box.
func TestRuntimeTreePreparationRefusesARedirectedDescendant(t *testing.T) {
	base := runtimeTreeTestBase(t)
	target := t.TempDir()
	root := filepath.Join(base, "zero", "runtime", "v1", "abcdef0123456789")
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	// The previous lifetime leaves a junction where cache used to be.
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", cache, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J unavailable here: %v\n%s", err, out)
	}
	// SETUP: the junction really redirects, or a clean target proves nothing.
	probe := filepath.Join(cache, "probe.txt")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		t.Skipf("the junction does not accept writes here: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "probe.txt")); err != nil {
		t.Skipf("SETUP: the junction does not redirect on this filesystem: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}

	err := ensureRuntimeTreeDirs(root, []string{root, cache, filepath.Join(cache, "npm")})
	if err == nil {
		t.Fatal("preparation descended through a junction the previous sandboxed command planted")
	}

	entries, readErr := os.ReadDir(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("preparation created %v beneath the redirected target", names)
	}
}

// And an ordinary tree is still created, or the refusal above would be satisfied
// by a preparation that refuses everything.
func TestRuntimeTreePreparationStillCreatesAnOrdinaryTree(t *testing.T) {
	base := runtimeTreeTestBase(t)
	root := filepath.Join(base, "zero", "runtime", "v1", "abcdef0123456789")
	cache := filepath.Join(root, "cache")
	npm := filepath.Join(cache, "npm")

	if err := ensureRuntimeTreeDirs(root, []string{root, cache, npm}); err != nil {
		t.Fatalf("an ordinary runtime tree was refused: %v", err)
	}
	for _, dir := range []string{root, cache, npm} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("%s was not created: err=%v", dir, err)
		}
	}
}

// Reusing an existing tree is the common case and must not be refused.
func TestRuntimeTreePreparationIsIdempotent(t *testing.T) {
	base := runtimeTreeTestBase(t)
	root := filepath.Join(base, "zero", "runtime", "v1", "abcdef0123456789")
	dirs := []string{root, filepath.Join(root, "cache"), filepath.Join(root, "cache", "npm")}
	if err := ensureRuntimeTreeDirs(root, dirs); err != nil {
		t.Fatalf("first preparation: %v", err)
	}
	if err := ensureRuntimeTreeDirs(root, dirs); err != nil {
		t.Fatalf("second preparation over an existing tree was refused: %v", err)
	}
}

// AND THE PRODUCTION ENTRY POINT USES IT, WHICH THE HELPER TESTS CANNOT SEE.
//
// The three tests above call ensureRuntimeTreeDirs directly, so reverting
// prepareSandboxRuntime to the old pathname loop leaves every one of them green.
// The defect was in what the entry point called, so this drives the entry point.
func TestPrepareSandboxRuntimeRefusesARedirectedDescendant(t *testing.T) {
	isolateSandboxRuntimeRoots(t)
	cacheRoot := t.TempDir()
	target := t.TempDir()
	previous := sandboxUserCacheDir
	t.Cleanup(func() { sandboxUserCacheDir = previous })
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }

	workspace := t.TempDir()
	root, ok := deterministicSandboxRuntimeRoot(canonicalSandboxWorkspaceRoot(workspace), canonicalSandboxWorkspaceRoot(cacheRoot))
	if !ok {
		t.Fatalf("SETUP INVALID: no deterministic runtime root for %s", workspace)
	}
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", cache, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J unavailable here: %v\n%s", err, out)
	}
	probe := filepath.Join(cache, "probe.txt")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		t.Skipf("the junction does not accept writes here: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "probe.txt")); err != nil {
		t.Skipf("SETUP: the junction does not redirect on this filesystem: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatal(err)
	}

	_, release, err := prepareSandboxRuntime(workspace)
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("prepareSandboxRuntime descended through a junction the previous sandboxed command planted")
	}

	entries, readErr := os.ReadDir(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("prepareSandboxRuntime created %v beneath the redirected target", names)
	}
}

// And an ordinary workspace still prepares through the entry point.
func TestPrepareSandboxRuntimeStillPreparesAnOrdinaryTree(t *testing.T) {
	isolateSandboxRuntimeRoots(t)
	cacheRoot := t.TempDir()
	previous := sandboxUserCacheDir
	t.Cleanup(func() { sandboxUserCacheDir = previous })
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }

	runtimeState, release, err := prepareSandboxRuntime(t.TempDir())
	if err != nil {
		t.Fatalf("an ordinary workspace was refused: %v", err)
	}
	defer release()
	for _, dir := range []string{runtimeState.Root, runtimeState.Cache, runtimeState.Data, runtimeState.Temp} {
		info, statErr := os.Stat(dir)
		if statErr != nil || !info.IsDir() {
			t.Fatalf("%s was not created: err=%v", dir, statErr)
		}
	}
}

// runtimeTreeTestBase makes the operator-owned base deterministic.
//
// runtimeCandidateBase answers by containment against the user cache directory,
// so a bare t.TempDir() only works where temp happens to sit under it. On a CI
// runner it does not, or its 8.3 short name compares unequal to the long form,
// and the tree is reported as having no owned base. Stubbing the cache root is
// what makes these tests about the descent rather than about where temp lives.
func runtimeTreeTestBase(t *testing.T) string {
	t.Helper()
	base := canonicalSandboxWorkspaceRoot(t.TempDir())
	previous := sandboxUserCacheDir
	t.Cleanup(func() { sandboxUserCacheDir = previous })
	sandboxUserCacheDir = func() (string, error) { return base, nil }
	return base
}

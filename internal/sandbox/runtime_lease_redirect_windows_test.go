//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE REUSABLE CHILD OF THE FALLBACK ANCHOR IS VALIDATED BEFORE THE LEASE.
//
// The anchor was checked, but its reusable "v1" child was created by pathname
// and the lease opened beside it, so a junction planted at v1 sent the .lease
// into another directory before validation ever ran. Preparation must refuse
// without any effect in the redirected target. A junction, not a symlink,
// because that is what an unprivileged account on this platform can create.
func TestPrepareSandboxRuntimeRefusesARedirectedFallbackChild(t *testing.T) {
	_, tempRoot := isolateSandboxRuntimeRoots(t)
	workspace := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	// Force the fallback: the cache root inside the workspace is unusable.
	original := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return filepath.Join(workspace, ".cache"), nil }
	t.Cleanup(func() { sandboxUserCacheDir = original })

	anchor := fallbackRuntimeAnchor()
	if !strings.HasPrefix(strings.ToLower(anchor), strings.ToLower(tempRoot)) {
		physical, _ := physicalDir(tempRoot)
		if physical == "" || !strings.HasPrefix(strings.ToLower(anchor), strings.ToLower(physical)) {
			t.Fatalf("SETUP INVALID: fallback anchor %s is not under the fixture temp %s", anchor, tempRoot)
		}
	}
	if err := os.MkdirAll(anchor, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	makeJunction(t, filepath.Join(anchor, "v1"), elsewhere)

	_, release, err := prepareSandboxRuntime(workspace)
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("preparation followed a redirected v1 instead of refusing")
	}
	entries, readErr := os.ReadDir(elsewhere)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("preparation left %v in the redirected target before refusing", names)
	}

	// CONTROL: with the junction gone, preparation works and its lease lands
	// beside the root it selected.
	if err := os.Remove(filepath.Join(anchor, "v1")); err != nil {
		t.Fatal(err)
	}
	state, release, err := prepareSandboxRuntime(workspace)
	if err != nil {
		t.Fatalf("ordinary preparation: %v", err)
	}
	defer release()
	if !strings.EqualFold(filepath.Dir(state.Root), filepath.Join(anchor, "v1")) {
		t.Fatalf("selected root %s is not under the fallback anchor's v1", state.Root)
	}
	if _, err := os.Lstat(sandboxRuntimeLeasePath(state.Root)); err != nil {
		t.Fatalf("lease was not created beside the selected root: %v", err)
	}
}

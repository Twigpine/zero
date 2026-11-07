package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// A LINK LEFT IN THE TREE BY ONE COMMAND IS NOT FOLLOWED BY THE NEXT ZERO.
//
// The runtime root is a write root, so a sandboxed command can replace any
// directory under it with a link to a host directory it cannot write. The
// fallback root is derived rather than minted, so the next Zero process,
// unsandboxed, prepares that same tree. It created and chmodded every child by
// pathname and followed the link: jatmn's probe got outside/npm created and the
// outside directory's mode changed through cache. Reported by @jatmn.
//
// Every child is covered, grandchildren included, because each is its own
// pathname and closing one leaves the others open.

// plantRuntimeLink replaces nothing; it makes link point at target, the way a
// sandboxed command would: a symlink off Windows, and a junction on Windows,
// where a symlink needs a privilege and a junction does not.
func plantRuntimeLink(t *testing.T, link, target string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Fatalf("SETUP INVALID: plant a junction at %s: %v %s", link, err, out)
		}
	} else if err := os.Symlink(target, link); err != nil {
		t.Fatalf("SETUP INVALID: plant a symlink at %s: %v", link, err)
	}
	linkInfo, err := os.Stat(link)
	if err != nil {
		t.Fatalf("SETUP INVALID: the planted link does not resolve: %v", err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil || !os.SameFile(linkInfo, targetInfo) {
		t.Fatalf("SETUP INVALID: %s does not lead to %s, so the prepare below would prove nothing", link, target)
	}
}

// fallbackRuntimeForTest makes the next prepareSandboxRuntime choose the temp
// fallback, inside a temp directory this test owns.
func fallbackRuntimeForTest(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	redirectSandboxTestTemp(t, t.TempDir())
	original := sandboxUserCacheDir
	// A cache inside the workspace puts the preferred root there, which is what
	// sends selection to the fallback.
	sandboxUserCacheDir = func() (string, error) { return filepath.Join(workspace, ".cache"), nil }
	t.Cleanup(func() { sandboxUserCacheDir = original })
	return workspace
}

func TestPreparingTheRuntimeDoesNotFollowAChildLinkLeftByACommand(t *testing.T) {
	for _, child := range runtimeTreeChildren {
		t.Run(filepath.ToSlash(child), func(t *testing.T) {
			workspace := fallbackRuntimeForTest(t)
			first, release, err := prepareSandboxRuntime(workspace, "")
			if err != nil {
				t.Fatalf("SETUP INVALID: the first prepare failed: %v", err)
			}
			release()
			requireWithinTestOwned(t, first.Root, os.TempDir())
			if pathWithinRoot(canonicalSandboxWorkspaceRoot(workspace), canonicalSandboxWorkspaceRoot(first.Root)) {
				t.Fatalf("SETUP INVALID: %s is the preferred root inside the workspace, not the fallback", first.Root)
			}

			// A host directory the command cannot write inside the sandbox, with a
			// mode a chmod through the link would change.
			outside := t.TempDir()
			if runtime.GOOS != "windows" {
				if err := os.Chmod(outside, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			planted := filepath.Join(first.Root, child)
			if err := os.RemoveAll(planted); err != nil {
				t.Fatalf("SETUP INVALID: remove %s: %v", planted, err)
			}
			plantRuntimeLink(t, planted, outside)

			second, release, err := prepareSandboxRuntime(workspace, "")
			if err == nil {
				release()
				t.Fatalf("the next prepare accepted a link at %s (root %s)", child, second.Root)
			}
			if !errors.Is(err, errRuntimeComponentAliased) {
				t.Fatalf("refused for a reason other than the link, so this does not pin the guard: %v", err)
			}
			entries, readErr := os.ReadDir(outside)
			if readErr != nil {
				t.Fatalf("read the outside directory: %v", readErr)
			}
			if len(entries) != 0 {
				t.Errorf("the prepare created %d entries inside the directory the link pointed at, starting with %s", len(entries), entries[0].Name())
			}
			if runtime.GOOS != "windows" {
				info, statErr := os.Stat(outside)
				if statErr != nil {
					t.Fatal(statErr)
				}
				if info.Mode().Perm() != 0o755 {
					t.Errorf("the prepare changed the mode of the directory the link pointed at to %04o", info.Mode().Perm())
				}
			}
		})
	}
}

// The ordinary case still works through the handle: a second prepare of an
// intact tree succeeds, and off Windows it puts a loosened child back to 0700.
func TestPreparingAnIntactRuntimeTwiceStillSecuresItsChildren(t *testing.T) {
	workspace := fallbackRuntimeForTest(t)
	first, release, err := prepareSandboxRuntime(workspace, "")
	if err != nil {
		t.Fatalf("the first prepare failed: %v", err)
	}
	release()
	requireWithinTestOwned(t, first.Root, os.TempDir())
	loosened := filepath.Join(first.Root, "cache", "npm")
	if runtime.GOOS != "windows" {
		if err := os.Chmod(loosened, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, release, err = prepareSandboxRuntime(workspace, "")
	if err != nil {
		t.Fatalf("the second prepare of an intact tree failed: %v", err)
	}
	release()
	for _, child := range runtimeTreeChildren {
		info, err := os.Lstat(filepath.Join(first.Root, child))
		if err != nil || !info.IsDir() {
			t.Fatalf("%s is missing or not a directory after prepare: %v", child, err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
			t.Errorf("%s is %04o after prepare, want 0700", child, info.Mode().Perm())
		}
	}
}

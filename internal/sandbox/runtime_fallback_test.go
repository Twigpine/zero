package sandbox

import (
	"path/filepath"
	"strings"
	"testing"
)

// SETUP AND EVERY COMMAND RUNNER MUST DERIVE THE SAME FALLBACK ROOT.
//
// They are separate processes. The fallback used to call os.MkdirTemp and cache
// the answer in a process-global map, so elevated setup granted the sandbox
// principal write access to the directory IT made, and each later
// __windows-command-runner made a different one and pointed TMP, GOCACHE and npm
// there. Those directories are created by the calling user and carry no ACE for
// the principal, so ordinary cache writes failed with a bare ACCESS_DENIED.
//
// A repeated call is what a second process looks like from here: no shared
// state, same inputs. The old implementation could not pass this, because the
// only thing that made repeat calls agree was the in-process map.
func TestFallbackRuntimeRootIsTheSameForEveryProcess(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "ws")

	first, err := fallbackSandboxRuntimeRoot(workspace)
	if err != nil {
		t.Fatalf("fallbackSandboxRuntimeRoot: %v", err)
	}
	second, err := fallbackSandboxRuntimeRoot(workspace)
	if err != nil {
		t.Fatalf("fallbackSandboxRuntimeRoot (second process): %v", err)
	}
	if first != second {
		t.Fatalf("two processes derived different runtime roots, so setup grants one and commands write to another:\n  %s\n  %s", first, second)
	}
	if first == "" {
		t.Fatal("no runtime root derived")
	}
}

// Different workspaces must not share a runtime tree, or one workspace's
// principal would hold an ACE on another's caches.
func TestFallbackRuntimeRootIsPerWorkspace(t *testing.T) {
	base := t.TempDir()
	first, err := fallbackSandboxRuntimeRoot(filepath.Join(base, "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := fallbackSandboxRuntimeRoot(filepath.Join(base, "beta"))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two workspaces share the runtime root %q", first)
	}
}

// It must NAME the tree without creating it. Teardown asks for this path on its
// way out, and materializing a directory there would leave one behind on every
// teardown.
func TestFallbackRuntimeRootCreatesNothing(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "ws")
	root, err := fallbackSandboxRuntimeRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if pathExists(root) {
		t.Errorf("naming the runtime root created %s", root)
	}
}

// The root must land outside the workspace, which is the entire reason this
// branch exists.
func TestFallbackRuntimeRootStaysOutsideTheWorkspace(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "ws")
	root, err := fallbackSandboxRuntimeRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if pathWithinRoot(workspace, root) {
		t.Errorf("runtime root %q is inside workspace %q", root, workspace)
	}
	if !strings.Contains(root, "zero") {
		t.Errorf("runtime root %q does not look like a zero-owned tree", root)
	}
}

package cli

import (
	"path/filepath"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
)

// AN ISOLATED PLAN'S CHILD MUST NOT GET THE PARENT TREE BACK. The headless
// wiring handed children execScope.Roots() — the parent workspace root
// included — as extra WRITE roots. A worktree-isolated plan exists so a
// write-capable task cannot touch the parent tree, and that line handed the
// parent tree back as a writable --add-dir. Only the run's explicit extra
// grants may ride along; the child's own workspace comes from its --cwd.
func TestExecChildWriteRootsExcludeTheParentWorkspace(t *testing.T) {
	// The scope stores symlink-resolved paths (/var → /private/var on macOS),
	// so both sides of every comparison resolve first.
	resolve := func(path string) string {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatalf("EvalSymlinks(%s): %v", path, err)
		}
		return resolved
	}
	workspace := resolve(t.TempDir())
	granted := resolve(t.TempDir())
	scope, err := sandbox.NewScope(workspace, []string{granted})
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	roots := execChildWriteRoots(scope)
	found := false
	for _, root := range roots {
		if root == workspace {
			t.Fatalf("the parent workspace root leaked into a child's write roots: %q", roots)
		}
		if root == granted {
			found = true
		}
	}
	if !found {
		t.Fatalf("the explicit grant must still ride along, got %q", roots)
	}
	if execChildWriteRoots(nil) != nil {
		t.Fatal("a nil scope must yield no roots")
	}
}

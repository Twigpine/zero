package cli

import (
	"path/filepath"
	"slices"
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
	for _, withinTurn := range []bool{true, false} {
		roots := childWriteRoots(scope, withinTurn)
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
			t.Fatalf("the explicit grant must still ride along (withinTurn=%v), got %q", withinTurn, roots)
		}
		if childWriteRoots(nil, withinTurn) != nil {
			t.Fatal("a nil scope must yield no roots")
		}
	}
}

// A TURN-SCOPED WRITE GRANT REACHES ONLY A CHILD THAT ENDS WITHIN THE TURN.
//
// Scope.ExtraRoots carried AddTemporaryWrite roots like permanent ones, so a
// turn-scoped request_permissions grant left as a plain --add-dir and a child
// that outlived the turn kept writing there after the parent had released it.
func TestChildWriteRootsWithholdATurnGrantFromAChildThatOutlivesTheTurn(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scope, err := sandbox.NewScope(workspace, nil)
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	// Outside the temp roots NewScope seeds as permanent: a turn grant under one
	// of them would be covered already and recorded as nothing.
	temporary := tempDirOutsideDefaultTemp(t)
	root, release, err := scope.AddTemporaryWrite(temporary)
	if err != nil {
		t.Fatalf("AddTemporaryWrite: %v", err)
	}
	defer release()

	if !slices.Contains(childWriteRoots(scope, true), root) {
		t.Fatalf("a child inside the turn lost the turn's grant: %v", childWriteRoots(scope, true))
	}
	if slices.Contains(childWriteRoots(scope, false), root) {
		t.Fatalf("a child that may outlive the turn was handed the turn-scoped grant %q: %v", root, childWriteRoots(scope, false))
	}
}

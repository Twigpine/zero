//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// THE RESTORE MUST LAND ON THE OBJECT THE DACL CAME FROM.
//
// Creation and the materialization unwind were both bound to handles, and the
// restore branch was left re-resolving the target by pathname. Its comment
// justified that with a no-follow re-open, which rules out a REPARSE POINT
// swapped in since apply and nothing else. The other substitution passes it
// untouched: rename the target aside and put an ordinary directory of the same
// name in its place. Nothing in that decoy is a link, so the no-follow open
// succeeds, the pre-apply DACL is written onto the attacker's object, and the
// real one keeps the ACEs from the setup that just aborted.
func TestRollbackRefusesToRestoreOntoAReplacedTarget(t *testing.T) {
	root := t.TempDir()
	approved := filepath.Join(root, "ws")
	target := filepath.Join(approved, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("seed the approved tree: %v", err)
	}

	handle, _, err := openWindowsACLTarget(target)
	if err != nil {
		t.Fatalf("open the target: %v", err)
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatalf("read the target DACL: %v", err)
	}
	identity, err := windowsIdentityOfHandle(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatalf("read the target identity: %v", err)
	}
	_ = windows.CloseHandle(handle)

	// The swap, exactly as it would happen between apply and rollback. Ordinary
	// directories throughout: no links, no reparse points.
	if err := os.Rename(target, target+"-moved"); err != nil {
		t.Skipf("cannot rename here: %v", err)
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("plant the replacement: %v", err)
	}

	snapshot := windowsACLSnapshot{Path: target, Descriptor: descriptor, TargetID: identity}
	err = rollbackWindowsACLSnapshots([]windowsACLSnapshot{snapshot})
	if err == nil {
		t.Fatal("rollback wrote the pre-apply DACL onto a different object wearing the target's name")
	}
	if !strings.Contains(err.Error(), "no longer the object") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
}

// And an untouched target still restores, or the guard above would be satisfied
// by a rollback that refuses everything.
func TestRollbackRestoresAnUnchangedTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("seed the target: %v", err)
	}

	handle, _, err := openWindowsACLTarget(target)
	if err != nil {
		t.Fatalf("open the target: %v", err)
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatalf("read the target DACL: %v", err)
	}
	identity, err := windowsIdentityOfHandle(handle)
	if err != nil {
		_ = windows.CloseHandle(handle)
		t.Fatalf("read the target identity: %v", err)
	}
	_ = windows.CloseHandle(handle)

	snapshot := windowsACLSnapshot{Path: target, Descriptor: descriptor, TargetID: identity}
	if err := rollbackWindowsACLSnapshots([]windowsACLSnapshot{snapshot}); err != nil {
		t.Fatalf("an unchanged target was refused: %v", err)
	}
}

// THE COMMENT AT rollbackWindowsACLSnapshots CLAIMED A TEST THAT DID NOT EXIST.
//
// It said "TestRollbackUnwindsDescendantsBeforeAncestors pins it" about the
// reverse iteration order, and grepping the repo found only that sentence. The
// ordering is load-bearing twice over: a materialized directory must be empty
// before its own removal is attempted, and SetSecurityInfo propagates
// inheritable ACEs downward so the ancestor has to be restored last. Neither
// was pinned by anything.
func TestRollbackUnwindsDescendantsBeforeAncestors(t *testing.T) {
	root := t.TempDir()
	ancestor := filepath.Join(root, "ws")
	descendant := filepath.Join(ancestor, "child")
	if err := os.MkdirAll(descendant, 0o700); err != nil {
		t.Fatalf("seed the tree: %v", err)
	}

	snapshotFor := func(path string) windowsACLSnapshot {
		t.Helper()
		handle, _, err := openWindowsACLTarget(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		defer windows.CloseHandle(handle)
		descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatalf("read the DACL for %s: %v", path, err)
		}
		identity, err := windowsIdentityOfHandle(handle)
		if err != nil {
			t.Fatalf("read the identity for %s: %v", path, err)
		}
		return windowsACLSnapshot{Path: path, Descriptor: descriptor, TargetID: identity}
	}

	// Ascending by path key, the order applyWindowsACLPlan produces.
	snapshots := []windowsACLSnapshot{snapshotFor(ancestor), snapshotFor(descendant)}

	var order []string
	restore := windowsACLRestoreHook
	windowsACLRestoreHook = func(path string) { order = append(order, path) }
	t.Cleanup(func() { windowsACLRestoreHook = restore })

	if err := rollbackWindowsACLSnapshots(snapshots); err != nil {
		t.Fatalf("rollbackWindowsACLSnapshots: %v", err)
	}
	if len(order) != 2 {
		t.Fatalf("restored %d targets, want 2: %v", len(order), order)
	}
	if order[0] != descendant || order[1] != ancestor {
		t.Errorf("restore order was %v; the descendant must be restored before its ancestor, because SetSecurityInfo propagates inheritable ACEs downward", order)
	}
}

// The snapshots arrive in APPLY order, not path order. applyWindowsACLPlan's
// deferred pass appends a group whose target only appeared during the first
// pass, typically an ancestor (.git after .git\config), so the ancestor is LAST
// in the list. Rollback must still unwind the descendant first.
func TestRollbackUnwindsADeferredAncestorAfterItsDescendants(t *testing.T) {
	root := t.TempDir()
	ancestor := filepath.Join(root, ".git")
	descendant := filepath.Join(ancestor, "config")
	if err := os.MkdirAll(descendant, 0o700); err != nil {
		t.Fatalf("seed the tree: %v", err)
	}

	snapshotFor := func(path string) windowsACLSnapshot {
		t.Helper()
		handle, _, err := openWindowsACLTarget(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		defer windows.CloseHandle(handle)
		descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatalf("read the DACL for %s: %v", path, err)
		}
		identity, err := windowsIdentityOfHandle(handle)
		if err != nil {
			t.Fatalf("read the identity for %s: %v", path, err)
		}
		return windowsACLSnapshot{Path: path, Descriptor: descriptor, TargetID: identity}
	}

	// First pass applied the descendant; the deferred pass applied the ancestor.
	snapshots := []windowsACLSnapshot{snapshotFor(descendant), snapshotFor(ancestor)}

	var order []string
	restore := windowsACLRestoreHook
	windowsACLRestoreHook = func(path string) { order = append(order, path) }
	t.Cleanup(func() { windowsACLRestoreHook = restore })

	if err := rollbackWindowsACLSnapshots(snapshots); err != nil {
		t.Fatalf("rollbackWindowsACLSnapshots: %v", err)
	}
	if len(order) != 2 {
		t.Fatalf("restored %d targets, want 2: %v", len(order), order)
	}
	if order[0] != descendant || order[1] != ancestor {
		t.Errorf("restore order was %v; a deferred ancestor must still be restored after its descendant", order)
	}
	if snapshots[0].Path != descendant || snapshots[1].Path != ancestor {
		t.Errorf("rollback reordered the caller's slice: %v, %v", snapshots[0].Path, snapshots[1].Path)
	}
}

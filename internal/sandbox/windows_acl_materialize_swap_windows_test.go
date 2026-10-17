//go:build windows

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// swapAncestorAside moves approved out of the way and leaves a junction wearing
// its name, pointing at elsewhere. This is the whole attack in three lines, and
// it needs no privilege: junctions are creatable by any user, which is exactly
// why an unprivileged workspace owner can aim elevated setup wherever they like.
//
// Returns the path the real directory now lives at.
func swapAncestorAside(t *testing.T, approved, elsewhere string) string {
	t.Helper()
	moved := approved + "-moved"
	if err := os.Rename(approved, moved); err != nil {
		t.Skipf("cannot rename a directory with an open handle on this filesystem: %v", err)
	}
	makeJunction(t, approved, elsewhere)
	return moved
}

// requireNothingEscaped fails when anything at all was created on the far side
// of the junction.
func requireNothingEscaped(t *testing.T, elsewhere string) {
	t.Helper()
	leaked, err := os.ReadDir(elsewhere)
	if err != nil {
		t.Fatalf("read the decoy directory: %v", err)
	}
	if len(leaked) == 0 {
		return
	}
	names := make([]string, 0, len(leaked))
	for _, entry := range leaked {
		names = append(names, entry.Name())
	}
	t.Fatalf("ESCAPED: created %v outside the approved tree, as Administrator", names)
}

// THE MATERIALIZATION RACE, ON THE PRODUCTION CALL PATH.
//
// The existing junction test plants its junction before the walk even starts, so
// the very first check sees it and refuses. That proves the easy half. The half
// the reviewer filed is the swap that happens AFTER a component has been
// verified and BEFORE it is used, and no test reached it: a race nobody can
// trigger on demand is not a regression test, so makeWindowsACLDirChainNoFollow
// carries a seam that fires at exactly that instant.
//
// The control arm matters as much as the fixed one. It performs the identical
// swap and then does what this code used to do, creating by pathname, and
// asserts that the object DOES escape. Without it, the fixed arm passing proves
// only that some code ran, not that the hole it closes was ever open.
func TestMaterializeSurvivesAnAncestorSwappedMidWalk(t *testing.T) {
	t.Run("control: creating by pathname escapes", func(t *testing.T) {
		root := t.TempDir()
		approved := filepath.Join(root, "ws")
		elsewhere := filepath.Join(root, "OUTSIDE")
		for _, dir := range []string{approved, elsewhere} {
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatalf("seed %s: %v", dir, err)
			}
		}
		// Verified, exactly as the walk verifies its anchor.
		if err := verifyWindowsACLPathComponentNotRedirected(approved); err != nil {
			t.Fatalf("anchor did not verify before the swap: %v", err)
		}
		moved := swapAncestorAside(t, approved, elsewhere)

		// The old create: a pathname, re-resolved by the kernel right now.
		if err := os.MkdirAll(filepath.Join(approved, "a", "b"), 0o700); err != nil {
			t.Fatalf("pathname create: %v", err)
		}
		if _, err := os.Stat(filepath.Join(elsewhere, "a", "b")); err != nil {
			t.Fatalf("the control arm did not reproduce the escape, so the fixed arm below proves nothing: %v", err)
		}
		if _, err := os.Stat(filepath.Join(moved, "a", "b")); err == nil {
			t.Error("the control arm created inside the verified directory, which is not the behaviour being contrasted")
		}
	})

	t.Run("fixed: creating through the pinned handle stays put", func(t *testing.T) {
		root := t.TempDir()
		approved := filepath.Join(root, "ws")
		elsewhere := filepath.Join(root, "OUTSIDE")
		for _, dir := range []string{approved, elsewhere} {
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatalf("seed %s: %v", dir, err)
			}
		}

		var moved string
		swapped := false
		windowsACLMaterializeSwapHook = func(anchor string) {
			if swapped {
				return
			}
			swapped = true
			if anchor != approved {
				t.Errorf("anchored on %q, want the deepest existing ancestor %q", anchor, approved)
			}
			moved = swapAncestorAside(t, approved, elsewhere)
		}
		t.Cleanup(func() { windowsACLMaterializeSwapHook = nil })

		created, err := materializeWindowsACLTarget(filepath.Join(approved, "a", "b"), false)
		if err != nil {
			t.Fatalf("materializeWindowsACLTarget: %v", err)
		}
		if !swapped {
			t.Fatal("the seam never fired, so the swap never happened and this test proved nothing")
		}
		requireNothingEscaped(t, elsewhere)
		if _, err := os.Stat(filepath.Join(moved, "a", "b")); err != nil {
			t.Errorf("the target did not land in the directory that was verified: %v", err)
		}
		// And the record must describe what to unwind, in components rather than
		// paths, shallowest first.
		if len(created.Chain) != 2 || created.Chain[0].Name != "a" || created.Chain[1].Name != "b" {
			t.Fatalf("created chain = %#v, want [a b] shallow to deep", created.Chain)
		}
		for _, step := range created.Chain {
			if !step.Made {
				t.Errorf("component %q was not recorded as created, so rollback would leave it behind", step.Name)
			}
		}
	})
}

// The FILE target has the same race, and it is the one that matters most:
// .git/config is materialized as a file on every stock setup, and it is the file
// whose credential.helper is worth stealing.
func TestMaterializeFileSurvivesAnAncestorSwappedMidWalk(t *testing.T) {
	root := t.TempDir()
	approved := filepath.Join(root, "ws")
	elsewhere := filepath.Join(root, "OUTSIDE")
	for _, dir := range []string{approved, elsewhere} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("seed %s: %v", dir, err)
		}
	}

	var moved string
	swapped := false
	windowsACLMaterializeSwapHook = func(string) {
		if swapped {
			return
		}
		swapped = true
		moved = swapAncestorAside(t, approved, elsewhere)
	}
	t.Cleanup(func() { windowsACLMaterializeSwapHook = nil })

	created, err := materializeWindowsACLTarget(filepath.Join(approved, ".git", "config"), true)
	if err != nil {
		t.Fatalf("materializeWindowsACLTarget: %v", err)
	}
	if !swapped {
		t.Fatal("the seam never fired, so this test proved nothing")
	}
	requireNothingEscaped(t, elsewhere)

	landed := filepath.Join(moved, ".git", "config")
	info, err := os.Stat(landed)
	if err != nil {
		t.Fatalf("the file did not land in the directory that was verified: %v", err)
	}
	if info.IsDir() {
		t.Error("materialized .git/config as a directory, which breaks git init")
	}
	if !created.FileMade || created.File != "config" {
		t.Errorf("file record = %q made=%v, want config/true", created.File, created.FileMade)
	}
}

// THE ROLLBACK RACE. The ancestor is swapped AFTER the target was created, which
// is the window the teardown path lives in: minutes or hours, not microseconds.
//
// The bystander is the point. If the unwind resolves by pathname it walks into
// the decoy and deletes what it finds there, recursively and elevated. Its
// survival is the only thing that proves the unwind did not.
func TestRollbackDoesNotFollowAnAncestorSwappedAfterCreation(t *testing.T) {
	root := t.TempDir()
	approved := filepath.Join(root, "ws")
	elsewhere := filepath.Join(root, "OUTSIDE")
	for _, dir := range []string{approved, elsewhere} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatalf("seed %s: %v", dir, err)
		}
	}

	created, err := materializeWindowsACLTarget(filepath.Join(approved, "a", "b"), false)
	if err != nil {
		t.Fatalf("materializeWindowsACLTarget: %v", err)
	}

	// A bystander tree under the decoy, shaped exactly like what we created, so a
	// pathname unwind would find something to destroy at every level.
	bystander := filepath.Join(elsewhere, "a", "b")
	if err := os.MkdirAll(bystander, 0o700); err != nil {
		t.Fatalf("seed bystander: %v", err)
	}
	witness := filepath.Join(bystander, "irreplaceable.txt")
	if err := os.WriteFile(witness, []byte("not yours to delete"), 0o600); err != nil {
		t.Fatalf("seed witness: %v", err)
	}

	moved := swapAncestorAside(t, approved, elsewhere)

	// The anchor pathname now names the decoy, and the decoy is a junction, so
	// the unwind must refuse rather than proceed. Either way it must not delete.
	_, err = rollbackWindowsACLMaterialization(created)

	if _, statErr := os.Stat(witness); statErr != nil {
		t.Fatalf("DESTRUCTIVE: rollback followed the junction and deleted a tree outside the approved directory: %v", statErr)
	}
	if _, statErr := os.Stat(bystander); statErr != nil {
		t.Fatalf("DESTRUCTIVE: rollback removed the bystander directory outside the approved directory: %v", statErr)
	}
	if err == nil {
		t.Error("rollback reported success while unwinding through a swapped ancestor; it must say it could not")
	}
	// Residue inside the real tree is the accepted price: leaving it is strictly
	// better than a recursive delete through a path someone else controls.
	if _, statErr := os.Stat(filepath.Join(moved, "a", "b")); statErr != nil {
		t.Logf("note: the real tree was also unwound (%v); leaving it would be acceptable too", statErr)
	}
}

// A real directory wearing the anchor's name is a swap with NO reparse point
// anywhere, so every no-follow check in this package passes it. Only the file
// identity notices.
func TestRollbackRefusesAnAnchorReplacedByARealDirectory(t *testing.T) {
	root := t.TempDir()
	approved := filepath.Join(root, "ws")
	if err := os.Mkdir(approved, 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}
	created, err := materializeWindowsACLTarget(filepath.Join(approved, "a"), false)
	if err != nil {
		t.Fatalf("materializeWindowsACLTarget: %v", err)
	}

	if err := os.Rename(approved, approved+"-moved"); err != nil {
		t.Skipf("cannot rename here: %v", err)
	}
	// An ordinary directory. Nothing is a link; nothing is a reparse point.
	if err := os.Mkdir(approved, 0o700); err != nil {
		t.Fatalf("plant the replacement: %v", err)
	}
	decoy := filepath.Join(approved, "a")
	if err := os.Mkdir(decoy, 0o700); err != nil {
		t.Fatalf("plant the decoy child: %v", err)
	}

	_, err = rollbackWindowsACLMaterialization(created)
	if err == nil {
		t.Error("rollback accepted a different directory wearing the anchor's name")
	} else if !strings.Contains(err.Error(), "no longer the directory") {
		t.Errorf("refused for the wrong reason: %v", err)
	}
	if _, statErr := os.Stat(decoy); statErr != nil {
		t.Errorf("rollback deleted a directory it never created: %v", statErr)
	}
}

// Rollback removes ONLY what this run created. A pre-existing ancestor is walked
// through and left alone.
func TestRollbackLeavesDirectoriesItDidNotCreate(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "ws", "already-here")
	if err := os.MkdirAll(existing, 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}
	target := filepath.Join(existing, "made", "deeper")

	created, err := materializeWindowsACLTarget(target, false)
	if err != nil {
		t.Fatalf("materializeWindowsACLTarget: %v", err)
	}
	if created.AnchorPath != existing {
		t.Fatalf("anchor = %q, want the deepest pre-existing directory %q", created.AnchorPath, existing)
	}
	if _, err := rollbackWindowsACLMaterialization(created); err != nil {
		t.Fatalf("rollbackWindowsACLMaterialization: %v", err)
	}
	if _, err := os.Stat(filepath.Join(existing, "made")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("created directory survived rollback: stat err = %v", err)
	}
	if _, err := os.Stat(existing); err != nil {
		t.Errorf("rollback removed a directory that already existed: %v", err)
	}
}

// The whole apply path, through the closure callers actually hold, rather than
// through rollbackWindowsACLSnapshots directly. Every other rollback test in
// this package calls the unwind by hand, which cannot catch applyWindowsACLPlan
// failing to carry the created record into the snapshots it hands over.
func TestApplyWindowsACLPlanClosureRemovesWhatItMaterialized(t *testing.T) {
	root := t.TempDir()
	directoryTarget := filepath.Join(root, "ws", "hooks")
	fileTarget := filepath.Join(root, "ws", "config")

	plan := WindowsACLPlan{Entries: []WindowsACLEntry{
		{Action: WindowsACLDenyWrite, Path: directoryTarget, Capability: "S-1-1-0", Materialize: true},
		{Action: WindowsACLDenyWrite, Path: fileTarget, Capability: "S-1-1-0", Materialize: true, MaterializeFile: true},
	}}

	rollback, err := applyWindowsACLPlan(plan)
	if err != nil {
		t.Fatalf("applyWindowsACLPlan: %v", err)
	}
	for _, path := range []string{directoryTarget, fileTarget} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was not materialized: %v", path, err)
		}
	}
	if err := rollback(); err != nil {
		t.Fatalf("rollback closure: %v", err)
	}
	for _, path := range []string{directoryTarget, fileTarget} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the rollback closure: stat err = %v", path, err)
		}
	}
	// The shared prefix both targets needed must go too, and it is created by
	// whichever group runs first rather than being owned by both.
	if _, err := os.Stat(filepath.Join(root, "ws")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the shared parent survived: stat err = %v", err)
	}
}

// A rollback that cannot remove something must SAY so. This is the regression
// guard for the trap a naive handle-relative port walks straight into:
// FILE_DELETE_ON_CLOSE reports success on a non-empty directory and leaves it
// there, which turns a loud failure into a silent lie. The directory being
// populated is not adversarial; .git/hooks fills up the moment git runs.
func TestRollbackReportsWhatItCouldNotRemove(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "ws", "hooks")
	created, err := materializeWindowsACLTarget(target, false)
	if err != nil {
		t.Fatalf("materializeWindowsACLTarget: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "pre-commit"), []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatalf("populate: %v", err)
	}

	_, err = rollbackWindowsACLMaterialization(created)
	if err == nil {
		t.Fatal("rollback reported success on a directory it could not empty, so callers cannot tell teardown failed")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "hooks") {
		t.Errorf("the error does not name what was left behind: %v", err)
	}
	// Left in place deliberately. Removing it would mean recursing, and recursion
	// through a path the workspace owner controls is the thing being avoided.
	if _, statErr := os.Stat(target); statErr != nil {
		t.Errorf("rollback recursed into a populated directory instead of reporting it: %v", statErr)
	}
}

// The primitives take a single component and the walk relies on that. A joined
// name is resolved the ordinary way by the kernel, so an intermediate junction
// inside it is followed and the object lands outside the anchor: the pinned
// parent buys nothing if the name itself walks.
func TestChildOperationsRefuseNamesThatAreNotSingleComponents(t *testing.T) {
	root := t.TempDir()
	parent, err := openWindowsACLDirectoryNoFollow(root)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer func() { _ = windows.CloseHandle(parent) }()

	for _, name := range []string{`sub\child`, "sub/child", "..", ".", "", `C:\absolute`, "stream:name"} {
		t.Run("create dir "+name, func(t *testing.T) {
			handle, _, err := createWindowsACLChildDirectory(parent, name)
			if err == nil {
				_ = windows.CloseHandle(handle)
				t.Fatalf("accepted %q, which the kernel would resolve through intermediate directories", name)
			}
		})
		t.Run("delete dir "+name, func(t *testing.T) {
			if err := deleteWindowsACLChildDirectory(parent, name); err == nil {
				t.Fatalf("accepted %q for deletion", name)
			}
		})
		t.Run("create file "+name, func(t *testing.T) {
			if _, _, err := createWindowsACLChildFile(parent, name); err == nil {
				t.Fatalf("accepted %q for file creation", name)
			}
		})
	}
}

// A junction sitting where a chain component should be must be refused when it
// is OPENED, not merely when it is created. FILE_OPEN_REPARSE_POINT hands back a
// handle to the junction itself, and using that as the next parent puts every
// deeper create on the far side of it.
func TestChildOperationsRefuseAnExistingJunction(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	makeJunction(t, filepath.Join(root, "hop"), elsewhere)

	parent, err := openWindowsACLDirectoryNoFollow(root)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer func() { _ = windows.CloseHandle(parent) }()

	handle, _, err := createWindowsACLChildDirectory(parent, "hop")
	if err == nil {
		_ = windows.CloseHandle(handle)
		t.Error("createWindowsACLChildDirectory returned a junction as the next parent in the walk")
	}
	opened, err := openWindowsACLChildDirectory(parent, "hop")
	if err == nil {
		_ = windows.CloseHandle(opened)
		t.Error("openWindowsACLChildDirectory returned a junction to descend through")
	}
}

// Rollback descends; it must never create. If a component was removed by
// something else in the meantime, re-making it and then deleting it would remove
// a directory the sandbox never made.
func TestRollbackDescentNeverCreatesAMissingComponent(t *testing.T) {
	root := t.TempDir()
	parent, err := openWindowsACLDirectoryNoFollow(root)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	defer func() { _ = windows.CloseHandle(parent) }()

	handle, err := openWindowsACLChildDirectory(parent, "never-existed")
	if err == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("the descent open created a directory that did not exist")
	}
	if !isWindowsNotExist(err) {
		t.Errorf("a missing component reported %v, which rollback cannot distinguish from a real failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "never-existed")); statErr == nil {
		t.Error("a directory appeared on disk from an open that should never create")
	}
}

//go:build windows

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// makeJunction creates a real directory junction.
//
// mklink /J, not os.Symlink, on purpose. A junction needs NO privilege, which is
// what makes this an attack an ordinary local user can mount against elevated
// setup, and it is a different reparse tag from a symlink: os.Lstat reports a
// junction as ModeIrregular rather than ModeSymlink, so a guard written against
// symlinks is inert against the thing that is actually reachable here.
func makeJunction(t *testing.T, link, target string) {
	t.Helper()
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("create the junction target: %v", err)
	}
	output, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("cannot create a junction in this environment: %v (%s)", err, output)
	}
}

func runtimeTailRoot(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(append([]string{base}, append(append([]string{}, windowsSandboxRuntimeOwnedNames...), "abcdef0123456789")...)...)
	return base, root
}

// A SWAP AT ANY OWNED ANCESTOR MUST BE REFUSED, at the moment the tree is used.
//
// The pre-creation and post-creation checks are check-then-use however many
// times they run: an ancestor replaced afterwards is followed by the next open,
// because FILE_FLAG_OPEN_REPARSE_POINT governs only the final component of a
// pathname. Every owned component is covered here, not just the deepest one:
// a junction at "zero" with ordinary directories created below it leaves the
// leaf looking perfectly normal.
func TestTheRootedTraversalRefusesAJunctionAtEveryOwnedComponent(t *testing.T) {
	for depth := range windowsSandboxRuntimeOwnedDepth {
		t.Run("swap "+string(rune('0'+depth))+" levels above the leaf", func(t *testing.T) {
			base, root := runtimeTailRoot(t)
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatalf("create the runtime tree: %v", err)
			}

			// Replace one owned component with a junction pointing somewhere else,
			// and recreate the components below it inside the attacker's target so
			// the leaf itself is an ordinary directory.
			swapped := root
			for range depth {
				swapped = filepath.Dir(swapped)
			}
			tail, err := filepath.Rel(swapped, root)
			if err != nil {
				t.Fatalf("relate the swapped component to the root: %v", err)
			}
			if err := os.RemoveAll(swapped); err != nil {
				t.Fatalf("clear the component to swap: %v", err)
			}
			target := filepath.Join(t.TempDir(), "attacker")
			makeJunction(t, swapped, target)
			if tail != "." {
				if err := os.MkdirAll(filepath.Join(target, tail), 0o700); err != nil {
					t.Fatalf("recreate the components below the junction: %v", err)
				}
			}
			// Above the leaf, the leaf itself is now an ORDINARY directory, which is
			// exactly why a check that looks only there passes while the open lands
			// inside the attacker's tree. Asserted so the subtest cannot quietly
			// degenerate into the easy leaf-swap case.
			if depth > 0 {
				if info, err := os.Lstat(root); err != nil || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
					t.Fatalf("the leaf is not an ordinary directory, so this case is not being exercised (err %v)", err)
				}
			}

			handle, err := openWindowsRuntimeTailDirectory(root, windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_TRAVERSE)
			if err == nil {
				_ = windows.CloseHandle(handle)
				t.Fatalf("the traversal followed a junction at an owned component and would have applied the elevated ACL inside %s", target)
			}
			// A distinctive phrase, not the word "link": t.TempDir() names its
			// directory after the test, so a subtest name can put the word the
			// assertion looks for into every path in the error.
			if !strings.Contains(err.Error(), "redirects the directory") {
				t.Errorf("the refusal does not name the reason: %v", err)
			}
			_ = base
		})
	}
}

// And the ordinary tree still opens, or the guard above would be satisfied by a
// traversal that refuses everything.
func TestTheRootedTraversalOpensAnOrdinaryRuntimeRoot(t *testing.T) {
	_, root := runtimeTailRoot(t)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime tree: %v", err)
	}
	handle, err := openWindowsRuntimeTailDirectory(root, windows.READ_CONTROL|windows.FILE_TRAVERSE)
	if err != nil {
		t.Fatalf("an ordinary runtime root was refused: %v", err)
	}
	_ = windows.CloseHandle(handle)
}

// A REDIRECTED LOCALAPPDATA IS NOT AN ATTACK. The base above the owned
// components belongs to the user, and on a machine with a redirected cache
// directory it is legitimately a reparse point. Refusing there would break
// ordinary setups on ordinary machines.
func TestTheRootedTraversalFollowsAJunctionAboveTheOwnedComponents(t *testing.T) {
	real := t.TempDir()
	base := filepath.Join(t.TempDir(), "redirected-localappdata")
	makeJunction(t, base, real)

	root := filepath.Join(append([]string{base}, append(append([]string{}, windowsSandboxRuntimeOwnedNames...), "abcdef0123456789")...)...)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime tree through the redirected base: %v", err)
	}
	handle, err := openWindowsRuntimeTailDirectory(root, windows.READ_CONTROL|windows.FILE_TRAVERSE)
	if err != nil {
		t.Fatalf("a redirected cache directory was refused: %v", err)
	}
	_ = windows.CloseHandle(handle)
}

// The stamp goes into the object the traversal reached. Writing it by pathname
// left a second unbound interval after the ACL apply: a replaced tree could be
// recreated and stamped without ever carrying the capability grant, and marker
// validation still passed because it only compares the stamp's contents.
func TestTheStampIsWrittenThroughTheTraversalAndRefusesASwappedTree(t *testing.T) {
	_, root := runtimeTailRoot(t)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime tree: %v", err)
	}
	if err := writeWindowsRuntimeStampThroughHandle(root, "planhash"); err != nil {
		t.Fatalf("write the stamp through the traversal: %v", err)
	}
	recorded, err := os.ReadFile(windowsSandboxRuntimeStampPath(root, testStampPlanHash))
	if err != nil || string(recorded) != "planhash" {
		t.Fatalf("the stamp did not land in the runtime root (%q, err %v)", recorded, err)
	}

	// Now replace an owned ancestor, as an attacker would between the ACL apply
	// and the stamp.
	parent := filepath.Dir(root)
	leaf := filepath.Base(root)
	if err := os.RemoveAll(parent); err != nil {
		t.Fatalf("clear the component to swap: %v", err)
	}
	target := filepath.Join(t.TempDir(), "attacker")
	makeJunction(t, parent, target)
	if err := os.MkdirAll(filepath.Join(target, leaf), 0o700); err != nil {
		t.Fatalf("recreate the leaf inside the attacker's tree: %v", err)
	}

	if err := writeWindowsRuntimeStampThroughHandle(root, "planhash"); err == nil {
		t.Fatal("the stamp was written through a junction, marking an unprovisioned tree as set up")
	}
	if _, err := os.Stat(filepath.Join(target, leaf, windowsSandboxRuntimeStampName(testStampPlanHash))); err == nil {
		t.Errorf("a stamp was written inside the attacker's tree at %s", target)
	}
}

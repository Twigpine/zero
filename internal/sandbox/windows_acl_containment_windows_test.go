//go:build windows

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// windowsACLTestDenySID is a well-known group this process is not a member of.
//
// A deny ACE is what the carveouts actually are, and denying a group the test
// runs as revokes its own WRITE_DAC: the apply succeeds and then nothing can
// put the DACL back, so the temporary tree cannot be removed. Guests keeps the
// deny semantics without locking the test out of its own fixture.
const windowsACLTestDenySID = "S-1-5-32-546"

// makeACLJunction plants a directory junction at link pointing at target.
//
// A junction rather than a symlink on purpose: mklink /J needs no privilege, so
// it is reachable by exactly the unprivileged workspace writer this guards
// against, and the test runs on an ordinary developer or CI account.
func makeACLJunction(t *testing.T, link, target string) {
	t.Helper()
	output, err := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("mklink /J unavailable here: %v (%s)", err, output)
	}
}

// AN ELEVATED ACL WRITE MUST NOT LEAVE THE WRITE ROOT THROUGH A JUNCTION.
//
// The apply opens its target with FILE_FLAG_OPEN_REPARSE_POINT, which refuses a
// reparse point at the FINAL component and resolves every component above it.
// The write-root carveouts are derived rather than named: <root>/.git/hooks and
// <root>/.git/config are constructed from the root, and <root>/.git is a name an
// unprivileged workspace writer can create before setup runs. A junction there
// makes the apply open <junction-target>/hooks, an ordinary directory with
// nothing wrong about its final component, and `zero sandbox setup` writes a
// deny-write ACE on it as Administrator, outside the workspace.
func TestApplyWindowsACLRefusesACarveoutRedirectedByAJunction(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(filepath.Join(outside, "hooks"), 0o700); err != nil {
		t.Fatal(err)
	}
	makeACLJunction(t, filepath.Join(root, ".git"), outside)

	carveout := filepath.Join(root, ".git", "hooks")
	group := windowsACLPathGroup{
		Path:   carveout,
		Anchor: root,
		Entries: []WindowsACLEntry{{
			Action:     WindowsACLDenyWrite,
			Path:       carveout,
			Capability: windowsACLTestDenySID,
			Anchor:     root,
		}},
	}

	_, applied, err := applyWindowsACLPathGroup(group)
	if applied {
		t.Error("applied a deny-write ACE through a junction, so an elevated setup rewrote the DACL of an object outside the workspace")
	}
	var containment windowsACLContainmentError
	if !errors.As(err, &containment) {
		t.Fatalf("err = %v, want a containment refusal", err)
	}
	if !strings.Contains(containment.Actual, "elsewhere") {
		t.Errorf("the refusal does not name where the target actually resolved: %v", containment)
	}
}

// The same carveout inside an ordinary workspace still gets its ACE, or the
// refusal above would prove only that the apply fails.
func TestApplyWindowsACLStillAppliesAnOrdinaryCarveout(t *testing.T) {
	root := t.TempDir()
	carveout := filepath.Join(root, ".git", "hooks")
	if err := os.MkdirAll(carveout, 0o700); err != nil {
		t.Fatal(err)
	}
	group := windowsACLPathGroup{
		Path:   carveout,
		Anchor: root,
		Entries: []WindowsACLEntry{{
			Action:     WindowsACLDenyWrite,
			Path:       carveout,
			Capability: windowsACLTestDenySID,
			Anchor:     root,
		}},
	}

	snapshot, applied, err := applyWindowsACLPathGroup(group)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// PUT IT BACK, OR THE TEMP TREE CANNOT BE REMOVED. The ACE just written
	// denies write to the group this test runs as, and t.TempDir removes the
	// tree when the test ends.
	if applied {
		t.Cleanup(func() {
			if err := rollbackWindowsACLSnapshots([]windowsACLSnapshot{snapshot}); err != nil {
				t.Errorf("rollback: %v", err)
			}
		})
	}
	if !applied {
		t.Fatal("an ordinary carveout inside the write root was not applied")
	}
	if snapshot.Path != carveout {
		t.Fatalf("snapshot path = %q, want %q", snapshot.Path, carveout)
	}
}

// MATERIALIZATION MUST NOT CREATE THROUGH THE JUNCTION EITHER. os.MkdirAll
// walks a pathname and follows every reparse point on it, so without the
// pre-create check an elevated setup makes the directory on the far side and
// only notices afterwards.
func TestApplyWindowsACLRefusesToMaterializeThroughAJunction(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	makeACLJunction(t, filepath.Join(root, ".git"), outside)

	carveout := filepath.Join(root, ".git", "hooks")
	if _, err := os.Lstat(filepath.Join(outside, "hooks")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SETUP INVALID: the target already holds hooks: %v", err)
	}
	group := windowsACLPathGroup{
		Path:        carveout,
		Anchor:      root,
		Materialize: true,
		Entries: []WindowsACLEntry{{
			Action:      WindowsACLDenyRead,
			Path:        carveout,
			Capability:  windowsACLTestDenySID,
			Anchor:      root,
			Materialize: true,
		}},
	}

	if _, applied, err := applyWindowsACLPathGroup(group); applied || err == nil {
		t.Errorf("materialized through a junction: applied=%v err=%v", applied, err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "hooks")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an elevated create landed outside the write root: %v", err)
	}
}

// An operator-named path carries no anchor and keeps the behaviour it had: the
// final component is still refused when it is a reparse point, and nothing above
// it is second-guessed, because above the write root the path belongs to the
// operator, who may keep a workspace under a junction.
func TestApplyWindowsACLLeavesAnUnanchoredPathAlone(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(filepath.Join(outside, "leaf"), 0o700); err != nil {
		t.Fatal(err)
	}
	makeACLJunction(t, filepath.Join(base, "link"), outside)

	named := filepath.Join(base, "link", "leaf")
	group := windowsACLPathGroup{
		Path: named,
		Entries: []WindowsACLEntry{{
			Action:     WindowsACLDenyWrite,
			Path:       named,
			Capability: windowsACLTestDenySID,
		}},
	}

	snapshot, applied, err := applyWindowsACLPathGroup(group)
	if err != nil {
		t.Fatalf("an operator-named path below a junction was refused: %v", err)
	}
	if applied {
		t.Cleanup(func() {
			if err := rollbackWindowsACLSnapshots([]windowsACLSnapshot{snapshot}); err != nil {
				t.Errorf("rollback: %v", err)
			}
		})
	}
	if !applied {
		t.Fatal("an operator-named path below a junction was not applied")
	}
}

// AND THE GUARD ON THE CREATE HAS TO BE PINNED WHERE IT CAN BE SEEN.
//
// Through the whole apply, dropping the pre-create check is invisible: the
// directory is made on the far side of the junction, the containment check on
// the handle then refuses, and the failure path removes what it made, so the
// filesystem afterwards looks the same either way. The transient elevated create
// outside the write root is the thing being prevented, so it is checked here,
// against the function that prevents it.
func TestVerifyWindowsACLPathUnderAnchorRefusesAMissingTargetBehindAJunction(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	makeACLJunction(t, filepath.Join(root, ".git"), outside)

	carveout := filepath.Join(root, ".git", "hooks")
	if _, err := os.Lstat(carveout); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SETUP INVALID: the carveout already exists, so nothing would be created: %v", err)
	}

	err := verifyWindowsACLPathUnderAnchor(root, carveout)
	var containment windowsACLContainmentError
	if !errors.As(err, &containment) {
		t.Fatalf("err = %v, want a containment refusal before anything is created", err)
	}

	// The ordinary case still says yes, or the refusal proves only that the
	// function rejects things.
	ordinary := t.TempDir()
	if err := verifyWindowsACLPathUnderAnchor(ordinary, filepath.Join(ordinary, ".git", "hooks")); err != nil {
		t.Errorf("an ordinary missing carveout was refused: %v", err)
	}
	// And an unanchored path is not this function's business.
	if err := verifyWindowsACLPathUnderAnchor("", carveout); err != nil {
		t.Errorf("an unanchored path was refused: %v", err)
	}
}

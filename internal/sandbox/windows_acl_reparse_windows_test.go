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

// An unprivileged user who controls the workspace can turn an ancestor of a
// configured ACL target into a junction before elevated setup runs. CreateFile
// resolves ancestors even with FILE_FLAG_OPEN_REPARSE_POINT, so the
// final-component check passes and setup would rewrite the DACL of an object
// outside the approved tree.
//
// Junctions, unlike symlinks, need no privilege — which is what makes this
// reachable by exactly the user the sandbox is containing.
func TestOpenWindowsACLTargetRefusesAJunctionAncestor(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	outside := filepath.Join(base, "OUTSIDE")
	for _, dir := range []string{workspace, outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// The object the attacker wants setup to touch.
	victim := filepath.Join(outside, "hooks")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}

	// .git is the ancestor, and it is a junction to OUTSIDE.
	gitDir := filepath.Join(workspace, ".git")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", gitDir, outside).CombinedOutput(); err != nil {
		t.Skipf("cannot create a junction here: %v %s", err, out)
	}

	// This is the path setup would be configured with.
	target := filepath.Join(gitDir, "hooks")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("precondition: the junction should make %s reachable: %v", target, err)
	}

	handle, _, err := openWindowsACLTarget(target)
	if err == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("opened an ACL target through a junction ancestor; setup would have rewritten a DACL outside the workspace")
	}
	if !strings.Contains(err.Error(), "reparse point") {
		t.Errorf("error = %v, want it to name the reparse point", err)
	}
	t.Logf("refused as expected: %v", err)
}

// The guard must not reject ordinary targets, including ones spelled
// non-canonically — a differently-cased path is the same object, not a redirect.
func TestOpenWindowsACLTargetAcceptsAnOrdinaryTarget(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "Nested")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, spelling := range []string{nested, strings.ToLower(nested)} {
		handle, isDir, err := openWindowsACLTarget(spelling)
		if err != nil {
			t.Fatalf("openWindowsACLTarget(%q): %v", spelling, err)
		}
		if !isDir {
			t.Errorf("%q reported as not a directory", spelling)
		}
		_ = windows.CloseHandle(handle)
	}
}

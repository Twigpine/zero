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

// THE COMPARISON BASIS MUST NOT FOLLOW WHAT IT IS CHECKING FOR.
//
// verifyWindowsACLTargetNotRedirected asks GetFinalPathNameByHandle where the
// handle landed and compares that against the requested path. It used to build
// the second side with filepath.EvalSymlinks, which is the same resolution the
// kernel had just performed, so for a directory symlink the two sides agreed
// BECAUSE the redirect happened, and elevated setup rewrote the DACL of an
// object outside the workspace.
//
// Junctions were caught, but only because Go reports one as ModeIrregular and
// EvalSymlinks declines it, leaving the lexical path to disagree. That is a
// standard-library detail, not a property of the guard.

// The property, testable with no privilege at all: the normalizer must not
// resolve a reparse point.
//
// HONEST LIMIT, because it would be easy to read more into this than it proves.
// A junction is used because creating one needs no privilege, and EvalSymlinks
// does not resolve a junction either — so this test passes against BOTH the old
// basis and the new one, and reverting the fix does not fail it. It is an
// invariant test, not a regression for this change: it pins that normalization
// leaves a reparse point alone, which is what would break first if a future Go
// started resolving mount points and silently disarmed the junction case.
//
// The case that actually separates the two bases is the directory symlink below,
// and that one skips without privilege. On this machine it skipped.
func TestACLComparablePathDoesNotResolveAReparsePoint(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "hooks"), 0o700); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	link := filepath.Join(base, "link")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
		t.Skipf("cannot create a junction here: %v (%s)", err, out)
	}

	through := filepath.Join(link, "hooks")
	comparable := windowsACLComparablePath(through)

	// Compared against the normalizer's OWN answer for each path, never against
	// the raw input. GetLongPathName legitimately rewrites the string: on a CI
	// runner the temp directory is an 8.3 short name, so expanding RUNNER~1 to
	// runneradmin makes the result differ from what was passed in while naming
	// exactly the same object. An earlier version of this test asserted equality
	// with the input and failed on Windows CI for that reason, which was the test
	// being wrong rather than the code.
	target := windowsACLComparablePath(filepath.Join(outside, "hooks"))

	// The whole property: normalizing a path THROUGH the reparse point must not
	// produce the target's normalization. If it did, the guard would be comparing
	// a redirected handle against a redirected expectation and matching itself.
	if strings.EqualFold(filepath.Clean(comparable), filepath.Clean(target)) {
		t.Errorf("windowsACLComparablePath resolved through the reparse point:\n  through %s\n  target  %s", comparable, target)
	}
	// And it still has to name the link component, or it resolved to something
	// else entirely rather than leaving the path alone.
	if !strings.Contains(strings.ToLower(comparable), "link") {
		t.Errorf("windowsACLComparablePath lost the link component: %s", comparable)
	}

	// The old basis is checked alongside for contrast: where it resolves, it
	// could not have been the comparison basis.
	if resolved := canonicalSandboxWorkspaceRoot(through); strings.EqualFold(filepath.Clean(resolved), filepath.Clean(filepath.Join(outside, "hooks"))) {
		t.Logf("confirmed: canonicalSandboxWorkspaceRoot resolves %s to %s, which is why it cannot be the basis", through, resolved)
	}
}

// The end-to-end symlink case the finding names. Directory symlinks need either
// Developer Mode or SeCreateSymbolicLinkPrivilege, so this skips where it cannot
// build the fixture rather than passing vacuously.
func TestOpenWindowsACLTargetRefusesASymlinkAncestor(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "hooks"), 0o700); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	workspace := filepath.Join(base, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}

	// A DIRECTORY symlink, not a junction: os.Symlink maps to CreateSymbolicLinkW
	// and the directory flag is inferred from the existing target.
	link := filepath.Join(workspace, ".git")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create a directory symlink here (needs Developer Mode or SeCreateSymbolicLinkPrivilege): %v", err)
	}

	target := filepath.Join(link, "hooks")
	handle, _, err := openWindowsACLTarget(target)
	if err == nil {
		_ = windows.CloseHandle(handle)
		t.Fatal("elevated setup opened a target through a symlinked ancestor; its DACL change would land outside the workspace")
	}
	if !strings.Contains(err.Error(), "reparse point") {
		t.Errorf("refusal does not name the cause: %v", err)
	}
}

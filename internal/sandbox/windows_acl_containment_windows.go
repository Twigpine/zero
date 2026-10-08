//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Keeping an elevated ACL write inside the tree it was planned for.
//
// FILE_FLAG_OPEN_REPARSE_POINT GUARDS ONE COMPONENT, AND A PATH HAS MANY. The
// apply opens its target with that flag, which stops the FINAL component from
// being followed and refuses the handle when it is a reparse point. Every
// component above it is resolved normally, as it must be for any absolute path
// to work at all.
//
// That is enough for a target the operator named, and not enough for one the
// sandbox derived. The write-root carveouts are derived: <root>/.git/hooks and
// <root>/.git/config are constructed from a root, and <root>/.git is a name an
// unprivileged workspace writer can create. `mklink /J` needs no privilege, so
// a junction there makes the apply open <junction-target>/hooks instead, and
// `zero sandbox setup` writes a deny-write ACE, as Administrator, on an object
// outside the workspace. The final-component flag never fires: the object at
// the end of the walk is an ordinary directory.
//
// So the derived targets carry the root they were derived FROM, and the object
// finally opened has to still be inside it. The check is on the handle, not on
// the name: GetFinalPathNameByHandle answers where the object this process is
// holding actually lives, and both sides of the comparison go through it so the
// spelling is normalized the same way (\\?\ prefix, long names, drive letter).
// A junction anywhere along the derived tail moves the answer out of the root
// and the apply refuses.
//
// The rule is deliberately not "no reparse point anywhere in the path". Above
// the write root the path belongs to the operator, who may legitimately keep a
// workspace under a junction or a mapped directory, and refusing that would
// break setups this has nothing to say about. Only the tail the sandbox itself
// appended is held strict.

// windowsFinalPathByHandle reports where an open handle's object actually lives.
func windowsFinalPathByHandle(handle windows.Handle) (string, error) {
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
	if err != nil {
		return "", err
	}
	if int(n) > len(buffer) {
		buffer = make([]uint16, n)
		n, err = windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
		if err != nil {
			return "", err
		}
	}
	return windows.UTF16ToString(buffer[:n]), nil
}

// windowsFinalPathOf opens path without following a final-component reparse
// point and reports where the object lives. A missing path is surfaced as
// os.ErrNotExist so callers can walk up to a parent that exists.
func windowsFinalPathOf(path string) (string, error) {
	utf16Path, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", fmt.Errorf("encode windows path %s: %w", path, err)
	}
	handle, err := windows.CreateFile(
		utf16Path,
		windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return "", fmt.Errorf("open windows path %s: %w", path, err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	final, err := windowsFinalPathByHandle(handle)
	if err != nil {
		return "", fmt.Errorf("resolve windows path %s: %w", path, err)
	}
	return final, nil
}

// windowsACLTailUnderAnchor returns the components target adds to anchor, and
// whether target is under anchor lexically at all.
func windowsACLTailUnderAnchor(anchor, target string) (string, bool) {
	relative, err := filepath.Rel(filepath.Clean(anchor), filepath.Clean(target))
	if err != nil {
		return "", false
	}
	if relative == "." {
		return "", true
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return relative, true
}

// windowsACLContainmentError reports a derived target that resolved outside the
// root it was derived from.
type windowsACLContainmentError struct {
	Anchor string
	Target string
	Actual string
}

func (err windowsACLContainmentError) Error() string {
	return fmt.Sprintf(
		"refusing to apply ACL to %s: it resolves to %s, outside the write root %s; "+
			"a reparse point on the way down redirects an elevated ACL write out of the sandbox",
		err.Target, err.Actual, err.Anchor)
}

// verifyWindowsACLHandleUnderAnchor refuses a handle whose object does not live
// where the plan said it would.
//
// anchor empty means the target was named rather than derived, and there is no
// owned tail to hold strict; the final-component check the caller already did is
// the whole guard for those.
func verifyWindowsACLHandleUnderAnchor(handle windows.Handle, anchor, target string) error {
	if strings.TrimSpace(anchor) == "" {
		return nil
	}
	tail, ok := windowsACLTailUnderAnchor(anchor, target)
	if !ok {
		return windowsACLContainmentError{Anchor: anchor, Target: target, Actual: target}
	}
	anchorFinal, err := windowsFinalPathOf(anchor)
	if err != nil {
		return fmt.Errorf("resolve write root %s: %w", anchor, err)
	}
	targetFinal, err := windowsFinalPathByHandle(handle)
	if err != nil {
		return fmt.Errorf("resolve windows ACL target %s: %w", target, err)
	}
	expected := anchorFinal
	if tail != "" {
		expected = filepath.Join(anchorFinal, tail)
	}
	if !strings.EqualFold(filepath.Clean(targetFinal), filepath.Clean(expected)) {
		return windowsACLContainmentError{Anchor: anchor, Target: target, Actual: targetFinal}
	}
	return nil
}

// verifyWindowsACLPathUnderAnchor makes the same check against the deepest
// component of target that exists, before anything is created.
//
// Materialization creates a missing target with os.MkdirAll, which walks a
// pathname and follows every reparse point on it, so without this an elevated
// setup creates the directory on the far side of a junction and only the
// containment check afterwards notices. The window between this check and that
// create is not closed here; closing it needs the components created relative to
// retained handles, which is the rooted descent tracked in #808.
func verifyWindowsACLPathUnderAnchor(anchor, target string) error {
	if strings.TrimSpace(anchor) == "" {
		return nil
	}
	if _, ok := windowsACLTailUnderAnchor(anchor, target); !ok {
		return windowsACLContainmentError{Anchor: anchor, Target: target, Actual: target}
	}
	existing := filepath.Clean(target)
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect windows ACL target %s: %w", existing, err)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return nil
		}
		existing = parent
	}
	tail, ok := windowsACLTailUnderAnchor(anchor, existing)
	if !ok || tail == "" {
		// Above or at the write root, which is the operator's to arrange.
		return nil
	}
	handle, err := windowsACLOpenForContainment(existing)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	// THE ANCESTOR ITSELF IS THE REDIRECTION, WHICH IS WHY ITS NAME LOOKS RIGHT.
	//
	// Asking only where this object lives answers "exactly where you asked": the
	// open above does not follow a final-component reparse point, so a junction
	// at <root>/.git comes back as <root>/.git. os.MkdirAll does follow it, and
	// creates the missing components on the far side. So what disqualifies this
	// ancestor is that it IS a reparse point, not where it reports living.
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("inspect windows ACL target %s: %w", existing, err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return windowsACLContainmentError{Anchor: anchor, Target: target, Actual: existing + " (a reparse point)"}
	}
	return verifyWindowsACLHandleUnderAnchor(handle, anchor, existing)
}

func windowsACLOpenForContainment(path string) (windows.Handle, error) {
	utf16Path, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("encode windows ACL target %s: %w", path, err)
	}
	handle, err := windows.CreateFile(
		utf16Path,
		windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return 0, fmt.Errorf("open windows ACL target %s: %w", path, err)
	}
	return handle, nil
}

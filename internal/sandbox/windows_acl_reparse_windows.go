//go:build windows

package sandbox

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// GetFinalPathNameByHandle flags. x/sys/windows does not export these.
const (
	windowsFileNameNormalized uint32 = 0x0
	windowsVolumeNameDOS      uint32 = 0x0
)

// verifyWindowsACLTargetNotRedirected fails when an opened handle resolved
// somewhere other than the requested path, which means a component along the way
// is a reparse point.
//
// openWindowsACLTarget's FILE_FLAG_OPEN_REPARSE_POINT check covers the FINAL
// component only; CreateFile still resolves ANCESTORS. A user who controls the
// workspace can turn an ancestor — .git, say — into a junction before elevated
// setup runs, and setup would then apply its DACL change to an object outside
// the approved tree while the final-component check still passed. Junctions need
// no privilege to create, unlike symlinks, so this is reachable by exactly the
// unprivileged user the sandbox exists to contain.
//
// GetFinalPathNameByHandle answers where the handle actually landed, covering
// every component in one call rather than walking the path and re-checking each
// component (which would also race between the checks).
//
// The comparison basis must NEVER follow a reparse point, and getting that wrong
// is what made this guard mostly ornamental.
//
// It used to compare against canonicalSandboxWorkspaceRoot(path), which runs
// filepath.EvalSymlinks — the same resolution the kernel just performed. So for a
// directory SYMLINK the two sides agreed precisely BECAUSE the redirect happened:
// GetFinalPathNameByHandle followed the link to produce one side, EvalSymlinks
// followed the same link to produce the other, and elevated setup went on to
// rewrite the DACL of an object outside the workspace.
//
// Junctions were rejected, but by accident rather than by design. Go reports a
// junction as ModeIrregular rather than ModeSymlink, so EvalSymlinks refuses it,
// canonicalSandboxWorkspaceRoot falls back to the lexical path, and the mismatch
// surfaces. That is a property of the standard library's mode bits, not of this
// check: a Go release that resolved mount points would silently disarm the one
// case this was known to catch.
//
// So the expected side is now normalized WITHOUT resolving anything. Casing and
// 8.3 short names still have to compare equal, since a legitimate target can be
// spelled either way and still be the same object — EqualFold covers the first
// and GetLongPathName the second, and neither follows a link.
//
// This does mean a target deliberately spelled through a symlink is now refused.
// That is the intended direction: the whole question here is whether the object
// the handle landed on is the object that was named, and a caller that wants a
// link's target should name the target.
func verifyWindowsACLTargetNotRedirected(handle windows.Handle, path string) error {
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), windowsFileNameNormalized|windowsVolumeNameDOS)
	if err != nil {
		return fmt.Errorf("resolve windows ACL target %s: %w", path, err)
	}
	if int(n) < len(buffer) {
		buffer = buffer[:n]
	}
	actual := trimWindowsExtendedPathPrefix(windows.UTF16ToString(buffer))
	expected := trimWindowsExtendedPathPrefix(windowsACLComparablePath(path))
	if !strings.EqualFold(filepath.Clean(actual), filepath.Clean(expected)) {
		return fmt.Errorf("refusing to apply ACL to %s: it resolves to %s, so a parent directory is a reparse point (possible path swap during elevated setup)", path, actual)
	}
	return nil
}

// windowsACLComparablePath normalizes a path for comparison against where a
// handle actually landed, WITHOUT resolving any reparse point.
//
// The two things that must still compare equal are casing and 8.3 short names,
// because either can spell the same object. GetLongPathName expands the short
// form by reading directory entries; it does not follow a symlink or a junction
// to its target, which is the entire reason it is used here instead of
// EvalSymlinks. Casing is left to the caller's EqualFold.
//
// Every failure degrades to the lexically cleaned path rather than to a resolved
// one. A path this cannot expand is compared as written, which can only produce
// a spurious MISMATCH — a refusal — and never a spurious match. That is the
// direction a redirect check has to fail in.
func windowsACLComparablePath(path string) string {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	if cleaned == "" || cleaned == "." {
		return cleaned
	}
	if absolute, err := filepath.Abs(cleaned); err == nil {
		cleaned = absolute
	}
	wide, err := windows.UTF16PtrFromString(cleaned)
	if err != nil {
		return cleaned
	}
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetLongPathName(wide, &buffer[0], uint32(len(buffer)))
	if err != nil || n == 0 || int(n) >= len(buffer) {
		return cleaned
	}
	return filepath.Clean(windows.UTF16ToString(buffer[:n]))
}

// trimWindowsExtendedPathPrefix strips the \?\ form GetFinalPathNameByHandle
// returns so it can be compared with an ordinary path.
func trimWindowsExtendedPathPrefix(path string) string {
	// Built from filepath.Separator rather than written as literals so the
	// backslashes cannot be miscounted by whatever writes this file.
	sep := string(filepath.Separator)
	devicePrefix := sep + sep + "?" + sep
	uncPrefix := devicePrefix + "UNC" + sep
	if strings.HasPrefix(path, uncPrefix) {
		return sep + sep + strings.TrimPrefix(path, uncPrefix)
	}
	return strings.TrimPrefix(path, devicePrefix)
}

// verifyWindowsACLPathComponentNotRedirected opens one path component no-follow
// and refuses it if it is a reparse point or resolves anywhere other than its own
// pathname. Because GetFinalPathNameByHandle answers for the WHOLE resolved path,
// verifying a single existing component also clears every ancestor above it.
//
// A missing component surfaces as os.ErrNotExist so the caller can walk further
// up. Only FILE_READ_ATTRIBUTES is requested: this inspects, it never writes, and
// asking for more would fail on ancestors the setup process has no rights on.
func verifyWindowsACLPathComponentNotRedirected(path string) error {
	utf16Path, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("encode windows ACL path component %s: %w", path, err)
	}
	handle, err := windows.CreateFile(
		utf16Path,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		// syscall.Errno.Is maps ERROR_FILE_NOT_FOUND/ERROR_PATH_NOT_FOUND to
		// os.ErrNotExist, so the caller's errors.Is check keeps working.
		return fmt.Errorf("open windows ACL path component %s: %w", path, err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("inspect windows ACL path component %s: %w", path, err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("refusing to materialize under reparse-point path component %s: possible path swap during elevated setup", path)
	}
	return verifyWindowsACLTargetNotRedirected(handle, path)
}

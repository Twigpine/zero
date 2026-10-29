//go:build windows

package sandbox

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// physicalTempDir returns os.TempDir as the object it actually names, so the
// fallback anchor's parent chain is physical and only the anchor itself is a
// new component.
//
// peermsg.EnsurePrivateDir walks from the volume root with OBJ_DONT_REPARSE and
// refuses any component that is a reparse point. That is right for the owned
// tail and wrong for the ancestors above it: a redirected %LOCALAPPDATA% is an
// ordinary Windows configuration, and TEMP sits beneath it. The constraint
// jatmn stated for #901 applies here identically: redirected cache and TEMP
// locations above the owned tail must keep working; the restriction is on what
// Zero owns.
//
// GetFinalPathNameByHandle rather than filepath.EvalSymlinks, because
// EvalSymlinks does not traverse a junction on Windows: it returns the
// junction's own path and the walker then refuses it. The handle answers where
// the directory actually is. Same recipe as verifyWindowsACLTargetNotRedirected,
// which is why the flag constants and the prefix trim are shared.
func physicalTempDir() (string, error) {
	return physicalDir(os.TempDir())
}

// physicalDir resolves any directory through a handle, for the other places
// that have to hand EnsurePrivateDir a physical parent. GetFinalPathNameByHandle
// rather than EvalSymlinks, because EvalSymlinks does not traverse a junction.
func physicalDir(path string) (string, error) {
	utf16Path, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", fmt.Errorf("encode directory %s: %w", path, err)
	}
	handle, err := windows.CreateFile(
		utf16Path,
		windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", fmt.Errorf("open directory %s: %w", path, err)
	}
	defer windows.CloseHandle(handle)
	buffer := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), windowsFileNameNormalized|windowsVolumeNameDOS)
	if err != nil {
		return "", fmt.Errorf("resolve directory %s: %w", path, err)
	}
	if int(n) < len(buffer) {
		buffer = buffer[:n]
	}
	return trimWindowsExtendedPathPrefix(windows.UTF16ToString(buffer)), nil
}

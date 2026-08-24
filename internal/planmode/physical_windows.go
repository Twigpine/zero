//go:build windows

package planmode

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// resolvePhysical returns path in its canonical physical spelling.
//
// filepath.EvalSymlinks cannot do this alone on Windows. It resolves name
// surrogates (directory symlinks) but not junctions, which os.Lstat reports as
// os.ModeIrregular rather than os.ModeSymlink, so EvalSymlinks hands a junction
// straight back. A junction needs no SeCreateSymbolicLinkPrivilege, so it is
// the reparse point an unprivileged process can actually plant, and treating
// one as its own physical path lets a staging directory that really lands in
// the workspace or the OS temp directory compare as though it sits outside
// both.
//
// GetFinalPathNameByHandle asks the filesystem what the open handle resolved
// to, which is the only answer that accounts for every reparse type at once.
// VOLUME_NAME_DOS also returns long names, so it subsumes the 8.3 short-name
// normalization (RUNNER~1) the caller needs anyway.
func resolvePhysical(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	pathUTF16, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return "", err
	}
	// FILE_FLAG_BACKUP_SEMANTICS is required to open a directory handle, and
	// no reparse flag is passed precisely so the open follows to the target
	// this call is asking about.
	handle, err := windows.CreateFile(
		pathUTF16,
		0, // Query the name only; no read or write access is needed.
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(handle)

	return finalPathName(handle)
}

// fileNameNormalized|volumeNameDOS is the GetFinalPathNameByHandle flag pair
// that asks for the normalized (long-name) path with a drive letter. Both are
// zero, and x/sys/windows does not export either, so they are named here
// rather than left as a bare literal.
const (
	fileNameNormalized = 0x0
	volumeNameDOS      = 0x0
)

// finalPathName reads the resolved path off an open handle, growing the buffer
// if the path is longer than MAX_PATH (a resolved path can be, which is why
// the API reports the size it needs).
func finalPathName(handle windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_PATH)
	for range 2 {
		// On success n excludes the terminating NUL; when the buffer is too
		// small n is the required size INCLUDING it, so n >= len(buf) is the
		// signal to grow rather than a result.
		n, err := windows.GetFinalPathNameByHandle(handle, &buf[0], uint32(len(buf)), fileNameNormalized|volumeNameDOS)
		if err != nil {
			return "", err
		}
		if n < uint32(len(buf)) {
			return trimExtendedLengthPrefix(windows.UTF16ToString(buf[:n])), nil
		}
		if n > windows.MAX_LONG_PATH {
			return "", fmt.Errorf("resolved path needs %d UTF-16 units, over the %d limit", n, windows.MAX_LONG_PATH)
		}
		buf = make([]uint16, n)
	}
	return "", errors.New("resolved path length kept growing between calls")
}

// trimExtendedLengthPrefix converts the extended-length spelling
// GetFinalPathNameByHandle returns back to the ordinary Win32 form, so the
// result compares against paths spelled the way the rest of the process
// spells them. `\\?\UNC\server\share` is a UNC path, not a drive path, and
// has to become `\\server\share` rather than `UNC\server\share`.
func trimExtendedLengthPrefix(path string) string {
	if rest, ok := strings.CutPrefix(path, `\\?\UNC\`); ok {
		return `\\` + rest
	}
	if rest, ok := strings.CutPrefix(path, `\\?\`); ok {
		return rest
	}
	return path
}

// pathIsReparsePoint reports whether path itself is a reparse point of any
// kind, junctions included. os.Lstat cannot answer this: it maps a junction to
// os.ModeIrregular, which is indistinguishable from other irregular files, so
// verifyPrivateDirectory's os.ModeSymlink test never fires for one.
func pathIsReparsePoint(path string) bool {
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	attrs, err := windows.GetFileAttributes(pathUTF16)
	if err != nil {
		return false
	}
	return attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

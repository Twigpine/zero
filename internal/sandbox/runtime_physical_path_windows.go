//go:build windows

package sandbox

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

const (
	// GetFinalPathNameByHandle flags. golang.org/x/sys/windows does not export
	// these; both are zero, and naming them keeps the call site readable.
	fileNameNormalized = 0x0
	volumeNameDOS      = 0x0
)

// physicalSandboxPath resolves path to the spelling the filesystem itself uses.
//
// canonicalSandboxWorkspaceRoot cannot do this on Windows. filepath.EvalSymlinks
// returns a directory JUNCTION unchanged, so a TEMP that reaches into the
// workspace through one still measures as outside it, and that is how a runtime
// tree ends up inside the very workspace the sandbox exists to confine.
// GetFinalPathNameByHandle answers with the target's real path, junctions and
// mount points followed and casing as stored on disk.
//
// This deliberately opens WITHOUT FILE_FLAG_OPEN_REPARSE_POINT, the opposite of
// openWindowsACLTarget. That helper must refuse to follow a reparse point,
// because following one is the path-swap it guards against. Here the whole
// question is where the reparse point leads, and the answer is only ever used to
// decide that a runtime root is contained, never that it is safe.
func physicalSandboxPath(path string) string {
	cleaned := canonicalSandboxWorkspaceRoot(path)
	if cleaned == "" || cleaned == "." {
		return cleaned
	}
	// The runtime root does not exist yet at derivation time, which is the point.
	// Resolve the deepest ancestor that does exist and re-append the rest, the
	// same shape canonicalSandboxWorkspaceRoot uses for EvalSymlinks.
	remainder := ""
	current := cleaned
	for {
		if resolved, ok := finalWindowsPathName(current); ok {
			if remainder == "" {
				return resolved
			}
			return filepath.Join(resolved, remainder)
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Nothing along the path could be opened. The cleaned form is the best
			// answer available, and the caller's spelling comparison already ran.
			return cleaned
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}

func finalWindowsPathName(path string) (string, bool) {
	utf16Path, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", false
	}
	// Zero desired access is enough: GetFinalPathNameByHandle reads metadata, so
	// this cannot be refused for lack of read rights on the directory contents.
	handle, err := windows.CreateFile(
		utf16Path,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return "", false
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	buffer := make([]uint16, windows.MAX_PATH)
	for attempt := 0; attempt < 2; attempt++ {
		n, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), fileNameNormalized|volumeNameDOS)
		if err != nil {
			return "", false
		}
		if int(n) >= len(buffer) {
			// TWO DIFFERENT CONVENTIONS, and the boundary is where they meet. On
			// success the return value EXCLUDES the terminating null; on an
			// insufficient buffer it INCLUDES it. So n == len(buffer) cannot be
			// read as a complete path: a success that large would not have fitted
			// its own terminator, which means the only reading left is a required
			// size. Retrying on >= costs one extra call in a case that may not be
			// reachable and removes the need to be right about which it was.
			buffer = make([]uint16, int(n)+1)
			continue
		}
		if n == 0 {
			return "", false
		}
		return trimWindowsExtendedPrefix(windows.UTF16ToString(buffer[:n])), true
	}
	return "", false
}

// trimWindowsExtendedPrefix converts the \\?\ form GetFinalPathNameByHandle
// returns into an ordinary path, so it compares against paths the rest of this
// package builds with filepath.Join. \\?\UNC\server\share becomes
// \\server\share; anything else loses the \\?\ and keeps its drive letter.
func trimWindowsExtendedPrefix(path string) string {
	if rest, ok := strings.CutPrefix(path, `\\?\UNC\`); ok {
		return `\\` + rest
	}
	return strings.TrimPrefix(path, `\\?\`)
}

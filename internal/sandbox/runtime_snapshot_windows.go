//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// snapshotRuntimeStampBound reads the runtime root's identity and its existing
// stamp through ONE handle.
//
// The two used to be taken separately: runtimeDirIdentity opened the root, read
// its volume and file ID, and closed the handle, and then os.ReadFile resolved
// the pathname again. A rename and substitution in that interval pairs A's
// identity with B's stamp bytes, and a rollback that correctly proves it holds A
// then writes B's bytes into it, corrupting an attestation that predates this
// run. The lease stops cleanup selecting the root; it does not stop the parent's
// owner renaming it.
//
// Reading the child relative to the identified handle removes the second
// resolution, so both facts describe the same object by construction.
//
// EVERY FAILURE IS AN ERROR, NOT AN ABSENCE. This used to collapse an encoding
// failure, a directory that would not open, an identity that could not be read,
// a denied child open and a short read all into the same "there was no stamp"
// answer that a genuine ERROR_FILE_NOT_FOUND produces. The stamp writer uses
// FILE_OVERWRITE_IF and can replace an existing stamp even where the read was
// denied, so that lie let a FAILED setup delete an attestation it had no record
// of, leaving the previous run's marker pointing at an unusable runtime root.
// Only a positive not-found produces runtimeStampAbsent.
func snapshotRuntimeStampBound(root string, name string) (identity string, identified bool, prior []byte, state runtimeStampState, err error) {
	utf16Root, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return "", false, nil, runtimeStampUnknown, fmt.Errorf("encode sandbox runtime root %s: %w", root, err)
	}
	directory, err := windows.CreateFile(
		utf16Root,
		windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		// A root that is simply not there yet is the ordinary first-run case: the
		// created-directory rollback owns it and there is no prior stamp to lose.
		if isWindowsNotFound(err) {
			return "", false, nil, runtimeStampAbsent, nil
		}
		return "", false, nil, runtimeStampUnknown, fmt.Errorf("open sandbox runtime root %s: %w", root, err)
	}
	defer windows.CloseHandle(directory)

	identity, idErr := handleRuntimeIdentity(directory)
	if idErr != nil {
		return "", false, nil, runtimeStampUnknown, fmt.Errorf("identify sandbox runtime root %s: %w", root, idErr)
	}

	stamp, err := openWindowsChildNoFollow(directory, name,
		windows.GENERIC_READ|windows.FILE_READ_ATTRIBUTES, windows.FILE_NON_DIRECTORY_FILE)
	if err != nil {
		if isWindowsNotFound(err) {
			// Proven absent. The identity still stands: it came from the handle
			// above, not from this.
			return identity, true, nil, runtimeStampAbsent, nil
		}
		return identity, true, nil, runtimeStampUnknown, fmt.Errorf("open the sandbox runtime stamp in %s: %w", root, err)
	}
	file := os.NewFile(uintptr(stamp), name)
	defer file.Close()
	data, readErr := io.ReadAll(file)
	if readErr != nil {
		return identity, true, nil, runtimeStampUnknown, fmt.Errorf("read the sandbox runtime stamp in %s: %w", root, readErr)
	}
	return identity, true, data, runtimeStampPresent, nil
}

// isWindowsNotFound reports the two statuses that mean the object genuinely is
// not there, as opposed to the many that mean it could not be looked at.
//
// openWindowsChildNoFollow wraps its NTSTATUS, and CreateFile returns the Win32
// errno, so both spellings are checked rather than assuming one layer.
func isWindowsNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND) ||
		errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) ||
		errors.Is(err, windows.STATUS_OBJECT_PATH_NOT_FOUND)
}

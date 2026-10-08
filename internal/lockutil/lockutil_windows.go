//go:build windows

package lockutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type platformLockState struct {
	overlapped windows.Overlapped
}

const lockByteOffsetHigh = 1

func openLockFileAt(root, relative, displayPath string) (_ *os.File, resultErr error) {
	rootPtr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, err
	}
	parent, err := windows.CreateFile(
		rootPtr,
		windows.FILE_READ_ATTRIBUTES|windows.FILE_TRAVERSE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("open lock root: %w", err)
	}
	defer func() {
		if parent != 0 {
			resultErr = errors.Join(resultErr, windows.CloseHandle(parent))
		}
	}()

	components := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	for _, component := range components[:len(components)-1] {
		next, openErr := openLockDirectoryAt(parent, component)
		if openErr != nil {
			return nil, fmt.Errorf("open lock path component %q: %w", component, openErr)
		}
		if closeErr := windows.CloseHandle(parent); closeErr != nil {
			_ = windows.CloseHandle(next)
			parent = 0
			return nil, fmt.Errorf("close lock path component: %w", closeErr)
		}
		parent = next
	}

	handle, err := openLockFileHandleAt(parent, components[len(components)-1])
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), displayPath)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("convert lock handle to file")
	}
	if err := windows.CloseHandle(parent); err != nil {
		parent = 0
		return nil, errors.Join(fmt.Errorf("close lock parent: %w", err), file.Close())
	}
	parent = 0
	return file, nil
}

func validateRootLockFile(file *os.File) error {
	return validateWindowsLockHandle(windows.Handle(file.Fd()), false)
}

func openLockDirectoryAt(parent windows.Handle, name string) (windows.Handle, error) {
	handle, err := openLockPathAt(
		parent,
		name,
		windows.FILE_READ_ATTRIBUTES|windows.FILE_TRAVERSE|windows.SYNCHRONIZE,
		windows.FILE_OPEN,
		windows.FILE_ATTRIBUTE_DIRECTORY,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
	)
	if err != nil {
		return 0, err
	}
	if err := validateWindowsLockHandle(handle, true); err != nil {
		return 0, errors.Join(err, windows.CloseHandle(handle))
	}
	return handle, nil
}

func openLockFileHandleAt(parent windows.Handle, name string) (windows.Handle, error) {
	for attempt := 0; attempt < 10; attempt++ {
		handle, err := openLockPathAt(
			parent,
			name,
			windows.GENERIC_READ|windows.GENERIC_WRITE|windows.SYNCHRONIZE,
			windows.FILE_OPEN,
			windows.FILE_ATTRIBUTE_NORMAL,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
		)
		if err == nil {
			if err := validateWindowsLockHandle(handle, false); err != nil {
				return 0, errors.Join(err, windows.CloseHandle(handle))
			}
			return handle, nil
		}
		if !windowsLockPathMissing(err) {
			return 0, err
		}

		handle, err = openLockPathAt(
			parent,
			name,
			windows.GENERIC_READ|windows.GENERIC_WRITE|windows.SYNCHRONIZE,
			windows.FILE_CREATE,
			windows.FILE_ATTRIBUTE_NORMAL,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
		)
		if err == windows.STATUS_OBJECT_NAME_COLLISION || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if err := validateWindowsLockHandle(handle, false); err != nil {
			return 0, errors.Join(err, windows.CloseHandle(handle))
		}
		return handle, nil
	}
	return 0, errors.New("lock file creation did not stabilize")
}

func windowsLockPathMissing(err error) bool {
	return err == windows.STATUS_NO_SUCH_FILE ||
		err == windows.STATUS_OBJECT_NAME_NOT_FOUND ||
		err == windows.STATUS_OBJECT_PATH_NOT_FOUND ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

func openLockPathAt(parent windows.Handle, name string, access, disposition, attributes, options uint32) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	objectAttributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		access,
		objectAttributes,
		&windows.IO_STATUS_BLOCK{},
		nil,
		attributes,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		disposition,
		options,
		0,
		0,
	)
	return handle, err
}

func validateWindowsLockHandle(handle windows.Handle, wantDirectory bool) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("inspect lock path handle: %w", err)
	}
	isDirectory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || isDirectory != wantDirectory {
		return errors.New("refusing non-file or reparse-point lock path")
	}
	if !wantDirectory && info.NumberOfLinks != 1 {
		return errors.New("refusing multiply-linked lock file")
	}
	return nil
}

func tryLockFile(file *os.File) (platformLockState, bool, error) {
	state := platformLockState{overlapped: windows.Overlapped{OffsetHigh: lockByteOffsetHigh}}
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, &state.overlapped)
	if err == nil {
		return state, false, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		return platformLockState{}, true, nil
	}
	return platformLockState{}, false, err
}

func unlockFile(file *os.File, state *platformLockState) error {
	if file == nil {
		return nil
	}
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &state.overlapped)
}

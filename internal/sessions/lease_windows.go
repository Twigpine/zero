//go:build windows

package sessions

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLockLease takes the lease lock on file without waiting: shared for a
// process that has the session open, exclusive for prune. It reports false when
// another holder's lock conflicts. LockFileEx locks belong to the handle, so a
// second handle in the same process conflicts like another process would.
func tryLockLease(file *os.File, exclusive bool) (bool, error) {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, new(windows.Overlapped))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return false, err
}

// openLeaseFile opens (creating it if needed) the lease file of a session whose
// directory exists.
//
// WITH FILE_SHARE_DELETE, unlike session.lock. A lease is held for the life of
// the process, and a file held open without share-delete cannot be removed: the
// session directory could then never be deleted while any process had it open,
// by prune or by anything else, including a test's temp-directory cleanup. With
// it, a delete removes the name at once (POSIX semantics) while the holder's
// handle stays valid.
func openLeaseFile(path string) (*os.File, error) {
	pathUTF16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := windows.CreateFile(pathUTF16, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

func unlockLease(file *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, new(windows.Overlapped))
}

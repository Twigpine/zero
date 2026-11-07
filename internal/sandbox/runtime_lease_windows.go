//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

type runtimeLeaseHandle struct {
	file       *os.File
	overlapped windows.Overlapped
}

// acquireSharedRuntimeLeaseIn opens or creates the lease relative to a handle on
// its parent that refuses a reparse point at the final component, so the
// pathname the caller just validated cannot be swapped underneath the open.
func acquireSharedRuntimeLeaseIn(parent, name string) (runtimeLeaseHandle, error) {
	dir, err := openWindowsACLDirectoryNoFollow(parent)
	if err != nil {
		return runtimeLeaseHandle{}, fmt.Errorf("open sandbox runtime parent: %w", err)
	}
	defer func() { _ = windows.CloseHandle(dir) }()
	if err := validateWindowsACLComponent(name); err != nil {
		return runtimeLeaseHandle{}, err
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return runtimeLeaseHandle{}, fmt.Errorf("encode sandbox runtime lease %s: %w", name, err)
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		RootDirectory: dir,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	if err := windows.NtCreateFile(
		&handle,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.SYNCHRONIZE,
		&attributes,
		&status,
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN_IF,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	); err != nil {
		return runtimeLeaseHandle{}, fmt.Errorf("open sandbox runtime lease %s: %w", name, err)
	}
	if err := rejectWindowsACLReparseHandle(handle, name); err != nil {
		_ = windows.CloseHandle(handle)
		return runtimeLeaseHandle{}, err
	}
	file := os.NewFile(uintptr(handle), filepath.Join(parent, name))
	lease := runtimeLeaseHandle{file: file}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), 0, 0, 1, 0, &lease.overlapped); err != nil {
		_ = file.Close()
		return runtimeLeaseHandle{}, err
	}
	return lease, nil
}

func acquireSharedRuntimeLease(path string) (runtimeLeaseHandle, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return runtimeLeaseHandle{}, err
	}
	handle := runtimeLeaseHandle{file: file}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), 0, 0, 1, 0, &handle.overlapped); err != nil {
		_ = file.Close()
		return runtimeLeaseHandle{}, err
	}
	return handle, nil
}

func tryAcquireExclusiveRuntimeLease(path string) (runtimeLeaseHandle, bool, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return runtimeLeaseHandle{}, false, err
	}
	handle := runtimeLeaseHandle{file: file}
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	if err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, &handle.overlapped); err != nil {
		_ = file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return runtimeLeaseHandle{}, true, nil
		}
		return runtimeLeaseHandle{}, false, err
	}
	return handle, false, nil
}

func (lease runtimeLeaseHandle) release() {
	if lease.file == nil {
		return
	}
	_ = windows.UnlockFileEx(windows.Handle(lease.file.Fd()), 0, 1, 0, &lease.overlapped)
	_ = lease.file.Close()
}

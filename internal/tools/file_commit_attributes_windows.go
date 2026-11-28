//go:build windows

package tools

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// carriedAttributeMask selects the attributes an overwrite takes from the file
// it replaces: the ones a person or a tool sets on a file to say how it is
// presented. The side file is a new file. It has none of the replaced file's,
// so a hidden file would reappear in listings once it had been edited, and it
// has whatever its directory hands a new file, which the replaced file may
// never have had.
//
// The rest are left alone on purpose. Read-only travels with the permission
// bits. Archive is set by the system on any new file, which is also what a
// write in place does. Compressed, encrypted, sparse, offline and the cloud
// placement bits describe how one file object is stored, and cannot be set as
// attributes on another.
const carriedAttributeMask = windows.FILE_ATTRIBUTE_HIDDEN |
	windows.FILE_ATTRIBUTE_SYSTEM |
	windows.FILE_ATTRIBUTE_NOT_CONTENT_INDEXED

// fileBasicInfo is FILE_BASIC_INFO. A zero time leaves that time as it is, and
// zero attributes leave the attributes as they are.
type fileBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
	_              uint32
}

// carriedFileAttributes returns the presentation attributes of the file an
// overwrite is about to replace, out of what was read through the handle whose
// identity the caller has already validated. The result is never nil for a
// file: a file with none of them set carries that fact, so the side file is
// cleared of anything its directory gave it.
func carriedFileAttributes(info os.FileInfo) *uint32 {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return nil
	}
	carried := data.FileAttributes & carriedAttributeMask
	return &carried
}

// applyFileAttributes sets the staged file's presentation attributes to
// exactly the carried ones, through its own handle, and keeps every other
// attribute it has. Nil means there is no original, and nothing is changed.
func applyFileAttributes(staged *os.File, attributes *uint32) error {
	if attributes == nil {
		return nil
	}
	raw, err := staged.SyscallConn()
	if err != nil {
		return err
	}
	var applyErr error
	if err := raw.Control(func(handle uintptr) {
		var current windows.ByHandleFileInformation
		if applyErr = windows.GetFileInformationByHandle(windows.Handle(handle), &current); applyErr != nil {
			return
		}
		wanted := current.FileAttributes&^carriedAttributeMask | *attributes&carriedAttributeMask
		if wanted == current.FileAttributes {
			return
		}
		if wanted == 0 {
			wanted = windows.FILE_ATTRIBUTE_NORMAL
		}
		basic := fileBasicInfo{FileAttributes: wanted}
		applyErr = windows.SetFileInformationByHandle(
			windows.Handle(handle),
			windows.FileBasicInfo,
			(*byte)(unsafe.Pointer(&basic)),
			uint32(unsafe.Sizeof(basic)),
		)
	}); err != nil {
		return err
	}
	if applyErr != nil {
		return fmt.Errorf("carry the existing file's attributes: %w", applyErr)
	}
	return nil
}

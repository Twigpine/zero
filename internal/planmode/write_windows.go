//go:build windows

package planmode

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// writePlanUnderBase creates missing intermediate directories under base with
// NtCreateFile(OBJ_DONT_REPARSE) and replaces the final name via a handle-
// relative temp create + FileRenameInformation rename. Intermediate reparse
// points are refused rather than followed, matching openPlanUnderBase.
func writePlanUnderBase(base, rel, displayPath, content string) error {
	parts, err := relComponents(rel)
	if err != nil {
		return err
	}

	absBase, err := filepath.Abs(base)
	if err != nil {
		return err
	}

	parent, err := openWindowsBaseDir(absBase)
	if err != nil {
		return fmt.Errorf("create plan directory: %w", err)
	}
	defer func() { _ = windows.CloseHandle(parent) }()

	for i := 0; i < len(parts)-1; i++ {
		next, err := ensureDirNoFollowWindows(parent, parts[i])
		if err != nil {
			if isWindowsSymlinkErr(err) {
				return errPlanSymlinkWrite(displayPath)
			}
			return fmt.Errorf("create plan directory: %w", err)
		}
		_ = windows.CloseHandle(parent)
		parent = next
	}

	final := parts[len(parts)-1]
	if err := refuseSymlinkAtWindows(parent, final, displayPath); err != nil {
		return err
	}

	tmpName := planTempName(final)
	h, err := createFileNoFollow(parent, tmpName)
	if err != nil {
		if isWindowsSymlinkErr(err) {
			return errPlanSymlinkWrite(displayPath)
		}
		return fmt.Errorf("write plan file: %w", err)
	}

	written := false
	defer func() {
		if !written {
			_ = windows.CloseHandle(h)
			_ = deleteAtWindows(parent, tmpName)
		}
	}()

	file := os.NewFile(uintptr(h), displayPath+" (tmp)")
	if file == nil {
		return fmt.Errorf("write plan file: invalid file handle")
	}
	// os.NewFile owns h; clear so the failure path does not double-close.
	// Keep a copy for rename (must reopen by name after Close, or rename
	// before Close). Prefer rename while still holding the handle.
	owned := h
	h = windows.InvalidHandle

	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		return fmt.Errorf("write plan file: %w", err)
	}
	// Flush data before rename so a crash mid-write cannot leave a partial
	// durable plan. Close is not enough on Windows without FlushFileBuffers
	// for some media; WriteString + Close is the same contract as the prior
	// pathname path, so keep that shape.
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("write plan file: %w", err)
	}

	// Rename while the write handle is still open (needs DELETE access, which
	// createFileNoFollow requested). Closing first would force a reopen race.
	if err := renameatWindows(owned, parent, final); err != nil {
		_ = file.Close()
		return fmt.Errorf("replace plan file: %w", err)
	}
	if err := file.Close(); err != nil {
		// Rename already landed; surface close error but do not unlink the
		// durable name.
		written = true
		return fmt.Errorf("write plan file: %w", err)
	}
	written = true
	return nil
}

// ensureDirNoFollowWindows opens name under parent as a directory without
// following reparse points, creating it when missing.
func ensureDirNoFollowWindows(parent windows.Handle, name string) (windows.Handle, error) {
	for try := 0; try < 2; try++ {
		next, err := openatNoFollow(parent, name, true)
		if err == nil {
			return next, nil
		}
		if isWindowsSymlinkErr(err) {
			return 0, err
		}
		if try > 0 {
			return 0, err
		}
		// Missing: create then reopen. EEXIST means a concurrent creator won;
		// loop back to open. Other create errors are fatal.
		if !errors.Is(err, os.ErrNotExist) && !os.IsNotExist(err) {
			return 0, err
		}
		if mkdirErr := mkdiratNoFollow(parent, name); mkdirErr != nil && !isWindowsExistErr(mkdirErr) {
			return 0, mkdirErr
		}
	}
	return 0, fmt.Errorf("create plan directory %s: exhausted retries", name)
}

func mkdiratNoFollow(dirfd windows.Handle, name string) error {
	objName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return err
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		RootDirectory: dirfd,
		ObjectName:    objName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	oa.Length = uint32(unsafe.Sizeof(*oa))

	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&h,
		windows.FILE_GENERIC_READ|windows.SYNCHRONIZE,
		oa,
		&iosb,
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_FOR_BACKUP_INTENT,
		0,
		0,
	)
	if err != nil {
		return mapWindowsOpenErr(err)
	}
	_ = windows.CloseHandle(h)
	return nil
}

// createFileNoFollow creates name under dirfd exclusively without following
// reparse points. DELETE access is requested so the handle can be renamed
// via FileRenameInformation without a reopen race.
func createFileNoFollow(dirfd windows.Handle, name string) (windows.Handle, error) {
	objName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		RootDirectory: dirfd,
		ObjectName:    objName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	oa.Length = uint32(unsafe.Sizeof(*oa))

	access := uint32(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.DELETE | windows.SYNCHRONIZE)
	options := uint32(windows.FILE_NON_DIRECTORY_FILE | windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_FOR_BACKUP_INTENT)

	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&h,
		access,
		oa,
		&iosb,
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_CREATE,
		options,
		0,
		0,
	)
	if err != nil {
		return 0, mapWindowsOpenErr(err)
	}
	return h, nil
}

// refuseSymlinkAtWindows fails when name under dirfd is a reparse point.
// Missing names are fine.
func refuseSymlinkAtWindows(dirfd windows.Handle, name, displayPath string) error {
	h, err := openatNoFollow(dirfd, name, false)
	if err != nil {
		if err == os.ErrNotExist || os.IsNotExist(err) {
			return nil
		}
		// A directory at the final name is not a symlink; the subsequent
		// FILE_NON_DIRECTORY_FILE create of the temp is fine, and rename
		// will fail clearly if the final name is a directory.
		if err == syscall.EISDIR {
			return nil
		}
		if isWindowsSymlinkErr(err) {
			return errPlanSymlinkWrite(displayPath)
		}
		// STATUS_OBJECT_NAME_NOT_FOUND already mapped; other open failures
		// (access denied on a planted reparse) surface as-is.
		return err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		_ = windows.CloseHandle(h)
		return err
	}
	_ = windows.CloseHandle(h)
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errPlanSymlinkWrite(displayPath)
	}
	return nil
}

// renameatWindows renames the open handle h into newname under newdirfd,
// replacing any existing regular file at the destination.
func renameatWindows(h windows.Handle, newdirfd windows.Handle, newname string) error {
	newNameUTF16, err := windows.UTF16FromString(newname)
	if err != nil {
		return err
	}
	fileNameLen := len(newNameUTF16)*2 - 2 // drop trailing NUL bytes from length
	if fileNameLen < 0 {
		return syscall.EINVAL
	}

	type fileRenameInformation struct {
		ReplaceIfExists uint32
		RootDirectory   windows.Handle
		FileNameLength  uint32
		FileName        [1]uint16
	}
	var dummy fileRenameInformation
	bufferSize := int(unsafe.Offsetof(dummy.FileName)) + fileNameLen
	buffer := make([]byte, bufferSize)
	info := (*fileRenameInformation)(unsafe.Pointer(&buffer[0]))
	info.ReplaceIfExists = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.RootDirectory = newdirfd
	info.FileNameLength = uint32(fileNameLen)
	copy((*[windows.MAX_LONG_PATH]uint16)(unsafe.Pointer(&info.FileName[0]))[:fileNameLen/2:fileNameLen/2], newNameUTF16)

	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtSetInformationFile(h, &iosb, &buffer[0], uint32(bufferSize), windows.FileRenameInformation)
	if err != nil {
		if st, ok := err.(windows.NTStatus); ok {
			return st.Errno()
		}
		return err
	}
	return nil
}

func deleteAtWindows(dirfd windows.Handle, name string) error {
	h, err := openForDelete(dirfd, name)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	var iosb windows.IO_STATUS_BLOCK
	// FileDispositionInformation = 13: mark handle for delete-on-close.
	type dispositionInfo struct{ DeleteFile uint8 }
	disp := dispositionInfo{DeleteFile: 1}
	return windows.NtSetInformationFile(h, &iosb, (*byte)(unsafe.Pointer(&disp)), uint32(unsafe.Sizeof(disp)), 13)
}

func openForDelete(dirfd windows.Handle, name string) (windows.Handle, error) {
	objName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		RootDirectory: dirfd,
		ObjectName:    objName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	oa.Length = uint32(unsafe.Sizeof(*oa))

	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&h,
		windows.DELETE|windows.SYNCHRONIZE|windows.FILE_READ_ATTRIBUTES,
		oa,
		&iosb,
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	)
	if err != nil {
		return 0, mapWindowsOpenErr(err)
	}
	return h, nil
}

func isWindowsExistErr(err error) bool {
	if err == nil {
		return false
	}
	if err == syscall.EEXIST {
		return true
	}
	if st, ok := err.(windows.NTStatus); ok && st == windows.STATUS_OBJECT_NAME_COLLISION {
		return true
	}
	return false
}

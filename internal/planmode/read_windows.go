//go:build windows

package planmode

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// openPlanUnderBase opens rel under base with a true no-follow walk using
// NtCreateFile and OBJ_DONT_REPARSE on every component (the same primitive
// behind Go's os.Root / O_NOFOLLOW_ANY). A reparse point at any component is
// refused rather than followed, including in-root final-component swaps that
// os.Root.Open would otherwise accept via checkSymlink.
func openPlanUnderBase(base, rel, displayPath string) (*os.File, error) {
	parts, err := relComponents(rel)
	if err != nil {
		return nil, err
	}

	absBase, err := filepath.Abs(base)
	if err != nil {
		return nil, err
	}

	// parent is the current directory handle in the walk. On success the final
	// file handle is transferred to *os.File; parent stays owned here and is
	// closed by the deferred cleanup. The closure must re-read parent so
	// intermediate reassignment is not leaked (defer args are evaluated now).
	parent, err := openWindowsBaseDir(absBase)
	if err != nil {
		return nil, err
	}
	defer func() { _ = windows.CloseHandle(parent) }()

	for i := 0; i < len(parts)-1; i++ {
		next, err := openatNoFollow(parent, parts[i], true)
		if err != nil {
			if isWindowsSymlinkErr(err) {
				return nil, errPlanSymlink(displayPath)
			}
			return nil, err
		}
		_ = windows.CloseHandle(parent)
		parent = next
	}

	final := parts[len(parts)-1]
	h, err := openatNoFollow(parent, final, false)
	if err != nil {
		if isWindowsSymlinkErr(err) {
			return nil, errPlanSymlink(displayPath)
		}
		// FILE_NON_DIRECTORY_FILE fails with EISDIR when the final name is a
		// directory; map it to the same regular-file refusal the attribute
		// check below uses so callers see one stable message.
		if err == syscall.EISDIR {
			return nil, fmt.Errorf("plan file %s is not a regular file", displayPath)
		}
		return nil, err
	}

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		_ = windows.CloseHandle(h)
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(h)
		return nil, errPlanSymlink(displayPath)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("plan file %s is not a regular file", displayPath)
	}

	f := os.NewFile(uintptr(h), displayPath)
	if f == nil {
		_ = windows.CloseHandle(h)
		return nil, fmt.Errorf("plan file %s: invalid file handle", displayPath)
	}
	return f, nil
}

// ntObjectPath builds the NT object manager path for an absolute Win32 path.
// Drive-letter paths become `\??\C:\...`. UNC paths (`\\server\share\...`)
// must go through the UNC device: `\??\UNC\server\share\...`. Concatenating
// `\??\` alone yields `\??\\\server\...`, which NtCreateFile rejects. A
// roaming %AppData% (plan storage base) can legitimately be a UNC path.
func ntObjectPath(absPath string) string {
	if strings.HasPrefix(absPath, `\\`) {
		return `\??\UNC\` + strings.TrimPrefix(absPath, `\\`)
	}
	return `\??\` + absPath
}

// openWindowsBaseDir opens the storage base as a directory handle that can be
// used as RootDirectory for subsequent relative NtCreateFile calls.
func openWindowsBaseDir(absBase string) (windows.Handle, error) {
	path := ntObjectPath(absBase)
	objName, err := windows.NewNTUnicodeString(path)
	if err != nil {
		return 0, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		ObjectName: objName,
		Attributes: windows.OBJ_CASE_INSENSITIVE,
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
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_FOR_BACKUP_INTENT,
		0,
		0,
	)
	if err != nil {
		return 0, mapWindowsOpenErr(err)
	}
	return h, nil
}

// openatNoFollow opens name relative to dirfd without following reparse points.
// When directory is true the target must be a directory; otherwise it must not
// be a directory.
func openatNoFollow(dirfd windows.Handle, name string, directory bool) (windows.Handle, error) {
	objName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		RootDirectory: dirfd,
		ObjectName:    objName,
		// OBJ_DONT_REPARSE is the O_NOFOLLOW_ANY equivalent used by Go's Root:
		// any reparse point fails with STATUS_REPARSE_POINT_ENCOUNTERED rather
		// than being followed.
		Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	oa.Length = uint32(unsafe.Sizeof(*oa))

	access := uint32(windows.FILE_GENERIC_READ | windows.SYNCHRONIZE)
	options := uint32(windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_FOR_BACKUP_INTENT)
	if directory {
		options |= windows.FILE_DIRECTORY_FILE
		access |= windows.FILE_LIST_DIRECTORY
	} else {
		options |= windows.FILE_NON_DIRECTORY_FILE
	}

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
		windows.FILE_OPEN,
		options,
		0,
		0,
	)
	if err != nil {
		return 0, mapWindowsOpenErr(err)
	}
	return h, nil
}

func isWindowsSymlinkErr(err error) bool {
	if err == nil {
		return false
	}
	if err == windows.STATUS_REPARSE_POINT_ENCOUNTERED {
		return true
	}
	// Some paths surface the mapped errno instead of the raw NT status.
	if err == syscall.ELOOP || err == windows.ERROR_CANT_RESOLVE_FILENAME {
		return true
	}
	if st, ok := err.(windows.NTStatus); ok && st == windows.STATUS_REPARSE_POINT_ENCOUNTERED {
		return true
	}
	return false
}

func mapWindowsOpenErr(err error) error {
	if err == nil {
		return nil
	}
	if st, ok := err.(windows.NTStatus); ok {
		switch st {
		// Missing final name, missing intermediate component, and the
		// filesystem "file not found" status all mean the plan is absent.
		// ReadPlan maps os.ErrNotExist to ("", false, nil).
		case windows.STATUS_OBJECT_NAME_NOT_FOUND,
			windows.STATUS_OBJECT_PATH_NOT_FOUND,
			windows.STATUS_NO_SUCH_FILE:
			return os.ErrNotExist
		case windows.STATUS_OBJECT_NAME_COLLISION:
			return syscall.EEXIST
		case windows.STATUS_REPARSE_POINT_ENCOUNTERED:
			return st
		case windows.STATUS_FILE_IS_A_DIRECTORY:
			return syscall.EISDIR
		case windows.STATUS_NOT_A_DIRECTORY:
			return syscall.ENOTDIR
		default:
			return st.Errno()
		}
	}
	return err
}

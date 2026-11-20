//go:build windows

package tools

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// fileSecurity is the access control an overwrite carries from the file it
// replaces to the file that takes its name.
//
// An overwrite is published by renaming a freshly created side file over the
// target. A fresh file takes the directory's inherited access control list, so
// without this the rename silently drops whatever was set on the original: an
// explicit entry disappears, and a file whose inheritance had been removed
// comes back with everything the directory grants.
type fileSecurity struct {
	// descriptor owns the memory dacl points into.
	descriptor *windows.SECURITY_DESCRIPTOR
	dacl       *windows.ACL
	// protected records that the original did not inherit from its directory.
	protected bool
}

// fileVolumeKeepsACLs reports whether the volume holding the open file stores
// access control lists at all. FAT and exFAT do not, and there is nothing to
// carry on them. Tests replace it; production asks the volume.
var fileVolumeKeepsACLs = func(handle windows.Handle) (bool, error) {
	var flags uint32
	if err := windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, &flags, nil, 0); err != nil {
		return false, err
	}
	return flags&windows.FILE_PERSISTENT_ACLS != 0, nil
}

// captureFileSecurity reads the access control list of the file an overwrite
// is about to replace. It works through the handle whose identity the caller
// has already validated, so a name swapped in afterwards cannot supply the
// list. A nil result means the volume keeps no lists.
func captureFileSecurity(file *os.File) (*fileSecurity, error) {
	handle, err := reopenFileForSecurity(file, windows.READ_CONTROL)
	if err != nil {
		return nil, fmt.Errorf("open the existing file's access control list: %w", err)
	}
	defer windows.CloseHandle(handle)
	keeps, err := fileVolumeKeepsACLs(handle)
	if err != nil {
		return nil, fmt.Errorf("inspect the volume's access control support: %w", err)
	}
	if !keeps {
		return nil, nil
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, fmt.Errorf("read the existing file's access control list: %w", err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return nil, fmt.Errorf("decode the existing file's access control list: %w", err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return nil, fmt.Errorf("read the existing file's access control flags: %w", err)
	}
	return &fileSecurity{
		descriptor: descriptor,
		dacl:       dacl,
		protected:  control&windows.SE_DACL_PROTECTED != 0,
	}, nil
}

// applyFileSecurity stamps the captured access control list on the staged
// file, through a handle reopened from the staged file's own handle, before
// the rename publishes it. An unprotected list is applied unprotected, so the
// system rebuilds the inherited entries from the directory and keeps the
// explicit ones; a protected list is applied as it was read.
func applyFileSecurity(staged *os.File, security *fileSecurity) error {
	if security == nil {
		return nil
	}
	handle, err := reopenFileForSecurity(staged, windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		return fmt.Errorf("open the staged file's access control list: %w", err)
	}
	defer windows.CloseHandle(handle)
	information := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if security.protected {
		information |= windows.SECURITY_INFORMATION(windows.PROTECTED_DACL_SECURITY_INFORMATION)
	} else {
		information |= windows.SECURITY_INFORMATION(windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, information, nil, nil, security.dacl, nil); err != nil {
		return fmt.Errorf("carry the existing file's access control list: %w", err)
	}
	return nil
}

// reopenFileForSecurity opens the same file object again with the rights a
// security query or update needs. The handles this package holds come from
// os.Root, which opens for data access only. An empty name relative to the
// existing handle reopens that object itself, with no path to resolve again.
func reopenFileForSecurity(file *os.File, access uint32) (windows.Handle, error) {
	raw, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var handle windows.Handle
	var openErr error
	if err := raw.Control(func(rawHandle uintptr) {
		objectName, nameErr := windows.NewNTUnicodeString("")
		if nameErr != nil {
			openErr = nameErr
			return
		}
		openErr = windows.NtCreateFile(
			&handle,
			access|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
			&windows.OBJECT_ATTRIBUTES{
				Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
				RootDirectory: windows.Handle(rawHandle),
				ObjectName:    objectName,
				Attributes:    windows.OBJ_CASE_INSENSITIVE,
			},
			&windows.IO_STATUS_BLOCK{},
			nil,
			0,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			windows.FILE_OPEN,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
			0,
			0,
		)
	}); err != nil {
		return 0, err
	}
	if openErr != nil {
		return 0, openErr
	}
	return handle, nil
}

//go:build windows

package fsutil

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// protectStaging applies the destination DACL after the complete content was
// written under the creation-time owner-only descriptor. Keep inheritance
// disabled on staging even when the source DACL is unprotected: its effective
// ACEs are copied explicitly, and parent grants must not be re-added here.
// ReplaceFileW preserves the destination security descriptor on publication.
func protectStaging(f *os.File, destPath string) error {
	descriptor, err := windows.GetNamedSecurityInfo(destPath, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("fsutil: reading DACL of %s for staging: %w", destPath, err)
	}
	if descriptor == nil {
		return fmt.Errorf("fsutil: destination %s has no security descriptor to protect the staging file", destPath)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		// A destination with no DACL at all is fully permissive, so an inherited
		// staging DACL cannot expose anything the destination does not already.
		if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
			return nil
		}
		return fmt.Errorf("fsutil: extracting DACL of %s for staging: %w", destPath, err)
	}
	if dacl == nil {
		// A present but NULL DACL grants everyone full control: the destination
		// is not restricted, so there is no narrower descriptor to carry over.
		// Setting a NULL DACL here would only widen the staging file.
		return nil
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION)
	handle, err := openStagingForDACL(f)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("fsutil: applying DACL of %s to staging file: %w", destPath, err)
	}
	return nil
}

// openStagingForDACL opens the staging file f names with the READ_CONTROL and
// WRITE_DAC rights SetSecurityInfo requires, which the handle inside f does not
// carry. The new handle is verified to reference the same volume and file index
// as f, and to not be a reparse point, so a directory entry swapped in after
// creation cannot receive the descriptor instead of the object this process
// created. The caller owns the returned handle.
func openStagingForDACL(f *os.File) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(f.Name())
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(
		name,
		windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return 0, fmt.Errorf("fsutil: opening staging file %s to apply its DACL: %w", f.Name(), err)
	}
	var created, opened windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &created); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("fsutil: querying created staging file %s: %w", f.Name(), err)
	}
	if err := windows.GetFileInformationByHandle(handle, &opened); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("fsutil: querying reopened staging file %s: %w", f.Name(), err)
	}
	if opened.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("fsutil: staging path %s is unexpectedly a reparse point", f.Name())
	}
	if created.VolumeSerialNumber != opened.VolumeSerialNumber ||
		created.FileIndexHigh != opened.FileIndexHigh ||
		created.FileIndexLow != opened.FileIndexLow {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("fsutil: staging path %s no longer names the created file", f.Name())
	}
	return handle, nil
}

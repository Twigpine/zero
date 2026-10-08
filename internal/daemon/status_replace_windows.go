//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// prepareStatusReplacement copies the existing status document's DACL to the
// complete staging file before Root.Rename replaces it. Both files are opened
// relative to the already-bound Root directory handle, so preserving Windows
// security metadata does not reintroduce a pathname/ancestor-swap boundary.
func prepareStatusReplacement(root *os.Root, src, dst string) (returnErr error) {
	if filepath.Base(src) != src || filepath.Base(dst) != dst {
		return fmt.Errorf("status replacement names must be root-relative base names")
	}
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open bound status directory for DACL preservation: %w", err)
	}
	defer func() {
		if err := directory.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close bound status directory for DACL preservation: %w", err))
		}
	}()

	raw, err := directory.SyscallConn()
	if err != nil {
		return fmt.Errorf("access bound status directory handle: %w", err)
	}
	var destination, source windows.Handle
	var openErr error
	if err := raw.Control(func(rootHandle uintptr) {
		destination, openErr = openStatusSecurityHandle(windows.Handle(rootHandle), dst, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES)
		if isMissingStatusObject(openErr) {
			openErr = nil // no destination descriptor exists to preserve
			return
		}
		if openErr != nil {
			return
		}
		source, openErr = openStatusSecurityHandle(windows.Handle(rootHandle), src, windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES)
	}); err != nil {
		return fmt.Errorf("open status files through bound directory: %w", err)
	}
	if destination != 0 {
		defer windows.CloseHandle(destination)
	}
	if source != 0 {
		defer windows.CloseHandle(source)
	}
	if openErr != nil {
		return fmt.Errorf("open status files for DACL preservation: %w", openErr)
	}
	if destination == 0 {
		return nil
	}

	descriptor, err := windows.GetSecurityInfo(destination, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read existing status DACL: %w", err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("decode existing status DACL: %w", err)
	}
	if dacl == nil {
		return fmt.Errorf("existing status file has an unrestricted Windows DACL")
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return fmt.Errorf("read existing status DACL control: %w", err)
	}
	securityInfo := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if control&windows.SE_DACL_PROTECTED != 0 {
		securityInfo |= windows.SECURITY_INFORMATION(windows.PROTECTED_DACL_SECURITY_INFORMATION)
	} else {
		securityInfo |= windows.SECURITY_INFORMATION(windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
	}
	if err := windows.SetSecurityInfo(source, windows.SE_FILE_OBJECT, securityInfo, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("preserve existing status DACL on replacement: %w", err)
	}
	return nil
}

func openStatusSecurityHandle(root windows.Handle, name string, access uint32) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: root,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle,
		access,
		attributes,
		&status,
		nil,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	)
	if err != nil {
		return 0, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		windows.CloseHandle(handle)
		return 0, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(handle)
		return 0, fmt.Errorf("refusing status DACL operation on reparse point %q", name)
	}
	return handle, nil
}

func isMissingStatusObject(err error) bool {
	return errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) ||
		errors.Is(err, windows.STATUS_OBJECT_PATH_NOT_FOUND)
}

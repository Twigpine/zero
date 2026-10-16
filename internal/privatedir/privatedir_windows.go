//go:build windows

package privatedir

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func harden(root *os.Root) (returnErr error) {
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open private directory hardening handle: %w", err)
	}
	defer func() {
		if err := directory.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close private directory hardening handle: %w", err))
		}
	}()
	raw, err := directory.SyscallConn()
	if err != nil {
		return fmt.Errorf("access private directory hardening handle: %w", err)
	}
	// Root.Open uses NtCreateFile, so ReOpenFile cannot accept its handle. An
	// empty relative NT object name reopens the bound directory itself with the
	// security rights needed for the DACL update and no pathname re-resolution.
	var securityHandle windows.Handle
	var openErr error
	if err := raw.Control(func(rawHandle uintptr) {
		objectName, nameErr := windows.NewNTUnicodeString("")
		if nameErr != nil {
			openErr = nameErr
			return
		}
		openErr = windows.NtCreateFile(
			&securityHandle,
			windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
			&windows.OBJECT_ATTRIBUTES{
				Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
				RootDirectory: windows.Handle(rawHandle),
				ObjectName:    objectName,
				Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
			},
			&windows.IO_STATUS_BLOCK{},
			nil,
			windows.FILE_ATTRIBUTE_DIRECTORY,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			windows.FILE_OPEN,
			windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT,
			0,
			0,
		)
	}); err != nil {
		return fmt.Errorf("open private directory security handle: %w", err)
	}
	if openErr != nil {
		return fmt.Errorf("open private directory with security access: %w", openErr)
	}
	if securityHandle == 0 || securityHandle == windows.InvalidHandle {
		return fmt.Errorf("open private directory with security access: invalid handle")
	}
	defer func() {
		if err := windows.CloseHandle(securityHandle); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close private directory security handle: %w", err))
		}
	}()

	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("resolve current Windows user: %w", err)
	}
	tokenOwner, err := windowsTokenOwner(token)
	if err != nil {
		return fmt.Errorf("resolve current Windows token owner: %w", err)
	}
	current, err := windows.GetSecurityInfo(securityHandle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read private directory owner before hardening: %w", err)
	}
	owner, _, err := current.Owner()
	if err != nil {
		return fmt.Errorf("read private directory owner before hardening: %w", err)
	}
	if owner == nil || (!owner.Equals(user.User.Sid) && !owner.Equals(tokenOwner)) {
		return fmt.Errorf("private directory is not owned by the current Windows token")
	}
	desired, err := windows.SecurityDescriptorFromString(
		fmt.Sprintf("O:%sD:P(A;OICI;GA;;;%s)(A;OICI;GA;;;SY)", user.User.Sid.String(), user.User.Sid.String()),
	)
	if err != nil {
		return fmt.Errorf("build private Windows directory DACL: %w", err)
	}
	dacl, _, err := desired.DACL()
	if err != nil {
		return fmt.Errorf("read private Windows directory DACL: %w", err)
	}
	if err := windows.SetSecurityInfo(
		securityHandle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		return fmt.Errorf("harden private Windows directory DACL: %w", err)
	}
	return nil
}

type tokenOwnerInfo struct {
	owner *windows.SID
}

func windowsTokenOwner(token windows.Token) (*windows.SID, error) {
	var size uint32
	err := windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &size)
	if err != windows.ERROR_INSUFFICIENT_BUFFER {
		return nil, err
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, &buffer[0], size, &size); err != nil {
		return nil, err
	}
	owner := (*tokenOwnerInfo)(unsafe.Pointer(&buffer[0])).owner
	if owner == nil {
		return nil, errors.New("windows access token has no default owner")
	}
	return owner.Copy()
}

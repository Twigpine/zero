//go:build windows

package peermsg

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func ensurePrivateDir(path string) (resultErr error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	descriptor, userSID, tokenOwnerSID, err := privateDirectoryDescriptor()
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(abs)
	current := volume + string(filepath.Separator)
	parent, err := openWindowsDirectory(current, windows.FILE_READ_ATTRIBUTES|windows.FILE_TRAVERSE|windows.SYNCHRONIZE)
	if err != nil {
		return err
	}
	parentPath := current
	defer func() {
		if parent != 0 {
			resultErr = errors.Join(resultErr, closePrivateWindowsHandle(parent, parentPath))
		}
	}()
	components := strings.Split(strings.TrimPrefix(abs[len(volume):], string(filepath.Separator)), string(filepath.Separator))
	for index, component := range components {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		access := uint32(windows.FILE_READ_ATTRIBUTES | windows.FILE_TRAVERSE | windows.SYNCHRONIZE)
		if index == len(components)-1 {
			access |= windows.READ_CONTROL | windows.WRITE_DAC
		}
		next, openErr := openOrCreatePrivateWindowsDirectory(parent, component, access, descriptor)
		if openErr != nil {
			return fmt.Errorf("open private runtime path component %q: %w", current, openErr)
		}
		if closeErr := closePrivateWindowsHandle(parent, parentPath); closeErr != nil {
			return errors.Join(closeErr, closePrivateWindowsHandle(next, current))
		}
		parent = next
		parentPath = current
	}
	if err := securePrivateDirectory(parent, abs, descriptor, userSID, tokenOwnerSID); err != nil {
		return err
	}
	if err := closePrivateWindowsHandle(parent, abs); err != nil {
		return err
	}
	parent = 0
	return nil
}

func closePrivateWindowsHandle(handle windows.Handle, path string) error {
	if err := windows.CloseHandle(handle); err != nil {
		return fmt.Errorf("close private runtime directory %q: %w", path, err)
	}
	return nil
}

func privateDirectoryDescriptor() (*windows.SECURITY_DESCRIPTOR, *windows.SID, *windows.SID, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve current Windows user: %w", err)
	}
	sid := user.User.Sid
	tokenOwnerSID, err := windowsTokenOwner(token)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("resolve current Windows token owner: %w", err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sD:P(A;OICI;GA;;;%s)(A;OICI;GA;;;SY)", sid.String(), sid.String()))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build private Windows directory ACL: %w", err)
	}
	return descriptor, sid, tokenOwnerSID, nil
}

type windowsTokenOwnerInfo struct {
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
	owner := (*windowsTokenOwnerInfo)(unsafe.Pointer(&buffer[0])).owner
	if owner == nil {
		return nil, errors.New("windows access token has no default owner")
	}
	return owner.Copy()
}

func openWindowsDirectory(path string, access uint32) (windows.Handle, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(
		pathPtr,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return 0, fmt.Errorf("open private runtime directory %q: %w", path, err)
	}
	return handle, nil
}

func openOrCreatePrivateWindowsDirectory(parent windows.Handle, name string, access uint32, descriptor *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	handle, err := openPrivateWindowsDirectoryAt(parent, name, access, windows.FILE_OPEN, nil)
	if err == nil {
		return handle, nil
	}
	if !windowsDirectoryMissing(err) {
		return 0, err
	}
	handle, err = openPrivateWindowsDirectoryAt(parent, name, access, windows.FILE_CREATE, descriptor)
	if err == windows.STATUS_OBJECT_NAME_COLLISION || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return openPrivateWindowsDirectoryAt(parent, name, access, windows.FILE_OPEN, nil)
	}
	return handle, err
}

func openPrivateWindowsDirectoryAt(parent windows.Handle, name string, access, disposition uint32, descriptor *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	objectAttributes := &windows.OBJECT_ATTRIBUTES{
		Length:             uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory:      parent,
		ObjectName:         objectName,
		Attributes:         windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		SecurityDescriptor: descriptor,
	}
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		access,
		objectAttributes,
		&windows.IO_STATUS_BLOCK{},
		nil,
		windows.FILE_ATTRIBUTE_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		disposition,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	)
	if err != nil {
		return 0, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return 0, errors.Join(err, closePrivateWindowsHandle(handle, name))
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return 0, errors.Join(
			fmt.Errorf("refusing non-directory or reparse-point component %q", name),
			closePrivateWindowsHandle(handle, name),
		)
	}
	return handle, nil
}

func windowsDirectoryMissing(err error) bool {
	return err == windows.STATUS_NO_SUCH_FILE ||
		err == windows.STATUS_OBJECT_NAME_NOT_FOUND ||
		err == windows.STATUS_OBJECT_PATH_NOT_FOUND ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND)
}

func securePrivateDirectory(handle windows.Handle, path string, desired *windows.SECURITY_DESCRIPTOR, allowedOwners ...*windows.SID) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("inspect private runtime directory %q: %w", path, err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return fmt.Errorf("refusing non-directory or reparse-point runtime path %q", path)
	}
	current, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read owner of private runtime directory %q: %w", path, err)
	}
	owner, _, err := current.Owner()
	if err != nil {
		return fmt.Errorf("read owner of private runtime directory %q: %w", path, err)
	}
	ownedByCurrentToken := false
	for _, allowedOwner := range allowedOwners {
		if allowedOwner != nil && owner.Equals(allowedOwner) {
			ownedByCurrentToken = true
			break
		}
	}
	if !ownedByCurrentToken {
		return fmt.Errorf("refusing runtime directory %q not owned by the current user", path)
	}
	dacl, _, err := desired.DACL()
	if err != nil {
		return fmt.Errorf("read private Windows directory DACL: %w", err)
	}
	if err := windows.SetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		return fmt.Errorf("secure private runtime directory %q: %w", path, err)
	}
	return nil
}

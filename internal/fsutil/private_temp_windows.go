//go:build windows

package fsutil

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// An explicit protected current-user DACL is installed by CreateFile/CreateDirectory,
// not by a second call after an inheritable object has become visible.
func privateSecurityAttributes() (*windows.SecurityAttributes, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;OICI;FA;;;%s)", user.User.Sid.String()))
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}, nil
}
func createPrivateFile(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	security, err := privateSecurityAttributes()
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, security, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "create private staging", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
func createPrivateDir(path string) error {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	security, err := privateSecurityAttributes()
	if err != nil {
		return err
	}
	if err := windows.CreateDirectory(name, security); err != nil {
		return &os.PathError{Op: "mkdir private staging", Path: path, Err: err}
	}
	return nil
}

//go:build darwin

package fsutil

import (
	"encoding/binary"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

var libc_open_extended_trampoline_addr uintptr
var libc_mkdir_extended_trampoline_addr uintptr

// These are the libSystem primitives underlying openx_np/mkdirx_np since 10.4.
//go:cgo_import_dynamic libc_open_extended __open_extended "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_mkdir_extended __mkdir_extended "/usr/lib/libSystem.B.dylib"

// kauth_filesec: magic, owner/group GUIDs, ACL entry count and ACL flags.
// An empty ACL with NO_INHERIT prevents the parent from granting access at birth.
func privateFilesec() []byte {
	blob := make([]byte, 44)
	binary.LittleEndian.PutUint32(blob, 0x012cc16d)
	binary.LittleEndian.PutUint32(blob[40:], 1<<17)
	return blob
}
func createPrivateFile(path string) (*os.File, error) {
	name, err := unix.BytePtrFromString(path)
	if err != nil {
		return nil, err
	}
	security := privateFilesec()
	const noID = uintptr(0xffffff9b) // KAUTH_UID_NONE / KAUTH_GID_NONE
	fd, _, errno := syscall_syscall6(libc_open_extended_trampoline_addr,
		uintptr(unsafe.Pointer(name)), unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC,
		noID, noID, 0o600, uintptr(unsafe.Pointer(&security[0])))
	runtime.KeepAlive(name)
	runtime.KeepAlive(security)
	if errno != 0 {
		return nil, &os.PathError{Op: "create private staging", Path: path, Err: errno}
	}
	return os.NewFile(fd, path), nil
}
func createPrivateDir(path string) error {
	name, err := unix.BytePtrFromString(path)
	if err != nil {
		return err
	}
	security := privateFilesec()
	const noID = uintptr(0xffffff9b)
	_, _, errno := syscall_syscall6(libc_mkdir_extended_trampoline_addr,
		uintptr(unsafe.Pointer(name)), noID, noID, 0o700, uintptr(unsafe.Pointer(&security[0])), 0)
	runtime.KeepAlive(name)
	runtime.KeepAlive(security)
	if errno != 0 {
		return &os.PathError{Op: "mkdir private staging", Path: path, Err: errno}
	}
	return nil
}

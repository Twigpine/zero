//go:build darwin

package fsutil

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

var getattrlistZero byte

var libc_getattrlist_trampoline_addr uintptr

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:cgo_import_dynamic libc_getattrlist getattrlist "/usr/lib/libSystem.B.dylib"

func getattrlist(path string, attrList *unix.Attrlist, attrBuf []byte, options uint32) error {
	p, err := unix.BytePtrFromString(path)
	if err != nil {
		return err
	}
	bufPtr := unsafe.Pointer(&getattrlistZero)
	if len(attrBuf) > 0 {
		bufPtr = unsafe.Pointer(&attrBuf[0])
	}
	_, _, e := syscall_syscall6(libc_getattrlist_trampoline_addr,
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(attrList)),
		uintptr(bufPtr),
		uintptr(len(attrBuf)),
		uintptr(options),
		0)
	if e != 0 {
		return e
	}
	return nil
}

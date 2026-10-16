//go:build linux || darwin || freebsd || netbsd

package fsutil

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const posixACLAccessXattr = "system.posix_acl_access"

// xattrListFunc, xattrGetFunc, xattrSetFunc and xattrRemoveFunc are seams for
// the four permitter syscalls so a test can exercise the fail-closed contract
// without a host that actually denies the operation. Production uses the real
// syscalls.
var (
	xattrListFunc = listXattrs
	xattrGetFunc  = getXattr
	xattrSetFunc  = func(fd int, name string, data []byte, flags int) error {
		return unix.Fsetxattr(fd, name, data, flags)
	}
	xattrRemoveFunc = func(fd int, name string) error {
		return unix.Fremovexattr(fd, name)
	}
)

// preserveXattrs copies every extended attribute of srcPath onto f. The copy is
// fail-closed: any attribute that cannot be listed, read, or set aborts the
// replacement and leaves the destination unchanged. There is deliberately no
// best-effort exception, not even for security.selinux: WriteFileAtomic's
// contract is that the destination's authorization metadata is either
// preserved in full or the call fails.
func preserveXattrs(f *os.File, srcPath string) error {
	names, err := xattrListFunc(srcPath)
	if err != nil {
		if isXattrUnsupported(err) {
			return nil
		}
		return fmt.Errorf("fsutil: listing xattrs of %s: %w", srcPath, err)
	}
	hasAccessACL := false
	for _, name := range names {
		if name == posixACLAccessXattr {
			hasAccessACL = true
		}
		data, err := xattrGetFunc(srcPath, name)
		if err != nil {
			return fmt.Errorf("fsutil: reading xattr %s from %s: %w", name, srcPath, err)
		}
		if err := xattrSetFunc(int(f.Fd()), name, data, 0); err != nil {
			return fmt.Errorf("fsutil: preserving xattr %s: %w", name, err)
		}
	}
	if !hasAccessACL {
		if err := xattrRemoveFunc(int(f.Fd()), posixACLAccessXattr); err != nil {
			if isXattrNotFound(err) {
				return nil
			}
			if isXattrUnsupported(err) {
				if _, getErr := xattrGetFunc(f.Name(), posixACLAccessXattr); isXattrNotFound(getErr) {
					return nil
				}
			}
			return fmt.Errorf("fsutil: removing inherited ACL from replacement: %w", err)
		}
	}
	return nil
}

func listXattrs(path string) ([]string, error) {
	dest := []byte(nil)
	for {
		size, err := unix.Listxattr(path, dest)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			return nil, nil
		}
		if len(dest) < size {
			dest = make([]byte, size)
			continue
		}
		return splitXattrNames(dest[:size]), nil
	}
}

func getXattr(path, name string) ([]byte, error) {
	dest := []byte(nil)
	for {
		size, err := unix.Getxattr(path, name, dest)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			return []byte{}, nil
		}
		if len(dest) < size {
			dest = make([]byte, size)
			continue
		}
		return dest[:size], nil
	}
}

func splitXattrNames(buf []byte) []string {
	names := make([]string, 0)
	start := 0
	for i, b := range buf {
		if b != 0 {
			continue
		}
		if i > start {
			names = append(names, string(buf[start:i]))
		}
		start = i + 1
	}
	if start < len(buf) {
		names = append(names, string(buf[start:]))
	}
	return names
}

func isXattrUnsupported(err error) bool {
	return errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}

//go:build darwin

package fsutil

import (
	"encoding/binary"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// preserveNativeACL copies the Darwin kauth/FILESEC ACL using getattrlist and
// setattrlist ATTR_CMN_EXTENDED_SECURITY. Listxattr/Getxattr/Fsetxattr cannot
// see com.apple.system.Security (protected namespace); that path is not used.
func preserveNativeACL(f *os.File, srcPath string) error {
	acl, err := readNativeACL(srcPath)
	if err != nil {
		return err
	}
	if err := applyNativeACL(f.Name(), acl); err != nil {
		return fmt.Errorf("fsutil: preserving native ACL on replacement for %s: %w", srcPath, err)
	}
	return nil
}

func extendedSecurityAttrlist() unix.Attrlist {
	return unix.Attrlist{
		Bitmapcount: unix.ATTR_BIT_MAP_COUNT,
		Commonattr:  unix.ATTR_CMN_EXTENDED_SECURITY,
	}
}

func readNativeACL(path string) ([]byte, error) {
	al := extendedSecurityAttrlist()
	buf := make([]byte, 4096)
	if err := getattrlist(path, &al, buf, 0); err != nil {
		if isXattrNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fsutil: reading native ACL of %s: %w", path, err)
	}
	total := binary.LittleEndian.Uint32(buf[:4])
	if total < 12 {
		return nil, fmt.Errorf("fsutil: invalid native ACL response for %s", path)
	}
	off := int32(binary.LittleEndian.Uint32(buf[4:8]))
	length := binary.LittleEndian.Uint32(buf[8:12])
	if length == 0 {
		return nil, nil
	}
	start := 4 + int(off)
	end := start + int(length)
	if start < 12 || end > int(total) || end > len(buf) {
		return nil, fmt.Errorf("fsutil: truncated native ACL on %s", path)
	}
	out := make([]byte, length)
	copy(out, buf[start:end])
	return out, nil
}

func applyNativeACL(path string, blob []byte) error {
	// A nil ACL is a verified absence, not permission to retain inherited ACEs.
	if blob == nil {
		blob = make([]byte, 44)
		binary.LittleEndian.PutUint32(blob, 0x012cc16d)      // KAUTH_FILESEC_MAGIC
		binary.LittleEndian.PutUint32(blob[36:], 0xffffffff) // KAUTH_FILESEC_NOACL
	}
	al := extendedSecurityAttrlist()
	buf := make([]byte, 8+len(blob))
	binary.LittleEndian.PutUint32(buf[0:4], 8)
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(blob)))
	copy(buf[8:], blob)
	if err := unix.Setattrlist(path, &al, buf, 0); err != nil {
		return err
	}
	return nil
}

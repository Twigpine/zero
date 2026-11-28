//go:build !windows

package sandbox

import (
	"fmt"
	"os"
	"syscall"
)

// runtimeDirIdentity identifies the directory currently at path.
//
// device plus inode, read with Lstat so a link substituted at the final
// component is identified as the link rather than followed. The Windows build
// cannot use os.SameFile for this and explains why; here the same eager capture
// keeps both platforms on one rule.
func runtimeDirIdentity(path string) (string, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), true
}

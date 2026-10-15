//go:build !windows

package fsutil

import (
	"os"
	"syscall"
)

var posixChown = func(f *os.File, uid, gid int) error {
	return f.Chown(uid, gid)
}

func preserveOwner(f *os.File, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if err := posixChown(f, int(stat.Uid), int(stat.Gid)); err != nil {
		if int(stat.Uid) != os.Getuid() || int(stat.Gid) != os.Getgid() {
			return err
		}
	}
	return nil
}

//go:build !windows

package daemon

import (
	"fmt"
	"os"
	"syscall"
)

func checkStatusDirOwner(_ *os.Root, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("status directory ownership metadata is unavailable")
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("status directory is owned by uid %d, not the current user", stat.Uid)
	}
	return nil
}

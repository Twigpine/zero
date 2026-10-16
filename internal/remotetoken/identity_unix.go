//go:build !windows

package remotetoken

import (
	"fmt"
	"os"
	"syscall"
)

func fileIdentity(info os.FileInfo) (string, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("unix:%d:%d", uint64(stat.Dev), uint64(stat.Ino)), true
}

func openFileIdentity(file *os.File) (string, bool) {
	if file == nil {
		return "", false
	}
	info, err := file.Stat()
	if err != nil {
		return "", false
	}
	return fileIdentity(info)
}

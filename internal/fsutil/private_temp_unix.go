//go:build linux

package fsutil

import "os"

// POSIX ACL inheritance intersects the ACL mask with the requested group bits.
// 0600/0700 therefore disable every inherited named-user and group grant.
func createPrivateFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
}
func createPrivateDir(path string) error { return os.Mkdir(path, 0o700) }

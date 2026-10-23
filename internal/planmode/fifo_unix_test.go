//go:build unix

package planmode

import "syscall"

func mkfifoForTest(path string) error {
	return syscall.Mkfifo(path, 0o600)
}

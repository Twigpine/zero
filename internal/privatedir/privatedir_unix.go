//go:build !windows

package privatedir

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func harden(root *os.Root) (returnErr error) {
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("open private directory hardening handle: %w", err)
	}
	defer func() {
		if err := directory.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close private directory hardening handle: %w", err))
		}
	}()
	info, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("inspect private directory before hardening: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("private directory path is not a directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("private directory ownership metadata is unavailable")
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("private directory is owned by uid %d, not the current user", stat.Uid)
	}
	if err := directory.Chmod(0o700); err != nil {
		return fmt.Errorf("harden private directory permissions: %w", err)
	}
	info, err = directory.Stat()
	if err != nil {
		return fmt.Errorf("verify private directory permissions: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("private directory permissions are %04o, want owner-only", info.Mode().Perm())
	}
	return nil
}

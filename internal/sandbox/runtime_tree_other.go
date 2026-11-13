//go:build !windows

package sandbox

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ensureRuntimeTreeDir creates the owned tail beneath a physical base using
// openat and mkdirat with O_NOFOLLOW, so a symlink planted at any component is
// refused rather than followed.
//
// fchmod on the retained descriptor rather than chmod on the pathname: chmod(2)
// follows symlinks, which is the half of this defect that actually applies here.
// Every owned component gets the mode, not only the leaf, because every one of
// them is ours.
func ensureRuntimeTreeDir(physicalBase string, tail []string) error {
	parent, err := unix.Open(physicalBase, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open the sandbox runtime base %s: %w", physicalBase, err)
	}
	defer func() { _ = unix.Close(parent) }()

	current := physicalBase
	for _, name := range tail {
		current = filepath.Join(current, name)
		if err := unix.Mkdirat(parent, name, 0o700); err != nil && err != unix.EEXIST {
			return fmt.Errorf("create sandbox runtime directory %s: %w", current, err)
		}
		child, openErr := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			if openErr == unix.ELOOP || openErr == unix.ENOTDIR {
				return fmt.Errorf("sandbox runtime component %s is a link or not a directory, so a previous sandboxed command may have redirected it: %w", current, os.ErrInvalid)
			}
			return fmt.Errorf("open sandbox runtime directory %s: %w", current, openErr)
		}
		if err := unix.Fchmod(child, 0o700); err != nil {
			_ = unix.Close(child)
			return fmt.Errorf("secure sandbox runtime directory %s: %w", current, err)
		}
		_ = unix.Close(parent)
		parent = child
	}
	return nil
}

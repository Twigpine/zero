//go:build unix

package peermsg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func EnsurePrivateDir(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	components := strings.Split(strings.TrimPrefix(filepath.Clean(abs), string(filepath.Separator)), string(filepath.Separator))
	parentFD, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	// THE DESCRIPTOR THIS FUNCTION OWNS CHANGES ON EVERY ITERATION. The walk
	// closes parentFD and replaces it with the child it just opened, so a defer
	// that captured the first number would close a descriptor this function no
	// longer owns (or one the kernel has since reused) and leave the last one it
	// does own open. Close whatever is current when we leave, exactly once, on
	// every path.
	defer func() { unix.Close(parentFD) }()
	for index, component := range components {
		if component == "" {
			continue
		}
		var stat unix.Stat_t
		err = unix.Fstatat(parentFD, component, &stat, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			if err = unix.Mkdirat(parentFD, component, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				return err
			}
			err = unix.Fstatat(parentFD, component, &stat, unix.AT_SYMLINK_NOFOLLOW)
		}
		if err != nil {
			return err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			return fmt.Errorf("refusing non-directory or symlink runtime path component %q", component)
		}
		nextFD, openErr := unix.Openat(parentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			return openErr
		}
		if index == len(components)-1 {
			if int(stat.Uid) != os.Getuid() {
				unix.Close(nextFD)
				return fmt.Errorf("refusing runtime directory not owned by the current user: %q", path)
			}
			if err = unix.Fchmod(nextFD, 0o700); err != nil {
				unix.Close(nextFD)
				return err
			}
		}
		unix.Close(parentFD)
		parentFD = nextFD
	}
	return nil
}

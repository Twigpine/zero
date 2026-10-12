//go:build !windows

package lockutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

type platformLockState struct{}

func openLockFileAt(root, relative, displayPath string) (_ *os.File, resultErr error) {
	parentFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open lock root: %w", err)
	}
	defer func() {
		if parentFD >= 0 {
			resultErr = errors.Join(resultErr, unix.Close(parentFD))
		}
	}()

	components := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	for _, component := range components[:len(components)-1] {
		nextFD, openErr := unix.Openat(parentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			return nil, fmt.Errorf("open lock path component %q: %w", component, openErr)
		}
		if closeErr := unix.Close(parentFD); closeErr != nil {
			_ = unix.Close(nextFD)
			parentFD = -1
			return nil, fmt.Errorf("close lock path component: %w", closeErr)
		}
		parentFD = nextFD
	}

	name := components[len(components)-1]
	fd := -1
	// Darwin can transiently return ENOENT when many callers race to create the
	// same O_NOFOLLOW entry. The bound parent handle cannot be redirected, so a
	// bounded retry preserves first-use contention without weakening no-follow.
	for attempt := 0; attempt < 10; attempt++ {
		fd, err = unix.Openat(parentFD, name, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if !errors.Is(err, unix.ENOENT) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), displayPath)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("convert lock handle to file")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, errors.Join(fmt.Errorf("inspect lock file: %w", err), file.Close())
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.Join(errors.New("refusing non-regular lock file"), file.Close())
	}
	if stat.Nlink != 1 {
		return nil, errors.Join(errors.New("refusing multiply-linked lock file"), file.Close())
	}
	if err := unix.Close(parentFD); err != nil {
		parentFD = -1
		return nil, errors.Join(fmt.Errorf("close lock parent: %w", err), file.Close())
	}
	parentFD = -1
	return file, nil
}

func validateRootLockFile(file *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fmt.Errorf("inspect rooted lock file: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return errors.New("refusing non-regular rooted lock file")
	}
	if stat.Nlink != 1 {
		return errors.New("refusing multiply-linked rooted lock file")
	}
	return nil
}

func tryLockFile(file *os.File) (platformLockState, bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return platformLockState{}, false, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return platformLockState{}, true, nil
	}
	return platformLockState{}, false, err
}

func unlockFile(file *os.File, _ *platformLockState) error {
	if file == nil {
		return nil
	}
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

//go:build !windows

package config

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func tryProviderWriteLock(file *os.File) (func() error, bool, error) {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
	released := false
	return func() error {
		if released {
			return nil
		}
		released = true
		if err := errors.Join(unix.Flock(int(file.Fd()), unix.LOCK_UN), file.Close()); err != nil {
			return fmt.Errorf("release provider config/key transaction lock: %w", err)
		}
		return nil
	}, true, nil
}

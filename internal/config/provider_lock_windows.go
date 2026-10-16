//go:build windows

package config

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func tryProviderWriteLock(file *os.File) (func() error, bool, error) {
	handle := windows.Handle(file.Fd())
	overlapped := new(windows.Overlapped)
	err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	released := false
	return func() error {
		if released {
			return nil
		}
		released = true
		if err := errors.Join(windows.UnlockFileEx(handle, 0, 1, 0, overlapped), file.Close()); err != nil {
			return fmt.Errorf("release provider config/key transaction lock: %w", err)
		}
		return nil
	}, true, nil
}

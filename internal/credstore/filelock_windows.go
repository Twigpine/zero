//go:build windows

package credstore

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// acquireFileLock takes an exclusive OS lock (LockFileEx) so a read-modify-write
// of the credential file is serialized, matching the flock behaviour on unix.
//
// The lock file is SEPARATE from the data file for the same reason: write
// publishes by rename, and a lock on the renamed file would be attached to
// something the next writer has already replaced.
func (s *Store) acquireFileLock(exclusive bool) (func() error, error) {
	path := s.lockPath()
	if err := os.MkdirAll(filepathDir(path), 0o700); err != nil {
		return nil, fmt.Errorf("credstore: lock dir: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("credstore: open lock: %w", err)
	}
	handle := windows.Handle(file.Fd())
	overlapped := new(windows.Overlapped)
	// A fixed 1-byte region, blocking (no LOCKFILE_FAIL_IMMEDIATELY) so a
	// waiter queues rather than failing the operation. Writers pass
	// LOCKFILE_EXCLUSIVE_LOCK; readers omit it (a shared lock) so they still
	// serialize against a writer's publish — which matters more on Windows,
	// where an unsynchronized reader holding the file open blocks the rename.
	flags := uint32(0)
	if exclusive {
		flags = windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	if err := windows.LockFileEx(handle, flags, 0, 1, 0, overlapped); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("credstore: lock: %w", err)
	}
	return func() error {
		// Reported rather than swallowed, for the same reason as the unix side: a
		// cleanup that did not complete must not look identical to one that did.
		// It matters more here — on Windows the handle staying open is what blocks
		// the next writer's rename, so a failed release has a visible consequence.
		unlockErr := windows.UnlockFileEx(handle, 0, 1, 0, overlapped)
		closeErr := file.Close()
		if err := errors.Join(unlockErr, closeErr); err != nil {
			return fmt.Errorf("credstore: release lock: %w", err)
		}
		return nil
	}, nil
}

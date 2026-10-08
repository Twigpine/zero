//go:build !windows

package credstore

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// acquireFileLock takes an exclusive advisory lock (flock) so a read-modify-write
// of the credential file is serialized against every other one — across
// processes AND across goroutines, since flock is held per open file
// description and two opens in one process contend exactly as two processes do.
//
// THE LOCK FILE IS SEPARATE FROM THE DATA FILE, and that is not tidiness. write
// publishes by os.Rename, which replaces the inode; a lock taken on the data
// file would be attached to an inode that the next writer has already replaced,
// so every writer would appear to hold it. The lock lives on a file nothing
// renames.
func (s *Store) acquireFileLock(exclusive bool) (func() error, error) {
	path := s.lockPath()
	if err := os.MkdirAll(filepathDir(path), 0o700); err != nil {
		return nil, fmt.Errorf("credstore: lock dir: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("credstore: open lock: %w", err)
	}
	// Writers take LOCK_EX; readers take LOCK_SH so they run concurrently with
	// each other but still serialize against a writer's publish (see Get).
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	if err := unix.Flock(int(file.Fd()), how); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("credstore: lock: %w", err)
	}
	return func() error {
		// Close alone drops the flock, so the explicit unlock is belt-and-braces —
		// but its failure is reported rather than swallowed, because a cleanup that
		// did not complete must not be indistinguishable from one that did. The
		// callers join this into their result, so it annotates a successful write
		// instead of masking it.
		unlockErr := unix.Flock(int(file.Fd()), unix.LOCK_UN)
		closeErr := file.Close()
		if err := errors.Join(unlockErr, closeErr); err != nil {
			return fmt.Errorf("credstore: release lock: %w", err)
		}
		return nil
	}, nil
}

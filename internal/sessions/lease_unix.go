//go:build !windows

package sessions

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLockLease takes the lease lock on file without waiting: shared for a
// process that has the session open, exclusive for prune. It reports false when
// another holder's lock conflicts. flock locks belong to the open file
// description, so a second descriptor in the same process conflicts like another
// process would.
func tryLockLease(file *os.File, exclusive bool) (bool, error) {
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	err := unix.Flock(int(file.Fd()), how|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.EWOULDBLOCK) {
		return false, nil
	}
	return false, err
}

// openLeaseFile opens (creating it if needed) the lease file of a session whose
// directory exists.
func openLeaseFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}

func unlockLease(file *os.File) {
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

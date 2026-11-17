//go:build !windows

package sandbox

import (
	"os"

	"golang.org/x/sys/unix"
)

type runtimeLeaseHandle struct {
	file *os.File
}

// BOTH SIDES HAVE TO MEAN THE SAME OBJECT.
//
// Cleanup opened the lease by full pathname while acquisition opened it relative
// to a verified parent, so a symlink at <digest>.lease gave the two of them
// different files: a live command held a shared lock on one, cleanup took an
// exclusive lock on the other, both succeeded, and cleanup went on to remove a
// runtime root that was still in use.
func tryAcquireExclusiveRuntimeLease(root string) (runtimeLeaseHandle, bool, error) {
	return tryAcquireExclusiveRuntimeLeaseRootedUnix(root)
}

func (lease runtimeLeaseHandle) release() {
	if lease.file == nil {
		return
	}
	_ = unix.Flock(int(lease.file.Fd()), unix.LOCK_UN)
	_ = lease.file.Close()
}

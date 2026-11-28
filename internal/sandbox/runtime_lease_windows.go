//go:build windows

package sandbox

import (
	"os"

	"golang.org/x/sys/windows"
)

type runtimeLeaseHandle struct {
	file       *os.File
	overlapped windows.Overlapped
}

// BOTH SIDES HAVE TO MEAN THE SAME OBJECT.
//
// This opened the lease by full pathname with os.OpenFile and no no-follow flag,
// while shared acquisition opened it relative to a retained parent handle. A file
// symbolic link at <digest>.lease therefore handed the two sides different
// objects: setup or a running command held a shared lock on the link, cleanup
// took an exclusive lock on its target, both calls succeeded, and cleanup went on
// to RemoveAll a root somebody was still using.
//
// Mutual exclusion is a property of the OBJECT, so cleanup resolves the name the
// way acquisition does and refuses the same reparse objects.
func tryAcquireExclusiveRuntimeLease(root string) (runtimeLeaseHandle, bool, error) {
	return tryAcquireExclusiveRuntimeLeaseRooted(root)
}

func (lease runtimeLeaseHandle) release() {
	if lease.file == nil {
		return
	}
	_ = windows.UnlockFileEx(windows.Handle(lease.file.Fd()), 0, 1, 0, &lease.overlapped)
	_ = lease.file.Close()
}

// Package fsutil provides small filesystem helpers shared across packages.
package fsutil

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
)

// CommittedReplacementCleanupError reports that a replacement was committed,
// but the old destination retained at BackupPath could not be removed. Callers
// must treat the replacement itself as successful and surface the cleanup
// problem separately.
type CommittedReplacementCleanupError struct {
	BackupPath string
	Cause      error
}

func (err *CommittedReplacementCleanupError) Error() string {
	return fmt.Sprintf("replacement committed, but backup %s could not be removed: %v", err.BackupPath, err.Cause)
}

func (err *CommittedReplacementCleanupError) Unwrap() error {
	return err.Cause
}

// ReplaceWithRetry publishes src over dst using the platform's replacement
// primitive, retrying on the same transient Windows lock errors RenameWithRetry
// handles. On Windows it uses ReplaceFileW so an existing destination keeps its
// DACL and selected metadata instead of receiving the temporary file's inherited
// DACL. ReplaceFileW is not observer-atomic and may briefly leave dst absent;
// callers that cannot tolerate that must synchronize their own readers. On Unix
// replacement uses os.Rename.
//
// replace overrides the platform primitive so tests can exercise the retry path;
// pass nil for the default.
func ReplaceWithRetry(src, dst string, replace func(src, dst string) error) error {
	if replace == nil {
		replace = replaceExisting
	}
	return RenameWithRetry(src, dst, replace)
}

// RenameWithRetry renames src to dst, retrying briefly on Windows when the
// destination is transiently locked (antivirus scanners, search indexers, or
// a concurrent reader holding the file open). rename overrides os.Rename so
// tests can exercise the retry path; pass nil to use os.Rename.
func RenameWithRetry(src, dst string, rename func(src, dst string) error) error {
	if rename == nil {
		rename = os.Rename
	}
	var err error
	for i := 0; i < 10; i++ {
		err = rename(src, dst)
		if err == nil {
			return nil
		}
		var committed *CommittedReplacementCleanupError
		if errors.As(err, &committed) {
			// The source has already been consumed. In particular, never retry if
			// the cleanup cause itself is a transient Windows sharing violation.
			break
		}
		if runtime.GOOS == "windows" {
			if os.IsPermission(err) || isWindowsSharingOrLockViolation(err) {
				time.Sleep(10 * time.Millisecond)
				continue
			}
		}
		break
	}
	return err
}

// IsSharingOrLockViolation reports whether err is a Windows sharing or lock
// violation: another open handle on the file denies the requested access. It
// is always false on other platforms, where the same errno values mean
// unrelated errors.
func IsSharingOrLockViolation(err error) bool {
	return runtime.GOOS == "windows" && isWindowsSharingOrLockViolation(err)
}

func isWindowsSharingOrLockViolation(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		const ERROR_SHARING_VIOLATION syscall.Errno = 32
		const ERROR_LOCK_VIOLATION syscall.Errno = 33
		return errno == ERROR_SHARING_VIOLATION || errno == ERROR_LOCK_VIOLATION
	}
	return false
}

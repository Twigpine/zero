// Package fsutil provides small filesystem helpers shared across packages.
package fsutil

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// ErrNonRegularDestination is returned when WriteFileAtomic would replace a
// FIFO, device, socket, directory, or other special file with a regular file.
var ErrNonRegularDestination = errors.New("fsutil: destination exists and is not a regular file")

// stagingModeObserver, when non-nil, receives the mode of the freshly created
// staging file before any metadata is copied onto it. Tests use it to assert
// that a replacement is staged no broader than its destination.
var stagingModeObserver func(os.FileMode)

// stagingProtectionObserver, when non-nil, receives the path of the staging
// file after protectStaging has copied the destination's authorization metadata
// onto the completed content, before publication. Tests use it to
// assert that a replacement is staged no broader than its destination on
// platforms (Windows) where the mode bits do not carry that answer.
var stagingProtectionObserver func(stagingPath string)

// WriteFileAtomic writes data to a temporary file in the same directory as filename,
// flushes and syncs it to disk, and replaces filename atomically via ReplaceWithRetry.
// For new files, it honors the process umask by creating the temporary file with
// os.OpenFile(..., perm). For existing regular files, it requires write access to
// the current destination and copies that destination's authorization metadata onto
// the replacement (mode bits including setuid/setgid/sticky, owner, and xattrs such
// as POSIX ACLs, SELinux labels, and capabilities), stripping any default-ACL xattr
// inherited by the staging file when the destination carried none. If that metadata
// cannot be listed, read, applied, or stripped, the call fails closed and leaves the
// destination unchanged. Non-regular, non-symlink destinations are refused before
// staging so a FIFO, device, or socket is not replaced.
//
// Platform differences on symlink destinations: On Unix (Linux/macOS), replacing a
// symlink destination replaces the symlink itself with the new regular file. On Windows,
// ReplaceFileW refuses symlink destinations outright and returns an error.
// Hard links to destination files are broken by design (temp-and-rename publishes a new inode).
//
// On Windows, replacing a destination that another process holds open without
// delete sharing fails with a sharing violation ("being used by another
// process"). ReplaceWithRetry retries briefly for a holder that is only passing
// through, but a long-lived handle (Go's os.Open, Python's open) makes the
// replacement fail closed: the old bytes stay and no temporary file is left
// behind.
func WriteFileAtomic(filename string, data []byte, perm os.FileMode) error {
	return writeFileAtomic(filename, data, perm, nil)
}

// WriteFileAtomicExclusive creates filename with data and fails if the path
// already exists or appears concurrently. Unlike WriteFileAtomic it never
// replaces a pre-existing object (regular file, symlink, or special file): the
// destination is staged and synced like WriteFileAtomic, then published with an
// atomic no-replace primitive, so a racing creator is refused with an error
// wrapping os.ErrExist rather than overwritten. Readers never observe a partial
// destination. This is the fail-closed equivalent of O_CREATE|O_EXCL for a
// caller that must not clobber a file it did not observe.
func WriteFileAtomicExclusive(filename string, data []byte, perm os.FileMode) error {
	return writeFileAtomicExclusive(filename, data, perm)
}

// exclusiveBeforePublish, when non-nil, runs after the staging file has been
// synced and closed but before the no-replace publication. Tests use it to
// prove that a file appearing in that window is refused, not overwritten.
var exclusiveBeforePublish func(filename string)

// exclusiveRemoveStaging removes the staging path after exclusive publication.
// Tests replace it to exercise post-commit cleanup failure handling.
var exclusiveRemoveStaging = os.Remove

func writeFileAtomicExclusive(filename string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmpFile, err := createTempFile(dir, perm)
	if err != nil {
		return err
	}
	tmpName := tmpFile.Name()
	closed := false
	removed := false
	defer func() {
		if !closed {
			_ = tmpFile.Close()
		}
		if !removed {
			_ = exclusiveRemoveStaging(tmpName)
		}
	}()

	if _, err := tmpFile.Write(data); err != nil {
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		return err
	}
	closed = true
	if err := tmpFile.Close(); err != nil {
		return err
	}

	if exclusiveBeforePublish != nil {
		exclusiveBeforePublish(filename)
	}
	if err := publishExclusive(tmpName, filename); err != nil {
		return err
	}
	removed = true
	removeErr := exclusiveRemoveStaging(tmpName)
	syncDir(dir)
	if removeErr != nil && !os.IsNotExist(removeErr) {
		return &CommittedReplacementCleanupError{
			BackupPath: tmpName,
			Cause:      removeErr,
		}
	}
	return nil
}

// The replacement dependency is local to the call, so failure tests need no global hook.
func writeFileAtomic(filename string, data []byte, perm os.FileMode, replace func(string, string) error) error {
	dir := filepath.Dir(filename)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	var existingMode *os.FileMode
	info, err := os.Lstat(filename)
	switch {
	case err == nil:
		mode := info.Mode()
		switch {
		case mode.IsRegular():
			if err := ensureWritable(filename); err != nil {
				return err
			}
			existingMode = &mode
		case mode&os.ModeSymlink != 0:
			// Documented platform-specific replacement of the symlink itself.
		default:
			return fmt.Errorf("%w: %s", ErrNonRegularDestination, filename)
		}
	case !os.IsNotExist(err):
		return err
	}

	var tmpFile *os.File
	if existingMode != nil {
		tmpFile, err = CreatePrivateTemp(dir, ".zero-tmp-*")
	} else {
		tmpFile, err = createTempFile(dir, perm)
	}
	if err != nil {
		return err
	}
	if stagingModeObserver != nil {
		if info, statErr := tmpFile.Stat(); statErr == nil {
			stagingModeObserver(info.Mode())
		}
	}
	tmpName := tmpFile.Name()
	closed := false
	defer func() {
		if !closed {
			_ = tmpFile.Close()
		}
		_ = os.Remove(tmpName)
	}()

	if _, err := tmpFile.Write(data); err != nil {
		return err
	}
	if existingMode != nil {
		if err := preserveOwner(tmpFile, info); err != nil {
			return err
		}
		// Remove/copy inherited authorization before broadening mode bits.
		if err := preserveNativeACL(tmpFile, filename); err != nil {
			return err
		}
		if err := preserveXattrs(tmpFile, filename); err != nil {
			return err
		}
		if err := protectStaging(tmpFile, filename); err != nil {
			return err
		}
		if err := tmpFile.Chmod(*existingMode); err != nil {
			return err
		}
		if stagingProtectionObserver != nil {
			stagingProtectionObserver(tmpName)
		}
	}
	if err := tmpFile.Sync(); err != nil {
		return err
	}
	closed = true
	if err := tmpFile.Close(); err != nil {
		return err
	}

	replaceErr := ReplaceWithRetry(tmpName, filename, replace)
	if replaceErr == nil || isCommittedReplacement(replaceErr) {
		syncDir(dir)
	}
	return replaceErr
}

func ensureWritable(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

func createTempFile(dir string, perm os.FileMode) (*os.File, error) {
	for i := 0; i < 10000; i++ {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		name := filepath.Join(dir, fmt.Sprintf(".zero-tmp-%x", b))
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return f, err
	}
	return nil, errors.New("fsutil: failed to create temporary file after repeated attempts")
}

func isCommittedReplacement(err error) bool {
	var committed *CommittedReplacementCleanupError
	return errors.As(err, &committed)
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

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

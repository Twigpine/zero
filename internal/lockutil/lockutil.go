// Package lockutil provides cross-process advisory file locks used by cron,
// daemon, hooks, oauth, and swarm. Locks are held by the kernel for the lifetime
// of an open file handle, so process exit releases them without a stale-file
// reclaim protocol. The lock file itself is stable: holders never rename or
// remove it while another process may still have its inode locked.
package lockutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	// ErrLockHeld reports that another process or goroutine currently holds the
	// advisory lock. Callers may wait and retry until their own deadline.
	ErrLockHeld = errors.New("lockutil: lock is held")
	// ErrLockReleased reports an attempt to update metadata after Release.
	ErrLockReleased = errors.New("lockutil: lock is released")
)

var holderSequence atomic.Uint64

// FileLock is an exclusive advisory lock held through an open file handle.
// Release is idempotent. The lock path remains on disk after release; deleting
// or replacing it would let different processes lock different inodes.
type FileLock struct {
	mu         sync.Mutex
	file       *os.File
	state      platformLockState
	released   bool
	releaseErr error
}

// TryAcquireFileLock attempts to take an exclusive advisory lock without
// waiting. A contended lock returns ErrLockHeld. The kernel releases a lock if
// its process exits, so an old unlocked file never needs stale reclamation.
func TryAcquireFileLock(path string) (*FileLock, error) {
	return TryAcquireFileLockAt(filepath.Dir(path), path)
}

// TryAcquireFileLockAt attempts to lock path while confining every component
// below root to a handle-relative traversal. root is the caller's trusted
// boundary and may itself be reached through a legitimate link; links and
// reparse points below it are rejected before metadata can be written.
func TryAcquireFileLockAt(root, path string) (*FileLock, error) {
	if root == "" {
		return nil, errors.New("lockutil: lock root is empty")
	}
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return nil, fmt.Errorf("lockutil: resolve lock path relative to root: %w", err)
	}
	relative = filepath.Clean(relative)
	if relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("lockutil: lock path %q escapes root %q", path, root)
	}

	file, err := openLockFileAt(root, relative, path)
	if err != nil {
		return nil, fmt.Errorf("lockutil: open lock file: %w", err)
	}
	return acquireOpenedFileLock(file)
}

// TryAcquireFileLockRoot attempts to lock name relative to an already-bound
// directory root. It is intended for lifecycle owners that must not resolve the
// root pathname again between validation and lock acquisition.
func TryAcquireFileLockRoot(root *os.Root, name, displayPath string) (*FileLock, error) {
	if root == nil {
		return nil, errors.New("lockutil: lock root is nil")
	}
	name = filepath.Clean(name)
	if name == "." || name == ".." || filepath.IsAbs(name) || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("lockutil: lock name %q escapes root", name)
	}
	file, err := openLockFileRoot(root, name, displayPath)
	if err != nil {
		return nil, fmt.Errorf("lockutil: open rooted lock file: %w", err)
	}
	return acquireOpenedFileLock(file)
}

func acquireOpenedFileLock(file *os.File) (*FileLock, error) {
	state, contended, err := tryLockFile(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lockutil: acquire advisory lock: %w", err)
	}
	if contended {
		_ = file.Close()
		return nil, ErrLockHeld
	}

	lock := &FileLock{file: file, state: state}
	metadata := []byte(fmt.Sprintf("%d-%d\n", os.Getpid(), holderSequence.Add(1)))
	if err := lock.WriteMetadata(metadata); err != nil {
		return nil, errors.Join(err, lock.Release())
	}
	return lock, nil
}

func openLockFileRoot(root *os.Root, name, displayPath string) (*os.File, error) {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		file, createErr := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if createErr != nil {
			return nil, createErr
		}
		if err := validateRootLockFile(file); err != nil {
			return nil, errors.Join(err, file.Close())
		}
		return file, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("refusing non-regular rooted lock file")
	}
	file, err := root.OpenFile(name, os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !os.SameFile(info, opened) {
		return nil, errors.Join(errors.New("rooted lock file changed while opening"), file.Close())
	}
	if err := validateRootLockFile(file); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

// WriteMetadata replaces the diagnostic contents of the lock file while the
// caller holds it. Metadata does not establish ownership; the kernel lock does.
func (lock *FileLock) WriteMetadata(data []byte) error {
	if lock == nil {
		return ErrLockReleased
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.released || lock.file == nil {
		return ErrLockReleased
	}
	if err := lock.file.Truncate(0); err != nil {
		return fmt.Errorf("lockutil: truncate lock metadata: %w", err)
	}
	if _, err := lock.file.Seek(0, 0); err != nil {
		return fmt.Errorf("lockutil: seek lock metadata: %w", err)
	}
	if _, err := lock.file.Write(data); err != nil {
		return fmt.Errorf("lockutil: write lock metadata: %w", err)
	}
	return nil
}

// Release unlocks and closes the held file handle. It deliberately leaves the
// stable lock file in place. Repeated calls return the first release result.
func (lock *FileLock) Release() error {
	if lock == nil {
		return nil
	}
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if lock.released {
		return lock.releaseErr
	}
	lock.released = true
	unlockErr := unlockFile(lock.file, &lock.state)
	closeErr := lock.file.Close()
	lock.file = nil
	if err := errors.Join(unlockErr, closeErr); err != nil {
		lock.releaseErr = fmt.Errorf("lockutil: release advisory lock: %w", err)
	}
	return lock.releaseErr
}

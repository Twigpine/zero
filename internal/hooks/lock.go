package hooks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Twigpine/zero/internal/lockutil"
)

// Cross-process lock tuning for the audit log. The lock is held only across a
// single read-then-append (milliseconds), so the timeout is generous relative
// to any real hold.
const (
	auditLockTimeout     = 10 * time.Second
	auditLockRetryDelay  = 20 * time.Millisecond
	configLockTimeout    = 10 * time.Second
	configLockRetryDelay = 20 * time.Millisecond
)

// lockConfigFile takes the cross-process advisory lock that serializes every
// mutation of a hooks config. The lock file is a stable sibling of the config
// (<path>.lock): the kernel releases it when the holder exits, so a crash never
// leaves a lock that needs reclaiming and the path must never be renamed or
// removed.
func lockConfigFile(path string) (func() error, error) {
	lockPath := path + ".lock"
	if err := ensureConfigDir(filepath.Dir(lockPath)); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(configLockTimeout)
	for {
		lock, err := lockutil.TryAcquireFileLock(lockPath)
		if err == nil {
			return lock.Release, nil
		}
		if !errors.Is(err, lockutil.ErrLockHeld) {
			return nil, fmt.Errorf("hooks: acquire config lock: %w", err)
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("hooks: timed out acquiring config lock %s", lockPath)
		}
		time.Sleep(configLockRetryDelay)
	}
}

// lockAudit takes a cross-process advisory lock on the audit log through a
// stable sibling "<auditPath>.lock" file. It makes
// the lastSequence()+append in append() atomic across processes; the audit log is
// a shared global file, so without it two processes can read the same last
// sequence and write duplicate numbers. Kernel lock lifetime provides crash
// recovery without moving or deleting the lock path.
func (store *AuditStore) lockAudit() (func(), error) {
	lockPath := store.auditPath + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(auditLockTimeout)
	for {
		lock, err := lockutil.TryAcquireFileLock(lockPath)
		if err == nil {
			return func() { _ = lock.Release() }, nil
		}
		if !errors.Is(err, lockutil.ErrLockHeld) {
			return nil, fmt.Errorf("hooks: acquire audit lock: %w", err)
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("hooks: timed out acquiring audit lock")
		}
		time.Sleep(auditLockRetryDelay)
	}
}

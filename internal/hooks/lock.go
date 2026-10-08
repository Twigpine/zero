package hooks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Gitlawb/zero/internal/lockutil"
)

// Cross-process lock tuning for the audit log. The lock is held only across a
// single read-then-append (milliseconds), so the timeout is generous relative
// to any real hold.
const (
	auditLockTimeout    = 10 * time.Second
	auditLockRetryDelay = 20 * time.Millisecond
)

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

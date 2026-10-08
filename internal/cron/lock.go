package cron

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Gitlawb/zero/internal/lockutil"
)

// Cross-process lock tuning. The lock is held only for a single metadata
// read-modify-write (milliseconds), never across a job's exec, so the timeout
// is generous relative to any real hold.
const (
	cronLockTimeout    = 10 * time.Second
	cronLockRetryDelay = 20 * time.Millisecond
)

// lockJob takes a cross-process advisory lock for one job on a stable sibling
// "<id>.lock" file next to the job directory. It serializes the read-modify-write
// of a job's metadata across concurrent schedulers and commands. The kernel
// releases the lock when a holder exits; the file is never renamed or removed.
func (s *Store) lockJob(id string) (func(), error) {
	if !validID(id) {
		return nil, fmt.Errorf("invalid cron job id %q", id)
	}
	lockPath := s.jobDir(id) + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(cronLockTimeout)
	for {
		lock, err := lockutil.TryAcquireFileLock(lockPath)
		if err == nil {
			return func() { _ = lock.Release() }, nil
		}
		if !errors.Is(err, lockutil.ErrLockHeld) {
			return nil, fmt.Errorf("cron: acquire job lock: %w", err)
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("cron: timed out acquiring job lock for %q", id)
		}
		time.Sleep(cronLockRetryDelay)
	}
}

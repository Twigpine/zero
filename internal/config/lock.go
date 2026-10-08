package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Gitlawb/zero/internal/lockutil"
)

// ErrLockUnavailable reports that the config lock was still held by another
// process (or goroutine) when configLockTimeout expired. A caller that would
// otherwise make a SECOND config write after a failed one branches on this: the
// lock has not been released, so the second attempt can only spend another full
// timeout to fail the same way, turning one keystroke into two stalls.
var ErrLockUnavailable = errors.New("config lock unavailable")

const (
	configLockTimeout    = 10 * time.Second
	configLockRetryDelay = 20 * time.Millisecond
)

// lockConfigFile serializes a config read-modify-write across processes.
//
// Every mutator in this package loads the whole document, edits independent
// fields, and publishes a complete replacement by rename. The rename is atomic,
// so a reader never sees partial JSON — but two processes that both loaded the
// same revision each write a full document, and the second rename silently
// discards the first one's acknowledged update (issue #832). Startup work makes
// this easy to hit: MigratePlaintextProviderKeys rewrites the config on every
// launch, so it collides with any concurrent mutation from another Zero.
//
// The lock must therefore span load, mutation, validation AND publication:
// holding it only around the write would still let both processes read the same
// stale revision first. Callers acquire before their first read, which is what
// makes the re-read inside the lock authoritative.
//
// The lock file is a sibling of the config, never the config itself. The kernel
// holds an advisory lock against an inode, and publishing the config by rename
// installs a NEW inode — locking the config directly would leave each process
// holding a different one. lockutil keeps the sibling's path stable for the
// same reason, and never removes it.
//
// The returned release function reports its own failure rather than swallowing
// it: a failed Release can leave the lock held for the rest of the process, so
// a mutator that returned success after it would be reporting a state the next
// mutation cannot reproduce. Callers join it into their result (AGENTS.md:
// "never report success when cleanup or unlock failed").
//
// This also serializes goroutines within one process: each acquisition opens
// its own file description, so a second in-process attempt contends exactly as
// another process would. The lock is NOT reentrant, so an exported mutator that
// needs another mutator's work calls the unexported *Locked form instead of
// re-entering through the public function.
// LockFile exposes lockConfigFile to packages that edit the SAME user config
// document without going through this package's mutators — internal/cli's MCP
// editor reads, edits and republishes it with the identical temp-file+rename
// shape. A writer that skipped this lock would reintroduce the lost update for
// every field, so the lock has to be one authority across packages rather than
// a private detail of this one.
func LockFile(path string) (func() error, error) {
	return lockConfigFileFn(path)
}

// lockConfigFileFn is the seam the mutators call. Release failures cannot be
// provoked through the public API — lockutil.Release is idempotent and reports
// nil once released — so substituting this is the only way to assert that a
// failed unlock actually reaches the caller instead of being swallowed.
var lockConfigFileFn = lockConfigFile

func lockConfigFile(path string) (func() error, error) {
	lockPath := path + ".lock"
	if dir := filepath.Dir(lockPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create config directory %s: %w", dir, err)
		}
	}
	deadline := time.Now().Add(configLockTimeout)
	for {
		lock, err := lockutil.TryAcquireFileLock(lockPath)
		if err == nil {
			return lock.Release, nil
		}
		if !errors.Is(err, lockutil.ErrLockHeld) {
			return nil, fmt.Errorf("config: acquire config lock: %w", err)
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("config: timed out acquiring config lock for %s: %w", path, ErrLockUnavailable)
		}
		time.Sleep(configLockRetryDelay)
	}
}

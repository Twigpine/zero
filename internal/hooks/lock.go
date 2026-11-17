package hooks

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/lockutil"
)

// Cross-process lock tuning for the audit log and hook configs. The locks are
// held only across a single read-modify-write (milliseconds), so the timeouts
// are generous relative to any real hold.
const (
	auditLockTimeout     = 10 * time.Second
	auditLockRetryDelay  = 20 * time.Millisecond
	configLockRetryDelay = 20 * time.Millisecond
)

// configLockTimeout bounds how long a mutation waits for a competing
// config-lock holder before giving up. It is a variable so the timeout test
// can shrink the wait; production callers must not write it.
var configLockTimeout = 10 * time.Second

// lockConfigFile takes the cross-process advisory lock that serializes every
// mutation of a hooks config. The lock file is stable: the kernel releases it
// when the holder exits, so a crash never leaves a lock that needs reclaiming
// and the path must never be renamed or removed.
//
// Where the lock lives depends on the config. The user config is already user
// state and keeps a stable sibling. The project config lives inside the user's
// repository — EXTENDING.md invites committing .zero/hooks.json — and a stable
// sibling there would sit in every repository where a hooks command ever ran,
// so its lock lives under the user state directory keyed by the config's
// canonical path instead. That scope is per user: two different accounts
// mutating the same project do not contend.
func lockConfigFile(path string) (func() error, error) {
	lockPath, err := configLockPath(path)
	if err != nil {
		return nil, err
	}
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

// configLockPath returns the stable lock file for the hooks config at path.
// The user config keeps its sibling; every other config (the project's
// .zero/hooks.json above all) locks a digest of its canonical path under the
// user state directory, which keeps hostile project path characters out of the
// lock file name and lets two symlinked spellings of the same directory —
// macOS /var vs /private/var, a symlinked project directory — contend on one
// lock. Windows junctions are not symlinks and are left unresolved by
// filepath.EvalSymlinks, so a real directory and a junction to it hash to two
// lock files; that is a documented limit (the kernel lock still prevents
// partial writes through either spelling).
func configLockPath(path string) (string, error) {
	canonical := canonicalConfigPath(path)
	if isUserConfigPath(canonical) {
		return canonical + ".lock", nil
	}
	root, err := configLockRoot()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(canonical))
	return filepath.Join(root, hex.EncodeToString(digest[:])+".lock"), nil
}

// isUserConfigPath reports whether path is the user-level hooks config the
// loader resolves by default. That file is user state, so it may keep a stable
// sibling lock; anything else is treated as inside a tree the user may commit.
func isUserConfigPath(path string) bool {
	userConfigPath, err := defaultUserConfigPath()
	if err != nil {
		return false
	}
	return userConfigPath == path
}

// defaultUserConfigPath resolves the default user-layer hooks config from the
// environment the same way ResolvePaths does, without needing a cwd.
func defaultUserConfigPath() (string, error) {
	home := strings.TrimSpace(firstNonEmpty(os.Getenv("HOME"), os.Getenv("USERPROFILE")))
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home: %w", err)
		}
	}
	configHome := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	return canonicalConfigPath(filepath.Join(configHome, "zero", "hooks.json")), nil
}

// configLockRoot resolves the user state directory that holds the locks of
// project hook configs, mirroring worktrees.DefaultBaseDir: LOCALAPPDATA on
// Windows, XDG_STATE_HOME elsewhere, and ~/.local/state as the final fallback.
func configLockRoot() (string, error) {
	return configLockRootFor(runtime.GOOS, os.Getenv)
}

// configLockRootFor is configLockRoot parameterized over the platform and the
// environment lookup, so the Windows branches and the HOME fallback that never
// run on a Linux unit-test host stay exercised by table tests on every host.
func configLockRootFor(goos string, getenv func(string) string) (string, error) {
	if goos == "windows" {
		if localAppData := strings.TrimSpace(getenv("LOCALAPPDATA")); localAppData != "" {
			return filepath.Join(localAppData, "zero", "hooks", "config-locks"), nil
		}
		if profile := strings.TrimSpace(getenv("USERPROFILE")); profile != "" {
			return filepath.Join(profile, "AppData", "Local", "zero", "hooks", "config-locks"), nil
		}
	}
	if stateHome := strings.TrimSpace(getenv("XDG_STATE_HOME")); stateHome != "" {
		return filepath.Join(stateHome, "zero", "hooks", "config-locks"), nil
	}
	home := strings.TrimSpace(firstNonEmpty(getenv("HOME"), getenv("USERPROFILE")))
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("hooks: resolve user state directory: %w", err)
		}
	}
	return filepath.Join(home, ".local", "state", "zero", "hooks", "config-locks"), nil
}

// canonicalConfigPath resolves the directory containing path to the physical
// form its existing ancestors resolve to, but keeps the final component (the
// config file) spelled exactly as given. Only the parent is resolved so that a
// symlinked hooks.json is replaced by the atomic rename in writeConfigData,
// matching the behavior before the lock was added; resolving the file itself
// would write through the link and clobber whatever it points at. Resolving
// the parent still folds two symlinked spellings of the same directory (macOS
// /var vs /private/var, a symlinked project directory) into one lock key.
//
// Windows junctions are not symlinks: filepath.EvalSymlinks leaves them
// unresolved, so a real directory and a junction to it hash to two lock files.
// That is a documented limit of the user-state lock, not a correctness issue:
// the kernel lock still makes partial writes impossible through either path.
func canonicalConfigPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	dir, base := filepath.Split(path)
	return filepath.Join(canonicalConfigDir(dir), base)
}

// canonicalConfigDir resolves dir to the physical form its existing ancestors
// resolve to, keeping the not-yet-created tail spelled under the resolved
// ancestor. A directory another writer creates concurrently resolves to the
// same physical ancestor either way, so both sides of that race key the same
// lock; the walk never fails, falling back to the cleaned absolute path.
func canonicalConfigDir(dir string) string {
	var missing []string
	for current := filepath.Clean(dir); ; {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return dir
		}
		missing = append([]string{filepath.Base(current)}, missing...)
		current = parent
	}
}

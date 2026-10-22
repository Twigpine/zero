package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ONE SETUP AT A TIME PER SANDBOX HOME.
//
// Setup publishes two things in two operations: the stamp inside the runtime
// tree, written while the capability ACL is applied, and the marker in the
// sandbox home, written last. The runtime lease it holds is SHARED, because its
// job is to keep an exclusive eviction away from the tree, and a shared lease is
// something two setup helpers can both hold. Nothing then ordered them:
//
//	A writes its stamp            B writes its stamp
//	                              B publishes its marker and succeeds
//	A publishes its marker and succeeds
//
// Each write is atomic and the pair is not. The home ends up with one setup's
// marker over another setup's tree, and compensation has the same problem in
// reverse: a setup that fails late puts back the snapshot it took at the start,
// over whatever a second setup committed in between. Reported by @jatmn.
//
// The ordering that is missing is between whole transactions, so that is what is
// locked: from before the first thing setup records about the previous state
// until after the marker is published or every compensation has run. The lock
// lives in the sandbox home because the marker does, and the marker is the half
// of the pair there is exactly one of per home. It is a file lock, held through
// an open handle, because the two setups are two elevated helper PROCESSES: a
// mutex inside one of them orders nothing.
//
// Commands do not take it. They read the pair after a setup has returned, and
// serializing every sandboxed command behind a lock file would be a cost with no
// bug behind it.
const windowsSandboxSetupLockName = "windows-setup.lock"

var (
	// windowsSandboxSetupLockTimeout bounds the wait for another setup. Setup is a
	// few seconds of work, so a holder that outlasts this is stuck rather than
	// slow, and failing with the reason is more use than hanging an elevated
	// terminal. Nothing has been changed when this fires.
	windowsSandboxSetupLockTimeout = 30 * time.Second
	windowsSandboxSetupLockRetry   = 25 * time.Millisecond

	// windowsSandboxSetupLockContendedHook fires once per acquisition that found
	// the lock held, before it starts waiting. Nil in production; a test uses it
	// as the barrier that proves a second setup is queued behind the first without
	// sleeping and hoping.
	windowsSandboxSetupLockContendedHook func()

	// The two halves of a release, as variables so a test can make either one
	// fail. Production uses the real calls.
	windowsSandboxSetupUnlock    = unlockGrantFile
	windowsSandboxSetupLockClose = func(file *os.File) error { return file.Close() }
)

// errWindowsSandboxSetupBusy is returned when another setup for the same home
// kept the lock for the whole wait.
var errWindowsSandboxSetupBusy = errors.New("another `zero sandbox setup` for this sandbox home is still running")

// lockWindowsSandboxSetup takes the per-home setup lock and returns its release.
//
// The release REPORTS. It used to discard both the unlock and the close, and
// setup defers it after publishing the marker, so an operator was told setup
// succeeded without anything knowing whether the lock had been let go. Closing
// the handle ordinarily releases the lock even when the explicit unlock failed,
// so this is rarely more than a reporting gap, but setup does not get to report
// success over a release it could not confirm. Reported by @jatmn.
//
// The lock file is never removed. Unlinking a lock file while another process is
// waiting on it lets a third one create a fresh file at the same name and hold
// "the lock" alongside the first, which is the failure this exists to prevent.
func lockWindowsSandboxSetup(sandboxHome string) (func() error, error) {
	sandboxHome = strings.TrimSpace(sandboxHome)
	if sandboxHome == "" {
		return nil, errors.New("windows sandbox setup requires a sandbox home to lock")
	}
	if err := os.MkdirAll(sandboxHome, 0o700); err != nil {
		return nil, fmt.Errorf("create the sandbox home for the setup lock: %w", err)
	}
	lockPath := filepath.Join(sandboxHome, windowsSandboxSetupLockName)
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the sandbox setup lock %s: %w", lockPath, err)
	}
	deadline := time.Now().Add(windowsSandboxSetupLockTimeout)
	announced := false
	for {
		locked, lockErr := tryLockGrantFile(file)
		if lockErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("lock the sandbox setup lock %s: %w", lockPath, lockErr)
		}
		if locked {
			return func() error {
				return releaseWindowsSandboxSetupLock(file, lockPath)
			}, nil
		}
		if !announced {
			announced = true
			if windowsSandboxSetupLockContendedHook != nil {
				windowsSandboxSetupLockContendedHook()
			}
		}
		if !time.Now().Before(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("%w (waited %s on %s); nothing was changed, re-run setup when the other one has finished",
				errWindowsSandboxSetupBusy, windowsSandboxSetupLockTimeout, lockPath)
		}
		time.Sleep(windowsSandboxSetupLockRetry)
	}
}

// releaseWindowsSandboxSetupLock unlocks and closes, always both, and reports
// whichever failed.
func releaseWindowsSandboxSetupLock(file *os.File, lockPath string) error {
	var failures []error
	if err := windowsSandboxSetupUnlock(file); err != nil {
		failures = append(failures, fmt.Errorf("unlock the sandbox setup lock %s: %w", lockPath, err))
	}
	if err := windowsSandboxSetupLockClose(file); err != nil {
		failures = append(failures, fmt.Errorf("close the sandbox setup lock %s: %w", lockPath, err))
	}
	return errors.Join(failures...)
}

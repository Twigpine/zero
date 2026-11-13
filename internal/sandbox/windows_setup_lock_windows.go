//go:build windows

package sandbox

import (
	"fmt"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Serializing elevated setup across processes.
//
// WHY THIS EXISTS. Setup is not one write, it is a transaction: it rotates the
// principal's password, stores the secret, read-modify-writes the ACL ledger,
// installs WFP filters and finally writes the marker. Two setups for the same
// workspace running at once interleave those steps, and the interleavings are
// not merely untidy. The account ends up with process B's password while the
// stored secret is process A's, so every later command fails to log on. The
// ledger's read-modify-write loses one process's path set, so ACEs that should
// have been revoked stay on disk with nothing recording them.
//
// Atomic individual writes do not help. Each step being all-or-nothing is
// orthogonal to two processes interleaving BETWEEN steps, which is the actual
// failure. The lock has to span the whole transaction.
//
// Worth recording: the reference implementation this was compared against has
// the identical bug. Its password rotation and secret write hold no lock at
// all, its named mutex guards only an unrelated read-ACL-only mode, and the
// singleflight that looks like a fix is process-local and gives no cross-process
// guarantee. There was nothing to port, so this is built.

// windowsSetupLockTimeout bounds the wait. Setup is interactive and elevated,
// so a minute is generous for a legitimate concurrent run and short enough that
// a stuck holder gets reported rather than hung on. A var rather than a const
// only so a contention test does not have to wait a real minute to prove the
// two setups exclude each other.
var windowsSetupLockTimeout = 60 * time.Second

// windowsSetupLockSDDL grants full control to Administrators and SYSTEM and
// nobody else, with the DACL protected so no inherited ACE widens it.
//
// Load-bearing rather than tidy. A sandbox principal able to open this object
// could hold it and stall every future setup, or create it first and squat the
// name so real setup believes it holds a lock it does not. Only accounts that
// could already run setup may touch it.
//
// A var so a test can exercise the mutex wait path without elevation. Setup
// itself always runs elevated, so unelevated callers cannot even open the
// object under this DACL, which would make the contention test unrunnable
// anywhere it matters. TestSetupLockIsAdministratorsOnly pins the production
// value so loosening it needs an explicit change here.
var windowsSetupLockSDDL = "D:P(A;;GA;;;BA)(A;;GA;;;SY)"

const (
	// windowsWaitTimeout is WAIT_TIMEOUT. x/sys exports WAIT_ABANDONED,
	// WAIT_OBJECT_0 and WAIT_FAILED at this version but not this one.
	windowsWaitTimeout = 0x00000102

	// windowsSetupLockKeyChars keeps the object name short while staying
	// collision-free in practice: the key is already a hash, so a prefix of it
	// distinguishes workspaces.
	windowsSetupLockKeyChars = 32
)

// windowsSandboxSetupLock is a held cross-process setup lock. Release it once.
type windowsSandboxSetupLock struct {
	handle   windows.Handle
	released bool
	// abandoned records that the previous holder died without releasing, which
	// means it stopped somewhere inside the transaction.
	abandoned bool
}

// windowsSandboxSetupLockName derives the object name for a workspace.
//
// Per-workspace rather than machine-wide because every artifact the lock
// protects is per-workspace: this account, this secret, this ledger. A global
// lock would serialize unrelated workspaces for no reason.
//
// Global rather than Local because the protected state is not session-scoped.
// The local account, its LSA logon rights and the WFP filters are machine
// state, so two setups in different logon sessions must still exclude each
// other. Creating a Global object needs SeCreateGlobalPrivilege, which the
// elevated setup helper has and which is the only caller.
func windowsSandboxSetupLockName(workspaceKey string) string {
	key := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return -1
		}
	}, workspaceKey)
	if key == "" {
		key = "default"
	}
	if len(key) > windowsSetupLockKeyChars {
		key = key[:windowsSetupLockKeyChars]
	}
	return `Global\ZeroSandboxSetup-` + key
}

// acquireWindowsSandboxSetupLock blocks until this process owns the setup lock
// for a workspace, or fails with an actionable error.
//
// The OS thread is pinned for the lock's lifetime. Win32 mutex ownership is
// thread-affine, so a goroutine that migrated between the wait and the release
// would call ReleaseMutex from a thread that does not own the object, leaving
// the mutex held until the process exits. That is a deadlock for every later
// setup on the machine, so the pinning is not optional.
func acquireWindowsSandboxSetupLock(workspaceKey string) (*windowsSandboxSetupLock, error) {
	descriptor, err := windows.SecurityDescriptorFromString(windowsSetupLockSDDL)
	if err != nil {
		return nil, fmt.Errorf("build the sandbox setup lock security descriptor: %w", err)
	}
	attributes := windows.SecurityAttributes{SecurityDescriptor: descriptor}
	attributes.Length = uint32(unsafe.Sizeof(attributes))

	name := windowsSandboxSetupLockName(workspaceKey)
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("encode the sandbox setup lock name: %w", err)
	}
	// initialOwner false, then wait explicitly. Asking for ownership at creation
	// would take the lock only when this process created the object, so the
	// create and open paths would differ and only one of them would ever wait.
	handle, err := windows.CreateMutex(&attributes, false, namePtr)
	if handle == 0 {
		return nil, fmt.Errorf("open the sandbox setup lock %s: %w", name, err)
	}
	runtime.LockOSThread()

	lock := &windowsSandboxSetupLock{handle: handle}
	state, waitErr := windows.WaitForSingleObject(handle, uint32(windowsSetupLockTimeout/time.Millisecond))
	switch state {
	case windows.WAIT_OBJECT_0:
		return lock, nil
	case windows.WAIT_ABANDONED:
		// The previous holder died inside the transaction. We own the mutex now.
		// Proceeding is right rather than refusing: setup is idempotent and
		// re-running it is exactly how a half-finished transaction is repaired,
		// whereas refusing would leave the machine stuck in that half state with
		// no way forward.
		lock.abandoned = true
		return lock, nil
	case windowsWaitTimeout:
		lock.releaseThreadAndHandle()
		return nil, fmt.Errorf("another `zero sandbox setup` is already running for this workspace and did not finish within %s; "+
			"wait for it to finish, or look for a stuck elevated setup process before re-running", windowsSetupLockTimeout)
	default:
		lock.releaseThreadAndHandle()
		if waitErr != nil {
			return nil, fmt.Errorf("wait for the sandbox setup lock %s: %w", name, waitErr)
		}
		return nil, fmt.Errorf("wait for the sandbox setup lock %s returned an unexpected state %#x", name, state)
	}
}

// Abandoned reports that the previous holder crashed mid-transaction, so this
// run is repairing a half-finished setup rather than starting a clean one.
func (lock *windowsSandboxSetupLock) Abandoned() bool {
	return lock != nil && lock.abandoned
}

// release drops ownership and unpins the thread. Safe to call twice, so a defer
// and an explicit call cannot double-release.
func (lock *windowsSandboxSetupLock) release() {
	if lock == nil || lock.released {
		return
	}
	lock.released = true
	// Release before closing. Closing a still-held mutex leaves it abandoned for
	// the next waiter, which is recoverable but reports a crash that never
	// happened.
	_ = windows.ReleaseMutex(lock.handle)
	lock.releaseThreadAndHandle()
}

// releaseThreadAndHandle unpins the thread and drops the handle without
// releasing ownership, for the paths that never acquired it.
func (lock *windowsSandboxSetupLock) releaseThreadAndHandle() {
	if lock.handle != 0 {
		_ = windows.CloseHandle(lock.handle)
		lock.handle = 0
	}
	runtime.UnlockOSThread()
}

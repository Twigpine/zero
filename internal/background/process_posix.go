//go:build !windows

package background

import (
	"errors"
	"os/exec"
	"syscall"
	"time"

	"github.com/Gitlawb/zero/internal/execution"
)

// terminationGracePeriod is how long a process has to exit after SIGTERM before
// it is force-killed with SIGKILL. Vars (not consts) so tests can shorten them.
var (
	terminationGracePeriod  = 3 * time.Second
	terminationPollInterval = 50 * time.Millisecond
)

// ConfigureChildProcessGroup puts a child into its own process group so the whole
// group can be signalled as a unit. terminateProcess depends on this: it signals
// the negative PID (the group), so any process the child forks dies with it
// instead of being orphaned. Must be called before cmd.Start.
func ConfigureChildProcessGroup(cmd *exec.Cmd) {
	execution.ConfigureProcessGroup(cmd)
}

// terminateProcess stops a background process. It first asks politely with
// SIGTERM (so processes can flush/clean up), then escalates to SIGKILL if it is
// still alive after terminationGracePeriod — so a process that traps or ignores
// SIGTERM cannot leak. It returns nil once the target is gone.
//
// When pid is its own process-group leader (the invariant ConfigureChildProcessGroup
// establishes for our children, pgid == pid), the whole group is signalled via the
// negative PID, so forked children die with the leader instead of being orphaned.
// If pid is NOT a leader, its group is some other group (possibly OUR OWN), so
// signalling -pgid there could kill unrelated processes — in that case we fall
// back to signalling only the individual PID, which also avoids reporting a false
// success when a non-leader group-signal returns ESRCH.
func terminateProcess(pid int) error {
	return execution.TerminateProcessTree(pid, terminationGracePeriod, terminationPollInterval)
}

// terminateOwnedProcess terminates cmd's process group directly when its launch
// configuration proves it was made a group leader. This avoids fragile Getpgid
// rediscovery after an owned leader exits. Ordinary commands fall back to the
// safe PID/tree path rather than assuming their PID is also a process-group ID.
//
// The bool result is whether the leader had already exited before termination
// was attempted — the POSIX analogue of Windows GetExitCodeProcess returning a
// value other than STILL_ACTIVE. A waitable zombie is already-exited: Wait can
// collect it, but kill(0) still succeeds, and when ps is unavailable
// signalTargetRunning conservatively treats that as "still running".
// TerminateCommand uses the flag to discard the resulting spurious
// SIGKILL-timeout once the reap has independently succeeded. The flag is about
// the leader only; group/tree termination is still invoked so live descendants
// are signalled.
//
// The Setpgid && Pgid == 0 guard only recognizes ConfigureChildProcessGroup's
// own convention. A command made its own session leader via Setsid (as opposed
// to Setpgid) is also its own process-group leader in practice, but takes the
// slower rediscovery path here since Setsid isn't checked. Harmless today
// because TerminateCommand has exactly one caller in this codebase; worth
// covering explicitly if Setsid-configured commands start using this path too.
func terminateOwnedProcess(cmd *exec.Cmd) (bool, error) {
	alreadyExited := leaderWaitableExited(cmd.Process.Pid)
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid && cmd.SysProcAttr.Pgid == 0 {
		return alreadyExited, execution.TerminateProcessGroup(cmd.Process.Pid, terminationGracePeriod, terminationPollInterval)
	}
	return alreadyExited, terminateProcess(cmd.Process.Pid)
}

// terminationTargetGoneAfterReap independently checks the exact signal target
// used for an owned command. In particular, reaping a zombie group leader does
// not make this return true while any descendant still occupies that group.
func terminationTargetGoneAfterReap(cmd *exec.Cmd) bool {
	target := cmd.Process.Pid
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Setpgid && cmd.SysProcAttr.Pgid == 0 {
		target = -target
	}
	return errors.Is(syscall.Kill(target, syscall.Signal(0)), syscall.ESRCH)
}

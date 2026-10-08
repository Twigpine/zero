//go:build !windows

package execution

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ConfigureProcessGroup makes cmd the leader of a process group so lifecycle
// operations cover descendants instead of orphaning them.
func ConfigureProcessGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// KillProcessTree immediately kills pid and, when it is a group leader, its
// descendant process group.
func KillProcessTree(pid int) error {
	target, err := processSignalTarget(pid)
	if err != nil {
		return err
	}
	if err := syscall.Kill(target, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// TerminateProcessTree requests graceful termination, then force-kills the
// process tree after grace. Callers retain their distinct persistence models;
// this function owns only the OS lifecycle primitive.
//
// The signal target is REDISCOVERED here via processSignalTarget(pid), which
// asks the OS for pid's current group. A caller that already knows pid was
// configured as its own group leader at launch (e.g. via ConfigureProcessGroup)
// should use TerminateProcessGroup instead — see its doc comment for why this
// rediscovery is fragile once the leader may have already exited.
func TerminateProcessTree(pid int, grace, poll time.Duration) error {
	target, err := processSignalTarget(pid)
	if err != nil {
		return err
	}
	return terminateTarget(pid, target, grace, poll)
}

// TerminateProcessGroup is like TerminateProcessTree, but for a caller that
// already knows pid is (or was, at launch) its own process-group leader — e.g.
// a command started after ConfigureProcessGroup(cmd), which sets Setpgid so
// cmd.Process.Pid always leads its own group at the moment it is signalled.
//
// TerminateProcessTree instead rediscovers the group via syscall.Getpgid(pid)
// at call time. On Darwin, that lookup can return ESRCH once an unreaped group
// leader has already exited, even though live descendants remain in the group
// it configured — TerminateProcessTree would then silently fall back to
// signalling only the (already dead) leader PID and leave those descendants
// running. Skipping rediscovery in favor of the launch-time invariant avoids
// that: the negative PID is used directly, regardless of whether the leader is
// still alive to be looked up.
func TerminateProcessGroup(pid int, grace, poll time.Duration) error {
	if pid <= 1 {
		return fmt.Errorf("refusing to signal invalid pid %d", pid)
	}
	return terminateTarget(pid, -pid, grace, poll)
}

// terminateTarget signals target (an individual PID or, negative, a process
// group) with SIGTERM then, after grace, SIGKILL. reportPid identifies the
// original process in error messages regardless of which form target takes.
func terminateTarget(reportPid, target int, grace, poll time.Duration) error {
	alive := func() bool { return signalTargetRunning(target) }
	if err := syscall.Kill(target, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	if poll <= 0 {
		poll = 50 * time.Millisecond
	}
	if grace < 0 {
		grace = 0
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !alive() {
			return nil
		}
		time.Sleep(poll)
	}
	if !alive() {
		return nil
	}
	if err := syscall.Kill(target, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline = time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !alive() {
			return nil
		}
		time.Sleep(poll)
	}
	if alive() {
		return fmt.Errorf("process %d did not exit after SIGKILL", reportPid)
	}
	return nil
}

// signalTargetRunning reports whether the signal target still has a process that
// is actually RUNNING, as opposed to one waiting to be reaped.
//
// kill(2) with signal 0 succeeds for a zombie: the PID still exists until its
// parent collects it. That matters because the target here is usually a process
// GROUP, and a terminated leader's child is briefly a zombie before init/launchd
// reaps it. Treating that window as "still alive" made termination sit through
// both grace periods, SIGKILL an already-dead group, and then report
// "did not exit after SIGKILL" for a tree that had in fact stopped — turning a
// successful cleanup into a spurious failure (and a flaky one, since it depends on
// reap timing).
//
// Zombie detection goes through ps, which is available on every Unix we target and
// avoids a /proc dependency Darwin does not have. It is only consulted when the
// cheap kill check says something is still there, so the common path costs nothing
// extra. If ps cannot be run or its output cannot be parsed, the conservative
// pre-existing answer (the kill result) stands.
func signalTargetRunning(target int) bool {
	if syscall.Kill(target, syscall.Signal(0)) != nil {
		return false
	}
	// A negative target is a process GROUP: check whether any member is
	// non-zombie. A positive target is an individual PID that is NOT its own
	// group leader (processSignalTarget's fallback) — its actual PGID differs
	// from its own PID, so matching rows by PGID here would look for a group
	// that doesn't exist and always report "unknown" (conservatively alive).
	// Check its own PID's state instead.
	var running, ok bool
	if target < 0 {
		running, ok = processGroupHasRunningMember(-target)
	} else {
		running, ok = processIsRunning(target)
	}
	if !ok {
		return true
	}
	return running
}

// processGroupHasRunningMember reports whether any member of pgid is in a state
// other than zombie. ok is false when the group's states could not be determined.
func processGroupHasRunningMember(pgid int) (running bool, ok bool) {
	return processTableStateMatches(func(_, rowPgid int) bool { return rowPgid == pgid })
}

// processIsRunning reports whether pid itself is in a state other than zombie.
// ok is false when pid's state could not be determined.
func processIsRunning(pid int) (running bool, ok bool) {
	return processTableStateMatches(func(rowPid, _ int) bool { return rowPid == pid })
}

// processTableStateMatches scans the process table once, via ps, and reports
// whether any row satisfying match is non-zombie, and whether at least one
// matching row was found at all (ok). No rows found means either the process
// (or group) genuinely doesn't exist (a race with exit) or ps's output was
// unparseable/restricted; either way, the caller should not claim knowledge.
func processTableStateMatches(match func(pid, pgid int) bool) (running bool, ok bool) {
	output, err := exec.Command("ps", "-A", "-o", "pid=,pgid=,stat=").Output()
	if err != nil {
		return false, false
	}
	found := false
	for line := range strings.SplitSeq(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		rowPid, errPid := strconv.Atoi(fields[0])
		rowPgid, errPgid := strconv.Atoi(fields[1])
		if errPid != nil || errPgid != nil || !match(rowPid, rowPgid) {
			continue
		}
		found = true
		if !strings.HasPrefix(fields[2], "Z") {
			return true, true
		}
	}
	return false, found
}

func processSignalTarget(pid int) (int, error) {
	if pid <= 1 {
		return 0, fmt.Errorf("refusing to signal invalid pid %d", pid)
	}
	target := pid
	if pgid, err := syscall.Getpgid(pid); err == nil {
		if pgid == pid {
			target = -pid
		}
	} else {
		if errors.Is(err, syscall.ESRCH) {
			// Preserve the individual target; the signal call below treats ESRCH as
			// already gone, which is a successful lifecycle outcome.
			return pid, nil
		}
		// Conservatively retain the individual PID after other lookup failures;
		// guessing and signalling a process group could affect unrelated processes.
	}
	return target, nil
}

//go:build !windows

package execution

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// requireProcessState refuses to let a broken ps pipeline masquerade as passing
// coverage. Every test in this file proves something about zombie detection,
// and every one of those proofs is read out of ps — so if ps cannot report
// state, the tests verify nothing whatever they do next.
//
// The old code answered that with t.Skip, which is indistinguishable from
// success in CI output. This probes BOTH ps pipelines against this test
// process, which is by definition alive and not a zombie, so a working ps must
// report a live state for it. Failing that:
//
//   - In CI (or with ZERO_REQUIRE_PS=1) it is a hard failure. ps is present on
//     every image used today, so its absence means the pipeline regressed and
//     that has to surface immediately rather than as skipped-green.
//   - Elsewhere it stays a skip, so a contributor on an exotic box is not
//     blocked by an environment the project does not claim to support.
//
// This only covers "ps cannot report state at all". Once it passes, a later
// failure to observe an EXPECTED state is a real defect, and the tests below
// treat it as one.
func requireProcessState(t *testing.T) {
	t.Helper()
	self := os.Getpid()
	var reasons []string
	// The production path: ps -A -o pid=,pgid=,stat=, parsed by
	// processTableStateMatches. This is what signalTargetRunning consults.
	if running, ok := processIsRunning(self); !ok || !running {
		reasons = append(reasons, "the process table read used by signalTargetRunning reported no live state for this test process")
	}
	// This file's independent oracle: ps -o stat= -p <pid>. It is deliberately
	// NOT the production parser, so a regression in that parser cannot hide by
	// breaking the test's own precondition check in the same way.
	if zombie, ok := processIsZombie(self); !ok || zombie {
		reasons = append(reasons, "the per-PID state read used by this test file reported no live state for this test process")
	}
	if len(reasons) == 0 {
		return
	}
	reason := "cannot read process state, so ps-based zombie detection is unverifiable here: " + strings.Join(reasons, "; ")
	if requireWorkingPS() {
		t.Fatal(reason + " — failing rather than skipping because CI (or ZERO_REQUIRE_PS) is set, where ps is expected to work")
	}
	t.Skip(reason)
}

// requireWorkingPS reports whether an unusable ps must fail the run instead of
// skipping it. ZERO_REQUIRE_PS lets a developer opt into the strict behavior
// locally to reproduce what CI does.
func requireWorkingPS() bool {
	if strings.TrimSpace(os.Getenv("ZERO_REQUIRE_PS")) != "" {
		return true
	}
	return strings.TrimSpace(os.Getenv("CI")) != ""
}

// awaitZombie blocks until pid is an unreaped zombie. The caller must not have
// waited on the child, so the zombie is a stable state rather than a race: it
// persists until something reaps it. Failing to reach it therefore means the
// child never exited or ps stopped reporting its state, and both are defects
// rather than reasons to skip.
func awaitZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		zombie, ok := processIsZombie(pid)
		if ok && zombie {
			return
		}
		if time.Now().After(deadline) {
			if !ok {
				t.Fatalf("pid %d left the process table without being reaped; this test never waited on it, so its zombie row must persist", pid)
			}
			t.Fatalf("pid %d did not reach the zombie state within 5s", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTerminateProcessTreeTreatsZombieGroupAsExited is the regression test for
// counting unreaped processes as alive: kill(2) with signal 0 succeeds for a
// zombie, so a group whose only remaining member is waiting to be reaped used to
// sit through both grace periods, take a pointless SIGKILL, and then be reported
// as "did not exit after SIGKILL" — a spurious cleanup failure whose timing
// depended on when the reaper ran.
func TestTerminateProcessTreeTreatsZombieGroupAsExited(t *testing.T) {
	requireProcessState(t)

	cmd := exec.Command("sh", "-c", "exit 0")
	ConfigureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	// Deliberately do NOT Wait: the child stays a zombie in its own group, which
	// is exactly the state a terminated tree passes through.
	t.Cleanup(func() { _ = cmd.Wait() })

	awaitZombie(t, pid)

	// Whether a zombie-only group can be signalled is a KERNEL difference, not a
	// ps one. On Linux the zombie still occupies the process table and
	// kill(-pid, 0) succeeds, so signalTargetRunning goes on to consult ps —
	// which is the branch this test was written for. Darwin returns EPERM for
	// the same call, so signalTargetRunning short-circuits on the kill and never
	// reaches the ps check.
	//
	// The OUTCOME is identical and is asserted on every platform below. Only the
	// branch that produces it differs, so record that instead of skipping the
	// whole test (which would drop Darwin's coverage of the outcome) or failing
	// it (which would report a supported platform as broken). Any errno other
	// than EPERM means the group is not in the state this test needs.
	psPathReachable := true
	if err := syscall.Kill(-pid, syscall.Signal(0)); err != nil {
		if !errors.Is(err, syscall.EPERM) {
			t.Fatalf("zombie group %d is unsignalable with an unexpected error (%v); the zombie case is not present, so nothing below would be meaningful", pid, err)
		}
		psPathReachable = false
		t.Logf("kernel refuses to signal zombie-only group %d (EPERM); signalTargetRunning answers from kill(2) rather than ps here, so the ps-based zombie check is asserted directly on platforms that allow the signal", pid)
	}

	// Where the signal IS permitted, pin the ps-based check itself. This is the
	// regression #774 was about, and asserting only signalTargetRunning would
	// let a ps-side regression hide behind the kill result.
	if psPathReachable {
		running, ok := processGroupHasRunningMember(pid)
		if !ok {
			t.Fatalf("process group %d has no rows while its zombie is unreaped; ps stopped reporting the group rather than the group being gone", pid)
		}
		if running {
			t.Fatal("the ps-based group check must not report a zombie-only group as running")
		}
	}

	// Valid on every platform: whichever branch answers, a zombie-only group is
	// not running. On a kernel that refuses the signal this is the whole of the
	// coverage, and it is more than the previous skip provided.
	if signalTargetRunning(-pid) {
		t.Fatal("a group holding only a zombie must not count as running")
	}

	if !psPathReachable {
		// terminateTarget's own kill(2) would hit the same EPERM and be returned
		// as a cleanup error, so this assertion cannot be made where the kernel
		// refuses the signal. That may be a real gap in TerminateProcessTree on
		// such a platform rather than a test limitation — a zombie-only tree is
		// already exited and should terminate cleanly — but confirming and
		// changing that is a production fix, not this test's business.
		t.Log("skipping the TerminateProcessTree assertion: its kill(2) would hit the same EPERM as the probe above")
		return
	}
	if err := TerminateProcessTree(pid, 50*time.Millisecond, 10*time.Millisecond); err != nil {
		t.Fatalf("TerminateProcessTree on an already-exited tree: %v", err)
	}
}

// TestTerminateProcessTreeStopsRunningGroup is the positive control: a group with
// a live member must still be seen as running and then actually stopped, so the
// zombie tolerance above cannot degrade into ignoring real processes.
func TestTerminateProcessTreeStopsRunningGroup(t *testing.T) {
	requireProcessState(t)

	cmd := exec.Command("sh", "-c", "sleep 30")
	ConfigureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Wait() }()

	if !signalTargetRunning(-pid) {
		t.Fatal("a group with a live member must count as running")
	}
	if err := TerminateProcessTree(pid, time.Second, 10*time.Millisecond); err != nil {
		t.Fatalf("TerminateProcessTree: %v", err)
	}
	// ok reports whether ANY row for this group was found. This test never waits
	// on the child, so its zombie row must still be there — an unknown state
	// means ps stopped answering, not that the group is gone. Treating that as a
	// pass is what let this assertion succeed without checking anything.
	running, ok := processGroupHasRunningMember(pid)
	if !ok {
		t.Fatalf("process group %d has no rows after termination; the unreaped child's zombie row must persist, so the group's state is undeterminable rather than clean", pid)
	}
	if running {
		t.Fatal("the group still has a running member after termination")
	}
}

// TestSignalTargetRunningTreatsZombieIndividualPIDAsExited is the regression
// test for jatmn's second #774 finding: signalTargetRunning's zombie check
// matched the individual-PID fallback target (a process that is NOT its own
// group leader — processSignalTarget's positive-PID case) against process-table
// rows by PGID. That target's actual PGID differs from its own PID by
// definition (that's exactly why processSignalTarget fell back to the
// individual PID instead of a negative group target), so no row ever matched,
// "unknown" resulted every time, and signalTargetRunning conservatively (and
// incorrectly) treated a zombie individual-PID target as still running.
func TestSignalTargetRunningTreatsZombieIndividualPIDAsExited(t *testing.T) {
	requireProcessState(t)

	cmd := exec.Command("sh", "-c", "exit 0")
	// Deliberately do NOT call ConfigureProcessGroup: the child inherits this
	// test process's group, so it is not its own leader and processSignalTarget
	// falls back to the individual (positive) PID.
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Wait() })

	awaitZombie(t, pid)

	target, err := processSignalTarget(pid)
	if err != nil {
		t.Fatalf("processSignalTarget: %v", err)
	}
	// The child inherited this process's group, so its PGID is this process's
	// PID and can never equal its own PID. A negative target here would mean
	// processSignalTarget misread the group, which is the very thing the
	// individual-PID fallback exists to handle.
	if target != pid {
		t.Fatalf("processSignalTarget(%d) = %d, want the individual PID: the child inherited this process's group, so it is not its own leader", pid, target)
	}
	if signalTargetRunning(target) {
		t.Fatal("a zombie individual-PID target must not count as running")
	}
}

// processIsZombie reports a process's zombie state via ps. It is deliberately a
// separate, per-PID ps invocation rather than a reuse of the production
// process-table parser: these tests assert what that parser concludes, so
// borrowing it to establish their own preconditions would let one regression
// silence both sides at once.
//
// ok is false when no state could be read, which after requireProcessState has
// passed means the process left the table rather than that ps is broken.
func processIsZombie(pid int) (zombie bool, ok bool) {
	output, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false, false
	}
	state := strings.TrimLeft(string(output), " \n\t")
	if state == "" {
		return false, false
	}
	return state[0] == 'Z', true
}

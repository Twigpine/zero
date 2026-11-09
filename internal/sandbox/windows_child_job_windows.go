//go:build windows

package sandbox

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// THE SUSPENDED CHILD NEEDS AN OWNER THAT OUTLIVES A KILLED HELPER.
//
// The helper publishes the launch fact and only then resumes, which is what
// makes "no report" mean "nothing ran" rather than a race (see
// publishThenResume). Creating the child suspended is what buys that, and it
// also creates a state nothing else in the helper covers: between
// CreateProcessAsUser and ResumeThread the child exists, holds the inherited
// stdin/stdout/stderr handles, and cannot execute a single instruction.
//
// Hook and plugin commands reach this through Engine.CommandContext, which uses
// the default exec.CommandContext cancellation: on timeout or cancel the helper
// is killed outright. It cannot run its deferred cleanup, cannot reach
// terminateSuspendedWindowsChild, and cannot resume. Windows does not terminate
// a child because its parent died, so the suspended process survives holding the
// pipe write ends, and a parent still reading them waits on a process that will
// never write and never exit.
//
// A job object with kill-on-close is the only mechanism that survives the
// helper being terminated rather than unwound: the kernel enforces it. The job
// is created and the helper joins it BEFORE any child exists, so every child
// created afterwards inherits membership at creation and there is no window in
// which a child is alive and unowned. TerminateProcess on the helper closes the
// last handle, and the kernel kills the job.
//
// Deliberately not solved with exec.Cmd.WaitDelay in the parent: that releases
// the caller while the orphan keeps running and keeps the pipes, which trades a
// visible hang for an invisible leak. It is a reasonable backstop, not the fix.
//
// Reported by @jatmn.
// windowsJoinChildKillJob is the seam the command runner calls. Production value
// only; a test substitutes it to prove the runner takes ownership on EVERY path,
// including the ones that refuse the command a few lines later.
var windowsJoinChildKillJob = joinWindowsChildKillJob

func joinWindowsChildKillJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)),
		uint32(unsafe.Sizeof(limits)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	// CurrentProcess() is a pseudo-handle, which AssignProcessToJobObject
	// accepts; the membership it creates is what descendants inherit.
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

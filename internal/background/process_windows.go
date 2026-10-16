//go:build windows

package background

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/Gitlawb/zero/internal/execution"
	"golang.org/x/sys/windows"
)

// stillActiveExitCode is the documented GetExitCodeProcess value for a process
// that has not terminated. x/sys/windows does not expose Windows' STILL_ACTIVE
// constant.
const stillActiveExitCode uint32 = 259

var terminateProcessForTest = terminateProcess

// ConfigureChildProcessGroup is a no-op on Windows: process-tree termination is
// delegated to execution.TerminateProcessTree, so no launch-time process-group
// setup is required (the POSIX build sets Setpgid here instead).
func ConfigureChildProcessGroup(cmd *exec.Cmd) { execution.ConfigureProcessGroup(cmd) }

func terminateProcess(pid int) error {
	return execution.TerminateProcessTree(pid, 0, 0)
}

// terminateOwnedProcess asks taskkill /T to terminate the tree rooted at cmd,
// with KillProcessTree's direct Process.Kill fallback if taskkill fails. taskkill
// can discover descendants only while the root PID still exists; unlike a POSIX
// process group, a Windows tree has no independently addressable identity after
// its root exits.
func terminateOwnedProcess(cmd *exec.Cmd) (bool, error) {
	var (
		alreadyExited bool
		terminateErr  error
	)
	err := cmd.Process.WithHandle(func(handle uintptr) {
		var exitCode uint32
		if windows.GetExitCodeProcess(windows.Handle(handle), &exitCode) == nil {
			alreadyExited = exitCode != stillActiveExitCode
		}
		// Keep the exact process identity pinned while the PID-based taskkill
		// operation runs, preventing the PID from being recycled underneath it.
		terminateErr = terminateProcessForTest(cmd.Process.Pid)
	})
	if err != nil {
		// WithHandle refuses only a process that has already been waited or
		// released, so it has finished. Say so in the form exec.Cmd understands:
		// a Cancel error wrapping os.ErrProcessDone is "nothing to cancel", where
		// the unexported "already released" error it would otherwise carry made
		// Wait report a spurious cancel failure. A dead root has no tree left to
		// find either way.
		return true, fmt.Errorf("pin process %d for termination: %w: %w", cmd.Process.Pid, os.ErrProcessDone, err)
	}
	return alreadyExited, terminateErr
}

// Windows has no persistent process-group identity to query after a dead root
// is reaped. Preserve TerminateCommand's existing, documented dead-root
// behavior; the stronger independent target check is available on POSIX.
func terminationTargetGoneAfterReap(*exec.Cmd) bool { return true }

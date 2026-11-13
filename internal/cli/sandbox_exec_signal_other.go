//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// signaledExitStatus reports the conventional shell status for a child that was
// terminated by a signal, which exec.ExitError cannot represent.
//
// ExitCode() returns -1 for a signaled child, and the top level hands that to
// os.Exit, which truncates it to 255. A child that takes SIGTERM is then
// indistinguishable from one that exited 255 of its own accord, which breaks the
// documented status contract for a command whose whole job is to report the
// child's own status faithfully. 128+signal is what a shell reports and what a
// harness comparing statuses expects.
func signaledExitStatus(state *os.ProcessState) (int, bool) {
	if state == nil {
		return 0, false
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return 0, false
	}
	return 128 + int(status.Signal()), true
}

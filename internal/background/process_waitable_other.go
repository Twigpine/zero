//go:build !windows && !linux && !darwin

package background

// leaderWaitableExited cannot positively identify a waitable-exited leader on
// this platform without ps. Returning false keeps TerminateCommand's existing
// conservative behaviour: a termination error is not discarded after reap.
func leaderWaitableExited(pid int) bool {
	return false
}

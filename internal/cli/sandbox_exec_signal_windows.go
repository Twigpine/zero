//go:build windows

package cli

import "os"

// signaledExitStatus has no Windows counterpart: a process there ends with an
// exit code, and there is no signal to fold into one. Windows keeps the exit
// code exec.ExitError already reports.
func signaledExitStatus(*os.ProcessState) (int, bool) {
	return 0, false
}

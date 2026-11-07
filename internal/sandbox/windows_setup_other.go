//go:build !windows

package sandbox

import (
	"fmt"
	"io"
)

func runWindowsSandboxSetup(config WindowsSandboxSetupConfig, stderr io.Writer) int {
	fmt.Fprintln(stderr, WindowsSandboxSetupName+": Windows sandbox setup is only available on Windows")
	return 1
}

// windowsCurrentUserSID has no meaning off Windows. Returning empty keeps
// BuildWindowsSandboxSetupArgs from emitting a --caller-sid nobody could
// interpret, which is also what a Windows caller does when its own token cannot
// be read.
func windowsCurrentUserSID() string { return "" }

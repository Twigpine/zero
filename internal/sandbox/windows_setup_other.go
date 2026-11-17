//go:build !windows

package sandbox

import (
	"fmt"
	"io"
)

func runWindowsSandboxSetup(config WindowsSandboxSetupConfig, stderr io.Writer) int {
	fmt.Fprintln(stderr, WindowsSandboxSetupName+": Windows sandbox setup is only available on Windows")
	return WindowsSandboxSetupUnpublishedExitCode
}

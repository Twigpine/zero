//go:build !linux

package sandbox

import (
	"fmt"
	"io"
)

func RunLinuxSandboxHelper(args []string, stderr io.Writer) int {
	if len(args) > 0 && args[0] == LinuxSandboxHelperSubcommand {
		args = args[1:]
	}
	if _, err := ParseLinuxSandboxHelperArgs(args); err != nil {
		fmt.Fprintln(stderr, LinuxSandboxHelperName+": "+err.Error())
		return 2
	}
	fmt.Fprintln(stderr, LinuxSandboxHelperName+": Linux sandbox helper is only available on Linux")
	return 125
}

//go:build linux

package sandbox

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

var (
	applyUnixSocketBlockFilter           = ApplyUnixSocketBlock
	applyLinuxIsolatedNetworkGuardFilter = ApplyLinuxIsolatedNetworkGuard
)

func RunLinuxSandboxHelper(args []string, stderr io.Writer) int {
	selfExec := false
	if len(args) > 0 && args[0] == LinuxSandboxHelperSubcommand {
		selfExec = true
		args = args[1:]
	}
	config, err := ParseLinuxSandboxHelperArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, LinuxSandboxHelperName+": "+err.Error())
		return 2
	}
	if config.ApplySeccompThenExec {
		return runLinuxSandboxInnerStage(config, stderr)
	}
	if config.UseLandlock {
		return runLinuxSandboxLandlockStage(config, stderr)
	}
	helperPath, err := os.Executable()
	if err != nil {
		fmt.Fprintln(stderr, LinuxSandboxHelperName+": resolve helper path: "+err.Error())
		return 125
	}
	bwrapPlan, err := buildLinuxSandboxBwrapPlan(LinuxSandboxBwrapOptions{
		Config:     config,
		HelperPath: helperPath,
		SelfExec:   selfExec,
	})
	if err != nil {
		fmt.Fprintln(stderr, LinuxSandboxHelperName+": "+err.Error())
		return 125
	}
	bwrapPath, err := findBubblewrapExecutable()
	if err != nil {
		fmt.Fprintln(stderr, LinuxSandboxHelperName+": bubblewrap is not available: "+err.Error())
		return 125
	}
	if len(bwrapPlan.ProtectedCreateTargets) > 0 {
		return runLinuxSandboxWithProtectedCreateMonitor(bwrapPath, bwrapPlan, config.PolicyReportPath, stderr)
	}
	if err := syscall.Exec(bwrapPath, append([]string{"bwrap"}, bwrapPlan.Args...), os.Environ()); err != nil {
		fmt.Fprintln(stderr, LinuxSandboxHelperName+": exec bubblewrap: "+err.Error())
		return 126
	}
	return 126
}

func findBubblewrapExecutable() (string, error) {
	if path, err := exec.LookPath("bwrap"); err == nil && path != "" {
		return path, nil
	}
	for _, candidate := range []string{"/usr/bin/bwrap", "/bin/bwrap"} {
		if executableRegularFile(candidate) {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

func runLinuxSandboxInnerStage(config LinuxSandboxHelperConfig, stderr io.Writer) int {
	if config.UseLandlock {
		fmt.Fprintln(stderr, LinuxSandboxHelperName+": inner seccomp stage is incompatible with Landlock mode")
		return 2
	}
	// The outer bubblewrap stage already enforces NetworkDeny with a private
	// network namespace. Do not install the broad seccomp network filter here:
	// it would also block AF_INET/AF_INET6 sockets inside that namespace and
	// break isolated localhost test servers. The namespace has loopback only,
	// so local bind/connect works while external egress remains unreachable.
	if shouldUnshareLinuxNetwork(config.PermissionProfile.Network) {
		if err := applyLinuxIsolatedNetworkGuardFilter(); err != nil {
			fmt.Fprintln(stderr, LinuxSandboxHelperName+": apply isolated network guard: "+err.Error())
			return 125
		}
	}
	if config.BlockUnixSockets {
		if err := applyUnixSocketBlockFilter(); err != nil {
			fmt.Fprintln(stderr, LinuxSandboxHelperName+": warning: "+err.Error()+"; running without the Unix-socket filter")
		}
	}
	binary, err := exec.LookPath(config.Command[0])
	if err != nil {
		fmt.Fprintln(stderr, LinuxSandboxHelperName+": "+err.Error())
		return 127
	}
	if err := syscall.Exec(binary, config.Command, os.Environ()); err != nil {
		fmt.Fprintln(stderr, LinuxSandboxHelperName+": exec command: "+err.Error())
		return 126
	}
	return 126
}

func runLinuxSandboxLandlockStage(config LinuxSandboxHelperConfig, stderr io.Writer) int {
	if err := ApplyLandlockFilesystemProfile(config.PermissionProfile, config.CommandCWD); err != nil {
		fmt.Fprintln(stderr, LinuxSandboxHelperName+": apply Landlock: "+err.Error())
		return 125
	}
	binary, err := exec.LookPath(config.Command[0])
	if err != nil {
		fmt.Fprintln(stderr, LinuxSandboxHelperName+": "+err.Error())
		return 127
	}
	if err := syscall.Exec(binary, config.Command, linuxHelperSandboxEnvironment(config.PermissionProfile, os.Environ())); err != nil {
		fmt.Fprintln(stderr, LinuxSandboxHelperName+": exec command: "+err.Error())
		return 126
	}
	return 126
}

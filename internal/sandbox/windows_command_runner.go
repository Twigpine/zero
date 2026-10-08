package sandbox

import (
	"fmt"
	"io"
)

func RunWindowsSandboxCommandRunner(args []string, stderr io.Writer) int {
	config, err := ParseWindowsSandboxCommandArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
		return 2
	}
	// Reject unsupported DenyRead profiles before minting persistent capability
	// SID state under SandboxHome (defense in depth: runWindowsSandboxCommand
	// also checks, but only after LoadOrCreateWindowsCapabilitySIDs).
	if err := windowsDenyReadRestrictedTokenUnsupported(config); err != nil {
		fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
		return 1
	}
	if _, err := LoadOrCreateWindowsCapabilitySIDs(config.SandboxHome); err != nil {
		fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
		return 1
	}
	return runWindowsSandboxCommand(config, stderr)
}

// windowsDenyReadRestrictedTokenUnsupported reports that Windows restricted-
// token sandboxing (elevated restricted-token or unelevated) cannot run
// profiles with DenyRead until access-time confinement exists. Both runner
// levels build the same fully restricted narrow-SID token when DenyRead is
// set: without Users/AuthUsers it cannot load ordinary system executables;
// adding those groups reopens write grants outside WriteRoots. Prefer a clear
// rejection over a silent launch failure. Do not recommend the other tier as a
// workaround: the limitation is the token mechanism, not elevation.
func windowsDenyReadRestrictedTokenUnsupported(config WindowsSandboxCommandConfig) error {
	switch config.SandboxLevel {
	case WindowsSandboxLevelRestrictedToken, WindowsSandboxLevelUnelevated:
	default:
		return nil
	}
	return windowsDenyReadRestrictedTokenUnsupportedProfile(config.PermissionProfile)
}

// windowsDenyReadRestrictedTokenUnsupportedProfile is the level-agnostic check
// used by the manager, command-plan builder, setup, and runner so DenyRead is
// rejected before any restricted-token path can provision or launch.
func windowsDenyReadRestrictedTokenUnsupportedProfile(profile PermissionProfile) error {
	if len(profile.FileSystem.DenyRead) == 0 {
		return nil
	}
	return fmt.Errorf(
		"DenyRead is not supported with the Windows restricted-token sandbox "+
			"(elevated or unelevated): without Users/Authenticated Users in the "+
			"restricting SID set, ordinary system binaries under Program Files and "+
			"Windows cannot load, and adding those groups would admit their existing "+
			"write grants outside WriteRoots. "+
			"Remove DenyRead from this configuration to use the sandbox on Windows. "+
			"Configured DenyRead path count: %d",
		len(profile.FileSystem.DenyRead),
	)
}

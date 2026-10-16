//go:build windows

package sandbox

import (
	"fmt"
	"io"
)

func runWindowsSandboxCommand(config WindowsSandboxCommandConfig, stderr io.Writer) int {
	// Fully restricted DenyRead tokens cannot load ordinary Users-granted
	// system binaries without SID broadening; broadening is permanently off
	// because it admits write grants outside WriteRoots. Reject on both the
	// elevated and unelevated restricted-token tiers before setup or launch
	// until access-time confinement exists (PR #640).
	if err := windowsDenyReadRestrictedTokenUnsupported(config); err != nil {
		fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
		return 1
	}
	switch config.SandboxLevel {
	case WindowsSandboxLevelRestrictedToken:
		if err := ValidateWindowsSandboxSetupMarker(WindowsSandboxSetupConfigFromCommand(config)); err != nil {
			fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
			return 1
		}
	case WindowsSandboxLevelUnelevated:
		if err := ensureWindowsUnelevatedSetup(config); err != nil {
			fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
			return 1
		}
	default:
		fmt.Fprintf(stderr, "%s: unsupported Windows sandbox level %q\n", WindowsSandboxCommandRunnerName, config.SandboxLevel)
		return 1
	}
	if err := ValidateWindowsNetworkPolicy(config.PermissionProfile.Network); err != nil {
		fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
		return 1
	}
	capabilitySIDs, err := WindowsCapabilitySIDsForConfig(config)
	if err != nil {
		fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
		return 1
	}
	offlineSID, err := WindowsOfflineMarkerSID(config.SandboxHome)
	if err != nil {
		fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
		return 1
	}
	// Compose the restricting-SID set: both modes keep the write-capability SIDs
	// (workspace write-jail); deny additionally carries the offline-marker SID
	// that the persistent WFP block filter matches — so a deny command has no
	// network while an approved allow command reaches it, both write-jailed.
	//
	// KNOWN LIMITATION: an approved online command reaches the network, but HTTPS
	// via Windows Schannel (e.g. a Schannel-backed curl.exe) fails inside this
	// restricted token with SEC_E_NO_CREDENTIALS — Schannel can't acquire its
	// per-user TLS credential under a WRITE_RESTRICTED/LUA token. This is a
	// fundamental restricted-token vs Schannel incompatibility (the standard
	// mitigation is to run TLS in a broker process, not the sandboxed one) and
	// has no clean in-token fix. Workarounds: the degraded path (no restricted
	// token) or the in-process web_fetch tool.
	//
	// KNOWN LIMITATION: MSYS2/Cygwin binaries (Git for Windows bash.exe,
	// sh.exe, and the usr\bin coreutils) cannot initialize under this token at
	// all, whether invoked directly or spawned internally by an otherwise
	// native command (git hooks, git/gh credential helpers). The MSYS runtime
	// secures its signal pipe and shared-memory sections with explicit DACLs
	// granting only the user, Administrators, and SYSTEM (msys2-runtime
	// sigproc.cc sigproc_init -> sec_user_nih -> __sec_user), and a
	// WRITE_RESTRICTED write check must ALSO match one of the token's
	// restricted SIDs (logon SID, Everyone, capability SIDs). None of the
	// granted SIDs can be added to the restricted list without collapsing the
	// write jail (each has write access nearly everywhere), so MSYS startup
	// dies with "couldn't create signal pipe" or "CreateFileMapping <SID>.1",
	// Win32 error 5, and exit status 0xC0000142. The System32 WSL bash
	// launcher fails equivalently (the restricted token cannot connect to the
	// WSL service: Bash/Service/CreateInstance/E_ACCESSDENIED). Like Schannel,
	// this has no in-token fix; preflight blocking and output hints live in
	// internal/tools/shell_runtime.go.
	tokenSIDs := windowsRuntimeTokenSIDs(capabilitySIDs, offlineSID, config.PermissionProfile.Network.Mode)
	// WRITE_RESTRICTED keeps reads unrestricted so sandboxed commands can load
	// Users-granted executables. It is only used when DenyRead is empty (#612:
	// WRITE_RESTRICTED skips restricted-SID deny ACEs for reads). Non-empty
	// DenyRead is rejected above for both runner levels rather than launching a
	// fully restricted narrow-SID token that cannot execute normal tools.
	writeRestricted := len(config.PermissionProfile.FileSystem.DenyRead) == 0
	token, err := createWindowsRestrictedTokenForCapabilitySIDs(tokenSIDs, writeRestricted)
	if err != nil {
		fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
		return 1
	}
	defer token.Close()
	exitCode, err := runWindowsCommandAsUser(token, config)
	if err != nil {
		fmt.Fprintln(stderr, WindowsSandboxCommandRunnerName+": "+err.Error())
		return 1
	}
	return exitCode
}

// ensureWindowsUnelevatedSetup applies the workspace ACL plan from the current
// (non-elevated) process so the write-restricted token has somewhere its
// capability SIDs are granted. DACL edits on user-owned workspace and temp
// roots need no Administrator rights; the WFP network filters DO, so this tier
// provisions no network enforcement — the offline-marker SID composed into the
// token stays inert until an elevated `zero sandbox setup` installs the block
// filters. Applied plans are recorded by hash so repeat commands skip the
// re-apply; like the elevated setup, grants are left in place (the rollback is
// deliberately discarded) because they only name synthetic capability SIDs
// that no other token carries.
func ensureWindowsUnelevatedSetup(config WindowsSandboxCommandConfig) error {
	applied, plan, err := buildWindowsUnelevatedAppliedPlan(config)
	if err != nil {
		return err
	}
	marker, err := loadWindowsUnelevatedSetupMarker(config.SandboxHome)
	if err != nil {
		return err
	}
	if marker.contains(applied) {
		return nil
	}
	if _, err := applyWindowsACLPlan(plan); err != nil {
		return fmt.Errorf("apply unelevated workspace ACLs: %w — the workspace may be on a filesystem the current user does not own; "+
			"run `zero sandbox setup` from an elevated (Administrator) terminal, or re-run with `--sandbox forbid` to skip OS sandboxing", err)
	}
	return recordWindowsUnelevatedAppliedPlan(config.SandboxHome, applied)
}

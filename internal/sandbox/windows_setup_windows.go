//go:build windows

package sandbox

import (
	"fmt"
	"io"

	"golang.org/x/sys/windows"
)

func runWindowsSandboxSetup(config WindowsSandboxSetupConfig, stderr io.Writer) int {
	// Do not provision DenyRead ACLs for a token mode that cannot launch normal
	// tools with DenyRead under the narrow restricting-SID set (PR #640).
	if err := windowsDenyReadRestrictedTokenUnsupportedProfile(config.PermissionProfile); err != nil {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		return 1
	}
	// Applying the WFP network filters and workspace ACLs requires Administrator
	// rights; without them WFP fails deep inside with a raw ACCESS_DENIED (0x5).
	// Check up front and return an actionable message instead.
	if !windowsProcessIsElevated() {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": Administrator rights are required. Re-run `zero sandbox setup` from an elevated (Run as administrator) terminal.")
		return 1
	}
	plan, err := BuildWindowsACLPlan(config.commandConfig())
	if err != nil {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		return 1
	}
	// Always provision the mode-INDEPENDENT infrastructure: the outbound block
	// filters scoped to the offline-marker SID. Runtime gates network per command
	// by whether the token carries that SID, so one setup serves both modes.
	networkPlan, err := BuildWindowsNetworkInfraPlan(config.commandConfig())
	if err != nil {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		return 1
	}
	rollback, err := applyWindowsACLPlan(plan)
	if err != nil {
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		return 1
	}
	if err := applyWindowsNetworkPlan(networkPlan); err != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			fmt.Fprintf(stderr, "%s: %v; rollback failed: %v\n", WindowsSandboxSetupName, err, rollbackErr)
			return 1
		}
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		return 1
	}
	if _, err := WriteWindowsSandboxSetupMarker(config); err != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			fmt.Fprintf(stderr, "%s: %v; rollback failed: %v\n", WindowsSandboxSetupName, err, rollbackErr)
			return 1
		}
		fmt.Fprintln(stderr, WindowsSandboxSetupName+": "+err.Error())
		return 1
	}
	return 0
}

// windowsProcessIsElevated reports whether the current process runs with an
// elevated (Administrator) token. On any error obtaining the token it returns
// true so the setup proceeds and surfaces the real WFP/ACL error rather than a
// false "needs admin" claim.
func windowsProcessIsElevated() bool {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return true
	}
	defer token.Close()
	return token.IsElevated()
}

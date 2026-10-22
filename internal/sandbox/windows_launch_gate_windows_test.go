//go:build windows

package sandbox

import (
	"bytes"
	"strings"
	"testing"
)

// THE GATE HAS TO BE ON THE PATH THAT LAUNCHES, NOT ONLY IN A FUNCTION.
//
// windows_elevated_grant_attest_test.go proves ValidateWindowsSandboxLaunchGrants
// answers correctly. It calls it directly, so it stays green if the call in
// runWindowsSandboxCommand is deleted, which is exactly how a check that reads
// correct becomes a check that never runs. This drives the runner.
func TestTheRestrictedTokenTierRefusesBeforeCreatingAToken(t *testing.T) {
	home := t.TempDir()
	config := WindowsSandboxCommandConfig{
		SandboxHome:    home,
		CommandCWD:     `C:\workspace`,
		WorkspaceRoots: []string{`C:\workspace`},
		SandboxLevel:   WindowsSandboxLevelRestrictedToken,
		Command:        []string{"cmd.exe", "/c", "echo hi"},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: `C:\workspace`}},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
	}
	if _, err := WriteWindowsSandboxSetupMarker(WindowsSandboxSetupConfigFromCommand(config)); err != nil {
		t.Fatalf("WriteWindowsSandboxSetupMarker: %v", err)
	}

	previous := windowsACLPlanApplied
	t.Cleanup(func() { windowsACLPlanApplied = previous })

	// With the grants intact the tier gets PAST both attestations. It fails later,
	// on this machine, for reasons that have nothing to do with the gate, so the
	// assertion is only that the refusal below is not what stopped it.
	windowsACLPlanApplied = func(WindowsACLPlan) bool { return true }
	var healthy bytes.Buffer
	runWindowsSandboxCommand(config, &healthy)
	if strings.Contains(healthy.String(), "no longer carry the permissions") {
		t.Fatalf("SETUP INVALID: the gate refused with the grants intact: %s", healthy.String())
	}

	windowsACLPlanApplied = func(WindowsACLPlan) bool { return false }
	var stderr bytes.Buffer
	code := runWindowsSandboxCommand(config, &stderr)
	if code == 0 {
		t.Fatal("the runner launched into a sandbox whose directories no longer carry their grants")
	}
	if !strings.Contains(stderr.String(), "no longer carry the permissions") {
		t.Errorf("the runner did not refuse for the missing grant; it stopped for something else: %s", stderr.String())
	}
}

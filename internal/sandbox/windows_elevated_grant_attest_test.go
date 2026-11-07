package sandbox

import (
	"strings"
	"testing"
)

// THE STAMP IS NOT THE GRANT.
//
// The elevated tier attested its runtime root with the marker comparisons and a
// stamp file. Those prove that setup's intent matches this command's, and that
// the directory was not removed and recreated under the same pathname. An
// ordinary file answers the second by existing, and it survives an ACL edit
// untouched, so an `icacls /reset`, an inheritance change on a parent, or a
// security product rewriting the DACL all leave a valid stamp over a runtime
// root the WRITE_RESTRICTED child cannot write. The marker agrees, the stamp
// agrees, and the first write into TMP or a package cache returns ACCESS_DENIED
// with nothing having said why.
//
// This is the function runWindowsSandboxCommand calls for the restricted-token
// tier, beside the marker validation. The unelevated tier answers the same
// question by reading the descriptors and re-applying; this one cannot repeat an
// elevated provisioning, so it refuses and names the action.
func TestTheLaunchGateRefusesAnUnappliedGrant(t *testing.T) {
	config := WindowsSandboxSetupConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     `C:\workspace`,
		WorkspaceRoots: []string{`C:\workspace`},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: `C:\workspace`}},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
	}

	previous := windowsACLPlanApplied
	t.Cleanup(func() { windowsACLPlanApplied = previous })

	windowsACLPlanApplied = func(WindowsACLPlan) bool { return true }
	if err := ValidateWindowsSandboxLaunchGrants(config); err != nil {
		t.Fatalf("SETUP INVALID: the gate refuses even with the grants intact: %v", err)
	}

	windowsACLPlanApplied = func(WindowsACLPlan) bool { return false }
	err := ValidateWindowsSandboxLaunchGrants(config)
	if err == nil {
		t.Fatal("a runtime root that no longer carries its grants passed the launch gate, so the command starts into a sandbox it cannot write")
	}
	// Actionable, and about the right thing: an operator told "permission roots
	// changed" goes looking at their policy for a problem that is not there.
	for _, want := range []string{"permissions", "zero sandbox setup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "out of date") {
		t.Errorf("the refusal reads as a policy edit, which sends the operator to the wrong place: %v", err)
	}
}

// And the marker comparison stays a marker comparison. Folding the grant check
// into it would have made every consumer of the marker, including `zero doctor`,
// depend on real security descriptors, which is a different question from the
// one that function's name asks.
func TestTheMarkerValidationDoesNotReadDescriptors(t *testing.T) {
	config := WindowsSandboxSetupConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     `C:\workspace`,
		WorkspaceRoots: []string{`C:\workspace`},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: `C:\workspace`}},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
	}
	if _, err := WriteWindowsSandboxSetupMarker(config); err != nil {
		t.Fatalf("WriteWindowsSandboxSetupMarker: %v", err)
	}
	previous := windowsACLPlanApplied
	t.Cleanup(func() { windowsACLPlanApplied = previous })
	windowsACLPlanApplied = func(WindowsACLPlan) bool { return false }

	if err := ValidateWindowsSandboxSetupMarker(config); err != nil {
		t.Errorf("the marker comparison consulted the descriptors: %v", err)
	}
}

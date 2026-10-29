package sandbox

import "testing"

// THE FINGERPRINT MUST DESCRIBE THE PLAN THAT ACTUALLY GETS APPLIED.
//
// windowsPrincipalPlanFingerprint builds a principal plan to hash into the setup
// marker, and it was the one of three buildWindowsPrincipalACLPlan call sites
// that did not pass DenyWrite. Apply and teardown both did. So a change to the
// policy's deny-write paths moved the plan those two build and left the marker's
// hash unchanged, which is the marker failing at the one job it has.
//
// It was not a live hole, because the capability ACLPlanHash covers the same
// paths and moves the marker anyway. That is exactly what would have kept it
// invisible until somebody changed the capability plan's shape.
func TestThePrincipalFingerprintCoversDenyWrite(t *testing.T) {
	workspace := t.TempDir()
	base := WindowsSandboxSetupConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     workspace,
		WorkspaceRoots: []string{workspace},
		PrincipalOptIn: true,
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: workspace}},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
	}

	without, err := windowsPrincipalPlanFingerprint(base)
	if err != nil {
		t.Fatalf("windowsPrincipalPlanFingerprint: %v", err)
	}

	withDeny := base
	withDeny.PermissionProfile.FileSystem.DenyWrite = []string{workspace + string('/') + "protected"}
	changed, err := windowsPrincipalPlanFingerprint(withDeny)
	if err != nil {
		t.Fatalf("windowsPrincipalPlanFingerprint: %v", err)
	}

	if without == changed {
		t.Error("adding a deny-write path did not move the principal fingerprint, so the marker cannot tell that the applied plan changed")
	}
}

package doctor

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/sandbox"
)

// A VALID MARKER IS NOT A LIVE PRINCIPAL. Marker validation compares serialized
// plans and hashes and names no account, so it stays valid after the selected
// account is deleted, its secret is removed, or the offline account is dropped
// out of ZeroSandboxOffline. In each of those states the runtime falls back to
// the restricted-token path with no read confinement, or fails outright.
//
// Doctor is the surface an operator checks precisely when they are unsure, so
// claiming read confinement it has not verified is worse than reporting less.
// This pins the narrower claim: setup is current, the role is named because it
// is derived rather than assumed, and nothing asserts the account is usable.
func TestPrincipalCheckDoesNotClaimAnUnverifiedPrincipalIsActive(t *testing.T) {
	sandboxHome := t.TempDir()
	workspaceRoot := t.TempDir()
	t.Setenv("ZERO_WINDOWS_SANDBOX_HOME", sandboxHome)
	t.Setenv("ZERO_WINDOWS_SANDBOX_IDENTITY", "1")

	sandboxConfig := config.SandboxConfig{}
	scope, err := sandbox.NewScope(workspaceRoot, sandboxConfig.AdditionalWriteRoots)
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	profile := sandbox.PermissionProfileFromPolicy(workspaceRoot, doctorSandboxPolicy(sandboxConfig), scope)

	// Mirrors what windowsSandboxSetupCheck derives, so the marker it writes is
	// the one the check will validate against. Nothing here provisions an
	// account, which is the whole point: the marker is valid and the principal
	// does not exist.
	if _, err := sandbox.WriteWindowsSandboxSetupMarker(sandbox.WindowsSandboxSetupConfig{
		SandboxHome:       sandboxHome,
		CommandCWD:        workspaceRoot,
		WorkspaceRoots:    []string{workspaceRoot},
		PermissionProfile: sandbox.WindowsSandboxProfileWithRuntimeRoots(profile, []string{workspaceRoot}),
		PrincipalOptIn:    true,
	}); err != nil {
		t.Fatalf("WriteWindowsSandboxSetupMarker: %v", err)
	}

	result := windowsSandboxSetupCheck("windows", sandbox.Backend{Name: sandbox.BackendWindowsRestrictedToken}, workspaceRoot, sandboxConfig)
	if result == nil {
		t.Fatal("expected a principal check once the marker validates and the opt-in is set")
	}
	if result.ID != "sandbox.principal" {
		t.Fatalf("check = %q (%s): the marker did not validate, so this test is not exercising the principal branch", result.ID, result.Message)
	}

	lowered := strings.ToLower(result.Message)
	for _, claim := range []string{"is active", "commands run as"} {
		if strings.Contains(lowered, claim) {
			t.Errorf("doctor asserts %q about a principal it never checked: %q", claim, result.Message)
		}
	}
	if active, ok := result.Details["active"]; ok {
		t.Errorf("details carry active=%v, which nothing here verified", active)
	}

	// The role is still worth reporting: it is derived from the network mode by
	// the shared rule rather than assumed, and it is what an operator debugging
	// network behaviour needs.
	if role, ok := result.Details["role"]; !ok || strings.TrimSpace(role.(string)) == "" {
		t.Errorf("the selected role should still be reported: %#v", result.Details)
	}
}

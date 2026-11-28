//go:build windows

package sandbox

import "testing"

func containsRole(roles []windowsSandboxRole, want windowsSandboxRole) bool {
	for _, role := range roles {
		if role == want {
			return true
		}
	}
	return false
}

// THE LEGACY ROLE BELONGS TO EXACTLY ONE OF THE TWO INVENTORIES.
//
// Both directions are real failures that have nearly happened here.
//
// In the retirable list: teardown retires legacy, and the is-it-still-installed
// guard used to ask only the live pair. An upgraded machine still holding the
// untagged pre-split account was therefore reported clean, and the opted-out
// marker claimed a teardown that had not happened.
//
// Out of the live list: provisioning legacy would create that pre-split account
// fresh on every setup of a machine that had already been upgraded past it.
func TestTheLegacyRoleIsRetirableButNeverProvisioned(t *testing.T) {
	if containsRole(windowsSandboxLiveRoles, windowsSandboxRoleLegacy) {
		t.Error("the legacy role is in the provisioning inventory; setup would recreate the pre-split account on every run")
	}
	if !containsRole(windowsSandboxRetirableRoles, windowsSandboxRoleLegacy) {
		t.Error("the legacy role is not in the retirable inventory; teardown and the installed check would both miss the pre-split account")
	}
	for _, role := range windowsSandboxLiveRoles {
		if !containsRole(windowsSandboxRetirableRoles, role) {
			t.Errorf("live role %q is not retirable, so teardown would leave it behind", role)
		}
	}
}

// And the installed check consults the retirable inventory, so a machine still
// holding only the legacy account is not reported clean.
func TestPrincipalIsInstalledSeesTheLegacyAccount(t *testing.T) {
	prev := lookupWindowsSandboxIdentityFn
	t.Cleanup(func() { lookupWindowsSandboxIdentityFn = prev })

	var asked []windowsSandboxRole
	lookupWindowsSandboxIdentityFn = func(_ string, role windowsSandboxRole) (windowsSandboxIdentity, error) {
		asked = append(asked, role)
		if role == windowsSandboxRoleLegacy {
			return windowsSandboxIdentity{}, nil
		}
		return windowsSandboxIdentity{}, errWindowsSandboxIdentityUnavailable
	}

	workspace := t.TempDir()
	config := WindowsSandboxCommandConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     workspace,
		WorkspaceRoots: []string{workspace},
	}
	if !windowsSandboxPrincipalIsInstalled(config) {
		t.Errorf("a machine still holding the legacy account was reported clean; roles asked: %v", asked)
	}
	if !containsRole(asked, windowsSandboxRoleLegacy) {
		t.Errorf("the legacy role was never looked up: %v", asked)
	}
}

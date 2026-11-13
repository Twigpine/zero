package sandbox

import (
	"errors"
	"sort"
	"strings"
	"testing"
)

// The predecessor of this branch provisioned ONE account per workspace, named
// "zero-sbx-<key>" with no role tag. Splitting the roles changed both names, so
// on an upgraded machine neither role resolves to the account that is actually
// installed. Every other path on this branch then reports the principal as never
// provisioned, and the untagged account keeps its secret, its logon rights, its
// ACEs and its ledger with nothing left that looks for them.
//
// windowsSandboxRoleLegacy exists to reproduce that name exactly so retirement
// can still find it. If its tag ever stops being empty, this stops matching what
// the predecessor wrote and the residue becomes unreachable again.
func TestLegacyRoleReproducesTheUntaggedPredecessorName(t *testing.T) {
	// Short enough that the 20-character account-name limit does not truncate,
	// so the comparison is against the spelling itself rather than the cap.
	const key = "abc123"

	legacy := windowsSandboxUserName(key, windowsSandboxRoleLegacy)
	if want := windowsSandboxUserPrefix + key; legacy != want {
		t.Fatalf("legacy account name = %q, want the pre-split spelling %q", legacy, want)
	}
	// And the cap still applies to it exactly as it did before the split, since
	// the predecessor truncated the same way.
	long := windowsSandboxUserName("averylongworkspacekeyindeed", windowsSandboxRoleLegacy)
	if len(long) > windowsSandboxUserNameMax {
		t.Errorf("legacy name %q is %d characters, over the %d-character limit", long, len(long), windowsSandboxUserNameMax)
	}
	if tag := windowsSandboxRoleLegacy.roleTag(); tag != "" {
		t.Errorf("legacy role tag = %q, want empty; a tag here would stop matching the installed account", tag)
	}

	// It must not collide with either live role, or retiring the predecessor
	// would delete an account this branch is still using.
	offline := windowsSandboxUserName(key, windowsSandboxRoleOffline)
	online := windowsSandboxUserName(key, windowsSandboxRoleOnline)
	for _, live := range []string{offline, online} {
		if strings.EqualFold(legacy, live) {
			t.Errorf("legacy name %q collides with a live role name %q", legacy, live)
		}
	}
	if strings.EqualFold(offline, online) {
		t.Fatalf("the two live roles derived the same account name %q", offline)
	}
}

// THE LEGACY ROLE IS RETIRED, NEVER PROVISIONED. It derives the untagged name
// on purpose, so provisioning it would create the very account this branch
// replaced, on every setup, forever. Adding it to the retirement list and the
// provisioning list are one character apart in the source and the difference is
// invisible at a glance, so it is asserted rather than left to review.
func TestSetupNeverProvisionsTheLegacyPrincipal(t *testing.T) {
	previous := provisionWindowsSandboxPrincipalForSetupFn
	t.Cleanup(func() { provisionWindowsSandboxPrincipalForSetupFn = previous })

	var provisioned []windowsSandboxRole
	provisionWindowsSandboxPrincipalForSetupFn = func(_ WindowsSandboxCommandConfig, role windowsSandboxRole) (windowsSandboxIdentity, bool, error) {
		provisioned = append(provisioned, role)
		return windowsSandboxIdentity{}, false, errStopProvisioningForTest
	}

	// The error stops setup at the first role; what matters is which roles it
	// was willing to ask for, and legacy must never be among them.
	_, _ = setupWindowsSandboxPrincipal(WindowsSandboxCommandConfig{SandboxHome: t.TempDir()})

	for _, role := range provisioned {
		if role == windowsSandboxRoleLegacy {
			t.Fatalf("setup tried to provision the legacy role, which would recreate the pre-split account: %v", provisioned)
		}
	}
}

var errStopProvisioningForTest = errors.New("stop provisioning for test")

// Opting out or re-running setup has to retire the predecessor as well as both
// live roles. Retiring only the two this branch knows about is what leaves a
// fully provisioned account on a machine whose marker says it has none.
func TestSetupRetirementCoversTheLegacyPrincipal(t *testing.T) {
	previous := removeWindowsSandboxPrincipalForSetupFn
	t.Cleanup(func() { removeWindowsSandboxPrincipalForSetupFn = previous })

	var retired []string
	removeWindowsSandboxPrincipalForSetupFn = func(_ WindowsSandboxCommandConfig, role windowsSandboxRole) error {
		retired = append(retired, string(role))
		return nil
	}

	if err := removeWindowsSandboxPrincipalsForSetup(WindowsSandboxCommandConfig{SandboxHome: t.TempDir()}); err != nil {
		t.Fatalf("removeWindowsSandboxPrincipalsForSetup: %v", err)
	}

	sort.Strings(retired)
	want := []string{"legacy", "offline", "online"}
	if len(retired) != len(want) {
		t.Fatalf("retired %v, want all of %v; a role left out keeps its account and everything it owns", retired, want)
	}
	for index := range want {
		if retired[index] != want[index] {
			t.Fatalf("retired %v, want %v", retired, want)
		}
	}
}

//go:build windows

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// The finding this record exists for, end to end and against real DACLs: setup
// grants two roots, the user removes one from their policy, setup runs again,
// and the removed root must not still name the principal.
//
// Both halves go through the PRODUCTION applyWindowsPrincipalACLs rather than
// the revoke helper, because the mechanism already worked — what did not was the
// call site's idea of which paths to revoke. It could only name the paths of the
// plan it was about to apply, and the dropped root is by definition absent from
// that plan, so the re-setup marker validation forces preserved the very ACE it
// was supposed to clear.
func TestReSetupRevokesARootTheNarrowedPolicyDropped(t *testing.T) {
	home := t.TempDir()
	username := "zerosbxregression"
	root := t.TempDir()
	kept := filepath.Join(root, "kept")
	dropped := filepath.Join(root, "dropped")
	for _, dir := range []string{kept, dropped} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// Guests: a real, resolvable trustee this process is not a member of, so the
	// ACEs below are observable without affecting the test process.
	principal := "S-1-5-32-546"

	wide := FileSystemPolicy{
		Kind:       FileSystemRestricted,
		WriteRoots: []WritableRoot{{Root: kept}, {Root: dropped}},
	}
	if _, err := applyWindowsPrincipalACLs(home, username, principal, wide, wide.WriteRoots); err != nil {
		t.Fatalf("first setup: %v", err)
	}
	if !hasACEForTrustee(t, dropped, principal) {
		t.Fatal("precondition: the first setup should have granted the root that is about to leave the policy")
	}

	// The user narrows their policy and re-runs setup. Nothing in this call
	// mentions the dropped root; only the record does.
	narrow := FileSystemPolicy{
		Kind:       FileSystemRestricted,
		WriteRoots: []WritableRoot{{Root: kept}},
	}
	if _, err := applyWindowsPrincipalACLs(home, username, principal, narrow, narrow.WriteRoots); err != nil {
		t.Fatalf("re-setup: %v", err)
	}

	if hasACEForTrustee(t, dropped, principal) {
		t.Error("the principal kept its grant on a root the narrowed policy removed")
	}
	if !hasACEForTrustee(t, kept, principal) {
		t.Error("revoking over the recorded paths also dropped the grant the narrowed policy still wants")
	}
	// And the record narrows with the policy, or it would accumulate every root
	// the workspace has ever had.
	recorded, ok := readWindowsPrincipalACLLedger(home, username)
	if !ok {
		t.Fatal("no record survived the re-setup")
	}
	if containsPathFold(recorded, dropped) {
		t.Errorf("the record still names %q after the policy dropped it", dropped)
	}
}

// The record is written BEFORE any DACL changes and as the union, not after and
// as the new set. A crash between the grant and a post-hoc write would otherwise
// leave a record missing paths this run granted, stranding them at the next
// policy change — the same bug, one interruption away.
func TestPrincipalACLRecordCoversTheGrantBeforeItIsMade(t *testing.T) {
	prevApply := applyWindowsACLPlanFn
	t.Cleanup(func() { applyWindowsACLPlanFn = prevApply })

	home := t.TempDir()
	username := "zerosbxtwophase"
	workspace := t.TempDir()
	stale := filepath.Join(t.TempDir(), "left-the-policy")
	if err := writeWindowsPrincipalACLLedger(home, username, []string{stale}); err != nil {
		t.Fatalf("seed the record: %v", err)
	}

	var atGrant []string
	applyWindowsACLPlanFn = func(plan WindowsACLPlan) (func() error, error) {
		if len(plan.Entries) > 0 && plan.Entries[0].Action != windowsACLRevoke {
			atGrant, _ = readWindowsPrincipalACLLedger(home, username)
		}
		return func() error { return nil }, nil
	}

	filesystem := FileSystemPolicy{
		Kind:       FileSystemRestricted,
		WriteRoots: []WritableRoot{{Root: workspace}},
	}
	if _, err := applyWindowsPrincipalACLs(home, username, "S-1-5-32-546", filesystem, filesystem.WriteRoots); err != nil {
		t.Fatalf("applyWindowsPrincipalACLs: %v", err)
	}

	if !containsPathFold(atGrant, stale) || !containsPathFold(atGrant, workspace) {
		t.Errorf("record at grant time = %v, want the union of the recorded and newly granted paths", atGrant)
	}
	after, ok := readWindowsPrincipalACLLedger(home, username)
	if !ok {
		t.Fatal("no record after a successful setup")
	}
	if containsPathFold(after, stale) {
		t.Errorf("record after = %v, want it narrowed to what is granted now", after)
	}
	if !containsPathFold(after, workspace) {
		t.Errorf("record after = %v, want the granted workspace root", after)
	}
}

// A principal from an earlier setup whose grants were never recorded is the one
// case where the prior set is not empty but unenumerable, and carrying on with
// it is the fail-open: revocation would then cover only what the new plan
// happens to name. Retiring the account instead makes every ACE that cannot be
// found name a SID Windows never reuses.
//
// The record is per role because the principals are: seeding one role's record
// must retire the other and only the other. Retiring both would destroy an
// account there is nothing wrong with, and retiring neither is the fail-open.
func TestSetupRetiresAPrincipalWithNoRecordOfItsGrants(t *testing.T) {
	for name, testCase := range map[string]struct {
		seedRoles     []windowsSandboxRole
		identityFound bool
		wantRetired   []windowsSandboxRole
	}{
		"no record and principals from an earlier setup": {
			identityFound: true,
			wantRetired:   []windowsSandboxRole{windowsSandboxRoleOffline, windowsSandboxRoleOnline},
		},
		"no record and nothing provisioned": {identityFound: false},
		"only the offline role has a record": {
			seedRoles:     []windowsSandboxRole{windowsSandboxRoleOffline},
			identityFound: true,
			wantRetired:   []windowsSandboxRole{windowsSandboxRoleOnline},
		},
		"both roles have a record to reconcile against": {
			seedRoles:     []windowsSandboxRole{windowsSandboxRoleOffline, windowsSandboxRoleOnline},
			identityFound: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			config := stubWindowsPrincipalSetup(t)
			key := windowsSandboxPrincipalKey(config)
			for _, role := range testCase.seedRoles {
				username := windowsSandboxUserName(key, role)
				if err := writeWindowsPrincipalACLLedger(config.SandboxHome, username, []string{`C:\ws\recorded`}); err != nil {
					t.Fatalf("seed the %s record: %v", role, err)
				}
			}

			lookupWindowsSandboxIdentityFn = func(_ string, role windowsSandboxRole) (windowsSandboxIdentity, error) {
				if testCase.identityFound {
					return windowsSandboxIdentity{Username: windowsSandboxUserName(key, role), SID: guestsSID(t)}, nil
				}
				return windowsSandboxIdentity{}, errWindowsSandboxIdentityUnavailable
			}
			var retired []windowsSandboxRole
			removeWindowsSandboxPrincipalForSetupFn = func(_ WindowsSandboxCommandConfig, role windowsSandboxRole) error {
				retired = append(retired, role)
				return nil
			}

			if _, err := setupWindowsSandboxPrincipal(config); err != nil {
				t.Fatalf("setupWindowsSandboxPrincipal: %v", err)
			}
			if !sameRoles(retired, testCase.wantRetired) {
				t.Errorf("retired %v, want %v", retired, testCase.wantRetired)
			}
		})
	}
}

func sameRoles(got []windowsSandboxRole, want []windowsSandboxRole) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

// A failure to retire has to fail the setup. Reporting success would leave the
// operator believing the sandbox is provisioned while a principal whose grants
// nobody can enumerate is still holding them.
func TestSetupFailsWhenAnUnrecordedPrincipalCannotBeRetired(t *testing.T) {
	config := stubWindowsPrincipalSetup(t)
	lookupWindowsSandboxIdentityFn = func(string, windowsSandboxRole) (windowsSandboxIdentity, error) {
		return windowsSandboxIdentity{Username: "zerosbx", SID: guestsSID(t)}, nil
	}
	removeWindowsSandboxPrincipalForSetupFn = func(WindowsSandboxCommandConfig, windowsSandboxRole) error {
		return errors.New("account is in use")
	}
	if _, err := setupWindowsSandboxPrincipal(config); err == nil {
		t.Fatal("setup reported success after failing to retire a principal it cannot reconcile")
	}
}

// Teardown had the same blind spot as setup: it computed the paths to revoke
// from the CURRENT policy, so retiring a principal cleared every ACE except the
// one on the root that had left the policy — and then deleted the account, which
// left that ACE naming a SID nothing could resolve to clean up later.
func TestTeardownRevokesRecordedPathsTheCurrentPolicyNoLongerNames(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	dropped := filepath.Join(t.TempDir(), "left-the-policy")
	config := WindowsSandboxCommandConfig{
		SandboxHome:    home,
		CommandCWD:     workspace,
		WorkspaceRoots: []string{workspace},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: workspace}},
			},
		},
	}
	role := windowsSandboxRoleOffline
	username := windowsSandboxUserName(windowsSandboxPrincipalKey(config), role)
	if err := writeWindowsPrincipalACLLedger(home, username, []string{dropped}); err != nil {
		t.Fatalf("seed the record: %v", err)
	}

	paths, err := windowsPrincipalRevocationPaths(config, "S-1-5-32-546", role)
	if err != nil {
		t.Fatalf("windowsPrincipalRevocationPaths: %v", err)
	}

	// The other role's revocation must not pick this up: separate accounts,
	// separate records, and retiring one must leave the other's ACEs alone.
	other, err := windowsPrincipalRevocationPaths(config, "S-1-5-32-546", windowsSandboxRoleOnline)
	if err != nil {
		t.Fatalf("windowsPrincipalRevocationPaths(online): %v", err)
	}
	if containsPathFold(other, dropped) {
		t.Errorf("the online role would revoke %q, which only the offline role's record names", dropped)
	}
	if !containsPathFold(paths, dropped) {
		t.Errorf("teardown would revoke %v, missing the recorded root %q the policy no longer names", paths, dropped)
	}
	if !containsPathFold(paths, workspace) {
		t.Errorf("teardown would revoke %v, missing the workspace the current policy grants", paths)
	}
}

// stubWindowsPrincipalSetup replaces every elevated call
// setupWindowsSandboxPrincipal makes, so the decision under test is reachable on
// a machine with nothing provisioned.
func stubWindowsPrincipalSetup(t *testing.T) WindowsSandboxCommandConfig {
	t.Helper()
	home := t.TempDir()
	workspace := t.TempDir()

	prevLookup := lookupWindowsSandboxIdentityFn
	prevRemove := removeWindowsSandboxPrincipalForSetupFn
	prevProvision := provisionWindowsSandboxIdentityFn
	prevGrant := grantWindowsSandboxLogonRightsFn
	prevReset := resetWindowsSandboxUserPasswordFn
	prevSecret := writeWindowsSandboxSecretFn
	prevApply := applyWindowsACLPlanFn
	prevCache := sandboxUserCacheDir
	t.Cleanup(func() {
		lookupWindowsSandboxIdentityFn = prevLookup
		removeWindowsSandboxPrincipalForSetupFn = prevRemove
		provisionWindowsSandboxIdentityFn = prevProvision
		grantWindowsSandboxLogonRightsFn = prevGrant
		resetWindowsSandboxUserPasswordFn = prevReset
		writeWindowsSandboxSecretFn = prevSecret
		applyWindowsACLPlanFn = prevApply
		sandboxUserCacheDir = prevCache
	})

	provisionWindowsSandboxIdentityFn = func(key string, role windowsSandboxRole) (windowsSandboxIdentity, string, bool, error) {
		return windowsSandboxIdentity{Username: windowsSandboxUserName(key, role), SID: guestsSID(t)}, "pw", true, nil
	}
	grantWindowsSandboxLogonRightsFn = func(*windows.SID) error { return nil }
	resetWindowsSandboxUserPasswordFn = func(string, string) error { return nil }
	writeWindowsSandboxSecretFn = func(string, string) error { return nil }
	applyWindowsACLPlanFn = func(WindowsACLPlan) (func() error, error) {
		return func() error { return nil }, nil
	}
	cache := t.TempDir()
	sandboxUserCacheDir = func() (string, error) { return cache, nil }
	// And the temp-derived fallback, or setup and preparation reach for the real
	// one whenever the cache candidate is not selected.
	fixtureTemp := filepath.Join(t.TempDir(), "temp")
	if err := os.MkdirAll(fixtureTemp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMP", fixtureTemp)
	t.Setenv("TEMP", fixtureTemp)

	return WindowsSandboxCommandConfig{
		SandboxHome:    home,
		CommandCWD:     workspace,
		WorkspaceRoots: []string{workspace},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: workspace}},
			},
		},
	}
}

func guestsSID(t *testing.T) *windows.SID {
	t.Helper()
	sid, err := windows.StringToSid("S-1-5-32-546")
	if err != nil {
		t.Fatalf("StringToSid: %v", err)
	}
	return sid
}

// containsPathFold compares the way the ACL plans do, which is the only
// comparison that means anything here.
//
// Comparing the raw spellings passed CI on nothing and failed on Windows: the
// plan builder runs every root through normalizeProfilePath, whose EvalSymlinks
// expands the 8.3 short name GitHub's runners hand out for TEMP, so the record
// holds C:\Users\runneradmin\... while t.TempDir() returned C:\Users\RUNNER~1\...
// and EqualFold called two spellings of one directory different paths. A
// developer whose TEMP has no short name never sees it.
//
// Production is self-consistent — both the recorded and the newly planned paths
// go through the same normalization — so this was only ever the test being
// naive about what "same path" means on Windows.
func containsPathFold(paths []string, want string) bool {
	wanted := windowsCapabilityPathKey(normalizeProfilePath(want))
	if wanted == "" {
		return false
	}
	for _, path := range paths {
		if windowsCapabilityPathKey(normalizeProfilePath(path)) == wanted {
			return true
		}
	}
	return false
}

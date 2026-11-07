//go:build windows

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// Policy deny-write has to reach the principal plan.
//
// The capability plan has always emitted these. The principal plan denied write
// only on protected metadata and read-only subpaths inside write roots, so once
// the runner used a principal token a policy deny sitting anywhere else was not
// enforced at the OS layer at all, and a shell child could write where the
// restricted-token backend would have stopped it.
func TestPrincipalACLPlanCarriesPolicyDenyWrite(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	denied := filepath.Join(root, "protected", "keep-out")

	plan, err := buildWindowsPrincipalACLPlan(windowsPrincipalACLInput{
		PrincipalSID: "S-1-5-21-0-0-0-1001",
		WriteRoots:   []WritableRoot{{Root: root}},
		DenyWrite:    []string{denied},
	})
	if err != nil {
		t.Fatalf("buildWindowsPrincipalACLPlan: %v", err)
	}

	entry, ok := findPrincipalACLEntry(plan, WindowsACLDenyWrite, denied)
	if !ok {
		t.Fatalf("no deny-write ACE for the policy path; plan = %+v", plan.Entries)
	}
	// Materialized for the same reason the capability plan does it: the applier
	// skips targets that do not exist, so a deny on a path created after setup
	// would never be written.
	if !entry.Materialize {
		t.Error("policy deny-write ACE is not materialized, so it is skipped when the path does not exist yet")
	}
}

// Deny-read has to be materialized too, which it was not.
func TestPrincipalACLPlanMaterializesDenyRead(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	secret := filepath.Join(t.TempDir(), "elsewhere", "creds")

	plan, err := buildWindowsPrincipalACLPlan(windowsPrincipalACLInput{
		PrincipalSID: "S-1-5-21-0-0-0-1001",
		WriteRoots:   []WritableRoot{{Root: root}},
		DenyRead:     []string{secret},
	})
	if err != nil {
		t.Fatalf("buildWindowsPrincipalACLPlan: %v", err)
	}
	entry, ok := findPrincipalACLEntry(plan, WindowsACLDenyRead, secret)
	if !ok {
		t.Fatalf("no deny-read ACE emitted; plan = %+v", plan.Entries)
	}
	if !entry.Materialize {
		t.Fatal("deny-read ACE is not materialized, so a path created after setup never gets one")
	}
}

// Deny entries must still precede the grants they carve out of, which is what
// makes them win under Windows DACL evaluation. Adding deny-write to the plan is
// only safe if it did not disturb that ordering.
func TestPrincipalACLPlanKeepsDeniesBeforeGrants(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	plan, err := buildWindowsPrincipalACLPlan(windowsPrincipalACLInput{
		PrincipalSID: "S-1-5-21-0-0-0-1001",
		WriteRoots:   []WritableRoot{{Root: root}},
		DenyWrite:    []string{filepath.Join(root, "nope")},
		DenyRead:     []string{filepath.Join(root, "secret")},
	})
	if err != nil {
		t.Fatalf("buildWindowsPrincipalACLPlan: %v", err)
	}
	firstGrant := -1
	for i, entry := range plan.Entries {
		switch entry.Action {
		case WindowsACLAllowWrite, WindowsACLAllowRead:
			if firstGrant == -1 {
				firstGrant = i
			}
		case WindowsACLDenyRead, WindowsACLDenyWrite:
			if firstGrant != -1 {
				t.Fatalf("deny entry at %d follows a grant at %d; the grant would win", i, firstGrant)
			}
		}
	}
}

func findPrincipalACLEntry(plan WindowsACLPlan, action WindowsACLAction, path string) (WindowsACLEntry, bool) {
	want := normalizeProfilePath(path)
	for _, entry := range plan.Entries {
		if entry.Action == action && entry.Path == want {
			return entry, true
		}
	}
	return WindowsACLEntry{}, false
}

// An adopted account must not have its password rotated during provisioning.
//
// Rotating there left every later step running against an account whose password
// had already been replaced with nothing on disk holding it. Any failure in
// between stranded a working principal: the rollback correctly declined to
// delete an account it had not created, so what remained was a live account
// authenticated by a password no longer stored anywhere, and the command path
// read the absent secret as "not provisioned" and quietly fell back to the
// weaker backend.
func TestProvisionWindowsSandboxIdentityDefersPasswordRotation(t *testing.T) {
	stubWindowsProvisioning(t, true, nil, nil, nil)

	rotated := false
	previous := resetWindowsSandboxUserPasswordFn
	t.Cleanup(func() { resetWindowsSandboxUserPasswordFn = previous })
	resetWindowsSandboxUserPasswordFn = func(string, string) error {
		rotated = true
		return nil
	}

	if _, _, _, err := provisionWindowsSandboxIdentity("workspacekey", windowsSandboxRoleOffline); err != nil {
		t.Fatalf("provisioning an adopted account: %v", err)
	}
	if rotated {
		t.Fatal("provisioning rotated the password; the window this closes lasts until the secret is committed")
	}
}

// The ownership comment carries the full workspace key, so two workspaces whose
// digests collide in the 11 characters the account name can hold are refused
// rather than silently sharing one account, one secret and one ACL identity.
func TestWindowsSandboxUserCommentDistinguishesWorkspaces(t *testing.T) {
	first := windowsSandboxUserCommentFor("aaaaaaaaaaaabbbbbbbb")
	second := windowsSandboxUserCommentFor("aaaaaaaaaaaacccccccc")
	if first == second {
		t.Fatal("two workspaces produced the same ownership comment, so a name collision would be adopted")
	}
	if !strings.HasPrefix(first, windowsSandboxUserComment) {
		t.Fatalf("comment %q lost the marker prefix that identifies it as ours", first)
	}
	// The names DO collide, which is the whole reason the comment has to carry
	// the key. If this stops being true the test is no longer covering anything.
	if windowsSandboxUserName("aaaaaaaaaaaabbbbbbbb", windowsSandboxRoleOffline) != windowsSandboxUserName("aaaaaaaaaaaacccccccc", windowsSandboxRoleOffline) {
		t.Skip("account names no longer collide for these keys; revisit what this test is for")
	}
}

// Rollback must not strip an adopted principal's logon rights.
//
// revokeWindowsSandboxLogonRights passes AllRights, which drops every right the
// account holds and deletes its LSA object. On an account this run created that
// is a rollback; on one it adopted it destroys the SeBatchLogonRight and
// deny-logon rights an earlier setup established, which is the working
// principal this path exists to preserve.
func TestSetupRollbackRevokesRightsOnlyForCreatedPrincipals(t *testing.T) {
	for name, testCase := range map[string]struct {
		existed     bool
		wantRevoked bool
	}{
		"adopted principal": {existed: true, wantRevoked: false},
		"created principal": {existed: false, wantRevoked: true},
	} {
		t.Run(name, func(t *testing.T) {
			stubWindowsProvisioning(t, testCase.existed, nil, nil, nil)

			revoked := false
			prevGrant, prevRevoke := grantWindowsSandboxLogonRightsFn, revokeWindowsSandboxLogonRightsFn
			t.Cleanup(func() {
				grantWindowsSandboxLogonRightsFn, revokeWindowsSandboxLogonRightsFn = prevGrant, prevRevoke
			})
			// Fail the grant so the undo path runs with rights already attempted,
			// which is the state that used to revoke unconditionally.
			grantWindowsSandboxLogonRightsFn = func(*windows.SID) error {
				return errors.New("LSA grant refused by policy")
			}
			revokeWindowsSandboxLogonRightsFn = func(*windows.SID) error {
				revoked = true
				return nil
			}

			config := WindowsSandboxCommandConfig{
				SandboxHome:    t.TempDir(),
				WorkspaceRoots: []string{`C:\ws`},
			}
			if _, _, err := provisionWindowsSandboxPrincipalForSetup(config, windowsSandboxRoleOffline); err == nil {
				t.Fatal("provisioning reported success despite an injected grant failure")
			}
			if revoked != testCase.wantRevoked {
				if testCase.wantRevoked {
					t.Fatal("rights were not revoked for an account this run created, leaving LSA entries keyed to a SID about to be deleted")
				}
				t.Fatal("rights were revoked for an adopted account; AllRights drops its pre-existing rights and deletes the LSA object")
			}
			// And the account itself: requested through the fake for the created
			// case, never for an adopted one. This fixture fakes creation, so the
			// only acceptable deletion is one the fake sees.
			removals := len(stubbedWindowsIdentityRemovals)
			if testCase.existed && removals != 0 {
				t.Fatalf("rollback deleted an adopted account: %v", stubbedWindowsIdentityRemovals)
			}
			if !testCase.existed && removals != 1 {
				t.Fatalf("rollback of a created account requested %d removals through the fake, want exactly one: %v", removals, stubbedWindowsIdentityRemovals)
			}
		})
	}
}

// A secret the current user cannot read is unavailability, not breakage.
//
// The secret's DACL names whoever ran setup. An operator who elevated with a
// separate administrative account, via runas or an over-the-shoulder UAC
// prompt, leaves a secret their ordinary account cannot open. Treating that as a
// hard error made every sandboxed command fail on a machine that was merely set
// up by a different admin; it belongs in the same fail-soft path as a missing
// secret, so the warning fires and the restricted token takes over.
func TestReadWindowsSandboxSecretTreatsPermissionDeniedAsUnavailable(t *testing.T) {
	previous := readWindowsSandboxSecretFile
	t.Cleanup(func() { readWindowsSandboxSecretFile = previous })
	readWindowsSandboxSecretFile = func(string) ([]byte, error) {
		return nil, &os.PathError{Op: "open", Path: "secret", Err: windows.ERROR_ACCESS_DENIED}
	}

	if _, err := readWindowsSandboxSecret(`C:\anything.secret`); err != errWindowsSandboxIdentityUnavailable {
		t.Fatalf("permission-denied read returned %v, want errWindowsSandboxIdentityUnavailable so the command falls back", err)
	}
}

// A workspace must not bind to another workspace's principal.
//
// The account name carries only 11 characters of the workspace digest, so two
// workspaces can derive the same name. Provisioning refuses that case by
// checking the full key in the account comment, but the command path resolved
// the name straight to a SID. The workspace that lost the race would have failed
// setup and then quietly run as the other one's principal, using its secret and
// its ACL identity.
func TestLookupWindowsSandboxIdentityRejectsForeignWorkspace(t *testing.T) {
	previous := windowsSandboxUserIsManagedFn
	t.Cleanup(func() { windowsSandboxUserIsManagedFn = previous })

	prevSID := resolveWindowsSandboxSIDFn
	t.Cleanup(func() { resolveWindowsSandboxSIDFn = prevSID })
	// The account resolves; whether it BELONGS to this workspace is the question.
	resolveWindowsSandboxSIDFn = func(string) (*windows.SID, error) {
		return windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	}

	var askedKey string
	windowsSandboxUserIsManagedFn = func(_ string, workspaceKey string) (bool, error) {
		askedKey = workspaceKey
		return false, nil
	}
	_, err := lookupWindowsSandboxIdentity("workspacekey", windowsSandboxRoleOffline)
	if err == nil {
		t.Fatal("lookup accepted an account belonging to another workspace")
	}
	if errors.Is(err, errWindowsSandboxIdentityUnavailable) {
		t.Fatal("a foreign account must not read as unprovisioned; that would silently fall back instead of reporting the conflict")
	}
	if askedKey != "" && askedKey != "workspacekey" {
		t.Fatalf("ownership was checked against %q, want the caller's workspace key", askedKey)
	}
}

// An account that does not exist must stay the unavailable sentinel rather than
// becoming a collision error, since that is the ordinary not-set-up state.
func TestLookupWindowsSandboxIdentityAbsentAccountIsUnavailable(t *testing.T) {
	previous := windowsSandboxUserIsManagedFn
	t.Cleanup(func() { windowsSandboxUserIsManagedFn = previous })
	windowsSandboxUserIsManagedFn = func(string, string) (bool, error) {
		t.Fatal("ownership must not be consulted for an account that does not resolve")
		return false, nil
	}
	if _, err := lookupWindowsSandboxIdentity("nosuchworkspacekey", windowsSandboxRoleOffline); !errors.Is(err, errWindowsSandboxIdentityUnavailable) {
		t.Fatalf("absent account returned %v, want errWindowsSandboxIdentityUnavailable", err)
	}
}

// The git control-plane carveouts must be materialized.
//
// gitMetadataWriteCarveouts supplies .git/config and .git/hooks as
// ReadOnlySubpaths of the workspace. On a workspace where git has not run yet
// they do not exist when setup applies the plan, and applyWindowsACLPlan skips
// an absent target, so the deny ACEs were never written. Once git created those
// paths the principal still held inherited write access and could install a
// hook or rewrite credential.helper.
func TestPrincipalACLPlanMaterializesReadOnlySubpaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	carveout := filepath.Join(root, ".git", "config")

	plan, err := buildWindowsPrincipalACLPlan(windowsPrincipalACLInput{
		PrincipalSID: "S-1-5-21-0-0-0-1001",
		WriteRoots:   []WritableRoot{{Root: root, ReadOnlySubpaths: []string{carveout}}},
	})
	if err != nil {
		t.Fatalf("buildWindowsPrincipalACLPlan: %v", err)
	}
	entry, ok := findPrincipalACLEntry(plan, WindowsACLDenyWrite, carveout)
	if !ok {
		t.Fatalf("no deny-write ACE for the git carveout; plan = %+v", plan.Entries)
	}
	if !entry.Materialize {
		t.Fatal("git carveout deny-write is not materialized, so it is skipped on a workspace where .git does not exist yet")
	}
}

// Setup must grant the principal the same runtime root that commands write to.
//
// permissionProfileWithRuntime appends this root to WriteRoots on every command
// and redirects HOME, GOCACHE and npm_config_cache into it, but it lives under
// the user cache rather than the workspace, so the profile setup sees never
// contains it. A principal is a separate account with no rights there, so
// without a grant every npm install or go build fails on a cache write.
//
// The assertion that matters is that the two derivations agree. If they drift,
// setup writes the ACE on one directory while commands use another, and the
// symptom is a bare ACCESS_DENIED with nothing pointing at the sandbox.
func TestSetupGrantsTheRuntimeRootCommandsActuallyUse(t *testing.T) {
	isolateSandboxRuntimeRoots(t)
	workspace := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}

	granted, err := setupWindowsSandboxRuntimeRoot(WindowsSandboxCommandConfig{
		WorkspaceRoots: []string{workspace},
	})
	if err != nil {
		t.Fatalf("setupWindowsSandboxRuntimeRoot: %v", err)
	}
	if len(granted) == 0 {
		t.Fatal("no runtime root resolved for a configured workspace")
	}
	// EVERY granted candidate has to exist, not just the one this process would
	// pick: applyWindowsACLPlan fails the whole run on a target that is absent.
	for _, root := range granted {
		if info, err := os.Stat(root); err != nil || !info.IsDir() {
			t.Fatalf("runtime root %q was not created; applyWindowsACLPlan skips absent targets so the grant would no-op (stat err %v)", root, err)
		}
	}

	// What a command would actually use.
	runtimeState, release, err := prepareSandboxRuntime(workspace)
	if err != nil {
		t.Fatalf("prepareSandboxRuntime: %v", err)
	}
	if release != nil {
		defer release()
	}
	// The command's choice has to be one setup provisioned. Setup grants the whole
	// candidate set precisely because the choice is made per process, so the
	// contract is membership rather than equality.
	if !grantedRuntimeRootsCover(granted, runtimeState.Root) {
		t.Fatalf("setup granted %q but commands write to %q", granted, runtimeState.Root)
	}
}

// grantedRuntimeRootsCover reports whether the root a command selected is one of
// the roots setup granted an ACE on.
func grantedRuntimeRootsCover(granted []string, selected string) bool {
	for _, root := range granted {
		if filepath.Clean(root) == filepath.Clean(selected) {
			return true
		}
	}
	return false
}

// No workspace root means nothing to grant, which is not an error.
func TestSetupRuntimeRootWithoutWorkspaceIsNotAnError(t *testing.T) {
	isolateSandboxRuntimeRoots(t)
	granted, err := setupWindowsSandboxRuntimeRoot(WindowsSandboxCommandConfig{})
	if err != nil {
		t.Fatalf("no workspace root should not error: %v", err)
	}
	if len(granted) != 0 {
		t.Fatalf("granted %q with no workspace configured", granted)
	}
}

// An account in a privileged group must not be adopted, even when name and
// ownership comment say it is ours.
//
// Adoption resets the password and hands the account to the sandbox. If that
// account is also in Administrators, the sandbox gains the rights the sandbox
// exists to withhold: rewriting the ACLs confining it, reading the secret locked
// to the invoking user, and stopping Zero.
func TestProvisionWindowsSandboxIdentityRefusesPrivilegedAccount(t *testing.T) {
	stubWindowsProvisioning(t, true, nil, nil, nil)

	previous := windowsSandboxUserIsPrivilegedFn
	t.Cleanup(func() { windowsSandboxUserIsPrivilegedFn = previous })
	windowsSandboxUserIsPrivilegedFn = func(string) (bool, error) { return true, nil }

	_, _, created, err := provisionWindowsSandboxIdentity("workspacekey", windowsSandboxRoleOffline)
	if !errors.Is(err, errWindowsSandboxPrivilegedAccount) {
		t.Fatalf("provisioning adopted a privileged account, err = %v", err)
	}
	if created {
		t.Fatal("created must stay false for an account this run refused to adopt")
	}
}

// The ordinary adopted account is unaffected.
func TestProvisionWindowsSandboxIdentityAdoptsUnprivilegedAccount(t *testing.T) {
	stubWindowsProvisioning(t, true, nil, nil, nil)

	previous := windowsSandboxUserIsPrivilegedFn
	t.Cleanup(func() { windowsSandboxUserIsPrivilegedFn = previous })
	windowsSandboxUserIsPrivilegedFn = func(string) (bool, error) { return false, nil }

	if _, _, _, err := provisionWindowsSandboxIdentity("workspacekey", windowsSandboxRoleOffline); err != nil {
		t.Fatalf("an unprivileged managed account must still be adopted: %v", err)
	}
}

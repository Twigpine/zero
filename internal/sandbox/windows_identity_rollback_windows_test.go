//go:build windows

package sandbox

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

// stubWindowsProvisioning replaces the four provisioning syscalls so the
// function can run on an ordinary machine. Every one of them needs an elevated
// caller and a real local account, so without this the test would stop at the
// first call and never reach the behaviour it is named for.
func stubWindowsProvisioning(t *testing.T, existed bool, groupErr error, offlineErr error, sidErr error) {
	t.Helper()
	prevGroup, prevUser := ensureWindowsSandboxGroupFn, ensureWindowsSandboxUserFn
	prevOfflineGroup := ensureWindowsSandboxOfflineGroupFn
	prevAdd, prevSID := addWindowsSandboxUserToGroupFn, resolveWindowsSandboxSIDFn
	prevManaged := windowsSandboxUserIsManagedFn
	prevOffline := addWindowsSandboxUserToOfflineGroupFn
	prevReset := resetWindowsSandboxUserPasswordFn
	prevRemove, prevUpgrade := removeWindowsSandboxIdentityFn, upgradeWindowsSandboxUserCommentFn
	stubbedWindowsIdentityRemovals = nil
	t.Cleanup(func() {
		ensureWindowsSandboxGroupFn, ensureWindowsSandboxUserFn = prevGroup, prevUser
		ensureWindowsSandboxOfflineGroupFn = prevOfflineGroup
		addWindowsSandboxUserToGroupFn, resolveWindowsSandboxSIDFn = prevAdd, prevSID
		windowsSandboxUserIsManagedFn = prevManaged
		addWindowsSandboxUserToOfflineGroupFn = prevOffline
		resetWindowsSandboxUserPasswordFn = prevReset
		removeWindowsSandboxIdentityFn, upgradeWindowsSandboxUserCommentFn = prevRemove, prevUpgrade
	})

	// THE INVERSE IS FAKED TOO. Creation is faked and ownership is faked, so a
	// rollback branch that trusts both would otherwise call the real NetUserDel
	// on the current user's deterministic principal for C:ws, an account this
	// fixture never created. Removals are recorded for the tests to assert on.
	removeWindowsSandboxIdentityFn = func(username, workspaceKey string) error {
		stubbedWindowsIdentityRemovals = append(stubbedWindowsIdentityRemovals, username+"|"+workspaceKey)
		return nil
	}
	upgradeWindowsSandboxUserCommentFn = func(string, string) error { return nil }
	ensureWindowsSandboxGroupFn = func() error { return nil }
	ensureWindowsSandboxOfflineGroupFn = func() error { return nil }
	// Adopted accounts are ours in these tests; the ownership check is a real
	// syscall and would otherwise refuse before the code under test runs.
	windowsSandboxUserIsManagedFn = func(string, string) (bool, error) { return true, nil }
	// Stubbed even though provisioning no longer rotates: rotation moved to the
	// caller, so nothing under test reaches this today. Left in so that if it
	// ever moves back, these tests fail rather than resetting the password of a
	// real managed account that happens to exist on the machine running them.
	resetWindowsSandboxUserPasswordFn = func(string, string) error {
		t.Fatal("provisioning must not rotate an account password; rotation belongs to the caller, immediately before the secret is written")
		return nil
	}
	ensureWindowsSandboxUserFn = func(string, string, string) (bool, error) { return existed, nil }
	addWindowsSandboxUserToGroupFn = func(string) error { return groupErr }
	addWindowsSandboxUserToOfflineGroupFn = func(string) error { return offlineErr }
	resolveWindowsSandboxSIDFn = func(username string) (*windows.SID, error) {
		if sidErr != nil {
			return nil, sidErr
		}
		return windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	}
}

// A failure after the account has been created must still hand back the name.
//
// The caller's rollback deletes by identity.Username, so returning a zero
// identity alongside created=true asked it to delete "" and quietly left the
// account this run had just made. Group attachment is the case that matters
// most: it is the enforcement boundary and it can fail under local policy.
func TestProvisionWindowsSandboxIdentityReturnsNameForRollback(t *testing.T) {
	groupFailure := errors.New("group attachment refused by policy")
	offlineFailure := errors.New("offline group attachment refused by policy")
	sidFailure := errors.New("sid lookup failed")

	for name, testCase := range map[string]struct {
		groupErr   error
		offlineErr error
		sidErr     error
	}{
		"group attachment fails":         {groupErr: groupFailure},
		"offline group attachment fails": {offlineErr: offlineFailure},
		"sid resolution fails":           {sidErr: sidFailure},
	} {
		t.Run(name, func(t *testing.T) {
			stubWindowsProvisioning(t, false, testCase.groupErr, testCase.offlineErr, testCase.sidErr)

			identity, _, created, err := provisionWindowsSandboxIdentity("workspacekey", windowsSandboxRoleOffline)
			if err == nil {
				t.Fatal("provisioning reported success despite an injected failure")
			}
			if !created {
				t.Fatal("created = false, so the rollback would skip an account this run made")
			}
			// The whole point: without a name there is nothing to delete.
			if identity.Username == "" {
				t.Fatal("identity carries no username, so the rollback deletes \"\" and strands the account")
			}
			if want := windowsSandboxUserName("workspacekey", windowsSandboxRoleOffline); identity.Username != want {
				t.Fatalf("username = %q, want %q", identity.Username, want)
			}
		})
	}
}

// An account that already existed must not be deleted because a later step
// failed. created=false is what stops the rollback turning a partial failure
// into the loss of a working principal from an earlier setup.
func TestProvisionWindowsSandboxIdentityDoesNotClaimPreexistingAccount(t *testing.T) {
	stubWindowsProvisioning(t, true, errors.New("group attachment refused"), nil, nil)

	_, _, created, err := provisionWindowsSandboxIdentity("workspacekey", windowsSandboxRoleOffline)
	if err == nil {
		t.Fatal("provisioning reported success despite an injected failure")
	}
	// created is the whole assertion. It is what stops the rollback deleting an
	// account it did not make, and it must stay false however provisioning
	// fails. The identity is deliberately not checked: an adopted account exits
	// early at the ownership check, which is a real syscall and not stubbed
	// here, so asserting on the name would be testing the stub rather than the
	// contract.
	if created {
		t.Fatal("created = true for an account this run did not create; rollback would delete a working principal")
	}
}

// The write grant has to include delete.
//
// FILE_GENERIC_WRITE covers creating and modifying but not removing or
// renaming, and a rename needs delete on the source. The old same-user token hid
// this because the caller already held inherited rights on its own tree; a
// principal is a separate account with none, so without these it can write files
// it can never remove, which fails ordinary editing and most git operations
// rather than an edge case.
func TestWindowsACLAllowWriteGrantsDelete(t *testing.T) {
	mode, mask, err := windowsACLAccess(WindowsACLAllowWrite)
	if err != nil {
		t.Fatalf("windowsACLAccess: %v", err)
	}
	if mode != windows.GRANT_ACCESS {
		t.Fatalf("mode = %v, want GRANT_ACCESS", mode)
	}
	// Atomic bits only. FILE_GENERIC_READ and FILE_GENERIC_WRITE both carry
	// READ_CONTROL and SYNCHRONIZE, so testing a composite constant with & is
	// satisfied by any grant at all and proves nothing.
	for label, bit := range map[string]windows.ACCESS_MASK{
		"DELETE":          windows.DELETE,
		"FILE_WRITE_DATA": windows.FILE_WRITE_DATA,
	} {
		if mask&bit == 0 {
			t.Errorf("write grant is missing %s", label)
		}
	}
	// Granting these would let the principal rewrite the very restrictions
	// placed on it. They are in the deny mask for that reason and must not
	// appear here.
	//
	// FILE_DELETE_CHILD belongs in this set and was originally in the one
	// above, on the reasoning that the grant should mirror the deny mask. On a
	// parent it authorises deleting a child whatever the child's own DACL says,
	// so on a write root it hands back the write-denied carve-outs underneath:
	// delete .git/config, recreate it, and the replacement inherits the grant
	// with no deny of its own. Mirroring the deny mask is the wrong instinct —
	// denying a capability is not a reason to grant it.
	for label, bit := range map[string]windows.ACCESS_MASK{
		"WRITE_DAC":         windows.WRITE_DAC,
		"WRITE_OWNER":       windows.WRITE_OWNER,
		"FILE_DELETE_CHILD": windowsFileDeleteChild,
	} {
		if mask&bit != 0 {
			t.Errorf("write grant unexpectedly includes %s", label)
		}
	}
}

// The read grant must stay read-only. Widening the write mask above is only
// safe if this one did not move with it.
func TestWindowsACLAllowReadGrantsNoDelete(t *testing.T) {
	_, mask, err := windowsACLAccess(WindowsACLAllowRead)
	if err != nil {
		t.Fatalf("windowsACLAccess: %v", err)
	}
	// Atomic bits, for the same reason as above: the read and write composites
	// overlap on the standard rights, so a composite check here would report a
	// failure that is not real.
	for label, bit := range map[string]windows.ACCESS_MASK{
		"DELETE":            windows.DELETE,
		"FILE_DELETE_CHILD": windowsFileDeleteChild,
		"FILE_WRITE_DATA":   windows.FILE_WRITE_DATA,
	} {
		if mask&bit != 0 {
			t.Errorf("read grant unexpectedly includes %s", label)
		}
	}
}

// stubbedWindowsIdentityRemovals records every account deletion the mocked
// provisioning fixture was asked for, newest last, as "username|workspaceKey".
var stubbedWindowsIdentityRemovals []string

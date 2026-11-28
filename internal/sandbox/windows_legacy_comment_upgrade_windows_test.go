//go:build windows

package sandbox

import (
	"testing"

	"golang.org/x/sys/windows"
)

// THE UPGRADE HAS TO BE OBSERVABLE, or it is not really there.
//
// windowsSandboxUserIsManaged accepts the old bare account comment and promises
// the workspace key gets stamped on so the bare form can eventually be retired.
// The probe for the legacy comment was seamed and the upgrade itself was not, so
// nothing in the suite could tell whether the call ran: deleting it left
// everything green. This repository has already had a fix silently reverted by a
// later change, which is the failure this pins.
func TestAdoptingALegacyAccountStampsTheWorkspaceKey(t *testing.T) {
	stub := func(t *testing.T, legacy bool) *string {
		t.Helper()
		var upgraded string

		prevGroup := ensureWindowsSandboxGroupFn
		prevOffline := ensureWindowsSandboxOfflineGroupFn
		prevEnsure := ensureWindowsSandboxUserFn
		prevManaged := windowsSandboxUserIsManagedFn
		prevLegacy := windowsSandboxUserHasLegacyCommentFn
		prevPrivileged := windowsSandboxUserIsPrivilegedFn
		prevUpgrade := upgradeWindowsSandboxUserCommentFn
		prevReset := resetWindowsSandboxUserPasswordFn
		prevGroupAdd := addWindowsSandboxUserToGroupFn
		prevOfflineAdd := addWindowsSandboxUserToOfflineGroupFn
		prevSID := resolveWindowsSandboxSIDFn
		t.Cleanup(func() {
			ensureWindowsSandboxGroupFn = prevGroup
			ensureWindowsSandboxOfflineGroupFn = prevOffline
			ensureWindowsSandboxUserFn = prevEnsure
			windowsSandboxUserIsManagedFn = prevManaged
			windowsSandboxUserHasLegacyCommentFn = prevLegacy
			windowsSandboxUserIsPrivilegedFn = prevPrivileged
			upgradeWindowsSandboxUserCommentFn = prevUpgrade
			resetWindowsSandboxUserPasswordFn = prevReset
			addWindowsSandboxUserToGroupFn = prevGroupAdd
			addWindowsSandboxUserToOfflineGroupFn = prevOfflineAdd
			resolveWindowsSandboxSIDFn = prevSID
		})

		ensureWindowsSandboxGroupFn = func() error { return nil }
		ensureWindowsSandboxOfflineGroupFn = func() error { return nil }
		// existed = true: the adoption path, which is the only one that upgrades.
		ensureWindowsSandboxUserFn = func(string, string, string) (bool, error) { return true, nil }
		windowsSandboxUserIsManagedFn = func(string, string) (bool, error) { return true, nil }
		windowsSandboxUserHasLegacyCommentFn = func(string) (bool, error) { return legacy, nil }
		windowsSandboxUserIsPrivilegedFn = func(string) (bool, error) { return false, nil }
		upgradeWindowsSandboxUserCommentFn = func(_ string, workspaceKey string) error {
			upgraded = workspaceKey
			return nil
		}
		resetWindowsSandboxUserPasswordFn = func(string, string) error { return nil }
		addWindowsSandboxUserToGroupFn = func(string) error { return nil }
		addWindowsSandboxUserToOfflineGroupFn = func(string) error { return nil }
		resolveWindowsSandboxSIDFn = func(string) (*windows.SID, error) {
			return windows.StringToSid("S-1-5-32-546")
		}
		return &upgraded
	}

	t.Run("a legacy comment is upgraded", func(t *testing.T) {
		upgraded := stub(t, true)
		if _, _, _, err := provisionWindowsSandboxIdentity("workspacekey", windowsSandboxRoleOffline); err != nil {
			t.Fatalf("provisionWindowsSandboxIdentity: %v", err)
		}
		if *upgraded != "workspacekey" {
			t.Errorf("the workspace key was never stamped onto the adopted legacy account (got %q); windowsSandboxUserIsManaged goes on accepting the bare comment forever", *upgraded)
		}
	})

	// And an account that already carries the key is left alone, or the
	// assertion above would be satisfied by an unconditional rewrite.
	t.Run("a current comment is left alone", func(t *testing.T) {
		upgraded := stub(t, false)
		if _, _, _, err := provisionWindowsSandboxIdentity("workspacekey", windowsSandboxRoleOffline); err != nil {
			t.Fatalf("provisionWindowsSandboxIdentity: %v", err)
		}
		if *upgraded != "" {
			t.Errorf("an account that already carried the workspace key was rewritten anyway: %q", *upgraded)
		}
	})
}

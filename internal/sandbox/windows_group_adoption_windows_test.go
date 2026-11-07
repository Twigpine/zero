//go:build windows

package sandbox

import (
	"errors"
	"strings"
	"testing"
)

// A NAME IS NOT PROOF OF PROVENANCE, for the USERS group specifically.
//
// ensureWindowsLocalGroup covers both managed groups, and the sibling test in
// windows_offline_group_ownership_windows_test.go pins the offline one. This
// pins ZeroSandboxUsers, which is the group anandh8x reported: it holds every
// sandbox principal, so adopting a same-named group created by an installer or
// by policy would hand the principal whatever that group already grants.
//
// Driven through the seams so it needs no Administrator and leaves no real local
// group on the machine running the suite.
func TestAdoptingAForeignUsersGroupIsRefused(t *testing.T) {
	for _, status := range []uintptr{nerrGroupExists, errorAliasExists} {
		prevAdd, prevOwned := addWindowsLocalGroupFn, windowsLocalGroupOwnedByZeroFn
		t.Cleanup(func() { addWindowsLocalGroupFn, windowsLocalGroupOwnedByZeroFn = prevAdd, prevOwned })

		addWindowsLocalGroupFn = func(string, string) (uintptr, error) { return status, nil }
		windowsLocalGroupOwnedByZeroFn = func(string, string) (bool, error) { return false, nil }

		err := ensureWindowsSandboxGroup()
		if err == nil {
			t.Fatalf("status %d adopted a group Zero did not create, so the principal inherits whatever it already grants", status)
		}
		if !strings.Contains(err.Error(), windowsSandboxGroupName) {
			t.Errorf("the refusal must name the group in the way, got %q", err)
		}
	}
}

// Our own group is still adopted, or re-running setup would fail on the group it
// created a moment ago and provisioning would never converge.
func TestAdoptingOurOwnUsersGroupSucceeds(t *testing.T) {
	prevAdd, prevOwned := addWindowsLocalGroupFn, windowsLocalGroupOwnedByZeroFn
	t.Cleanup(func() { addWindowsLocalGroupFn, windowsLocalGroupOwnedByZeroFn = prevAdd, prevOwned })

	addWindowsLocalGroupFn = func(string, string) (uintptr, error) { return nerrGroupExists, nil }
	windowsLocalGroupOwnedByZeroFn = func(name, comment string) (bool, error) {
		if name != windowsSandboxGroupName || comment != windowsSandboxGroupComment {
			t.Errorf("ownership probed for %q/%q, want the users group", name, comment)
		}
		return true, nil
	}
	if err := ensureWindowsSandboxGroup(); err != nil {
		t.Errorf("refused a group carrying Zero's own comment: %v", err)
	}
}

// A freshly created group is ours by construction, so no ownership probe runs.
func TestCreatingTheUsersGroupDoesNotProbeOwnership(t *testing.T) {
	prevAdd, prevOwned := addWindowsLocalGroupFn, windowsLocalGroupOwnedByZeroFn
	t.Cleanup(func() { addWindowsLocalGroupFn, windowsLocalGroupOwnedByZeroFn = prevAdd, prevOwned })

	probed := false
	addWindowsLocalGroupFn = func(string, string) (uintptr, error) { return nerrSuccess, nil }
	windowsLocalGroupOwnedByZeroFn = func(string, string) (bool, error) { probed = true; return false, nil }

	if err := ensureWindowsSandboxGroup(); err != nil {
		t.Fatalf("a successful create was rejected: %v", err)
	}
	if probed {
		t.Error("ownership was probed for a group we had just created ourselves")
	}
}

// A failed ownership probe is not read as "ours" and not as "not ours" either.
func TestAnUnreadableUsersGroupIsNotGuessedEitherWay(t *testing.T) {
	prevAdd, prevOwned := addWindowsLocalGroupFn, windowsLocalGroupOwnedByZeroFn
	t.Cleanup(func() { addWindowsLocalGroupFn, windowsLocalGroupOwnedByZeroFn = prevAdd, prevOwned })

	sentinel := errors.New("NetLocalGroupGetInfo: status 5")
	addWindowsLocalGroupFn = func(string, string) (uintptr, error) { return nerrGroupExists, nil }
	windowsLocalGroupOwnedByZeroFn = func(string, string) (bool, error) { return false, sentinel }

	if err := ensureWindowsSandboxGroup(); !errors.Is(err, sentinel) {
		t.Fatalf("a failed ownership probe was swallowed, got %v", err)
	}
}

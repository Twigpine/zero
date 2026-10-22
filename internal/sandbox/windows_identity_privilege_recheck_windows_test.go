//go:build windows

package sandbox

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

// Group membership is not frozen at setup. An account provisioned clean can be
// added to Administrators afterwards — by an operator, or by an attacker who
// already has that access and wants the sandbox to hand it back. Provisioning's
// refusal cannot see that; only the path that mints the token can.
func TestLookupPrincipalForCommandRefusesAnAccountThatBecamePrivileged(t *testing.T) {
	sid, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid: %v", err)
	}
	restoreLookupSeams(t, sid)

	privilegedCalls := 0
	windowsSandboxUserIsPrivilegedFn = func(string) (bool, error) {
		privilegedCalls++
		return true, nil
	}

	_, err = lookupWindowsSandboxPrincipalForCommand("workspace-key", windowsSandboxRoleOnline)
	if !errors.Is(err, errWindowsSandboxPrivilegedAccount) {
		t.Fatalf("err = %v, want errWindowsSandboxPrivilegedAccount", err)
	}
	if privilegedCalls != 1 {
		t.Errorf("privileged check ran %d times, want exactly 1", privilegedCalls)
	}
	// It must be a hard refusal, not the unavailable sentinel — that one is the
	// quiet "not provisioned" fallback and would silently drop the sandbox back
	// to the restricted token instead of telling the operator.
	if errors.Is(err, errWindowsSandboxIdentityUnavailable) {
		t.Error("privileged refusal must not read as the not-provisioned fallback")
	}
}

func TestLookupPrincipalForCommandAcceptsAnUnprivilegedAccount(t *testing.T) {
	sid, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid: %v", err)
	}
	restoreLookupSeams(t, sid)
	windowsSandboxUserIsPrivilegedFn = func(string) (bool, error) { return false, nil }

	identity, err := lookupWindowsSandboxPrincipalForCommand("workspace-key", windowsSandboxRoleOnline)
	if err != nil {
		t.Fatalf("lookupWindowsSandboxPrincipalForCommand: %v", err)
	}
	if identity.Username == "" {
		t.Error("expected the resolved principal")
	}
}

// Teardown must stay able to clean up an account that has become privileged.
// If the refusal lived inside lookupWindowsSandboxIdentity, the account this
// guard exists to catch would become undeletable by Zero.
func TestLookupIdentityItselfDoesNotConsultPrivilege(t *testing.T) {
	sid, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid: %v", err)
	}
	restoreLookupSeams(t, sid)
	windowsSandboxUserIsPrivilegedFn = func(string) (bool, error) {
		t.Error("teardown's lookup must not be gated on privilege")
		return true, nil
	}

	if _, err := lookupWindowsSandboxIdentity("workspace-key", windowsSandboxRoleOnline); err != nil {
		t.Fatalf("lookupWindowsSandboxIdentity: %v", err)
	}
}

func restoreLookupSeams(t *testing.T, sid *windows.SID) {
	t.Helper()
	prevResolve := resolveWindowsSandboxSIDFn
	prevManaged := windowsSandboxUserIsManagedFn
	prevPrivileged := windowsSandboxUserIsPrivilegedFn
	t.Cleanup(func() {
		resolveWindowsSandboxSIDFn = prevResolve
		windowsSandboxUserIsManagedFn = prevManaged
		windowsSandboxUserIsPrivilegedFn = prevPrivileged
	})
	resolveWindowsSandboxSIDFn = func(string) (*windows.SID, error) { return sid, nil }
	windowsSandboxUserIsManagedFn = func(string, string) (bool, error) { return true, nil }
}

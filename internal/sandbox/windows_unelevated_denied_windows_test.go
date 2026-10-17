//go:build windows

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// denyWriteDACTarget returns a disposable directory this process cannot re-DACL.
//
// A plain deny ACE is not enough: the owner keeps an implicit WRITE_DAC that
// defeats it, and the apply succeeds. OWNER_RIGHTS (S-1-3-4) is what replaces
// that implicit grant, so the DACL becomes the whole story and the access check
// actually fails. Probed rather than reasoned; the deny-only shape was measured
// to still let the mutation through.
func denyWriteDACTarget(t *testing.T) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "denied")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	ownerRights, err := windows.StringToSid("S-1-3-4")
	if err != nil {
		t.Fatalf("parse OWNER RIGHTS: %v", err)
	}
	// READ_CONTROL only: the applier can read the descriptor and then fails on the
	// write, which is the exact production shape this pins.
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.READ_CONTROL | windows.SYNCHRONIZE | windows.FILE_GENERIC_READ,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(ownerRights),
		},
	}}, nil)
	if err != nil {
		t.Fatalf("build the deny-WRITE_DAC descriptor: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(
		target,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	); err != nil {
		t.Skipf("cannot rig a deny-WRITE_DAC directory here: %v", err)
	}
	return target
}

// The diagnostic reads a path back out of an error string produced two
// functions away. That coupling is invisible to the compiler, so it is pinned
// here by driving the REAL producer rather than by hand-writing the message:
// if openWindowsACLTarget ever rewords its error, this fails instead of the
// diagnostic silently going quiet and users losing the one clue they had.
//
// THE TARGET IS DISPOSABLE, AND THAT IS THE POINT.
//
// This used to apply the real plan to C:\Windows\System32 and use the result as
// its privilege probe: on success it skipped, having already mutated. The
// snapshot and the applied result were both discarded at the call site, so
// nothing could put the descriptor back. Any process that does hold WRITE_DAC
// there would leave an inheritable allow ACE for a synthetic SID on System32 and
// everything later created beneath it, with no code path anywhere to remove it.
//
// The rigged directory below produces the same access denial from the same
// producer without depending on a system path or on the test's own privilege
// level, so the coupling stays pinned and no run can leave residue outside
// t.TempDir().
func TestDeniedPathIsRecoveredFromARealApplyFailure(t *testing.T) {
	target := denyWriteDACTarget(t)

	_, _, err := applyWindowsACLPathGroup(windowsACLPathGroup{
		Path: target,
		Entries: []WindowsACLEntry{{
			Action:     WindowsACLAllowWrite,
			Path:       target,
			Capability: testPrincipalSID,
		}},
	})
	if err == nil {
		t.Fatal("SETUP INVALID: the apply succeeded on a directory rigged to refuse WRITE_DAC, so the denial path was not exercised")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Skipf("failed for a reason other than access denial, nothing to extract here: %v", err)
	}

	got := windowsACLPlanDeniedPath(err)
	if got == "" {
		t.Fatalf("no path recovered from a real access-denied apply failure, so the operator is told only that something was denied: %v", err)
	}
	if got != target {
		t.Errorf("recovered %q, want %q", got, target)
	}
}

// Anything that is not an access denial must return empty, so the caller falls
// back to the generic message rather than naming an innocent path.
func TestDeniedPathIgnoresUnrelatedErrors(t *testing.T) {
	for _, err := range []error{nil, errors.New(`open windows ACL target C:\somewhere: disk full`), os.ErrNotExist} {
		if got := windowsACLPlanDeniedPath(err); got != "" {
			t.Errorf("recovered %q from %v, want empty", got, err)
		}
	}
}

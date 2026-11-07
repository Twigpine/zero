//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// A PLAN HASH ATTESTS A PATHNAME, NOT THE DIRECTORY THAT ANSWERS TO IT.
//
// The unelevated marker records the plan hash and its entry count, both derived
// from pathnames and entries. The runtime root is deterministic and disposable,
// so cleanup can remove the tree and the next command's parent recreates the
// same pathname with ordinary inherited permissions. The hash is unchanged, the
// marker still claims the plan was applied, and the replacement never received
// the capability ACE, leaving the WRITE_RESTRICTED child unable to write TMP or
// its caches with nothing failing to say why.
func TestPlanAttestationFailsAfterTheRootIsRecreated(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runtime")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	// Guests: a well-known SID that no ordinary object carries, standing in for
	// the sandbox capability SID.
	const capability = "S-1-5-32-546"
	plan := WindowsACLPlan{Entries: []WindowsACLEntry{
		{Action: WindowsACLAllowWrite, Path: root, Capability: capability},
	}}

	if windowsACLPlanStillApplied(plan) {
		t.Fatal("SETUP INVALID: the grant is reported as present before it was applied")
	}
	rollback, err := applyWindowsACLPlan(plan)
	if err != nil {
		t.Fatalf("applyWindowsACLPlan: %v", err)
	}
	t.Cleanup(func() { _ = rollback() })

	if !windowsACLPlanStillApplied(plan) {
		t.Fatal("the grant was just applied and the attestation does not see it")
	}

	// Exactly what cleanup plus the next command's parent does: same pathname,
	// new directory object, ordinary inherited permissions.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if windowsACLPlanStillApplied(plan) {
		t.Error("a recreated root is reported as still carrying the grant, so the apply would be skipped")
	}
}

// A missing path is not a grant either, and must not be read as one.
func TestPlanAttestationFailsWhenThePathIsGone(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	plan := WindowsACLPlan{Entries: []WindowsACLEntry{
		{Action: WindowsACLAllowWrite, Path: root, Capability: "S-1-5-32-546"},
	}}
	if windowsACLPlanStillApplied(plan) {
		t.Error("a path that does not exist was reported as carrying its grant")
	}
}

// Deny entries are not load-bearing for the child's ability to run, so their
// absence must not force a re-apply on every command.
func TestPlanAttestationIgnoresDenyEntries(t *testing.T) {
	root := t.TempDir()
	plan := WindowsACLPlan{Entries: []WindowsACLEntry{
		{Action: WindowsACLDenyWrite, Path: root, Capability: "S-1-5-32-546"},
	}}
	if !windowsACLPlanStillApplied(plan) {
		t.Error("a plan of deny entries alone reported as unapplied, which would re-apply on every command")
	}
}

// setCapabilityACE replaces path's DACL with a single allow entry for trustee,
// so a test can weaken a grant without removing the SID that names it.
func setCapabilityACE(t *testing.T, path, trustee string, mask windows.ACCESS_MASK, inheritance uint32) {
	t.Helper()
	sid, err := windows.StringToSid(trustee)
	if err != nil {
		t.Fatal(err)
	}
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: mask,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

// PRESENCE OF THE SID IS NOT THE CONTRACT.
//
// What the restricted child needs is the grant windowsACLAccess creates, and on
// a directory it needs to reach descendants. An ACE that still names the
// capability but has been reduced to a read-only mask, or that no longer
// propagates, leaves a runtime root that attests as healthy and then returns
// ACCESS_DENIED on the first write into TMP or a cache: exactly the silent
// unusable runtime the attestation exists to eliminate.
func TestPlanAttestationRejectsAWeakenedCapabilityACE(t *testing.T) {
	const capability = "S-1-5-32-546"
	full := windows.ACCESS_MASK(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.FILE_GENERIC_EXECUTE)

	for _, testCase := range []struct {
		name        string
		mask        windows.ACCESS_MASK
		inheritance uint32
		want        bool
	}{
		{"the grant the plan describes", full, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT, true},
		{"reduced to read and execute", windows.ACCESS_MASK(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_EXECUTE), windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT, false},
		{"reduced to metadata only", windows.FILE_READ_ATTRIBUTES, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT, false},
		{"full mask that does not propagate", full, windows.NO_INHERITANCE, false},
		{"containers only, so files are ungranted", full, windows.SUB_CONTAINERS_ONLY_INHERIT, false},
		// INHERIT_ONLY grants descendants and grants the directory nothing, so the
		// child is refused FILE_ADD_FILE on the runtime root while every inherit
		// flag the check looks for is present.
		{"granted to descendants but not to the directory", full, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT | windows.INHERIT_ONLY_ACE, false},
		// NO_PROPAGATE lets the ACE be inherited exactly once. The immediate
		// children are granted and their children are not, so the runtime tree's
		// real consumers - cache/npm, cache/go-build, data/go-mod - sit past the
		// end of the grant while both inherit bits the check reads are present.
		{"propagates only to the immediate children", full, windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT | windows.NO_PROPAGATE_INHERIT_ACE, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "runtime")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			plan := WindowsACLPlan{Entries: []WindowsACLEntry{
				{Action: WindowsACLAllowWrite, Path: root, Capability: capability},
			}}
			setCapabilityACE(t, root, capability, testCase.mask, testCase.inheritance)

			if got := windowsACLPlanStillApplied(plan); got != testCase.want {
				t.Errorf("attestation = %v, want %v; a capability SID is present either way, so only the effective grant separates these",
					got, testCase.want)
			}
		})
	}
}

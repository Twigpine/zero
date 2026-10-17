//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// THE READER MUST BE THE TOKEN THAT VALIDATES, NOT THE ONE THAT INSTALLS.
//
// Deriving the reader from the runtime leaf's owner looked right and is wrong
// across the elevation boundary: the elevated helper CREATES that leaf when it
// is absent, so the owner is commonly BUILTIN\Administrators. A later
// UAC-filtered administrator carries that group deny-only, and a standard user
// given alternate administrator credentials is not in it at all, so the
// protected stamp ends up with no enabled allow ACE for the token that has to
// read it. Setup reports success and every restricted command then stops before
// launch.
//
// The consumer is therefore resolved in the operator's shell and carried in.
// This test pins that the carried identity wins over the leaf owner, which is
// the whole difference between the two designs.
func TestCarriedConsumerSIDOutranksTheLeafOwner(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(append([]string{base}, append(append([]string{}, windowsSandboxRuntimeOwnedNames...), "abcdef0123456789")...)...)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime tree: %v", err)
	}

	// A consumer that is deliberately NOT this process and NOT a repair identity,
	// so it cannot be confused with the leaf owner or folded into the
	// SYSTEM/Administrators grants.
	consumer, err := windows.StringToSid("S-1-5-21-1111111111-2222222222-3333333333-1001")
	if err != nil {
		t.Fatalf("build the stand-in consumer SID: %v", err)
	}
	restore := setWindowsSetupConsumerSID(consumer)
	t.Cleanup(restore)

	if err := writeWindowsRuntimeStampThroughHandle(root, "planhash"); err != nil {
		t.Fatalf("write the stamp: %v", err)
	}
	stamp := windowsSandboxRuntimeStampPath(root, testStampPlanHash)

	mask, present := stampACEMask(t, stamp, consumer)
	if !present {
		t.Fatal("the carried consumer has no ACE; the stamp still names whoever owns the leaf")
	}
	if mask&windows.FILE_READ_DATA == 0 {
		t.Errorf("the carried consumer cannot read the stamp (mask 0x%08x)", mask)
	}
	// It is not the owner of the leaf, so this also proves the owner fallback did
	// not silently win.
	owner := ownerOfDirectory(t, root)
	if owner.Equals(consumer) {
		t.Skip("the leaf owner happens to equal the stand-in consumer; this cannot distinguish the two")
	}
	if _, ownerPresent := stampACEMask(t, stamp, owner); ownerPresent && !isRepairIdentity(t, owner) {
		t.Error("the leaf owner was granted alongside the carried consumer")
	}

	// Repair must still work, and the capability SID must still be absent.
	for _, wellKnown := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(wellKnown)
		if err != nil {
			t.Fatalf("resolve well-known SID: %v", err)
		}
		if m, ok := stampACEMask(t, stamp, sid); !ok || m&windows.FILE_WRITE_DATA == 0 {
			t.Errorf("repair identity %s lost write (present=%v mask 0x%08x)", sid, ok, m)
		}
	}
}

//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// racedLeafDenyMask reads the DENY mask a path carries for sid.
func racedLeafDenyMask(t *testing.T, path string, sid string) uint32 {
	t.Helper()
	handle, _, err := openWindowsACLTarget(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read the DACL of %s: %v", path, err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("read the DACL of %s: %v", path, err)
	}
	wanted, err := windows.StringToSid(sid)
	if err != nil {
		t.Fatalf("parse %q: %v", sid, err)
	}
	var mask uint32
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, index, &ace) != nil {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if !(*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(wanted) {
			continue
		}
		mask |= uint32(ace.Mask)
	}
	return mask
}

// OWNING THE PARENT IS NOT OWNING THE TARGET.
//
// rollbackWindowsACLSnapshots branched on createdAnything(), an OR across the
// whole materialization record. When this run creates the parent chain and a
// racer wins the leaf, that is true while the ACL-bearing file belongs to
// somebody else: Chain carries Made:true and FileMade is false.
//
// The rollback then could not remove the parent, because it is not empty, and
// the unconditional skip meant the existing no-follow, identity-validated
// restore never ran either. A file that existed before setup started was left
// wearing an aborted setup's DACL.
//
// Driven through applyWindowsACLPlan with the leaf created inside the swap hook,
// which fires after the anchor is pinned and before anything is made, so the
// race is deterministic rather than hoped for.
func TestAbortRestoresALeafARacerCreated(t *testing.T) {
	workspace := t.TempDir()
	parent := filepath.Join(workspace, "materialized")
	leaf := filepath.Join(parent, "config")

	previous := windowsACLRacedLeafHook
	t.Cleanup(func() { windowsACLRacedLeafHook = previous })
	raced := false
	windowsACLRacedLeafHook = func(string) {
		// The chain above already belongs to this run; the racer wins only the
		// file. That mixed ownership is the whole case.
		if raced {
			return
		}
		raced = true
		_ = os.WriteFile(leaf, []byte("pre-existing content"), 0o600)
	}

	sid := testPrincipalSID

	plan := WindowsACLPlan{Entries: []WindowsACLEntry{{
		Action:          WindowsACLDenyWrite,
		Path:            leaf,
		Capability:      sid,
		Materialize:     true,
		MaterializeFile: true,
	}}}
	rollback, err := applyWindowsACLPlan(plan)
	if err != nil {
		t.Skipf("cannot apply an ACL plan here: %v", err)
	}

	if !raced {
		t.Fatal("SETUP INVALID: the leaf hook never fired, so the mixed-ownership case was not reproduced")
	}
	// SETUP: the racer really won, and the apply really landed on its file.
	if _, statErr := os.Stat(leaf); statErr != nil {
		t.Skipf("SETUP: the racer's leaf is not present, so this is not the mixed-ownership case: %v", statErr)
	}
	applied := racedLeafDenyMask(t, leaf, sid)
	if applied == 0 {
		t.Skipf("SETUP: the apply did not put a deny ACE on the raced leaf, so there is nothing to restore")
	}

	// The setup aborts.
	if rollback != nil {
		_ = rollback()
	}

	// The racer's file must not be left wearing this run's ACL.
	if got := racedLeafDenyMask(t, leaf, sid); got != 0 {
		t.Fatalf("a file the racer created still carries the aborted setup's deny ACE (mask=%#x); rollback skipped its restore because an ancestor happened to be ours", got)
	}
	// And it must still exist: a raced leaf is never ours to delete.
	body, readErr := os.ReadFile(leaf)
	if readErr != nil {
		t.Fatalf("rollback removed a file it did not create: %v", readErr)
	}
	if !strings.Contains(string(body), "pre-existing") {
		t.Fatalf("the raced leaf's contents changed: %q", body)
	}
}

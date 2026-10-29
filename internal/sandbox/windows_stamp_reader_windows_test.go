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

// stampACEMask returns the access mask the stamp's DACL grants sid, and whether
// an ACE for it exists at all.
func stampACEMask(t *testing.T, path string, sid *windows.SID) (uint32, bool) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read the stamp security descriptor: %v", err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("read the stamp DACL: %v", err)
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			t.Fatalf("read ACE %d: %v", index, err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		if (*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(sid) {
			return uint32(ace.Mask), true
		}
	}
	return 0, false
}

func ownerOfDirectory(t *testing.T, path string) *windows.SID {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read the runtime root owner: %v", err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		t.Fatalf("read the runtime root owner: %v", err)
	}
	return owner
}

// THE STAMP HAS TO BE READABLE BY THE IDENTITY THAT VALIDATES IT LATER, AND
// WRITABLE BY NEITHER IT NOR THE SANDBOX.
//
// Setup writes the stamp elevated; runWindowsSandboxCommand and zero doctor read
// it afterwards from an ordinary shell. The DACL used to name WinCreatorOwnerSid
// at GENERIC_ALL. SetSecurityInfo does substitute that placeholder even in a
// NO_INHERITANCE ACE, so a concrete SID did land in the ACE, but the one it
// named was whoever ran setup. Elevation by a different administrator account
// therefore produced a stamp the ordinary reader could not open, and a reader
// named in no ACE gets Access is denied, so a successful setup handed over an
// unreadable attestation.
//
// Resolving the reader from the runtime root binds the grant to the install
// rather than to the elevation, and read-only keeps the attestation out of reach
// of everything but setup and repair.
func TestStampGrantsTheRuntimeRootOwnerReadOnly(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(append([]string{base}, append(append([]string{}, windowsSandboxRuntimeOwnedNames...), "abcdef0123456789")...)...)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime tree: %v", err)
	}
	if err := writeWindowsRuntimeStampThroughHandle(root, "planhash"); err != nil {
		t.Fatalf("write the stamp: %v", err)
	}
	stamp := windowsSandboxRuntimeStampPath(root, testStampPlanHash)
	owner := ownerOfDirectory(t, root)

	mask, present := stampACEMask(t, stamp, owner)
	if !present {
		t.Fatalf("the stamp names no ACE for the runtime root owner %s, so the post-setup reader is locked out", owner)
	}

	// Read, because doctor and the launch gate have to open it.
	const readBits = windows.FILE_READ_DATA
	if mask&readBits == 0 {
		t.Errorf("the runtime root owner cannot read the stamp (mask 0x%08x)", mask)
	}
	// Not write, UNLESS the owner is itself a repair identity. A runtime root
	// created by an elevated process is commonly owned by BUILTINAdministrators
	// rather than by the invoking user, which is what CI runners do, and repair
	// has to keep write there. Asserting no-write unconditionally failed on every
	// runner while passing on an unelevated box.
	const writeBits = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.WRITE_DAC | windows.WRITE_OWNER
	if isRepairIdentity(t, owner) {
		if mask&windows.FILE_WRITE_DATA == 0 {
			t.Errorf("the runtime root is owned by a repair identity that cannot rewrite the stamp (mask 0x%08x)", mask)
		}
	} else if mask&writeBits != 0 {
		t.Errorf("the runtime root owner can rewrite the stamp (mask 0x%08x, write bits 0x%08x)", mask, mask&writeBits)
	}

	// The end of the handoff: it is actually readable.
	if _, err := os.ReadFile(stamp); err != nil {
		t.Errorf("the post-setup reader cannot open the stamp: %v", err)
	}

	// And repair still can, or a damaged stamp could never be replaced.
	for _, wellKnown := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(wellKnown)
		if err != nil {
			t.Fatalf("resolve well-known SID: %v", err)
		}
		mask, present := stampACEMask(t, stamp, sid)
		if !present || mask&windows.FILE_WRITE_DATA == 0 {
			t.Errorf("repair identity %s cannot rewrite the stamp (present=%v mask 0x%08x)", sid, present, mask)
		}
	}
}

// An identity that could not be resolved is not permission to protect the stamp
// with a DACL naming nobody. The builder is what refuses, so it is called
// directly: nothing else in the path could produce this error first.
func TestStampDACLRefusesWithoutAResolvedReader(t *testing.T) {
	_, err := windowsRuntimeStampDACL(nil)
	if err == nil {
		t.Fatal("building the stamp DACL with no resolved reader succeeded, which would lock out the identity that has to validate it")
	}
	if !strings.Contains(err.Error(), "no identity was supplied") {
		t.Fatalf("refused for the wrong reason, so this does not pin the guard: %v", err)
	}
	if _, err := windowsRuntimeStampSecurityDescriptor(nil); err == nil || !strings.Contains(err.Error(), "no identity was supplied") {
		t.Fatalf("the descriptor the stamp is created with skipped the same guard: %v", err)
	}
}

func isRepairIdentity(t *testing.T, sid *windows.SID) bool {
	t.Helper()
	for _, wellKnown := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		known, err := windows.CreateWellKnownSid(wellKnown)
		if err != nil {
			t.Fatalf("resolve well-known SID: %v", err)
		}
		if sid.Equals(known) {
			return true
		}
	}
	return false
}

// A READER THAT IS ALSO A REPAIR IDENTITY MUST NOT LOSE WRITE.
//
// The reader entry and the repair entries can name the same SID, because an
// elevated create commonly leaves BUILTINAdministrators as the owner. Naming it
// twice let the narrower read-only entry win, and repair could no longer rewrite
// the stamp: setup succeeded and left an attestation nothing could replace. My
// own regression caught this on CI and not here, since an unelevated box owns
// the directory as the ordinary user.
func TestReaderThatIsARepairIdentityKeepsWrite(t *testing.T) {
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatalf("resolve the Administrators SID: %v", err)
	}
	restore := setWindowsSetupConsumerSID(administrators)
	defer restore()

	root := t.TempDir()
	directory := openStampTestDirectory(t, root)
	name := windowsSandboxRuntimeStampName("planhash")
	if err := writeWindowsRuntimeStampToDirectoryHandle(directory, name, "planhash"); err != nil {
		t.Fatalf("publish the stamp: %v", err)
	}

	mask, present := stampACEMask(t, filepath.Join(root, name), administrators)
	if !present {
		t.Fatal("Administrators has no ACE at all")
	}
	if mask&windows.FILE_WRITE_DATA == 0 {
		t.Errorf("Administrators lost write when it was also the resolved reader (mask 0x%08x)", mask)
	}
}

// openStampTestDirectory opens root the way the rooted writer holds it.
func openStampTestDirectory(t *testing.T, root string) windows.Handle {
	t.Helper()
	path, err := windows.UTF16PtrFromString(root)
	if err != nil {
		t.Fatal(err)
	}
	directory, err := windows.CreateFile(
		path,
		windows.FILE_TRAVERSE|windowsFileAddFile|windows.READ_CONTROL|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		t.Fatalf("open the runtime root: %v", err)
	}
	t.Cleanup(func() { _ = windows.CloseHandle(directory) })
	return directory
}

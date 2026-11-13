//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// stampDACLGrants reports whether the stamp's DACL grants the named SID, and
// whether it still inherits from the runtime root.
func stampDACLGrants(t *testing.T, path string, sid *windows.SID) (granted bool, inherits bool) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read the stamp security descriptor: %v", err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatalf("read the stamp descriptor control bits: %v", err)
	}
	inherits = control&windows.SE_DACL_PROTECTED == 0
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatalf("read the stamp DACL: %v", err)
	}
	if dacl == nil {
		return false, inherits
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			t.Fatalf("read ACE %d: %v", index, err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		if (*windows.SID)(unsafePointerOfSID(ace)).Equals(sid) {
			granted = true
		}
	}
	return granted, inherits
}

// THE ATTESTATION MUST NOT SIT IN THE SUBJECT'S OWN WRITABLE NAMESPACE.
//
// The runtime root grants the capability SID FILE_GENERIC_WRITE with
// SUB_CONTAINERS_AND_OBJECTS_INHERIT, which is what lets a sandboxed command
// write TMP, GOCACHE and the package caches beneath it. A stamp created inside
// that root inherits the same grant, so the restricted command could overwrite
// the file attesting its own setup: its current command would continue, and
// every later elevated command and zero doctor would reject the altered plan
// hash until an Administrator re-ran setup.
func TestTheStampDoesNotInheritTheCapabilityGrant(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(append([]string{base}, append(append([]string{}, windowsSandboxRuntimeOwnedNames...), "abcdef0123456789")...)...)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime tree: %v", err)
	}

	// Grant a capability SID write on the root, inheritable, exactly as the ACL
	// plan does for a real runtime root.
	capability, err := windows.StringToSid("S-1-5-32-546")
	if err != nil {
		t.Fatalf("resolve the stand-in capability SID: %v", err)
	}
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_WRITE | windows.GENERIC_READ,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(capability),
		},
	}}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatalf("build the root DACL: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(root, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Skipf("cannot set an inheritable ACL here: %v", err)
	}

	// An ordinary runtime descendant DOES inherit it: that is the grant the
	// sandbox needs, and the precondition that makes this test meaningful.
	cache := filepath.Join(root, "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatalf("create the runtime cache: %v", err)
	}
	if granted, _ := stampDACLGrants(t, cache, capability); !granted {
		t.Skip("the inheritable grant did not reach an ordinary descendant here, so this case is not being exercised")
	}

	if err := writeWindowsRuntimeStampThroughHandle(root, "planhash"); err != nil {
		t.Fatalf("write the stamp: %v", err)
	}
	stamp := windowsSandboxRuntimeStampPath(root, testStampPlanHash)

	granted, inherits := stampDACLGrants(t, stamp, capability)
	if granted {
		t.Error("the stamp grants the capability SID; a sandboxed command could overwrite the attestation about its own setup")
	}
	if inherits {
		t.Error("the stamp DACL is not protected, so the root's inheritable capability grant still applies to it")
	}

	// And setup can still read what it wrote, or the protection would have
	// locked out doctor and every later elevated command.
	body, err := os.ReadFile(stamp)
	if err != nil || string(body) != "planhash" {
		t.Fatalf("the stamp is unreadable by its own writer (%q, err %v)", body, err)
	}
}

func unsafePointerOfSID(ace *windows.ACCESS_ALLOWED_ACE) unsafe.Pointer {
	return unsafe.Pointer(&ace.SidStart)
}

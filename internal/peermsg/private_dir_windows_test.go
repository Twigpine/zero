//go:build windows

package peermsg

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestEnsurePrivateDirAppliesOwnerOnlyProtectedDACL(t *testing.T) {
	const directoryAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff

	path := filepath.Join(t.TempDir(), "zero", "peers", "registry")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	worldSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	var pinner runtime.Pinner
	pinner.Pin(worldSID)
	defer pinner.Unpin()
	broadDACL, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(worldSID),
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		broadDACL,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if !windowsDirectoryDACLContains(t, path, worldSID) {
		t.Fatal("test setup did not grant the broad Everyone ACE")
	}
	if err := EnsurePrivateDir(path); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("runtime directory DACL inherits access from its parent")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		t.Fatal(err)
	}
	tokenOwner, err := windowsTokenOwner(windows.GetCurrentProcessToken())
	if err != nil {
		t.Fatal(err)
	}
	if !owner.Equals(user.User.Sid) && !owner.Equals(tokenOwner) {
		t.Fatalf("runtime directory owner = %s, want user %s or token owner %s", owner.String(), user.User.Sid.String(), tokenOwner.String())
	}
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	wantTrustees := []*windows.SID{user.User.Sid, systemSID}
	seen := make([]bool, len(wantTrustees))
	for index := uint32(0); ; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			break
		}
		hasFullAccess := ace.Mask == windows.GENERIC_ALL || ace.Mask&directoryAllAccess == directoryAllAccess
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !hasFullAccess {
			t.Fatalf("DACL ACE %d has type %d and mask %#x", index, ace.Header.AceType, ace.Mask)
		}
		trustee := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		matched := false
		for trusteeIndex, wanted := range wantTrustees {
			if trustee.Equals(wanted) {
				seen[trusteeIndex] = true
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("DACL ACE %d grants access to unexpected trustee %s", index, trustee.String())
		}
	}
	for index, found := range seen {
		if !found {
			t.Fatalf("DACL does not grant access to required trustee %s", wantTrustees[index].String())
		}
	}
}

func windowsDirectoryDACLContains(t *testing.T, path string, wanted *windows.SID) bool {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	for index := uint32(0); ; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return false
		}
		if (*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(wanted) {
			return true
		}
	}
}

func TestSecurePrivateDirectoryRejectsOwnerMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private")
	if err := EnsurePrivateDir(path); err != nil {
		t.Fatal(err)
	}
	handle, err := openWindowsDirectory(path, windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.SYNCHRONIZE|windows.READ_CONTROL|windows.WRITE_DAC)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	descriptor, _, _, err := privateDirectoryDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	unexpectedOwner, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	if err := securePrivateDirectory(handle, path, descriptor, unexpectedOwner); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("owner mismatch error = %v", err)
	}
}

func TestSecurePrivateDirectoryReportsDACLWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private")
	if err := EnsurePrivateDir(path); err != nil {
		t.Fatal(err)
	}
	handle, err := openWindowsDirectory(path, windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.SYNCHRONIZE|windows.READ_CONTROL)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	descriptor, userSID, tokenOwnerSID, err := privateDirectoryDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	if err := securePrivateDirectory(handle, path, descriptor, userSID, tokenOwnerSID); err == nil || !strings.Contains(err.Error(), "secure private runtime directory") {
		t.Fatalf("DACL write error = %v", err)
	}
}

func TestClosePrivateWindowsHandleReportsFailure(t *testing.T) {
	err := closePrivateWindowsHandle(windows.Handle(0), `C:\private`)
	if err == nil || !strings.Contains(err.Error(), "close private runtime directory") {
		t.Fatalf("close handle error = %v", err)
	}
}

func TestEnsurePrivateDirRejectsWindowsReparseParent(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create Windows directory symlink: %v", err)
	}
	if err := EnsurePrivateDir(filepath.Join(link, "peers")); err == nil {
		t.Fatal("expected reparse-point parent to be rejected")
	}
}

func TestEnsurePrivateDirRejectsWindowsFileComponent(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(filepath.Join(file, "peers")); err == nil {
		t.Fatal("expected file path component to be rejected")
	}
}

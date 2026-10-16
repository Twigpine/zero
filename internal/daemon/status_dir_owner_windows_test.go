//go:build windows

package daemon

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func secureStatusTestDirPlatform(t *testing.T, dir string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := windows.SecurityDescriptorFromString(
		fmt.Sprintf("O:%sD:P(A;OICI;GA;;;%s)(A;OICI;GA;;;SY)", user.User.Sid.String(), user.User.Sid.String()),
	)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(
		dir,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		t.Fatal(err)
	}
}

func broadenStatusTestDirPlatform(t *testing.T, dir string) {
	t.Helper()
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
		dir,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		broadDACL,
		nil,
	); err != nil {
		t.Fatal(err)
	}
}

func TestCheckStatusDirOwnerRejectsBroadDACL(t *testing.T) {
	dir := t.TempDir()
	secureStatusTestDirPlatform(t, dir)
	broadenStatusTestDirPlatform(t, dir)

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	info, err := root.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	err = checkStatusDirOwner(root, info)
	if err == nil || !strings.Contains(err.Error(), "unexpected trustee") {
		t.Fatalf("checkStatusDirOwner error = %v, want broad DACL rejection", err)
	}
}

//go:build windows

package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// The creation observer checks the initial descriptor; the publication observer
// checks the destination DACL transfer. Both boundaries must exclude Everyone.
func TestProtectStagingCopiesRestrictiveDACL(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	destinationACE := "(A;;FA;;;" + sid + ")"
	dir := t.TempDir()

	// Make the parent more permissive than the destination so an inherited
	// staging DACL is observably broader than the destination's.
	if err := grantEveryoneInheritableDACL(dir); err != nil {
		t.Skipf("cannot widen the parent directory DACL on this filesystem: %v", err)
	}

	target := filepath.Join(dir, "restricted.txt")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	restricted, err := windows.SecurityDescriptorFromString("D:P" + destinationACE)
	if err != nil {
		t.Skipf("cannot build the restrictive test descriptor: %v", err)
	}
	ownerOnly, _, err := restricted.DACL()
	if err != nil {
		t.Skipf("cannot read the restrictive test DACL: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(
		target,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, ownerOnly, nil,
	); err != nil {
		t.Skipf("cannot apply a restrictive DACL on this filesystem: %v", err)
	}
	if got, err := readDACLString(target); err != nil {
		t.Skipf("cannot read back the restrictive destination DACL: %v", err)
	} else if !strings.Contains(got, destinationACE) {
		t.Skipf("the restrictive DACL did not take effect on this filesystem: %q", got)
	}

	observedCreation := 0
	previousCreation := privateCreationObserver
	privateCreationObserver = func(path string) {
		observedCreation++
		acl, err := readDACLString(path)
		if err != nil || !strings.Contains(acl, ";;;"+sid+")") || strings.Contains(acl, ";;;WD)") || !strings.Contains(acl, "D:P") {
			t.Fatalf("initial staging DACL = %q, %v", acl, err)
		}
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() && info.Size() != 0 {
			t.Fatalf("observer did not run at creation: %v, %v", info, err)
		}
	}
	defer func() { privateCreationObserver = previousCreation }()
	var (
		stagingDACL string
		captureErr  error
	)
	previous := stagingProtectionObserver
	stagingProtectionObserver = func(stagingPath string) {
		stagingDACL, captureErr = readDACLString(stagingPath)
	}
	defer func() { stagingProtectionObserver = previous }()

	if err := WriteFileAtomic(target, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if captureErr != nil {
		t.Fatalf("reading the staging DACL before publication: %v", captureErr)
	}
	if stagingDACL == "" {
		t.Fatal("staging DACL was not observed: the protection hook did not run before publication")
	}
	if !strings.Contains(stagingDACL, destinationACE) {
		t.Fatalf("staging DACL = %q, want the current-user-only destination DACL", stagingDACL)
	}
	if strings.Contains(stagingDACL, ";;;WD)") {
		t.Fatalf("staging DACL = %q is broader than the destination (inherited Everyone)", stagingDACL)
	}

	failure := errors.New("replace failed")
	if err := writeFileAtomic(target, []byte("must not land"), 0o600, func(string, string) error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "new" {
		t.Fatalf("failure changed destination: %q, %v", got, err)
	}
	stagingDir, err := CreatePrivateTempDir(dir, ".zero-fmt-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(stagingDir); err != nil {
		t.Fatal(err)
	}
	if observedCreation != 3 {
		t.Fatalf("initial creation observations: %d", observedCreation)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".zero-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("staging leftovers: %v, %v", leftovers, err)
	}
	fresh := filepath.Join(dir, "fresh")
	if err := WriteFileAtomic(fresh, []byte("public"), 0o644); err != nil {
		t.Fatal(err)
	}
	inherited, err := readDACLString(fresh)
	if err != nil || !strings.Contains(inherited, ";;;WD)") {
		t.Fatalf("new file lost inheritance: %q, %v", inherited, err)
	}
	after, err := readDACLString(target)
	if err != nil {
		t.Fatalf("reading the destination DACL after replacement: %v", err)
	}
	if !strings.Contains(after, destinationACE) || strings.Contains(after, ";;;WD)") {
		t.Fatalf("destination DACL after replacement = %q, want the original current-user-only DACL", after)
	}
}

// grantEveryoneInheritableDACL replaces path's DACL with a single inheritable
// grant of full control to Everyone (S-1-1-0), making it broader than any
// current-user-only destination inside it.
func grantEveryoneInheritableDACL(path string) error {
	everyone, err := windows.StringToSid("S-1-1-0")
	if err != nil {
		return err
	}
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_WELL_KNOWN_GROUP,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	)
}

// readDACLString returns the SDDL DACL portion of path's security descriptor.
func readDACLString(path string) (string, error) {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	if descriptor == nil {
		return "", nil
	}
	text := descriptor.String()
	if index := strings.Index(text, "D:"); index >= 0 {
		return text[index:], nil
	}
	return text, nil
}

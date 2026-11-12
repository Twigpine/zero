//go:build windows

package tools

import (
	"testing"

	"golang.org/x/sys/windows"
)

// writeOnlyFileMask is FILE_GENERIC_WRITE, and deliberately not FILE_READ_DATA:
// os.Stat still reports the file and os.WriteFile still replaces it, but
// os.ReadFile is denied. Windows has no chmod, so the write-only shape has to be
// expressed as a DACL.
//
// FILE_READ_ATTRIBUTES keeps os.Stat cheap, DELETE lets t.TempDir clean up, and
// WRITE_DAC is required for the restore: an OWNER_RIGHTS ACE replaces the
// owner's implicit right to rewrite the descriptor, so it must be granted here.
const writeOnlyFileMask = "0x170196"

// makeFileWriteOnly replaces the file's DACL with a protected owner-only ACE
// that grants everything except reading its bytes, and returns a func restoring
// the descriptor it found.
func makeFileWriteOnly(t *testing.T, path string) func() {
	t.Helper()
	original, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Skipf("cannot read the current DACL: %v", err)
	}
	originalDACL, _, err := original.DACL()
	if err != nil {
		t.Skipf("cannot parse the current DACL: %v", err)
	}
	writeOnly, err := windows.SecurityDescriptorFromString("D:P(A;;" + writeOnlyFileMask + ";;;OW)")
	if err != nil {
		t.Skipf("cannot build a write-only security descriptor: %v", err)
	}
	dacl, _, err := writeOnly.DACL()
	if err != nil {
		t.Skipf("cannot read the write-only DACL: %v", err)
	}
	if err := setFileDACL(path, dacl, true); err != nil {
		t.Skipf("cannot apply a write-only DACL on this filesystem: %v", err)
	}
	return func() {
		if err := setFileDACL(path, originalDACL, false); err != nil {
			t.Fatalf("cannot restore the original DACL: %v", err)
		}
	}
}

func setFileDACL(path string, dacl *windows.ACL, protected bool) error {
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if protected {
		info |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		info |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil)
}

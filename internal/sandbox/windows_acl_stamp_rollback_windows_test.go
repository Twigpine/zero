//go:build windows

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// daclOf returns the DACL of path as one line per access-control entry.
//
// NOT the SDDL of the whole descriptor. That string also carries the owner, the
// group and the control flags, and none of those are what this test is about:
// SetSecurityInfo sets SE_DACL_AUTO_INHERITED when it writes a DACL, and
// GetNamedSecurityInfo does not report owner and group identically on every
// machine, so comparing full SDDL fails on a difference that grants nobody
// anything. The entries are the access, so the entries are what gets compared.
func daclOf(t *testing.T, path string) []string {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read the DACL of %s: %v", path, err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatalf("read the DACL entries of %s: %v", path, err)
	}
	if dacl == nil {
		return nil
	}
	entries := make([]string, 0, dacl.AceCount)
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			t.Fatalf("read ACE %d of %s: %v", index, path, err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		entries = append(entries, fmt.Sprintf("type=%d flags=%#x mask=%#x sid=%s",
			ace.Header.AceType, ace.Header.AceFlags, ace.Mask, sid.String()))
	}
	return entries
}

// THE ACE AND ITS STAMP ARE ONE TRANSACTION.
//
// SetSecurityInfo commits before the stamp is written, and the apply returns no
// rollback closure on its error paths, so the caller's compensations have
// nothing to undo. A failed setup therefore reported failure while leaving the
// capability grant on a pre-existing runtime root: the tree stays writable by
// the restricted token and nothing on disk records that it should not be.
func TestAStampFailureRestoresTheCommittedACL(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(append([]string{base}, append(append([]string{}, windowsSandboxRuntimeOwnedNames...), "abcdef0123456789")...)...)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime tree: %v", err)
	}
	before := daclOf(t, root)

	previous := windowsACLStampWriteHook
	windowsACLStampWriteHook = func(string) error { return errors.New("disk full") }
	t.Cleanup(func() { windowsACLStampWriteHook = previous })

	plan := WindowsACLPlan{Entries: []WindowsACLEntry{
		{Action: WindowsACLAllowWrite, Path: root, Capability: "S-1-5-32-546"},
	}}
	rollback, err := applyWindowsACLPlanWithStamp(plan, stampRequestFor(t, root, "planhash"))
	if err == nil {
		if rollback != nil {
			_ = rollback()
		}
		t.Fatal("the apply reported success even though the stamp could not be written")
	}
	if rollback != nil {
		// A rollback closure here would be the other acceptable shape, but the
		// caller only receives one on success, so it must not be relied on.
		_ = rollback()
	}

	if after := daclOf(t, root); !slices.Equal(after, before) {
		t.Errorf("the committed capability grant survived a failed setup:\nbefore %s\nafter  %s",
			strings.Join(before, " | "), strings.Join(after, " | "))
	}
	if _, err := os.Stat(filepath.Join(root, windowsSandboxRuntimeStampName(testStampPlanHash))); err == nil {
		t.Error("a stamp exists even though the stamp step failed")
	}
}

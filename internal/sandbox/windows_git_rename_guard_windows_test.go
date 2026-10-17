//go:build windows

package sandbox

import (
	"testing"

	"golang.org/x/sys/windows"
)

// The mask is the whole point of the action, so it is asserted bit by bit.
//
// Renaming a directory needs DELETE on the directory itself, so denying DELETE
// is what stops .git being replaced. Everything else in the mask is there to
// stop the principal removing the guard: WRITE_DAC would let it rewrite the
// DACL, WRITE_OWNER would let it take ownership and then rewrite the DACL.
//
// What must NOT be in it matters just as much. FILE_GENERIC_WRITE would stop git
// writing index, objects and refs. FILE_DELETE_CHILD would stop git deleting its
// own lock files and refs. Either one turns a rename guard into a broken repo.
func TestDenyDeleteMaskStopsRenameWithoutStoppingGit(t *testing.T) {
	mode, mask, err := windowsACLAccess(WindowsACLDenyDelete)
	if err != nil {
		t.Fatalf("windowsACLAccess(deny-delete): %v", err)
	}
	if mode != windows.DENY_ACCESS {
		t.Fatalf("access mode = %v, want DENY_ACCESS", mode)
	}

	for _, required := range []struct {
		name string
		bit  windows.ACCESS_MASK
	}{
		{"DELETE", windows.DELETE},
		{"WRITE_DAC", windows.WRITE_DAC},
		{"WRITE_OWNER", windows.WRITE_OWNER},
	} {
		if mask&required.bit == 0 {
			t.Errorf("mask %#x is missing %s, so the guard can be removed or bypassed", mask, required.name)
		}
	}
	for _, forbidden := range []struct {
		name   string
		bit    windows.ACCESS_MASK
		breaks string
	}{
		{"FILE_GENERIC_WRITE", windows.FILE_GENERIC_WRITE, "git writing index/objects/refs"},
		{"FILE_DELETE_CHILD", windowsFileDeleteChild, "git deleting its own lock files and refs"},
	} {
		if mask&forbidden.bit != 0 {
			t.Errorf("mask %#x includes %s, which breaks %s", mask, forbidden.name, forbidden.breaks)
		}
	}
}

// Inheritance is the other half. An inherited deny would reach every file inside
// .git and stop git deleting anything at all, so this ACE has to apply to the
// directory object alone while the other actions keep inheriting as before.
func TestDenyDeleteDoesNotInheritWhileOtherActionsStillDo(t *testing.T) {
	entries := []WindowsACLEntry{
		{Action: WindowsACLDenyDelete, Path: `C:\work\repo\.git`, Capability: "S-1-5-32-9999"},
		{Action: WindowsACLDenyWrite, Path: `C:\work\repo\.zero`, Capability: "S-1-5-32-9999"},
		{Action: WindowsACLAllowWrite, Path: `C:\work\repo`, Capability: "S-1-5-32-9999"},
	}

	_, access, err := prepareWindowsACLPathGroupEntries(entries, true, nil)
	if err != nil {
		t.Fatalf("prepareWindowsACLPathGroupEntries: %v", err)
	}
	if len(access) != len(entries) {
		t.Fatalf("got %d access entries, want %d", len(access), len(entries))
	}
	if access[0].Inheritance != windows.NO_INHERITANCE {
		t.Errorf("deny-delete inheritance = %#x, want NO_INHERITANCE; an inherited deny would stop git deleting inside .git", access[0].Inheritance)
	}
	for index, entry := range entries[1:] {
		if got := access[index+1].Inheritance; got != windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT {
			t.Errorf("%s inheritance = %#x, want the directory default to be unchanged", entry.Action, got)
		}
	}
}

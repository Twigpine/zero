//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// gitGuardDenyDeleteApplied reports whether the applied DACL on path carries a
// DENY ace granting DELETE to sid. Read back from the object rather than from
// the plan, because the plan is what was already wrong.
func gitGuardDenyDeleteApplied(t *testing.T, path string, sid string) bool {
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
		t.Fatalf("parse the capability SID %q: %v", sid, err)
	}
	// GetAce hands back a generic header that is reinterpreted here. Sound only
	// for the fixed-layout ACE types: an object ACE carries Flags and two GUIDs
	// ahead of the trustee, so SidStart would land mid-structure. Allowed and
	// denied ACEs share that fixed layout, and nothing under test builds an
	// object ACE, so a type this does not recognise is skipped rather than
	// decoded into a nonsense SID.
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !aceSID.Equals(wanted) {
			continue
		}
		if ace.Mask&windows.DELETE != 0 {
			return true
		}
	}
	return false
}

// THE GUARD BELONGS TO THE GRANT, NOT TO ONE PLANNER.
//
// The allow-write mask both backends share includes DELETE, and it inherits from
// the write root onto .git. What protects git is attached to .git/config and
// .git/hooks as OBJECTS, so renaming .git aside and recreating it discards those
// carveouts: the fresh config and hooks inherit the workspace allow with no deny
// of their own, which hands back credential.helper and core.hooksPath.
//
// The principal planner denied DELETE on .git; the capability planner did not,
// and the capability backend is the default. So on the reachable path the
// caller's own token and the capability restricting SID could both authorise the
// rename.
//
// Read back from the applied object, not asserted on the plan, because the plan
// is exactly what was wrong. DACL edits on a user-owned directory need no
// Administrator rights, so this runs unelevated.
func TestCapabilityPlanDeniesDeleteOnGit(t *testing.T) {
	workspace := t.TempDir()
	gitDir := filepath.Join(workspace, ".git")
	if err := os.MkdirAll(gitDir, 0o700); err != nil {
		t.Fatal(err)
	}

	config := WindowsSandboxCommandConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     workspace,
		WorkspaceRoots: []string{workspace},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: workspace}},
			},
		},
	}
	plan, err := BuildWindowsACLPlan(config)
	if err != nil {
		t.Fatalf("BuildWindowsACLPlan: %v", err)
	}
	sid, err := windowsCapabilitySIDForWriteRoot(config, workspace)
	if err != nil {
		t.Fatalf("resolve the workspace capability SID: %v", err)
	}
	// SETUP: the plan must actually grant this root, or the guard below would be
	// vacuously satisfied by a plan that grants nothing.
	granted := false
	for _, entry := range plan.Entries {
		if entry.Action == WindowsACLAllowWrite && entry.Path == workspace {
			granted = true
		}
	}
	if !granted {
		t.Fatalf("SETUP INVALID: the capability plan does not grant write on %s", workspace)
	}

	if _, err := applyWindowsACLPlan(plan); err != nil {
		t.Skipf("cannot apply an ACL plan here: %v", err)
	}

	if !gitGuardDenyDeleteApplied(t, gitDir, sid) {
		t.Fatalf(".git carries no deny-DELETE ace for the capability SID %s, so the sandboxed token can rename it and recreate it without the config and hooks carveouts", sid)
	}
}

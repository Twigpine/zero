//go:build windows

package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// appliedDenyMask returns the DENY mask an applied DACL carries for sid, read
// back off the object rather than asserted on the plan, because the plan is what
// was wrong.
func appliedDenyMask(t *testing.T, path string, sid string) uint32 {
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
	// Fixed-layout ACE types only; an object ACE puts GUIDs ahead of the trustee
	// so SidStart would land mid-structure. Nothing here builds one, and an
	// unrecognised type is skipped rather than decoded into a nonsense SID.
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
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

// A GUARD ON AN OBJECT THAT DOES NOT EXIST YET IS NOT A GUARD.
//
// The capability planner emitted the deny-delete on .git and the deny-writes on
// .git\config and .git\hooks without Materialize. On a workspace that had no
// .git when setup ran, the first apply pass skipped all three as missing and the
// deferred pass skipped them again, because nothing else in the capability plan
// created them. Setup recorded success anyway.
//
// A later git init then created .git, config and hooks beneath the
// already-granted workspace. They inherit the workspace allow, which carries
// DELETE, with no object-specific deny of their own, so the sandboxed command
// could rename .git aside, recreate it, and get credential.helper and
// core.hooksPath back.
//
// This is the DEFAULT backend, so the weaker of the two planners' rules was the
// one almost every Windows user got. The principal plan was never affected: its
// carveouts are materialized, which both applies them and creates .git as their
// parent so the deferred retry lands the deny-delete.
//
// Read back off the applied object, and driven through the same
// BuildWindowsACLPlan + applyWindowsACLPlan pair both setup tiers use. DACL edits
// on a user-owned directory need no Administrator rights.
func TestCapabilityGitGuardReachesAWorkspaceThatGetsGitLater(t *testing.T) {
	workspace := t.TempDir()
	gitDir := filepath.Join(workspace, ".git")

	// SETUP: no .git at setup time. That is the entire case.
	if _, err := os.Stat(gitDir); err == nil {
		t.Fatal("SETUP INVALID: .git already exists, so this is the covered case, not the uncovered one")
	}

	config := WindowsSandboxCommandConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     workspace,
		WorkspaceRoots: []string{workspace},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind: FileSystemRestricted,
				WriteRoots: []WritableRoot{{
					Root:                   workspace,
					ReadOnlySubpaths:       gitMetadataWriteCarveouts(workspace),
					ProtectedMetadataNames: sandboxFullyProtectedMetadataNames,
				}},
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
	if _, err := applyWindowsACLPlan(plan); err != nil {
		t.Skipf("cannot apply an ACL plan here: %v", err)
	}

	// The workspace grant really landed, so a missing deny below is a missing
	// deny and not an apply that did nothing.
	if appliedAllowMask(t, workspace, sid) == 0 {
		t.Fatal("SETUP INVALID: the workspace carries no allow ACE for the capability SID, so the plan did not apply")
	}

	// Now git arrives, the way it does for a scaffold or a clone after setup.
	if out, err := exec.Command("git", "init", workspace).CombinedOutput(); err != nil {
		t.Skipf("git init unavailable here: %v\n%s", err, out)
	}

	if mask := appliedDenyMask(t, gitDir, sid); mask&uint32(windows.DELETE) == 0 {
		t.Fatalf(".git carries no deny-DELETE for the capability SID after git init (mask=%#x); the sandboxed token can rename it aside and recreate it without the config and hooks carveouts", mask)
	}
	hooks := filepath.Join(gitDir, "hooks")
	if _, err := os.Stat(hooks); err == nil {
		if mask := appliedDenyMask(t, hooks, sid); mask == 0 {
			t.Error(".git\\hooks carries no deny for the capability SID, so core.hooksPath is writable from inside the sandbox")
		}
	}
}

// appliedAllowMask is the allow-side companion, used only as a setup guard.
func appliedAllowMask(t *testing.T, path string, sid string) uint32 {
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
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		if !(*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(wanted) {
			continue
		}
		mask |= uint32(ace.Mask)
	}
	return mask
}

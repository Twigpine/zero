//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// windowsPathDeniesDelete reports whether the object's real DACL carries a deny
// ACE covering DELETE for the given SID.
//
// The plan-shape tests assert what the planner emits. This reads what actually
// landed on disk, which is the gap that let the guard go missing: the plan was
// right the whole time and the apply silently dropped it.
func windowsPathDeniesDelete(t *testing.T, path, sid string) bool {
	t.Helper()
	handle, _, err := openWindowsACLTarget(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read DACL for %s: %v", path, err)
	}
	// SDDL rather than walking the ACE buffer by hand: this x/sys does not export
	// an ACE enumerator, and unsafe pointer arithmetic in a test that exists to
	// catch a security regression is its own hazard.
	//
	// An ACE renders as (type;flags;rights;guid;inherit_guid;sid), so the deny
	// entries are the ones whose type field is D. Rights come back as SDDL
	// abbreviations when they fit and as a hex mask when they do not, so both are
	// accepted; SD is the abbreviation for DELETE.
	for _, ace := range strings.Split(descriptor.String(), "(") {
		fields := strings.Split(strings.TrimSuffix(strings.TrimSpace(ace), ")"), ";")
		if len(fields) != 6 || fields[0] != "D" || !strings.EqualFold(fields[5], sid) {
			continue
		}
		rights := fields[2]
		if strings.Contains(rights, "SD") {
			return true
		}
		if mask, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(rights), "0x"), 16, 32); err == nil {
			if uint32(mask)&uint32(windows.DELETE) != 0 {
				return true
			}
		}
	}
	return false
}

// THE GUARD MUST REACH DISK ON A WORKSPACE THAT HAD NO .git.
//
// This is the case the whole plan is built around and it was the one case where
// the guard was absent. .git deliberately carries no Materialize, because an
// empty .git breaks `git init`. Groups are applied in ascending path order, so
// the .git group ran while .git did not yet exist, was skipped as a
// non-materializing missing target, and nothing revisited it once the
// .git\config carveout created .git as its parent chain moments later.
//
// Four tests already covered the deny-delete mask and the plan emitting it.
// None of them applied the plan and looked at the object, which is exactly why
// a green suite said nothing.
func TestGitRenameGuardReachesDiskOnAWorkspaceWithoutGit(t *testing.T) {
	const principal = testPrincipalSID // Unaliased, so it renders literally in SDDL.
	workspace := t.TempDir()
	gitDir := filepath.Join(workspace, ".git")
	if _, err := os.Stat(gitDir); err == nil {
		t.Fatal("the workspace already has .git, so this proves nothing")
	}

	plan, err := buildWindowsPrincipalACLPlan(windowsPrincipalACLInput{
		PrincipalSID: principal,
		WriteRoots: []WritableRoot{{
			Root:                   workspace,
			ProtectedMetadataNames: sandboxFullyProtectedMetadataNames,
			ReadOnlySubpaths:       gitMetadataWriteCarveouts(workspace),
		}},
	})
	if err != nil {
		t.Fatalf("buildWindowsPrincipalACLPlan: %v", err)
	}

	rollback, err := applyWindowsACLPlan(plan)
	if err != nil {
		t.Fatalf("applyWindowsACLPlan: %v", err)
	}
	t.Cleanup(func() {
		if rollback != nil {
			_ = rollback()
		}
	})

	// The carveouts create .git on the way to .git\config.
	if _, err := os.Stat(gitDir); err != nil {
		t.Fatalf(".git was never created by the carveout materialization: %v", err)
	}
	if !windowsPathDeniesDelete(t, gitDir, principal) {
		t.Fatal("no deny-delete ACE on .git after applying the plan: the principal can rename it aside and recreate it without the config and hooks carveouts")
	}
}

// The same guard on a workspace that already had .git must keep working. This
// case was already correct, and it is kept so a fix aimed at the fresh
// workspace cannot quietly trade one for the other.
func TestGitRenameGuardStillReachesDiskWhenGitAlreadyExists(t *testing.T) {
	const principal = testPrincipalSID
	workspace := t.TempDir()
	gitDir := filepath.Join(workspace, ".git")
	if err := os.Mkdir(gitDir, 0o700); err != nil {
		t.Fatalf("seed .git: %v", err)
	}

	plan, err := buildWindowsPrincipalACLPlan(windowsPrincipalACLInput{
		PrincipalSID: principal,
		WriteRoots: []WritableRoot{{
			Root:                   workspace,
			ProtectedMetadataNames: sandboxFullyProtectedMetadataNames,
			ReadOnlySubpaths:       gitMetadataWriteCarveouts(workspace),
		}},
	})
	if err != nil {
		t.Fatalf("buildWindowsPrincipalACLPlan: %v", err)
	}
	rollback, err := applyWindowsACLPlan(plan)
	if err != nil {
		t.Fatalf("applyWindowsACLPlan: %v", err)
	}
	t.Cleanup(func() {
		if rollback != nil {
			_ = rollback()
		}
	})

	if !windowsPathDeniesDelete(t, gitDir, principal) {
		t.Fatal("no deny-delete ACE on a .git that already existed")
	}
}

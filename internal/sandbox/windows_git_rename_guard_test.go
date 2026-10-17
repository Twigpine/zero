package sandbox

import (
	"path/filepath"
	"testing"
)

// A sandbox principal must not be able to REPLACE .git.
//
// The write-denied carveouts are attached to .git/config and .git/hooks as
// objects. Rename .git aside, recreate it, and those objects are gone: the fresh
// config and hooks inherit the workspace allow with no deny of their own, which
// hands back credential.helper and core.hooksPath, and with them arbitrary code
// execution on the next git command.
//
// .git cannot join sandboxFullyProtectedMetadataNames to fix this, because that
// list emits DenyWrite, whose mask includes FILE_GENERIC_WRITE. Git has to write
// index, objects and refs. So the directory needs DELETE denied on itself while
// staying writable underneath.
func TestThePrincipalCannotRenameTheGitDirectory(t *testing.T) {
	root := filepath.FromSlash("/ws/repo")
	plan, err := buildWindowsPrincipalACLPlan(windowsPrincipalACLInput{
		PrincipalSID: "S-1-5-21-1-2-3-1001",
		WriteRoots: []WritableRoot{{
			Root:                   root,
			ProtectedMetadataNames: sandboxFullyProtectedMetadataNames,
			ReadOnlySubpaths:       gitMetadataWriteCarveouts(root),
		}},
	})
	if err != nil {
		t.Fatalf("buildWindowsPrincipalACLPlan: %v", err)
	}

	// The plan normalizes every write root before it names an ACE, so the
	// expected path has to be normalized too or this compares two spellings of
	// the same directory. Hardcoding a drive letter instead would pass on
	// Windows and fail everywhere else, since the builder is portable code.
	gitDir := filepath.Join(normalizeProfilePath(root), ".git")
	var denyDelete *WindowsACLEntry
	for index := range plan.Entries {
		if plan.Entries[index].Action == WindowsACLDenyDelete && plan.Entries[index].Path == gitDir {
			denyDelete = &plan.Entries[index]
			break
		}
	}
	if denyDelete == nil {
		t.Fatalf("no deny-delete entry for %s, so the principal can rename .git and recreate it without the carveouts:\n%#v", gitDir, plan.Entries)
	}
	if denyDelete.Capability != "S-1-5-21-1-2-3-1001" {
		t.Errorf("deny-delete names %q, want the principal SID", denyDelete.Capability)
	}
}

// The guard must not become a write ban. Git writes constantly inside .git, so a
// deny that reached the children would break every commit rather than just the
// rename. It also must not be materialized into existence: .git is git's to
// create, and an empty .git directory made by setup breaks `git init`.
func TestTheGitRenameGuardDoesNotBlockGitsOwnWrites(t *testing.T) {
	root := filepath.FromSlash("/ws/repo")
	plan, err := buildWindowsPrincipalACLPlan(windowsPrincipalACLInput{
		PrincipalSID: "S-1-5-21-1-2-3-1001",
		WriteRoots: []WritableRoot{{
			Root:                   root,
			ProtectedMetadataNames: sandboxFullyProtectedMetadataNames,
			ReadOnlySubpaths:       gitMetadataWriteCarveouts(root),
		}},
	})
	if err != nil {
		t.Fatalf("buildWindowsPrincipalACLPlan: %v", err)
	}

	gitDir := filepath.Join(normalizeProfilePath(root), ".git")
	for _, entry := range plan.Entries {
		if entry.Path != gitDir {
			continue
		}
		if entry.Action == WindowsACLDenyWrite {
			t.Errorf("deny-write on %s would stop git writing index/objects/refs", gitDir)
		}
		if entry.Action == WindowsACLDenyDelete && entry.Materialize {
			t.Errorf("the rename guard materializes %s; git must create it, an empty .git breaks git init", gitDir)
		}
	}

	// The existing carveouts must survive unchanged.
	for _, want := range []string{filepath.Join(gitDir, "config"), filepath.Join(gitDir, "hooks")} {
		found := false
		for _, entry := range plan.Entries {
			if entry.Action == WindowsACLDenyWrite && entry.Path == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the write-deny carveout for %s disappeared", want)
		}
	}
}

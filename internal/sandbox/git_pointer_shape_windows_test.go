//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func pointerWorkspace(t *testing.T) (workspace string, pointer string) {
	t.Helper()
	workspace = t.TempDir()
	pointer = filepath.Join(workspace, ".git")
	if err := os.WriteFile(pointer, []byte("gitdir: ../real/.git/worktrees/w\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return workspace, pointer
}

func pointerPlanConfig(t *testing.T, workspace string) WindowsSandboxCommandConfig {
	t.Helper()
	return WindowsSandboxCommandConfig{
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
}

// THE SHAPE HAS TO SURVIVE THE STAGE BOUNDARY, NOT JUST BE CORRECT AT STAGE ONE.
//
// gitMetadataWriteCarveoutSpecs types a linked worktree's .git as a file. That
// typed result was flattened to a path before ACL planning, and the planner tried
// to recover the shape by deriving suffixes under a sentinel root whose .git never
// exists. The derivation therefore always took the directory branch, and a bare
// <root>\.git could never match as a file.
//
// The window that turns a wrong plan into damage is the caller-to-elevated-helper
// gap: the profile is built in the user's shell and the plan is applied in a
// separately launched helper, across the UAC prompt. If the pointer goes away in
// between, the applier materializes a DIRECTORY at the pointer path and the
// worktree is broken, persistently, because the runner discards the rollback.
//
// Driven through BuildWindowsACLPlan and applyWindowsACLPlan with the pointer
// removed after planning, which is the only place this is visible. A stage-one
// assertion on specs[0].IsFile passed throughout the whole time the bug was live.
func TestALinkedWorktreePointerIsNeverMaterializedAsADirectory(t *testing.T) {
	workspace, pointer := pointerWorkspace(t)
	plan, err := BuildWindowsACLPlan(pointerPlanConfig(t, workspace))
	if err != nil {
		t.Fatalf("BuildWindowsACLPlan: %v", err)
	}

	// SETUP: the plan really does carry the pointer, typed as a file.
	var found bool
	for _, entry := range plan.Entries {
		if entry.Path == pointer && entry.Action == WindowsACLDenyWrite {
			found = true
			if !entry.MaterializeFile {
				t.Fatalf("the plan types %s as a directory; applying it would replace the worktree pointer with a directory", pointer)
			}
		}
	}
	if !found {
		t.Fatalf("SETUP INVALID: no deny-write entry for the pointer %s: %+v", pointer, plan.Entries)
	}

	// The window: the pointer goes away between planning and applying.
	if err := os.Remove(pointer); err != nil {
		t.Fatal(err)
	}
	if _, err := applyWindowsACLPlan(plan); err != nil {
		t.Skipf("cannot apply an ACL plan here: %v", err)
	}

	info, err := os.Lstat(pointer)
	if err != nil {
		// Nothing recreated is also acceptable: what must not happen is a directory.
		return
	}
	if info.IsDir() {
		t.Fatalf("a directory was created at the worktree pointer %s, which breaks the worktree", pointer)
	}
}

// Control: with the pointer present at apply, it stays a file and is not
// replaced. Without this, the assertion above could be satisfied by an apply
// that does nothing at all.
func TestALinkedWorktreePointerSurvivesAnApply(t *testing.T) {
	workspace, pointer := pointerWorkspace(t)
	plan, err := BuildWindowsACLPlan(pointerPlanConfig(t, workspace))
	if err != nil {
		t.Fatalf("BuildWindowsACLPlan: %v", err)
	}
	if _, err := applyWindowsACLPlan(plan); err != nil {
		t.Skipf("cannot apply an ACL plan here: %v", err)
	}
	info, err := os.Lstat(pointer)
	if err != nil {
		t.Fatalf("the pointer vanished across the apply: %v", err)
	}
	if info.IsDir() {
		t.Fatalf("the pointer at %s became a directory", pointer)
	}
	body, err := os.ReadFile(pointer)
	if err != nil || len(body) == 0 {
		t.Fatalf("the pointer lost its contents: body=%q err=%v", body, err)
	}
}

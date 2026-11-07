//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE UNELEVATED PLAN CAN BE APPLIED ON FRESH ROOTS.
//
// The unelevated command's plan grants both runtime candidates, but nothing on
// that tier created the one this process does not select, so with a fresh
// usable cache the runner refused its own plan with "windows ACL target does
// not exist" and the command never launched. The parent now creates whichever
// candidates are missing, with setup's own ownership and no-follow checks,
// before the runner applies. Driven through the real planner and the real
// runner-side apply, on fresh roots, with the preferred candidate selected and
// with the fallback selected.
func TestUnelevatedPlanAppliesOnFreshRuntimeRoots(t *testing.T) {
	for name, cacheInsideWorkspace := range map[string]bool{"preferred candidate": false, "fallback selected": true} {
		t.Run(name, func(t *testing.T) {
			isolateSandboxRuntimeRoots(t)
			config := testWindowsUnelevatedCommandConfig(t)
			if cacheInsideWorkspace {
				original := sandboxUserCacheDir
				sandboxUserCacheDir = func() (string, error) { return filepath.Join(config.WorkspaceRoots[0], ".cache"), nil }
				t.Cleanup(func() { sandboxUserCacheDir = original })
			}
			config.PermissionProfile = windowsSandboxProfileWithRuntime(config.PermissionProfile, config.WorkspaceRoots)
			candidates := windowsSandboxRuntimeCandidates(config.WorkspaceRoots)
			if len(candidates) == 0 {
				t.Fatal("SETUP INVALID: no runtime candidates")
			}
			for _, root := range candidates {
				if _, err := os.Lstat(root); err == nil {
					t.Fatalf("SETUP INVALID: candidate %s already exists", root)
				}
			}

			// What the parent does for this tier before the runner starts.
			if err := ensureMissingWindowsSandboxRuntimeCandidates(config.WorkspaceRoots); err != nil {
				t.Fatalf("ensureMissingWindowsSandboxRuntimeCandidates: %v", err)
			}
			for _, root := range candidates {
				if info, err := os.Lstat(root); err != nil || !info.IsDir() {
					t.Fatalf("candidate %s was not created: %v", root, err)
				}
			}
			// What the runner does: build and apply the plan for real, as this user.
			if err := ensureWindowsUnelevatedSetup(config); err != nil {
				t.Fatalf("the unelevated plan could not be applied on fresh roots: %v", err)
			}
		})
	}
}

// AN EXISTING CANDIDATE IS LEFT AS IT IS.
//
// This runs on every command, and an existing candidate carries the capability
// ACE the previous command applied. Re-preparing it would strip that grant and
// force a reapply on every launch.
func TestEnsureMissingCandidatesLeavesAnExistingOneAlone(t *testing.T) {
	isolateSandboxRuntimeRoots(t)
	config := testWindowsUnelevatedCommandConfig(t)
	candidates := windowsSandboxRuntimeCandidates(config.WorkspaceRoots)
	if len(candidates) != 2 {
		t.Fatalf("SETUP INVALID: want two candidates, got %v", candidates)
	}
	existing := candidates[0]
	if err := ensureRuntimeCandidateDir(existing); err != nil {
		t.Fatal(err)
	}
	rollback, err := applyWindowsACLPlan(WindowsACLPlan{Entries: []WindowsACLEntry{{
		Action: WindowsACLAllowWrite, Path: existing, Capability: windowsACLTestDenySID,
	}}})
	if err != nil {
		t.Fatalf("grant the existing candidate: %v", err)
	}
	t.Cleanup(func() { _ = rollback() })

	if err := ensureMissingWindowsSandboxRuntimeCandidates(config.WorkspaceRoots); err != nil {
		t.Fatal(err)
	}
	if !runtimeRootGrants(t, existing, windowsACLTestDenySID) {
		t.Fatalf("preparing the missing candidates rewrote the DACL of the existing one at %s", existing)
	}
	if info, err := os.Lstat(candidates[1]); err != nil || !info.IsDir() {
		t.Fatalf("the missing candidate %s was not created: %v", candidates[1], err)
	}
	// A file wearing a candidate's name is refused by name, since the apply
	// would otherwise put an ACE on it without complaint.
	if err := os.RemoveAll(candidates[1]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidates[1], []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = ensureMissingWindowsSandboxRuntimeCandidates(config.WorkspaceRoots)
	if err == nil || !strings.Contains(err.Error(), candidates[1]) || !strings.Contains(err.Error(), "not a plain directory") {
		t.Fatalf("a file standing in for a candidate was not refused by name: %v", err)
	}
}

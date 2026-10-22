//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CLEANUP AND SETUP DISAGREE ABOUT WHO OWNS THE RUNTIME DIRECTORY.
//
// cleanupSandboxRuntimeRoots reclaims inactive sibling workspace roots on an age
// and count policy, treating them as disposable cache state. Setup and its marker
// treat their DACL as durable provisioned state. When the reclaimed workspace runs
// again, command-side preparation recreates the SAME deterministic pathname as
// the ordinary caller, and the new directory inherits from its parent without the
// capability SID ACE elevated setup applied to the object that used to be there.
//
// The marker still validates, because it fingerprints pathnames and actions
// rather than the identity of the ACL-bearing object. So the restricted child
// launched and then failed its cache, temp and package-cache writes with a bare
// access-denied and nothing pointing at setup.
//
// Driven by applying a real plan, reclaiming the directory the way cleanup does,
// recreating the pathname the way an ordinary run does, and asking the check the
// command now makes. DACL edits on a user-owned directory need no Administrator
// rights, so this runs unelevated.
//
// Not covered here: an actual write performed by a real capability token, which
// needs a provisioned sandbox this box cannot create. The ACE presence is the
// fact the command consumes, and it is what this pins.
func TestRuntimeRootReclaimedAndRecreatedLosesItsCapability(t *testing.T) {
	cacheRoot := t.TempDir()
	previous := sandboxUserCacheDir
	t.Cleanup(func() { sandboxUserCacheDir = previous })
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }

	workspace := t.TempDir()
	runtimeRoot, ok := deterministicSandboxRuntimeRoot(canonicalSandboxWorkspaceRoot(workspace), canonicalSandboxWorkspaceRoot(cacheRoot))
	if !ok {
		t.Fatalf("SETUP INVALID: no deterministic runtime root for %s", workspace)
	}
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	config := WindowsSandboxCommandConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     workspace,
		WorkspaceRoots: []string{workspace},
		PermissionProfile: PermissionProfile{
			Runtime: &SandboxRuntime{Root: runtimeRoot},
			FileSystem: FileSystemPolicy{
				Kind: FileSystemRestricted,
				WriteRoots: []WritableRoot{
					{Root: workspace},
					{Root: runtimeRoot},
				},
			},
		},
	}

	plan, err := BuildWindowsACLPlan(config)
	if err != nil {
		t.Fatalf("BuildWindowsACLPlan: %v", err)
	}
	if _, err := applyWindowsACLPlan(plan); err != nil {
		t.Skipf("cannot apply an ACL plan here: %v", err)
	}

	// A provisioned machine passes, or the assertion below would be satisfied by a
	// check that refuses unconditionally.
	if err := verifyWindowsRuntimeRootCapability(config); err != nil {
		t.Fatalf("SETUP INVALID: a freshly provisioned runtime root was rejected: %v", err)
	}

	// Cleanup reclaims it, and the next ordinary run recreates the same pathname.
	if err := os.RemoveAll(runtimeRoot); err != nil {
		t.Fatalf("reclaim the runtime root the way cleanup does: %v", err)
	}
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		t.Fatalf("recreate the pathname the way an ordinary run does: %v", err)
	}

	err = verifyWindowsRuntimeRootCapability(config)
	if err == nil {
		t.Fatal("a recreated runtime root with no capability grant was accepted; the command would launch and then fail every sandboxed write with nothing explaining why")
	}
	message := err.Error()
	if !strings.Contains(message, runtimeRoot) {
		t.Errorf("the refusal does not name the root: %v", err)
	}
	if !strings.Contains(strings.ToLower(message), "sandbox setup") {
		t.Errorf("the refusal does not point at the remedy: %v", err)
	}

	// And the marker on its own still says everything is fine, which is exactly
	// why the object has to be checked separately.
	if _, statErr := os.Stat(filepath.Dir(runtimeRoot)); statErr != nil {
		t.Fatalf("the runtime parent vanished: %v", statErr)
	}
}

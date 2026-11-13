//go:build windows

package sandbox

import (
	"os"
	"testing"
)

// THE UNELEVATED MARKER ATTESTS TO A PLAN, NOT TO THE OBJECT THE COMMAND USES.
//
// cleanupSandboxRuntimeRoots reclaims inactive sibling runtime roots on an age
// and count policy. When the reclaimed workspace runs again, ordinary
// preparation recreates the SAME deterministic pathname with the caller-private
// DACL and no capability ACE. The serialized plan is unchanged, so the cached
// marker matched and setup returned before anything looked at the directory. The
// restricted child still carried the capability SID; the new object simply did
// not grant it, so every temp, package-cache and build-cache write failed after
// launch with a bare access denial and nothing pointing at setup.
//
// The restricted-token tier got an object check in this branch already. This one
// covers the tier that actually reaches it without elevation, and it must REAPPLY
// rather than refuse: this tier owns its plan, and telling an ordinary user to
// run elevated setup is both unnecessary and unfollowable for someone with no
// Administrator account.
//
// Driven through ensureWindowsUnelevatedSetup, which is what the command runner
// calls, rather than through the verifier helper: the helper was already correct,
// and the defect was that this path never consulted it. DACL edits on a
// user-owned directory need no Administrator rights, so this runs unelevated.
func TestUnelevatedSetupRestoresAReclaimedRuntimeRoot(t *testing.T) {
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
		SandboxLevel:   WindowsSandboxLevelUnelevated,
		PermissionProfile: PermissionProfile{
			Runtime: &SandboxRuntime{Root: runtimeRoot},
			FileSystem: FileSystemPolicy{
				Kind: FileSystemRestricted,
				WriteRoots: []WritableRoot{
					{Root: workspace},
					{Root: runtimeRoot},
				},
				ReadRoots: []string{workspace},
			},
		},
	}

	// First run provisions and records the marker.
	if err := ensureWindowsUnelevatedSetup(config); err != nil {
		t.Skipf("cannot run unelevated setup here: %v", err)
	}
	sid, err := windowsCapabilitySIDForWriteRoot(config, runtimeRoot)
	if err != nil {
		t.Fatalf("resolve the runtime capability SID: %v", err)
	}
	granted, err := windowsPathGrantsCapability(runtimeRoot, sid)
	if err != nil || !granted {
		t.Skipf("SETUP: the first run did not grant the runtime root here (granted=%v err=%v)", granted, err)
	}

	// Cleanup reclaims it; the next ordinary run recreates the same pathname.
	if err := os.RemoveAll(runtimeRoot); err != nil {
		t.Fatalf("reclaim the runtime root the way cleanup does: %v", err)
	}
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		t.Fatalf("recreate the pathname the way an ordinary run does: %v", err)
	}

	// SETUP: the case is really reproduced, or the assertion below would hold for
	// a run that never lost anything.
	stillGranted, err := windowsPathGrantsCapability(runtimeRoot, sid)
	if err != nil {
		t.Fatalf("inspect the recreated runtime root: %v", err)
	}
	if stillGranted {
		t.Fatal("SETUP INVALID: the recreated directory still carries the capability grant, so nothing was lost to restore")
	}

	// The second run must notice and reapply rather than trust the marker.
	if err := ensureWindowsUnelevatedSetup(config); err != nil {
		t.Fatalf("the second run refused instead of restoring the reclaimed root: %v", err)
	}
	restored, err := windowsPathGrantsCapability(runtimeRoot, sid)
	if err != nil {
		t.Fatalf("inspect the restored runtime root: %v", err)
	}
	if !restored {
		t.Fatal("the cached marker was accepted for a runtime object that no longer carries the grant; the command would launch and then fail every sandboxed write into it with nothing explaining why")
	}
}

// And an untouched runtime root still takes the cached fast path, so the object
// check does not turn every command into a full reapply.
func TestUnelevatedSetupStillTrustsAnIntactRuntimeRoot(t *testing.T) {
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
		SandboxLevel:   WindowsSandboxLevelUnelevated,
		PermissionProfile: PermissionProfile{
			Runtime: &SandboxRuntime{Root: runtimeRoot},
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: workspace}, {Root: runtimeRoot}},
				ReadRoots:  []string{workspace},
			},
		},
	}
	if err := ensureWindowsUnelevatedSetup(config); err != nil {
		t.Skipf("cannot run unelevated setup here: %v", err)
	}
	if err := ensureWindowsUnelevatedSetup(config); err != nil {
		t.Fatalf("a second run over an intact runtime root was refused: %v", err)
	}
}

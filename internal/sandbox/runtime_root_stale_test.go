package sandbox

import (
	"testing"
)

// A RECORDED ROOT IS HISTORY, NOT PROOF THAT A COMMAND WOULD STILL SELECT IT.
//
// Doctor pins the marker's runtime root so it can check the stamp without
// taking a lease. A command does not pin blindly: it derives the current cache
// and fallback candidates and honours the marker only when its root is one of
// them. Run setup with the cache at A, relocate the cache so commands derive B,
// and the stamped A tree stays behind. Pinning A reported a healthy machine
// immediately before every real command rejected A and failed on the marker.
//
// This records a marker with the cache resolver pointed at A, flips the
// resolver to B, and asks the same question a command asks. The seam is the
// production resolver, so the derivation under test is the command's own.
func TestRecordedRuntimeRootIsNotCurrentOnceTheCacheMoves(t *testing.T) {
	cacheA := t.TempDir()
	cacheB := t.TempDir()
	workspace := t.TempDir()

	previous := sandboxUserCacheDir
	t.Cleanup(func() { sandboxUserCacheDir = previous })
	sandboxUserCacheDir = func() (string, error) { return cacheA, nil }

	config := WindowsSandboxSetupConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     workspace,
		WorkspaceRoots: []string{workspace},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: workspace}},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
	}
	// The marker records profile.Runtime.Root, which setup sets from the root
	// it SELECTS. Select it here the way a command under cache A would, and
	// put it on the profile the way doctor does, so the recorded root is the
	// genuine A-derived one rather than a value this test invented.
	rootA, err := sandboxRuntimeRootFor(canonicalSandboxWorkspaceRoot(workspace), canonicalSandboxWorkspaceRoot(cacheA))
	if err != nil {
		t.Fatalf("derive the runtime root under cache A: %v", err)
	}
	config.PermissionProfile = PermissionProfileWithRuntimeRoot(
		WindowsSandboxProfileWithRuntimeRoots(config.PermissionProfile, config.WorkspaceRoots),
		rootA,
	)
	if _, err := WriteWindowsSandboxSetupMarker(config); err != nil {
		t.Fatalf("WriteWindowsSandboxSetupMarker: %v", err)
	}

	recorded, current, err := WindowsSandboxRecordedRuntimeRootIsCurrent(config.SandboxHome, workspace)
	if err != nil {
		t.Fatalf("with the cache still at A: %v", err)
	}
	if recorded == "" {
		t.Fatal("SETUP INVALID: the marker recorded no runtime root, so the comparison proves nothing")
	}
	if !current {
		t.Fatalf("the root setup just recorded (%s) is reported stale while the cache has not moved", recorded)
	}

	// The cache moves. Commands now derive under B; the stamped tree is under A.
	sandboxUserCacheDir = func() (string, error) { return cacheB, nil }

	recordedAfter, currentAfter, err := WindowsSandboxRecordedRuntimeRootIsCurrent(config.SandboxHome, workspace)
	if err != nil {
		t.Fatalf("with the cache at B: %v", err)
	}
	if recordedAfter != recorded {
		t.Errorf("the recorded root changed on read: %q then %q", recorded, recordedAfter)
	}
	if currentAfter {
		t.Fatalf("the marker's root %s is under cache A, commands derive under B, and it was still reported current", recorded)
	}
}

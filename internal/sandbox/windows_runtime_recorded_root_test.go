package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// blockCacheRuntimeRoot makes the cache-derived runtime root unleasable, and
// returns the function that frees it again.
//
// A file where prepareSandboxRuntimeLease wants a directory: MkdirAll on the
// parent fails, the lease attempt fails with it, and selection relocates to the
// temp fallback. It stands in for any reason the preferred root is unavailable
// for a moment, which is the whole point -- the defect never depended on which
// reason it was.
func blockCacheRuntimeRoot(t *testing.T, workspaceRoot string) (string, func()) {
	t.Helper()
	cacheRoot, err := sandboxUserCacheDir()
	if err != nil {
		t.Fatalf("SETUP INVALID: no user cache directory: %v", err)
	}
	preferred, err := sandboxRuntimeRootFor(canonicalSandboxWorkspaceRoot(workspaceRoot), canonicalSandboxWorkspaceRoot(cacheRoot))
	if err != nil {
		t.Fatalf("SETUP INVALID: no cache-derived runtime root: %v", err)
	}
	blocker := filepath.Dir(preferred)
	if err := os.MkdirAll(filepath.Dir(blocker), 0o700); err != nil {
		t.Fatalf("create the blocker's parent: %v", err)
	}
	_ = os.RemoveAll(blocker)
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block the cache runtime root: %v", err)
	}
	return preferred, func() { _ = os.Remove(blocker) }
}

// A TRANSIENT SELECTION IS NOT A DURABLE CONFIGURATION.
//
// Setup selected a runtime root, released the lease, and recorded a plan hash
// over pathnames. A command later ran the SAME selector, and the selector
// consults a lease: with the cache root briefly unusable setup provisioned and
// recorded the temp fallback, and once it freed up the command selected the
// cache root instead. Its plan hash and stamp path then named a tree setup had
// never provisioned, so the marker rejected every command with "permission roots
// or deny lists changed" -- and re-running setup did not help, because setup was
// equally free to pick the other root.
//
// Setup's choice is recorded now and the command consumes it, so the two cannot
// disagree no matter what changes in between.
func TestTheCommandHonoursTheRootSetupActuallyProvisioned(t *testing.T) {
	config := runtimeRootTestConfig(t)
	workspace := config.WorkspaceRoots[0]
	t.Setenv("ZERO_WINDOWS_SANDBOX_HOME", config.SandboxHome)

	preferred, unblock := blockCacheRuntimeRoot(t, workspace)

	// Setup, with the cache root unusable. honorRecorded is false because setup
	// is the one making the choice.
	setupRoot, setupLease, _, err := selectSandboxRuntimeRoot(workspace, false, "")
	if err != nil {
		t.Fatalf("selectSandboxRuntimeRoot (setup): %v", err)
	}
	setupLease.release()
	if sameWindowsRuntimeRootPath(setupRoot, preferred) {
		t.Fatalf("setup selected the cache root %s even though it was blocked; this case is not being exercised", setupRoot)
	}

	setup := WindowsSandboxSetupConfigFromCommand(config)
	setup.PermissionProfile = WindowsSandboxProfileWithRuntimeRoots(
		permissionProfileWithRuntime(setup.PermissionProfile, SandboxRuntime{Root: setupRoot}),
		config.WorkspaceRoots,
	)
	if _, err := WriteWindowsSandboxSetupMarker(setup); err != nil {
		t.Fatalf("WriteWindowsSandboxSetupMarker: %v", err)
	}

	// And now the cache root frees up, which is exactly when the old code
	// diverged.
	unblock()

	commandRoot, commandLease, _, err := selectSandboxRuntimeRoot(workspace, true, "")
	if err != nil {
		t.Fatalf("selectSandboxRuntimeRoot (command): %v", err)
	}
	commandLease.release()
	if !sameWindowsRuntimeRootPath(commandRoot, setupRoot) {
		t.Fatalf("setup provisioned %s and the command selected %s once the cache root freed up; the marker can never validate across that", setupRoot, commandRoot)
	}

	command := config
	command.PermissionProfile = WindowsSandboxProfileWithRuntimeRoots(
		permissionProfileWithRuntime(config.PermissionProfile, SandboxRuntime{Root: commandRoot}),
		config.WorkspaceRoots,
	)
	if err := ValidateWindowsSandboxSetupMarker(WindowsSandboxSetupConfigFromCommand(command)); err != nil {
		t.Fatalf("the first command after setup was rejected: %v", err)
	}
}

// The record belongs to the workspace setup ran for. One sandbox home serves
// whichever workspace ran setup last, so honouring a root recorded for a
// different workspace would point this command's runtime at somebody else's
// tree.
func TestARootRecordedForAnotherWorkspaceIsNotHonoured(t *testing.T) {
	config := runtimeRootTestConfig(t)
	t.Setenv("ZERO_WINDOWS_SANDBOX_HOME", config.SandboxHome)

	foreign := filepath.Join(t.TempDir(), "somebody-elses-runtime")
	setup := WindowsSandboxSetupConfigFromCommand(config)
	setup.PermissionProfile = permissionProfileWithRuntime(setup.PermissionProfile, SandboxRuntime{Root: foreign})
	if _, err := WriteWindowsSandboxSetupMarker(setup); err != nil {
		t.Fatalf("WriteWindowsSandboxSetupMarker: %v", err)
	}

	selected, lease, _, err := selectSandboxRuntimeRoot(config.WorkspaceRoots[0], true, "")
	if err != nil {
		t.Fatalf("selectSandboxRuntimeRoot: %v", err)
	}
	lease.release()
	if sameWindowsRuntimeRootPath(selected, foreign) {
		t.Fatalf("a root recorded for another workspace was honoured: %s", selected)
	}
}

// A recorded root that cannot be leased must FAIL, not relocate.
//
// Relocating is what produced the permanent brick: the other root carries no
// capability ACE, so the command is rejected anyway, with a message about
// permissions that sends the operator looking in the wrong place.
func TestAnUnusableRecordedRootFailsInsteadOfRelocating(t *testing.T) {
	config := runtimeRootTestConfig(t)
	workspace := config.WorkspaceRoots[0]
	t.Setenv("ZERO_WINDOWS_SANDBOX_HOME", config.SandboxHome)

	recorded, lease, _, err := selectSandboxRuntimeRoot(workspace, false, "")
	if err != nil {
		t.Fatalf("selectSandboxRuntimeRoot (setup): %v", err)
	}
	lease.release()

	setup := WindowsSandboxSetupConfigFromCommand(config)
	setup.PermissionProfile = WindowsSandboxProfileWithRuntimeRoots(
		permissionProfileWithRuntime(setup.PermissionProfile, SandboxRuntime{Root: recorded}),
		config.WorkspaceRoots,
	)
	if _, err := WriteWindowsSandboxSetupMarker(setup); err != nil {
		t.Fatalf("WriteWindowsSandboxSetupMarker: %v", err)
	}

	// Make the provisioned root unusable after the fact.
	blocker := filepath.Dir(recorded)
	if err := os.RemoveAll(blocker); err != nil {
		t.Skipf("cannot displace the recorded root in this environment: %v", err)
	}
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Skipf("cannot displace the recorded root in this environment: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(blocker) })

	selected, selectedLease, _, err := selectSandboxRuntimeRoot(workspace, true, "")
	if err == nil {
		selectedLease.release()
		t.Fatalf("selection relocated to %s instead of reporting that the provisioned root is unusable", selected)
	}
	if !strings.Contains(err.Error(), "provisioned by setup") || !strings.Contains(err.Error(), "zero sandbox setup") {
		t.Errorf("the error does not name the situation or the action that fixes it: %v", err)
	}
}

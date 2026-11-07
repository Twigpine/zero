package sandbox

import (
	"os"
	"strings"
	"testing"
)

// A LEASE FALLBACK MUST NOT BRICK THE WORKSPACE.
//
// Setup used to fingerprint a plan built from the cache-derived root, while a
// command derived the same root, failed to LEASE it, and silently relocated to
// the temp fallback. The command's plan then named a tree setup had never
// provisioned, and the marker rejected it with "permission roots or deny lists
// changed", which blames permissions for a runtime-root disagreement.
//
// Re-running setup could not recover: sandboxRuntimeRootFor rejects a candidate
// only for landing inside the workspace, never for being unusable, so setup
// chose the same unleasable root again. The only escapes were deleting the
// marker, which silently drops WFP network enforcement, or turning the sandbox
// off. Neither is mentioned by the error.
//
// Setup and the command select through one function now, so a fallback is
// something they agree on rather than something that splits them.
func TestSetupAndCommandSelectTheSameRuntimeRoot(t *testing.T) {
	config := runtimeRootTestConfig(t)

	setupRoot, setupLease, _, err := selectSandboxRuntimeRoot(config.WorkspaceRoots[0], true, "")
	if err != nil {
		t.Fatalf("selectSandboxRuntimeRoot (setup side): %v", err)
	}
	setupLease.release()

	commandRoot, commandLease, _, err := selectSandboxRuntimeRoot(config.WorkspaceRoots[0], true, "")
	if err != nil {
		t.Fatalf("selectSandboxRuntimeRoot (command side): %v", err)
	}
	commandLease.release()

	if setupRoot != commandRoot {
		t.Fatalf("setup selected %s and the command selected %s; the marker cannot validate across that", setupRoot, commandRoot)
	}
}

// THE MARKER MUST BE ABOUT AN OBJECT, NOT ABOUT A PATHNAME.
//
// cleanupSandboxRuntimeRoots evicts inactive roots with os.RemoveAll on an age
// and count policy, and the next run for that workspace recreates the same
// deterministic pathname with ordinary inherited permissions. The plan hash is
// over pathnames, so it was unchanged, and both the elevated and the unelevated
// marker checks reported setup as current while the recreated directory carried
// no capability ACE at all: a WRITE_RESTRICTED token could not write TMP,
// GOCACHE or anything else beneath it, with nothing saying why.
func TestAnEvictedRuntimeRootInvalidatesTheMarker(t *testing.T) {
	config := runtimeRootTestConfig(t)

	selected, lease, _, err := selectSandboxRuntimeRoot(config.WorkspaceRoots[0], true, "")
	if err != nil {
		t.Fatalf("selectSandboxRuntimeRoot: %v", err)
	}
	lease.release()

	setup := WindowsSandboxSetupConfigFromCommand(config)
	setup.PermissionProfile = WindowsSandboxProfileWithRuntimeRoots(
		permissionProfileWithRuntime(setup.PermissionProfile, SandboxRuntime{Root: selected}),
		config.WorkspaceRoots,
	)
	if _, err := WriteWindowsSandboxSetupMarker(setup); err != nil {
		t.Fatalf("WriteWindowsSandboxSetupMarker: %v", err)
	}

	command := config
	command.PermissionProfile = WindowsSandboxProfileWithRuntimeRoots(
		permissionProfileWithRuntime(config.PermissionProfile, SandboxRuntime{Root: selected}),
		config.WorkspaceRoots,
	)
	// The premise: it validates while the provisioned tree is intact. Without
	// this the eviction assertion below could pass for the wrong reason.
	if err := ValidateWindowsSandboxSetupMarker(WindowsSandboxSetupConfigFromCommand(command)); err != nil {
		t.Fatalf("SETUP INVALID: the marker did not validate before eviction: %v", err)
	}

	// Eviction, exactly as cleanupSandboxRuntimeRoots performs it.
	if err := os.RemoveAll(selected); err != nil {
		t.Fatalf("evict the runtime root: %v", err)
	}
	// And the pathname comes back, as prepareSandboxRuntime recreates it, with
	// ordinary permissions and no capability ACE. This is the state that used to
	// validate.
	if err := os.MkdirAll(selected, 0o700); err != nil {
		t.Fatalf("recreate the runtime root: %v", err)
	}

	err = ValidateWindowsSandboxSetupMarker(WindowsSandboxSetupConfigFromCommand(command))
	if err == nil {
		t.Fatal("the marker still validates after the provisioned tree was evicted and recreated, so the command runs with no capability ACE and nothing reports it")
	}
	if !strings.Contains(err.Error(), "removed since setup ran") {
		t.Errorf("the error does not explain that the runtime tree was removed, so the operator cannot act on it: %v", err)
	}
}

// A profile carrying no runtime root has nothing to check, which is the setup
// side itself and every non-restricted profile. The stamp must not become a
// requirement where there is no tree.
func TestRuntimeStampIsNotRequiredWithoutARuntimeRoot(t *testing.T) {
	if err := validateWindowsSandboxRuntimeStamp(PermissionProfile{}, "somehash"); err != nil {
		t.Errorf("a profile carrying no runtime root was rejected: %v", err)
	}
}

package sandbox

import (
	"os"
	"strings"
	"testing"
)

// runtimeRootTestConfig is the shape every command reaches the Windows runner
// with: a restricted filesystem rooted at the workspace, which is what makes the
// runtime root necessary in the first place.
func runtimeRootTestConfig(t *testing.T) WindowsSandboxCommandConfig {
	t.Helper()
	// REDIRECT THE AMBIENT CACHE PRODUCER, not only the obvious inputs.
	//
	// The workspace and sandbox home below are test-owned, but
	// windowsSandboxRuntimeCandidates still derives a candidate from
	// sandboxUserCacheDir, and the runtime-root test creates every candidate and
	// registers os.RemoveAll cleanup for them. That reaches into the real user
	// cache: it fails outright on a read-only home, and on an ordinary developer
	// or CI account it creates and then deletes a path outside the test's
	// storage. The workspace hash makes a collision unlikely; it does not make
	// somebody else's directory test-owned.
	cache := t.TempDir()
	previousCache := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return cache, nil }
	t.Cleanup(func() { sandboxUserCacheDir = previousCache })

	workspace := t.TempDir()
	return WindowsSandboxCommandConfig{
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
}

// A marker written by a fresh setup must accept the ordinary command, and the
// ordinary command is the RUNTIME-AUGMENTED one: Engine.run calls
// permissionProfileWithRuntime before the Windows runner ever sees the profile,
// so the profile presented at validation always carries the selected runtime
// root as an extra write root.
//
// Setup used to fingerprint the bare profile. The extra write root changed the
// ACL plan, the plan hash changed with it, and validation rejected a marker
// written seconds earlier with "permission roots or deny lists changed" — so on
// a restricted filesystem no command could run at all, including the very
// command that had just been set up for.
//
// Asserted for BOTH candidates because which one a process selects is not fixed:
// sandboxRuntimeRootFor prefers the cache-derived root and falls back to the
// temp-derived one, and a marker that only accepts the preferred root bricks
// every machine that falls back.
func TestWindowsSandboxSetupMarkerAcceptsRuntimeAugmentedCommand(t *testing.T) {
	config := runtimeRootTestConfig(t)
	// The setup half, as BuildWindowsSandboxSetupArgs prepares it in the
	// operator's shell before the elevated helper ever runs.
	setup := WindowsSandboxSetupConfigFromCommand(config)
	setup.PermissionProfile = WindowsSandboxProfileWithRuntimeRoots(setup.PermissionProfile, config.WorkspaceRoots)
	if _, err := WriteWindowsSandboxSetupMarker(setup); err != nil {
		t.Fatalf("WriteWindowsSandboxSetupMarker: %v", err)
	}

	candidates := windowsSandboxRuntimeCandidates(config.WorkspaceRoots)
	if len(candidates) == 0 {
		t.Fatal("windowsSandboxRuntimeCandidates returned none, so this test proves nothing")
	}
	for _, candidate := range candidates {
		augmented := config
		// The command half, in the same order the real path builds it: the engine
		// appends the SELECTED root, then the Windows plan folds in the candidate
		// set before serializing the profile to the runner.
		augmented.PermissionProfile = WindowsSandboxProfileWithRuntimeRoots(
			permissionProfileWithRuntime(config.PermissionProfile, SandboxRuntime{Root: candidate}),
			config.WorkspaceRoots,
		)
		err := ValidateWindowsSandboxSetupMarker(WindowsSandboxSetupConfigFromCommand(augmented))
		if err != nil {
			t.Fatalf("ValidateWindowsSandboxSetupMarker with runtime root %s: %v", candidate, err)
		}
	}

	// THE RUNNER'S TEMP IS NOT THE OPERATOR'S.
	//
	// sandboxRuntimeEnvironment points TMPDIR/TMP/TEMP at the sandbox runtime
	// temp for everything the sandbox launches, and the command runner inherits
	// that env. While the runner derived the candidate set itself, os.TempDir()
	// there returned the redirected value, so it produced a temp-derived root
	// under the runtime tree while setup produced one under the real temp: two
	// plans with the SAME entry count and different hashes, and every sandboxed
	// command refused to run with "permission roots or deny lists changed".
	//
	// Validation must not move when that variable does.
	// Augmented FIRST, standing in for the parent, whose TEMP is still real.
	runner := config
	runner.PermissionProfile = WindowsSandboxProfileWithRuntimeRoots(
		permissionProfileWithRuntime(config.PermissionProfile, SandboxRuntime{Root: candidates[0]}),
		config.WorkspaceRoots,
	)
	// Only THEN does the environment become the runner's. Anything downstream of
	// this line that re-derives a runtime root gets the redirected answer, which
	// is precisely the defect: validation has to be settled before here.
	t.Setenv("TEMP", t.TempDir())
	t.Setenv("TMP", os.Getenv("TEMP"))
	if err := ValidateWindowsSandboxSetupMarker(WindowsSandboxSetupConfigFromCommand(runner)); err != nil {
		t.Fatalf("ValidateWindowsSandboxSetupMarker with a redirected TEMP: %v", err)
	}

	// The guard has to still bite, or the test above passes for the wrong reason
	// — a validator that accepts everything would satisfy it too.
	changed := config
	changed.PermissionProfile.FileSystem.DenyRead = []string{`C:\workspace\secret`}
	if err := ValidateWindowsSandboxSetupMarker(WindowsSandboxSetupConfigFromCommand(changed)); err == nil {
		t.Fatal("ValidateWindowsSandboxSetupMarker accepted a changed deny list, so it no longer detects drift")
	} else if !strings.Contains(err.Error(), "out of date") {
		t.Fatalf("ValidateWindowsSandboxSetupMarker changed error = %v, want out of date", err)
	}
}

// The runtime root needs BOTH sides of the write-restricted grant.
//
// A principal command runs on a token restricted to the capability SIDs, and a
// WRITE_RESTRICTED token grants a write only when the normal token check AND the
// restricting-SID check both pass. The runtime root used to be appended to the
// principal plan alone, so it carried the account ACE and no capability ACE: the
// normal check passed, the restricted check found nothing, and every cache and
// temp write was denied even once the marker agreed.
//
// This asserts the capability side, which is the side that was missing. Both
// candidates again, for the same reason as above.
func TestWindowsSandboxRuntimeRootsAreInTheCapabilityPlan(t *testing.T) {
	config := runtimeRootTestConfig(t)
	candidates := windowsSandboxRuntimeCandidates(config.WorkspaceRoots)
	if len(candidates) == 0 {
		t.Fatal("windowsSandboxRuntimeCandidates returned none, so this test proves nothing")
	}

	setup := WindowsSandboxSetupConfigFromCommand(config)
	setup.PermissionProfile = WindowsSandboxProfileWithRuntimeRoots(setup.PermissionProfile, config.WorkspaceRoots)
	plan, err := BuildWindowsACLPlan(setup.commandConfig())
	if err != nil {
		t.Fatalf("BuildWindowsACLPlan: %v", err)
	}
	granted := make(map[string]struct{}, len(plan.Entries))
	for _, entry := range plan.Entries {
		granted[windowsCapabilityPathKey(entry.Path)] = struct{}{}
	}
	for _, candidate := range candidates {
		if _, ok := granted[windowsCapabilityPathKey(candidate)]; !ok {
			t.Fatalf("capability ACL plan has no entry for runtime root %s; writes there fail the restricting-SID check", candidate)
		}
	}
}

// EVERY write root the capability plan grants must exist by the time setup
// applies it.
//
// The capability plan deliberately refuses to materialize a write root, so a
// granted path that is merely absent fails the entire setup run with
//
//	windows ACL target does not exist: ...\zero\runtime\v1\<hash>
//
// which is exactly what an elevated run hit: the runtime candidates were added
// to the plan while only the selected one was ever created, and `zero sandbox
// setup` stopped working altogether. The two halves are separate functions, so
// nothing but this test stops one from growing a candidate without the other.
func TestWindowsSandboxSetupProvisionsEveryGrantedWriteRoot(t *testing.T) {
	config := runtimeRootTestConfig(t)
	candidates := windowsSandboxRuntimeCandidates(config.WorkspaceRoots)
	if len(candidates) == 0 {
		t.Fatal("windowsSandboxRuntimeCandidates returned none, so this test proves nothing")
	}
	// The workspace is unique per run, and the roots are derived from it, so these
	// cannot pre-exist. Assert that rather than trust it: if they did, the test
	// would pass with the provisioning step deleted.
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			t.Fatalf("runtime root %s already exists before provisioning, so this test proves nothing", candidate)
		}
		t.Cleanup(func() { _ = os.RemoveAll(candidate) })
	}

	if err := ensureWindowsSandboxRuntimeCandidates(config.WorkspaceRoots); err != nil {
		t.Fatalf("ensureWindowsSandboxRuntimeCandidates: %v", err)
	}

	setup := WindowsSandboxSetupConfigFromCommand(config)
	setup.PermissionProfile = WindowsSandboxProfileWithRuntimeRoots(setup.PermissionProfile, config.WorkspaceRoots)
	plan, err := BuildWindowsACLPlan(setup.commandConfig())
	if err != nil {
		t.Fatalf("BuildWindowsACLPlan: %v", err)
	}
	for _, entry := range plan.Entries {
		if entry.Action != WindowsACLAllowWrite || entry.Materialize {
			continue
		}
		if _, err := os.Stat(entry.Path); err != nil {
			t.Errorf("capability plan grants write on %s but nothing created it, so setup fails with "+
				"\"windows ACL target does not exist\": %v", entry.Path, err)
		}
	}
}

// Both candidates are pure functions of the workspace root. Setup provisions the
// set and a later command selects from it in a different process, so a candidate
// that varied per process (a random or time-seeded fallback) would be granted by
// setup and never selected, or selected and never granted.
func TestWindowsSandboxRuntimeCandidatesAreDeterministic(t *testing.T) {
	config := runtimeRootTestConfig(t)
	first := windowsSandboxRuntimeCandidates(config.WorkspaceRoots)
	if len(first) == 0 {
		t.Fatal("windowsSandboxRuntimeCandidates returned none, so this test proves nothing")
	}
	second := windowsSandboxRuntimeCandidates(config.WorkspaceRoots)
	if len(first) != len(second) {
		t.Fatalf("candidate count = %d then %d, want stable", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("candidate %d = %q then %q, want stable", i, first[i], second[i])
		}
	}

	other := runtimeRootTestConfig(t)
	otherCandidates := windowsSandboxRuntimeCandidates(other.WorkspaceRoots)
	for _, candidate := range otherCandidates {
		for _, mine := range first {
			if candidate == mine {
				t.Fatalf("workspaces %s and %s share runtime root %s, so one workspace's grant covers the other",
					config.CommandCWD, other.CommandCWD, candidate)
			}
		}
	}
}

// EVERY ROOT THE SELECTOR CAN CHOOSE HAS TO BE IN THE SET SETUP GRANTS.
//
// The tests above all iterate whatever windowsSandboxRuntimeCandidates returns,
// so they hold for any self-consistent candidate set, including one missing an
// entry. That is the half this pins: sandboxRuntimeRootFor prefers the
// cache-derived root and falls back to the temp-derived one when the cache root
// is unusable, and prepareSandboxRuntime takes that fallback when the primary
// lease cannot be acquired. A candidate set carrying only the preferred root
// would be internally consistent and still leave the fallback ungranted, so a
// machine that fell back would be refused the write it needs. Reported by
// @anandh8x.
//
// Driven through the selector rather than by re-deriving the fallback here, so
// the assertion stays true if the selection rule changes.
func TestWindowsSandboxRuntimeCandidatesCoverEverySelectableRoot(t *testing.T) {
	config := runtimeRootTestConfig(t)
	workspace := canonicalSandboxWorkspaceRoot(config.WorkspaceRoots[0])
	candidates := windowsSandboxRuntimeCandidates(config.WorkspaceRoots)
	if len(candidates) == 0 {
		t.Fatal("windowsSandboxRuntimeCandidates returned none, so this test proves nothing")
	}
	granted := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		granted[windowsCapabilityPathKey(candidate)] = struct{}{}
	}

	cacheRoot, err := sandboxUserCacheDir()
	if err != nil {
		t.Fatalf("sandboxUserCacheDir: %v", err)
	}
	// The ordinary selection, and then the selection a machine makes when the
	// cache-derived root is not usable. Naming an unusable cache root reproduces
	// that second case without depending on the host's real cache directory.
	preferred, err := sandboxRuntimeRootFor(workspace, canonicalSandboxWorkspaceRoot(cacheRoot))
	if err != nil {
		t.Fatalf("sandboxRuntimeRootFor(usable cache): %v", err)
	}
	fallback, err := sandboxRuntimeRootFor(workspace, workspace)
	if err != nil {
		t.Fatalf("sandboxRuntimeRootFor(unusable cache): %v", err)
	}
	if preferred == fallback {
		t.Fatalf("both selections returned %s, so this test cannot tell the fallback from the preferred root", preferred)
	}
	for _, selected := range []string{preferred, fallback} {
		if _, ok := granted[windowsCapabilityPathKey(selected)]; !ok {
			t.Errorf("the selector can choose runtime root %s but setup grants only %v, "+
				"so a command that selects it is refused the write it was provisioned for", selected, candidates)
		}
	}
}

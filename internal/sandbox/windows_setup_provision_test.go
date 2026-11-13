package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The tests here drive PRODUCTION entry points rather than the helpers those entry
// points call. The distinction is the point: this runtime-root work shipped once
// with tests that called windowsSandboxProfileWithRuntime and
// ensureWindowsSandboxRuntimeCandidates directly, and they stayed green while the
// production call sites were missing outright. A test that reaches past the caller
// proves the helper works and says nothing about whether anything calls it.

// windowsRuntimeTestRoots returns a workspace plus the runtime candidates derived
// for it, with EVERY location the derivation reads pointed at test-owned
// directories.
//
// Stubbing sandboxUserCacheDir is not tidiness. windowsSandboxRuntimeCandidates
// reads the real user cache when it is left alone, so a test that then clears a
// candidate to prove provisioning recreates it was deleting the developer's own
// zero runtime tree under the real cache on every run, and failing outright on a
// read-only home. The test has no ownership claim on that path, so it must not
// derive one. windows_runner_marker_windows_test.go had this right already.
func windowsRuntimeTestRoots(t *testing.T) (string, []string) {
	t.Helper()
	workspaceRoot := t.TempDir()
	cacheRoot := t.TempDir()
	tempRoot := t.TempDir()

	originalCacheDir := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }
	t.Cleanup(func() { sandboxUserCacheDir = originalCacheDir })
	redirectSandboxTestTemp(t, tempRoot)

	candidates := windowsSandboxRuntimeRoots(PermissionProfile{}, []string{workspaceRoot})
	if len(candidates) == 0 {
		// Every input is owned by this point, so nothing about the machine can
		// explain an empty answer, and skipping would quietly switch off every test
		// built on this fixture.
		t.Fatal("SETUP INVALID: no runtime candidates derivable from a workspace, cache and temp this test owns")
	}
	// Belt and braces: refuse to run rather than touch anything the test does not
	// own, so a later change to the derivation cannot quietly reintroduce this.
	//
	// BOTH SIDES CANONICALIZED, because pathWithinRoot compares spellings and the
	// candidate arrives canonical: the derivation runs the cache root through
	// canonicalSandboxWorkspaceRoot before joining. Measuring that against a raw
	// t.TempDir() is the same one-sided comparison this PR fixes in
	// fallbackSandboxRuntimeRoot, and it fired on exactly the machines that carry a
	// second spelling: macOS /var vs /private/var, and a CI Windows runner whose
	// profile has an 8.3 name (RUNNER~1 against runneradmin). It passed locally
	// because this box has neither.
	ownedCache := canonicalSandboxWorkspaceRoot(cacheRoot)
	ownedTemp := canonicalSandboxWorkspaceRoot(tempRoot)
	for _, candidate := range candidates {
		canonical := canonicalSandboxWorkspaceRoot(candidate)
		if !pathWithinRoot(ownedCache, canonical) && !pathWithinRoot(ownedTemp, canonical) {
			t.Fatalf("candidate %s (canonically %s) is outside the test-owned cache (%s) and temp (%s) roots; refusing to modify it", candidate, canonical, ownedCache, ownedTemp)
		}
	}
	// AND THE CANDIDATE THIS CALL DID NOT SEE. windowsSandboxRuntimeRoots returns
	// the preferred, cache-derived root only, so the loop above says nothing about
	// the temp fallback, which is derived from a different input. A caller that
	// blocks the preferred root (blockCacheRuntimeRoot) makes selection
	// materialize that fallback, and with TMPDIR left alone it was derived from
	// the ambient Unix temp. Checked here, before any caller can create it.
	// Reported by @jatmn.
	fallback, err := fallbackSandboxRuntimeRoot(canonicalSandboxWorkspaceRoot(workspaceRoot))
	if err != nil {
		t.Fatalf("SETUP INVALID: no fallback runtime root under the test-owned temp %s: %v", ownedTemp, err)
	}
	requireWithinTestOwned(t, fallback, tempRoot)
	return workspaceRoot, candidates
}

// The fixture's fallback boundary, exercised rather than assumed: with the
// preferred root deliberately unavailable, the PRODUCTION selector relocates, and
// what it selects and creates has to be inside the temp the fixture owns.
func TestRuntimeTestRootsOwnTheFallbackSelectionToo(t *testing.T) {
	workspaceRoot, _ := windowsRuntimeTestRoots(t)
	ownedTemp := os.TempDir()

	preferred, unblock := blockCacheRuntimeRoot(t, workspaceRoot)
	defer unblock()

	selected, lease, _, err := selectSandboxRuntimeRoot(workspaceRoot, false, "")
	if err != nil {
		t.Fatalf("selectSandboxRuntimeRoot with the preferred root blocked: %v", err)
	}
	lease.release()
	if sameWindowsRuntimeRootPath(selected, preferred) {
		t.Fatalf("SETUP INVALID: selection still returned the preferred root %s, so the fallback was not exercised", selected)
	}
	requireWithinTestOwned(t, selected, ownedTemp)
}

// TestBuildWindowsSandboxSetupACLPlanCreatesTheRootsItGrants is the regression for
// the defect this PR shipped with: the plan named the runtime roots as AllowWrite
// targets and nothing created them. applyWindowsACLPlan materializes only DenyRead
// targets, so an absent AllowWrite target aborts the whole elevated setup with
// "windows ACL target does not exist".
func TestBuildWindowsSandboxSetupACLPlanCreatesTheRootsItGrants(t *testing.T) {
	workspaceRoot, candidates := windowsRuntimeTestRoots(t)
	for _, candidate := range candidates {
		if err := os.RemoveAll(candidate); err != nil {
			t.Fatalf("clear candidate %s: %v", candidate, err)
		}
	}

	config := WindowsSandboxSetupConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     workspaceRoot,
		WorkspaceRoots: []string{workspaceRoot},
		// Augmented, because that is what the elevated helper parses off the wire:
		// BuildWindowsSandboxSetupArgs folds the runtime roots in before the re-exec,
		// so by the time runWindowsSandboxSetup builds this plan the roots are
		// already write roots. Feeding a bare profile here would test a shape
		// production never produces.
		PermissionProfile: WindowsSandboxProfileWithRuntimeRoots(
			PermissionProfileFromPolicy(workspaceRoot, DefaultPolicy(), nil),
			[]string{workspaceRoot},
		),
	}
	plan, _, err := buildWindowsSandboxSetupACLPlan(config)
	if err != nil {
		t.Fatalf("buildWindowsSandboxSetupACLPlan: %v", err)
	}

	granted := map[string]struct{}{}
	for _, entry := range plan.Entries {
		if entry.Action == WindowsACLAllowWrite {
			granted[windowsCapabilityPathKey(entry.Path)] = struct{}{}
		}
	}
	for _, candidate := range candidates {
		if _, ok := granted[windowsCapabilityPathKey(candidate)]; !ok {
			t.Fatalf("plan does not grant runtime root %s; the marker and the command plan will disagree", candidate)
		}
		info, err := os.Stat(candidate)
		if err != nil {
			t.Fatalf("runtime root %s is granted but absent: %v; applyWindowsACLPlan aborts the entire setup with \"windows ACL target does not exist\"", candidate, err)
		}
		if !info.IsDir() {
			t.Fatalf("runtime root %s exists but is not a directory", candidate)
		}
	}
}

// TestBuildWindowsSandboxSetupArgsCarriesEveryRuntimeCandidate closes the gap the
// reviewer named: every prior test fed BuildWindowsSandboxSetupArgs a profile the
// caller had already augmented, so deleting the augmentation inside the builder
// left the suite green. This one hands it a BARE profile and decodes the argument
// the elevated helper actually receives.
func TestBuildWindowsSandboxSetupArgsCarriesEveryRuntimeCandidate(t *testing.T) {
	workspaceRoot, candidates := windowsRuntimeTestRoots(t)
	bare := PermissionProfileFromPolicy(workspaceRoot, DefaultPolicy(), nil)
	for _, root := range bare.FileSystem.WriteRoots {
		for _, candidate := range candidates {
			if windowsCapabilityPathKey(root.Root) == windowsCapabilityPathKey(candidate) {
				t.Fatalf("the bare profile already contains runtime root %s, so this test could not detect the augmentation going missing", candidate)
			}
		}
	}

	setupPlan, err := BuildWindowsSandboxSetupArgs(WindowsSandboxSetupArgsOptions{
		SandboxHome: t.TempDir(),
		CommandCWD:  workspaceRoot,
		// Deliberately NOT pre-augmented: the builder folds the runtime roots in
		// itself, and passing them here would hide it if that ever stopped.
		PermissionProfile: bare,
		WorkspaceRoots:    []string{workspaceRoot},
	})
	if err != nil {
		t.Fatalf("BuildWindowsSandboxSetupArgs: %v", err)
	}
	args := setupPlan.Args

	encoded := ""
	for i, arg := range args {
		if arg == "--permission-profile" && i+1 < len(args) {
			encoded = args[i+1]
			break
		}
		if strings.HasPrefix(arg, "--permission-profile=") {
			encoded = strings.TrimPrefix(arg, "--permission-profile=")
			break
		}
	}
	if encoded == "" {
		t.Fatalf("no --permission-profile argument in %v", args)
	}
	var decoded PermissionProfile
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		t.Fatalf("decode --permission-profile: %v", err)
	}

	present := map[string]struct{}{}
	for _, root := range decoded.FileSystem.WriteRoots {
		present[windowsCapabilityPathKey(root.Root)] = struct{}{}
	}
	for _, candidate := range candidates {
		if _, ok := present[windowsCapabilityPathKey(candidate)]; !ok {
			t.Fatalf("the setup args omit runtime root %s; setup would fingerprint a profile no command reproduces and every command would die on \"permission roots or deny lists changed\"", candidate)
		}
	}
}

// TestSetupMarkerSurvivesADifferentTempInALaterProcess is the regression for the
// finding that the marker depended on the caller's transient TEMP.
//
// The sequence is the real one and the old code could not survive it: elevated
// setup runs from one terminal, and a later command is planned by a parent
// process an IDE or service started with a different TEMP. The cache runtime is
// untouched and healthy throughout. While both candidates were folded in
// unconditionally, the second process derived a different temp-side path, the
// plan hashes disagreed, and every command died on "permission roots or deny
// lists changed" with nothing wrong.
func TestSetupMarkerSurvivesADifferentTempInALaterProcess(t *testing.T) {
	workspaceRoot := t.TempDir()
	cacheRoot := t.TempDir()
	originalCacheDir := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }
	t.Cleanup(func() { sandboxUserCacheDir = originalCacheDir })

	runtimeRootUnder := func(temp string) string {
		t.Helper()
		redirectSandboxTestTemp(t, temp)
		roots := windowsSandboxRuntimeRoots(PermissionProfile{}, []string{workspaceRoot})
		if len(roots) != 1 {
			t.Fatalf("expected one runtime root under TEMP=%s, got %v", temp, roots)
		}
		return roots[0]
	}

	atSetup := runtimeRootUnder(t.TempDir())
	atLaterCommand := runtimeRootUnder(t.TempDir())

	if atSetup != atLaterCommand {
		t.Fatalf("the runtime root moved when only TEMP changed:\n  setup          %s\n  later process  %s\nboth feed the ACL plan the marker fingerprints, so every command would fail validation with \"permission roots or deny lists changed\" while the cache runtime sat there healthy",
			atSetup, atLaterCommand)
	}

	// Scope, stated rather than implied. This asserts the RUNTIME ROOT no longer
	// tracks TEMP, which is the part this PR introduced and this change removes.
	// The whole plan hash is still TEMP-dependent for a separate, older reason:
	// PermissionProfileFromPolicy grants os.TempDir() itself as a write root when
	// the policy allows temp, so the profile carries the caller's TEMP before any
	// runtime augmentation happens. Asserting on the full hash here would fail for
	// that pre-existing reason and read as though this fix were broken.
	base := PermissionProfileFromPolicy(workspaceRoot, DefaultPolicy(), nil)
	carriesAmbientTemp := false
	for _, root := range base.FileSystem.WriteRoots {
		if pathWithinRoot(canonicalSandboxWorkspaceRoot(os.TempDir()), canonicalSandboxWorkspaceRoot(root.Root)) {
			carriesAmbientTemp = true
		}
	}
	if !carriesAmbientTemp {
		t.Log("the base profile no longer grants the ambient temp dir; the wider TEMP dependency may now be closed and this note can go")
	}
}

// TestRuntimeRootsPinToTheProfileTheCommandActuallyHolds covers the other half:
// once a profile carries a runtime, the plan names THAT tree and does not
// re-derive one. prepareSandboxRuntime relocates on a lease failure, so a
// re-derivation would name the tree the command is not writing to.
func TestRuntimeRootsPinToTheProfileTheCommandActuallyHolds(t *testing.T) {
	workspaceRoot := t.TempDir()
	cacheRoot := t.TempDir()
	originalCacheDir := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }
	t.Cleanup(func() { sandboxUserCacheDir = originalCacheDir })
	tempRoot := t.TempDir()
	redirectSandboxTestTemp(t, tempRoot)

	derived := windowsSandboxRuntimeRoots(PermissionProfile{}, []string{workspaceRoot})
	if len(derived) != 1 {
		t.Fatalf("expected exactly one derived runtime root, got %v", derived)
	}

	// A runtime the process actually selected, deliberately NOT the derived one,
	// standing in for the lease-failure relocation.
	relocated := filepath.Join(t.TempDir(), "relocated-runtime")
	profile := PermissionProfile{Runtime: &SandboxRuntime{Root: relocated}}

	pinned := windowsSandboxRuntimeRoots(profile, []string{workspaceRoot})
	if len(pinned) != 1 || pinned[0] != relocated {
		t.Fatalf("the plan did not pin to the runtime the profile holds:\n  profile runtime %s\n  plan named      %v\nthe command would write to one tree while the plan grants another", relocated, pinned)
	}
	if pinned[0] == derived[0] {
		t.Fatalf("pinned and derived roots are identical (%s), so this test cannot tell them apart", pinned[0])
	}
}

// TestFallbackSandboxRuntimeRootIsSpellingStable pins the canonicalization added
// for the aliased-TEMP finding. Two spellings of one directory must produce one
// runtime root; producing two is what let a root resolving inside the workspace
// pass the containment check.
func TestFallbackSandboxRuntimeRootIsSpellingStable(t *testing.T) {
	workspaceRoot := t.TempDir()
	tempRoot := t.TempDir()

	// An UPPER-CASED spelling is the one alias available without privilege or a
	// volume setting: 8.3 generation is disabled on many volumes and creating a
	// symlink needs a privilege the test process may not hold, while a
	// case-insensitive filesystem resolves this to the same directory and
	// GetLongPathName returns the on-disk casing.
	alias := strings.ToUpper(tempRoot)
	if alias == tempRoot {
		t.Skip("the temp path has no distinct upper-cased spelling here")
	}

	// Decide whether the alias is usable by asking the filesystem, never by asking
	// canonicalSandboxWorkspaceRoot. Skipping on what the function under test says
	// would turn a canonicalization regression into a silent skip on the one
	// platform this test exists to protect.
	realInfo, err := os.Stat(tempRoot)
	if err != nil {
		t.Fatalf("stat temp dir: %v", err)
	}
	aliasInfo, err := os.Stat(alias)
	if err != nil || !os.SameFile(realInfo, aliasInfo) {
		t.Skip("case-sensitive filesystem: the upper-cased spelling is a different directory")
	}

	// Case folding is a Windows property of canonicalSandboxWorkspaceRoot, not a
	// cross-platform one: it folds case only because filepath.EvalSymlinks returns
	// the on-disk spelling there. On a case-insensitive macOS volume os.SameFile
	// calls these one directory and canonicalization still keeps them apart, which
	// is a real gap but not one this branch claims to close, and it cannot produce
	// the setup-versus-command disagreement fixed here because the elevated setup
	// marker is Windows-only. Gate on the platform, never on what the function
	// under test returns.
	if runtime.GOOS != "windows" {
		t.Skip("canonicalization folds case only on windows; nothing to assert here")
	}

	// One directory under two spellings. Canonicalization has to fold them, and a
	// failure here is the regression, not a reason to stop testing.
	canonical := canonicalSandboxWorkspaceRoot(tempRoot)
	if got := canonicalSandboxWorkspaceRoot(alias); got != canonical {
		t.Fatalf("canonicalization did not fold two spellings of one directory:\n  %s\n    -> %s\n  %s\n    -> %s\nos.SameFile says these are the same directory, so the runtime roots derived from them will disagree",
			tempRoot, canonical, alias, got)
	}

	redirectSandboxTestTemp(t, tempRoot)
	viaReal, err := fallbackSandboxRuntimeRoot(workspaceRoot)
	if err != nil {
		t.Fatalf("fallbackSandboxRuntimeRoot(real spelling): %v", err)
	}

	redirectSandboxTestTemp(t, alias)
	viaAlias, err := fallbackSandboxRuntimeRoot(workspaceRoot)
	if err != nil {
		t.Fatalf("fallbackSandboxRuntimeRoot(alias): %v", err)
	}

	if viaReal != viaAlias {
		t.Fatalf("two spellings of ONE temp directory produced two runtime roots:\n  via %s\n    -> %s\n  via %s\n    -> %s\nsetup and the command side derive from the same function, so they would grant and expect different paths, and pathWithinRoot would measure the workspace against a spelling it does not match", tempRoot, viaReal, alias, viaAlias)
	}
}

// TestWindowsSandboxRuntimeCandidatesUsesOneWorkspaceRoot pins the single-root
// contract rather than treating it as a defect. The marker compares plan hashes
// for EQUALITY and a command presents exactly one root, so deriving candidates for
// every root would put entries in the marker that no command reproduces. Whoever
// widens this has to change the marker comparison in the same change.
func TestWindowsSandboxRuntimeCandidatesUsesOneWorkspaceRoot(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()

	// Owned cache and temp even though this test only reads: the derivation would
	// otherwise depend on the developer's real cache directory, which makes the
	// result environment-dependent as well as impolite.
	cacheRoot := t.TempDir()
	originalCacheDir := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }
	t.Cleanup(func() { sandboxUserCacheDir = originalCacheDir })
	tempRoot := t.TempDir()
	redirectSandboxTestTemp(t, tempRoot)

	combined := windowsSandboxRuntimeRoots(PermissionProfile{}, []string{first, second})
	alone := windowsSandboxRuntimeRoots(PermissionProfile{}, []string{first})
	if len(combined) == 0 {
		t.Skip("no runtime candidates derivable in this environment")
	}
	separator := string(filepath.ListSeparator)
	if strings.Join(combined, separator) != strings.Join(alone, separator) {
		t.Fatalf("a second workspace root changed the candidate set:\n  [first, second] %v\n  [first]         %v\nsetup would grant roots no single command reproduces", combined, alone)
	}
}

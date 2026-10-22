//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Setup grants the principal a runtime tree; every command derives that tree
// again from the workspace root. If the two normalize differently the grant
// lands somewhere nothing reads, and the only symptom is a bare ACCESS_DENIED
// on the first cache write.
//
// This needs no symlink and no privilege. Windows opens a path whatever its
// casing, and Engine.resolveCommandDir runs EvalSymlinks (runner.go) which
// canonicalizes it, while setup used to only Clean.
func TestSetupRuntimeRootMatchesTheCommandPathForADifferentlyCasedRoot(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "MyWorkspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lowered := strings.ToLower(workspace)
	if _, err := os.Stat(lowered); err != nil {
		t.Skipf("filesystem is case-sensitive here: %v", err)
	}
	if lowered == workspace {
		t.Skip("temp path has no case to differ on")
	}

	// What the command path ends up keyed to, per resolveCommandDir.
	commandRoot := lowered
	if resolved, err := filepath.EvalSymlinks(filepath.Clean(lowered)); err == nil {
		commandRoot = resolved
	}
	if commandRoot == lowered {
		t.Skip("EvalSymlinks changed nothing on this host; no divergence to assert")
	}

	// Drive the PRODUCTION derivation, not the helper. A test that called
	// canonicalSandboxWorkspaceRoot directly would pass just as happily
	// with setup still doing filepath.Clean, which is exactly the bug.
	fromSetup, err := windowsSandboxRuntimeRootPath(WindowsSandboxCommandConfig{
		WorkspaceRoots: []string{lowered},
		CommandCWD:     lowered,
	})
	if err != nil {
		t.Fatalf("windowsSandboxRuntimeRootPath: %v", err)
	}
	cacheRoot, err := sandboxUserCacheDir()
	if err != nil {
		t.Fatalf("sandboxUserCacheDir: %v", err)
	}
	fromCommand, err := sandboxRuntimeRootFor(commandRoot, filepath.Clean(cacheRoot))
	if err != nil {
		t.Fatalf("sandboxRuntimeRootFor(command): %v", err)
	}
	if !grantedRuntimeRootsCover(fromSetup, fromCommand) {
		t.Errorf("setup grants a runtime tree commands never use:\n  setup:   %s\n  command: %s", fromSetup, fromCommand)
	}
}

// A root whose final segments do not exist still normalizes: the existing
// ancestor resolves and the missing remainder is re-appended.
//
// This asserted the whole cleaned path unchanged at first, which was the
// all-or-nothing behaviour the ancestor walk replaced. Windows CI failed it —
// correctly — because RUNNER~1 resolved to runneradmin while never-created
// stayed put, which is exactly the behaviour the walk exists to produce.
func TestCanonicalWorkspaceRootResolvesTheExistingAncestor(t *testing.T) {
	parent := t.TempDir()
	missing := filepath.Join(parent, "never-created", "deeper")

	got := canonicalSandboxWorkspaceRoot(missing)
	want := filepath.Join(canonicalSandboxWorkspaceRoot(parent), "never-created", "deeper")
	if got != want {
		t.Errorf("canonical(%q) = %q, want %q", missing, got, want)
	}
	// The missing segments must survive rather than be dropped to the ancestor.
	if !strings.HasSuffix(got, filepath.Join("never-created", "deeper")) {
		t.Errorf("canonical(%q) = %q, lost the segments that do not exist yet", missing, got)
	}
	if canonicalSandboxWorkspaceRoot("   ") != "" {
		t.Error("a blank root should stay blank, not become the process directory")
	}
}

// The pair has to agree, not just each side individually. CI caught this the
// hard way: canonicalizing only the setup side made setup and
// prepareSandboxRuntime disagree on a Windows runner, whose TEMP is an 8.3
// short path that resolution expands. Lowercasing reproduces the same class of
// non-canonical spelling without needing a short name or any privilege.
func TestSetupAndPrepareRuntimeAgreeOnANonCanonicalRoot(t *testing.T) {
	isolateSandboxRuntimeRoots(t)
	base := t.TempDir()
	workspace := filepath.Join(base, "MyWs")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lowered := strings.ToLower(workspace)
	if _, err := os.Stat(lowered); err != nil {
		t.Skipf("filesystem is case-sensitive here: %v", err)
	}

	granted, err := setupWindowsSandboxRuntimeRoot(WindowsSandboxCommandConfig{
		WorkspaceRoots: []string{lowered},
		CommandCWD:     lowered,
	})
	if err != nil {
		t.Fatalf("setupWindowsSandboxRuntimeRoot: %v", err)
	}
	state, release, err := prepareSandboxRuntime(lowered)
	if err != nil {
		t.Fatalf("prepareSandboxRuntime: %v", err)
	}
	if release != nil {
		defer release()
	}
	if !grantedRuntimeRootsCover(granted, state.Root) {
		t.Errorf("setup granted %q but commands write to %q", granted, state.Root)
	}
}

// The carveout shape has to survive a non-canonical root too. The first fix
// rebuilt the spec paths from the RESOLVED write root and compared them against
// subpaths that could not resolve (.git/config does not exist yet), so on a
// short-name or differently-cased path the match missed and .git/config went
// back to being created as a directory.
func TestGitConfigCarveoutShapeSurvivesANonCanonicalRoot(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "MyWs")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lowered := strings.ToLower(workspace)
	if _, err := os.Stat(lowered); err != nil {
		t.Skipf("filesystem is case-sensitive here: %v", err)
	}

	plan, err := buildWindowsPrincipalACLPlan(windowsPrincipalACLInput{
		PrincipalSID: "S-1-5-32-546",
		WriteRoots: []WritableRoot{{
			Root:             lowered,
			ReadOnlySubpaths: gitMetadataWriteCarveouts(lowered),
		}},
	})
	if err != nil {
		t.Fatalf("buildWindowsPrincipalACLPlan: %v", err)
	}
	found := false
	for _, entry := range plan.Entries {
		if !strings.EqualFold(filepath.Base(entry.Path), "config") {
			continue
		}
		found = true
		if !entry.MaterializeFile {
			t.Errorf(".git/config entry %q lost its file shape on a non-canonical root", entry.Path)
		}
	}
	if !found {
		t.Fatal("no .git/config entry in the plan")
	}
}

// Teardown must name the runtime tree without creating anything. The comment
// on windowsPrincipalTeardownPaths claimed that and it was false: the resolver
// it used ends in sandboxRuntimeRootFor, whose fallback calls os.MkdirTemp, so
// a workspace whose cache root sits inside it made setup's cleanup path create
// a fresh temp directory on its way out — and a useless one, since the fallback
// root is random per process and never matches what commands used.
func TestTeardownPathDerivationCreatesNothing(t *testing.T) {
	workspace := t.TempDir()
	// A PRIVATE OBSERVATION ROOT. The assertions below snapshot the temp
	// directory and fail on anything that appears, and the shared one is written
	// by every other package the runner is testing at the same time. It also
	// keeps the fallback runtime root this test drives inside the fixture.
	observed := filepath.Join(t.TempDir(), "observed")
	if err := os.Mkdir(observed, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMP", observed)
	t.Setenv("TEMP", observed)
	// Force the branch that falls back: cache root inside the workspace.
	original := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return filepath.Join(workspace, ".cache"), nil }
	t.Cleanup(func() { sandboxUserCacheDir = original })

	before := tempDirEntryNames(t)

	// Drive the PRODUCTION teardown path, not the helper. Calling the resolver
	// directly passes just as happily with the call site reverted to the one
	// that creates.
	paths, err := windowsPrincipalTeardownPaths(WindowsSandboxCommandConfig{
		WorkspaceRoots: []string{workspace},
		CommandCWD:     workspace,
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: workspace}},
			},
		},
	}, "S-1-5-32-546")
	if err != nil {
		t.Fatalf("windowsPrincipalTeardownPaths: %v", err)
	}
	if len(paths) == 0 {
		t.Error("teardown named no paths at all; the workspace root should still be revoked")
	}
	assertCreatedNothing(t, before, "teardown path derivation")

	// Setup's resolver must agree with teardown's, and create nothing either.
	//
	// This assertion has now been inverted twice, so the history is worth keeping.
	// It first required setup to fall back to a usable tree. That was then wrong,
	// because the fallback was os.MkdirTemp memoized only in-process: setup would
	// grant an ACE on temp root A, the next command would derive root B and fail
	// ACCESS_DENIED, and teardown would clean a third. So it was changed to demand
	// setup report NOTHING here.
	//
	// That is what is wrong now. fallbackSandboxRuntimeRoot derives its path by
	// hashing the workspace and creates nothing, so every process reaches the same
	// answer. Commands in this exact layout therefore DO select it and redirect
	// TMP, GOCACHE and the package caches into it, while setup granting nothing
	// left both principals without an ACE on the one tree those writes land in.
	// Reporting none is no longer the safe answer; it is the ACCESS_DENIED.
	beforeSetup := tempDirEntryNames(t)
	setupRoots, err := windowsSandboxRuntimeRootPath(WindowsSandboxCommandConfig{
		WorkspaceRoots: []string{workspace},
		CommandCWD:     workspace,
	})
	if err != nil {
		t.Fatalf("windowsSandboxRuntimeRootPath: %v", err)
	}
	if len(setupRoots) == 0 {
		t.Error("setup named no runtime root for a workspace whose cache-derived root is unusable; commands still select the stable fallback and would write there with no ACE")
	}
	// Purity is a property of RESOLUTION. Assert it here, before preparation,
	// which is allowed to create the fallback anchor it selects; asserting after
	// it blamed the resolver for preparation's legitimate directory.
	assertCreatedNothing(t, beforeSetup, "setup path derivation")
	// And it must be the tree a command actually picks, not merely some tree.
	commandState, release, err := prepareSandboxRuntime(workspace)
	if err != nil {
		t.Fatalf("prepareSandboxRuntime: %v", err)
	}
	if release != nil {
		defer release()
	}
	if !grantedRuntimeRootsCover(setupRoots, commandState.Root) {
		t.Errorf("setup granted %q but commands write to %q", setupRoots, commandState.Root)
	}
}

// Setup and teardown must derive the SAME root in the ordinary case, since one
// grants the ACE the other revokes. This is the case the fix above must not
// break: reporting "no root" is only correct when the root is underivable.
func TestSetupAndTeardownDeriveTheSameRuntimeRoot(t *testing.T) {
	workspace := t.TempDir()
	cacheRoot := t.TempDir() // outside the workspace, so the derivation is usable
	previous := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }
	t.Cleanup(func() { sandboxUserCacheDir = previous })

	setupRoot, err := windowsSandboxRuntimeRootPath(WindowsSandboxCommandConfig{
		WorkspaceRoots: []string{workspace},
		CommandCWD:     workspace,
	})
	if err != nil {
		t.Fatalf("windowsSandboxRuntimeRootPath: %v", err)
	}
	if len(setupRoot) == 0 {
		t.Fatal("setup named no runtime root for a derivable workspace")
	}
	teardownRoot, ok := deterministicSandboxRuntimeRoot(
		canonicalSandboxWorkspaceRoot(workspace), canonicalSandboxWorkspaceRoot(cacheRoot))
	if !ok {
		t.Fatal("precondition: the deterministic root should be usable here")
	}
	// The cache-derived root is the one a command picks in this layout, so it has
	// to be among what setup granted. Setup covering the fallback as well is the
	// point rather than a discrepancy: whichever the command lands on is
	// provisioned.
	if !grantedRuntimeRootsCover(setupRoot, teardownRoot) {
		t.Errorf("setup grants on %q but teardown revokes %q", setupRoot, teardownRoot)
	}
}

// tempDirEntryNames snapshots what the shared temp directory holds.
//
// NAMES, NOT A COUNT. The assertion is that deriving a path CREATES nothing,
// and a count cannot tell creation from removal: the shared temp root is also
// used by every other test binary running at the same time and by the OS, so a
// concurrent cleanup made the count fall and the test reported that naming the
// paths had "gained -1 entries". Comparing the sets answers the question that
// was actually being asked and is indifferent to anything disappearing.
func tempDirEntryNames(t *testing.T) map[string]struct{} {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	names := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		names[entry.Name()] = struct{}{}
	}
	return names
}

// assertCreatedNothing reports entries that appeared since the snapshot.
func assertCreatedNothing(t *testing.T, before map[string]struct{}, what string) {
	t.Helper()
	for name := range tempDirEntryNames(t) {
		if _, existed := before[name]; !existed {
			t.Errorf("%s created %q in the temp directory; naming the paths must not create one", what, name)
		}
	}
}

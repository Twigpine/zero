package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildWindowsACLPlanForWorkspaceWriteProfile(t *testing.T) {
	home := t.TempDir()
	config := WindowsSandboxCommandConfig{
		SandboxHome:    home,
		WorkspaceRoots: []string{`C:\workspace`},
		SandboxLevel:   WindowsSandboxLevelRestrictedToken,
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind: FileSystemRestricted,
				WriteRoots: []WritableRoot{
					{
						Root:                   `C:\workspace`,
						ReadOnlySubpaths:       []string{`C:\workspace\vendor`},
						ProtectedMetadataNames: []string{".git", ".zero"},
					},
					{Root: `D:\cache`},
				},
				DenyRead:  []string{`C:\workspace\secret-read`},
				DenyWrite: []string{`C:\workspace\secret-write`},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
	}

	plan, err := BuildWindowsACLPlan(config)
	if err != nil {
		t.Fatalf("BuildWindowsACLPlan: %v", err)
	}
	workspaceSID, err := WindowsWorkspaceCapabilitySID(home, `c:/workspace`)
	if err != nil {
		t.Fatalf("WindowsWorkspaceCapabilitySID: %v", err)
	}
	cacheSID, err := WindowsWritableRootCapabilitySID(home, `d:/cache`)
	if err != nil {
		t.Fatalf("WindowsWritableRootCapabilitySID: %v", err)
	}

	assertWindowsACLEntry(t, plan, WindowsACLAllowWrite, `C:\workspace`, workspaceSID, false)
	assertWindowsACLEntry(t, plan, WindowsACLAllowWrite, `D:\cache`, cacheSID, false)
	assertWindowsACLEntry(t, plan, WindowsACLDenyWrite, `C:\workspace\vendor`, workspaceSID, false)
	assertWindowsACLEntry(t, plan, WindowsACLDenyWrite, `C:\workspace\.git`, workspaceSID, false)
	assertWindowsACLEntry(t, plan, WindowsACLDenyWrite, `C:\workspace\.zero`, workspaceSID, false)
	assertWindowsACLEntry(t, plan, WindowsACLDenyWrite, `C:\workspace\secret-write`, workspaceSID, false)
	assertWindowsACLEntry(t, plan, WindowsACLDenyWrite, `C:\workspace\secret-write`, cacheSID, false)
	assertWindowsACLEntry(t, plan, WindowsACLDenyRead, `C:\workspace\secret-read`, workspaceSID, true)
	assertWindowsACLEntry(t, plan, WindowsACLDenyRead, `C:\workspace\secret-read`, cacheSID, true)

	// SID broadening is disabled, so the plan must not stamp shared system-path
	// DenyWrite ACEs or revoke legacy capability SIDs. Revocation could weaken
	// the boundary of a command launched by an earlier build.
	assertNoSharedSystemDenyWrites(t, plan)
	assertNoWindowsACLRevokes(t, plan)
}

// TestBuildWindowsACLPlanOmitsSharedDenyPathsWithoutDenyRead pins that
// profiles without DenyRead never stamp shared system-path DenyWrite ACEs or
// revoke old capability-SID guards that a running sandbox may still require.
func TestBuildWindowsACLPlanOmitsSharedDenyPathsWithoutDenyRead(t *testing.T) {
	home := t.TempDir()
	plan, err := BuildWindowsACLPlan(WindowsSandboxCommandConfig{
		SandboxHome:    home,
		WorkspaceRoots: []string{`C:\workspace`},
		SandboxLevel:   WindowsSandboxLevelRestrictedToken,
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: `C:\workspace`}},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
	})
	if err != nil {
		t.Fatalf("BuildWindowsACLPlan: %v", err)
	}
	assertNoSharedSystemDenyWrites(t, plan)
	assertNoWindowsACLRevokes(t, plan)
}

// TestBuildWindowsACLPlanOmitsSharedDenyPathsWhenUnelevated pins that the
// unelevated tier never stamps shared system-path DenyWrite ACEs (it also
// never broadens the restricted-SID list).
func TestBuildWindowsACLPlanOmitsSharedDenyPathsWhenUnelevated(t *testing.T) {
	home := t.TempDir()
	plan, err := BuildWindowsACLPlan(WindowsSandboxCommandConfig{
		SandboxHome:    home,
		WorkspaceRoots: []string{`C:\workspace`},
		SandboxLevel:   WindowsSandboxLevelUnelevated,
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: `C:\workspace`}},
				DenyRead:   []string{`C:\workspace\secret`},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
	})
	if err != nil {
		t.Fatalf("BuildWindowsACLPlan: %v", err)
	}
	assertNoSharedSystemDenyWrites(t, plan)
	assertNoWindowsACLRevokes(t, plan)
}

// TestBuildWindowsACLPlanDoesNotRevokeLegacyGuards pins that a setup run does
// not remove persistent guards installed by an older build. A previously
// launched sandbox can still carry the legacy capability SID, so removing its
// deny would widen that process's access.
func TestBuildWindowsACLPlanDoesNotRevokeLegacyGuards(t *testing.T) {
	home := t.TempDir()
	plan, err := BuildWindowsACLPlan(WindowsSandboxCommandConfig{
		SandboxHome:    home,
		WorkspaceRoots: []string{`C:\workspace`},
		SandboxLevel:   WindowsSandboxLevelRestrictedToken,
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: `C:\workspace`}},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
	})
	if err != nil {
		t.Fatalf("BuildWindowsACLPlan: %v", err)
	}
	assertNoWindowsACLRevokes(t, plan)
}

func assertNoSharedSystemDenyWrites(t *testing.T, plan WindowsACLPlan) {
	t.Helper()
	for _, path := range []string{`C:\`, `C:\ProgramData`, `C:\Windows\Temp`, `C:\Users\Public`} {
		for _, entry := range plan.Entries {
			if entry.Action == WindowsACLDenyWrite && windowsCapabilityPathKey(entry.Path) == windowsCapabilityPathKey(path) {
				t.Fatalf("plan stamps shared system DenyWrite on %q = %#v; SID broadening is disabled so shared denies must not be planned", path, entry)
			}
		}
	}
}

func assertNoWindowsACLRevokes(t *testing.T, plan WindowsACLPlan) {
	t.Helper()
	for _, entry := range plan.Entries {
		if entry.Action == WindowsACLRevokeCapability {
			t.Fatalf("plan = %#v, want no WindowsACLRevokeCapability entries", plan.Entries)
		}
	}
}

func TestBuildWindowsACLPlanRejectsUnrestrictedProfiles(t *testing.T) {
	_, err := BuildWindowsACLPlan(WindowsSandboxCommandConfig{
		SandboxHome: t.TempDir(),
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{Kind: FileSystemUnrestricted},
			Network:    NetworkPolicy{Mode: NetworkAllow},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "restricted filesystem") {
		t.Fatalf("BuildWindowsACLPlan error = %v, want restricted filesystem error", err)
	}
}

func TestPlanWindowsDenyReadPathsIncludesCanonicalExistingPath(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	wantRealDir, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatalf("EvalSymlinks real dir: %v", err)
	}
	linkDir := filepath.Join(root, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	paths := planWindowsDenyReadPaths([]string{linkDir})
	if !windowsPathListContains(paths, linkDir) {
		t.Fatalf("deny-read paths = %#v, want lexical path %q", paths, linkDir)
	}
	if !windowsPathListContains(paths, wantRealDir) {
		t.Fatalf("deny-read paths = %#v, want canonical path %q", paths, wantRealDir)
	}
}

func assertWindowsACLEntry(t *testing.T, plan WindowsACLPlan, action WindowsACLAction, path string, capability string, materialize bool) {
	t.Helper()
	assertWindowsACLEntryInheritance(t, plan, action, path, capability, materialize, false)
}

func assertWindowsACLEntryInheritance(t *testing.T, plan WindowsACLPlan, action WindowsACLAction, path string, capability string, materialize bool, noInherit bool) {
	t.Helper()
	for _, entry := range plan.Entries {
		if entry.Action == action &&
			windowsCapabilityPathKey(entry.Path) == windowsCapabilityPathKey(path) &&
			strings.EqualFold(entry.Capability, capability) &&
			entry.Materialize == materialize &&
			entry.NoInherit == noInherit {
			return
		}
	}
	t.Fatalf("ACL entries = %#v, want %s %q capability %q materialize=%v noInherit=%v", plan.Entries, action, path, capability, materialize, noInherit)
}

func windowsPathListContains(paths []string, want string) bool {
	wantKey := windowsCapabilityPathKey(want)
	for _, path := range paths {
		if windowsCapabilityPathKey(path) == wantKey {
			return true
		}
	}
	return false
}

// TestDedupeWindowsACLEntriesKeepsInheritanceVariants pins NoInherit as part
// of the entry identity: a direct-only deny and an inheritable deny on the
// same path and SID are different ACL shapes, and collapsing them could
// silently promote a deliberately non-inherited shared-path deny into an
// inheritable one that SetNamedSecurityInfo would propagate across a huge
// existing subtree.
func TestDedupeWindowsACLEntriesKeepsInheritanceVariants(t *testing.T) {
	entries := []WindowsACLEntry{
		{Action: WindowsACLDenyWrite, Path: `C:\shared`, Capability: "S-1-5-21-1", NoInherit: true},
		{Action: WindowsACLDenyWrite, Path: `C:\shared`, Capability: "S-1-5-21-1"},
		{Action: WindowsACLDenyWrite, Path: `C:\shared`, Capability: "S-1-5-21-1", NoInherit: true},
	}
	out := dedupeWindowsACLEntries(entries)
	if len(out) != 2 {
		t.Fatalf("dedupe = %#v, want the NoInherit and inheritable variants kept distinct", out)
	}
	if !out[0].NoInherit || out[1].NoInherit {
		t.Fatalf("dedupe order/shape = %#v, want first NoInherit then inheritable", out)
	}
}

// THE PLAN HAS TO CARRY THE ANCHOR, OR THE APPLY HAS NOTHING TO ENFORCE.
//
// The apply-side tests hand applyWindowsACLPathGroup an anchor directly, so they
// pass whether or not anything ever sets one. This drives the real builder and
// checks which entries come out anchored: the carveouts derived from a write
// root, and not the paths the operator named, which have no owned tail and whose
// intermediates are the operator's own business.
//
// NATIVE PATHS, NOT WINDOWS LITERALS. The builder is cross-platform and so is
// this test, and the anchoring decision runs through pathWithinRoot, which is
// filepath.Rel underneath. On Linux a backslash is an ordinary character, so
// `C:\workspace\.git\hooks` is one component and is not under `C:\workspace`;
// the neighbouring tests get away with such literals because they only compare
// strings they built the same way and never ask whether one contains another.
func TestBuildWindowsACLPlanAnchorsDerivedCarveouts(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	carveout := filepath.Join(workspace, ".git", "hooks")
	metadata := filepath.Join(workspace, ".zero")
	named := filepath.Join(t.TempDir(), "named-by-the-operator")
	config := WindowsSandboxCommandConfig{
		SandboxHome:    home,
		WorkspaceRoots: []string{workspace},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind: FileSystemRestricted,
				WriteRoots: []WritableRoot{{
					Root:                   workspace,
					ReadOnlySubpaths:       []string{carveout},
					ProtectedMetadataNames: []string{".zero"},
				}},
				DenyWrite: []string{named},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
	}

	plan, err := BuildWindowsACLPlan(config)
	if err != nil {
		t.Fatalf("BuildWindowsACLPlan: %v", err)
	}

	anchors := map[string]string{}
	for _, entry := range plan.Entries {
		anchors[strings.ToLower(entry.Path)] = entry.Anchor
	}
	for _, derived := range []string{carveout, metadata} {
		anchor, present := anchors[strings.ToLower(derived)]
		if !present {
			t.Fatalf("SETUP INVALID: the plan has no entry for the derived carveout %s", derived)
		}
		if anchor != workspace {
			t.Errorf("derived carveout %s carries anchor %q, want the write root it came from", derived, anchor)
		}
	}
	operatorAnchor, present := anchors[strings.ToLower(named)]
	if !present {
		t.Fatal("SETUP INVALID: the plan has no entry for the operator-named deny path")
	}
	if operatorAnchor != "" {
		t.Errorf("operator-named path carries anchor %q, want none: its intermediates are not the sandbox's to police", operatorAnchor)
	}
}

// AND AN OUT-OF-ROOT READ-ONLY SUBPATH IS NOT ANCHORED AT ALL.
//
// ReadOnlySubpaths is a profile field an operator can set to any path. One
// placed outside the write root is a configuration that works today, and
// anchoring it to that root would turn it into a containment refusal: the
// object is not under the root and never was. Only the paths that are actually
// under it are held to it.
func TestBuildWindowsACLPlanLeavesAnOutOfRootSubpathUnanchored(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	inside := filepath.Join(workspace, "vendor")
	outside := filepath.Join(t.TempDir(), "shared")
	plan, err := BuildWindowsACLPlan(WindowsSandboxCommandConfig{
		SandboxHome:    home,
		WorkspaceRoots: []string{workspace},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind: FileSystemRestricted,
				WriteRoots: []WritableRoot{{
					Root:             workspace,
					ReadOnlySubpaths: []string{inside, outside},
				}},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
	})
	if err != nil {
		t.Fatalf("BuildWindowsACLPlan: %v", err)
	}

	seen := map[string]string{}
	for _, entry := range plan.Entries {
		seen[strings.ToLower(entry.Path)] = entry.Anchor
	}
	insideAnchor, present := seen[strings.ToLower(inside)]
	if !present {
		t.Fatal("SETUP INVALID: the plan has no entry for the in-root subpath")
	}
	if insideAnchor != workspace {
		t.Errorf("in-root subpath carries anchor %q, want the write root", insideAnchor)
	}
	outsideAnchor, present := seen[strings.ToLower(outside)]
	if !present {
		t.Fatal("SETUP INVALID: the plan has no entry for the out-of-root subpath")
	}
	if outsideAnchor != "" {
		t.Errorf("out-of-root subpath carries anchor %q, want none: the apply would refuse a config that works today", outsideAnchor)
	}
}

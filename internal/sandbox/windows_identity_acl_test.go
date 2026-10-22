package sandbox

import (
	"path/filepath"
	"strings"
	"testing"
)

const testPrincipalSID = "S-1-5-21-1111111111-2222222222-3333333333-1005"

func testPrincipalInput() windowsPrincipalACLInput {
	return windowsPrincipalACLInput{
		PrincipalSID: testPrincipalSID,
		WriteRoots: []WritableRoot{{
			Root:                   filepath.FromSlash("/ws/project"),
			ReadOnlySubpaths:       []string{filepath.FromSlash("/ws/project/.git/config")},
			ProtectedMetadataNames: []string{".zero", ".agents"},
		}},
		ReadRoots: []string{filepath.FromSlash("/usr/lib")},
		DenyRead:  []string{filepath.FromSlash("/shared/secrets")},
	}
}

// Windows evaluates an explicit DACL deny-before-allow, so a carve-out inside a
// granted root only survives if its deny ACE is written first. If the grant on
// the workspace landed before the deny on .zero, the protected metadata would be
// writable from inside the sandbox.
func TestPrincipalACLPlanEmitsDeniesBeforeAllows(t *testing.T) {
	plan, err := buildWindowsPrincipalACLPlan(testPrincipalInput())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	lastDeny, firstAllow := -1, -1
	for index, entry := range plan.Entries {
		switch entry.Action {
		case WindowsACLDenyRead, WindowsACLDenyWrite:
			lastDeny = index
		case WindowsACLAllowWrite, WindowsACLAllowRead:
			if firstAllow == -1 {
				firstAllow = index
			}
		}
	}
	if firstAllow == -1 || lastDeny == -1 {
		t.Fatalf("plan is missing a deny or an allow: %+v", plan.Entries)
	}
	if lastDeny > firstAllow {
		t.Fatalf("deny at %d comes after allow at %d; carve-outs would be overridden", lastDeny, firstAllow)
	}
}

// Every ACE must name the sandbox principal. An entry with any other trustee
// would change access for a real user.
func TestPrincipalACLPlanNamesOnlyThePrincipal(t *testing.T) {
	plan, err := buildWindowsPrincipalACLPlan(testPrincipalInput())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, entry := range plan.Entries {
		if entry.Capability != testPrincipalSID {
			t.Fatalf("entry %+v names %q, want the principal SID", entry, entry.Capability)
		}
	}
}

// A principal is a separate account with no inherent access, so a write root
// must be granted read+write and a read root granted read. Without the grant the
// sandbox cannot open its own workspace.
func TestPrincipalACLPlanGrantsRoots(t *testing.T) {
	plan, err := buildWindowsPrincipalACLPlan(testPrincipalInput())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var grantedWrite, grantedRead bool
	for _, entry := range plan.Entries {
		if entry.Action == WindowsACLAllowWrite && entry.Path == normalizeProfilePath(filepath.FromSlash("/ws/project")) {
			grantedWrite = true
		}
		if entry.Action == WindowsACLAllowRead && entry.Path == normalizeProfilePath(filepath.FromSlash("/usr/lib")) {
			grantedRead = true
		}
	}
	if !grantedWrite {
		t.Fatal("write root was not granted; the sandbox could not write its workspace")
	}
	if !grantedRead {
		t.Fatal("read root was not granted; the sandbox could not read it")
	}
}

// Protected metadata is denied write and marked Materialize so the ACE is
// created even when the directory does not exist yet, closing the window where
// a sandboxed command creates .zero before the deny lands.
func TestPrincipalACLPlanProtectsMetadata(t *testing.T) {
	plan, err := buildWindowsPrincipalACLPlan(testPrincipalInput())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	found := map[string]WindowsACLEntry{}
	for _, entry := range plan.Entries {
		found[entry.Path] = entry
	}
	for _, name := range []string{".zero", ".agents"} {
		path := filepath.Join(normalizeProfilePath(filepath.FromSlash("/ws/project")), name)
		entry, ok := found[path]
		if !ok {
			t.Fatalf("no entry protecting %s", path)
		}
		if entry.Action != WindowsACLDenyWrite {
			t.Fatalf("%s has action %q, want deny-write", path, entry.Action)
		}
		if !entry.Materialize {
			t.Fatalf("%s must be materialized so the deny exists before the directory does", path)
		}
	}
}

// A missing principal SID must be a hard error: an empty trustee would either
// fail at apply time or, worse, be interpreted as some other account.
func TestPrincipalACLPlanRequiresSID(t *testing.T) {
	input := testPrincipalInput()
	input.PrincipalSID = ""
	if _, err := buildWindowsPrincipalACLPlan(input); err == nil {
		t.Fatal("an empty principal SID must be rejected")
	}
}

// A plan with no roots at all is a caller mistake rather than a valid empty
// grant, since the resulting sandbox could not run anything.
func TestPrincipalACLPlanRequiresRoots(t *testing.T) {
	input := windowsPrincipalACLInput{PrincipalSID: testPrincipalSID}
	if _, err := buildWindowsPrincipalACLPlan(input); err == nil {
		t.Fatal("a plan with no roots must be rejected")
	}
}

// Revocation is keyed to the trustee, so retiring a principal removes every ACE
// naming it without having to remember what was granted. This is the cleanup
// path the capability-SID model lacks.
func TestPrincipalRevokePlanTargetsTrustee(t *testing.T) {
	paths := []string{filepath.FromSlash("/ws/project"), filepath.FromSlash("/usr/lib")}
	plan, err := windowsPrincipalRevokePlan(testPrincipalSID, paths)
	if err != nil {
		t.Fatalf("revoke plan: %v", err)
	}
	if len(plan.Entries) != len(paths) {
		t.Fatalf("got %d entries, want %d", len(plan.Entries), len(paths))
	}
	for _, entry := range plan.Entries {
		if entry.Action != windowsACLRevoke {
			t.Fatalf("entry %+v is not a revoke", entry)
		}
		if entry.Capability != testPrincipalSID {
			t.Fatalf("revoke names %q, want the principal", entry.Capability)
		}
	}
}

func TestPrincipalRevokePlanRequiresSID(t *testing.T) {
	if _, err := windowsPrincipalRevokePlan("", []string{"/ws"}); err == nil {
		t.Fatal("revoking without a principal SID must be rejected")
	}
}

// The action strings end up in a serialized plan consumed by the elevated
// helper, so they must stay stable and distinct from the existing actions.
func TestPrincipalACLActionsAreDistinct(t *testing.T) {
	actions := []WindowsACLAction{
		WindowsACLAllowWrite, WindowsACLAllowRead,
		WindowsACLDenyRead, WindowsACLDenyWrite, windowsACLRevoke,
	}
	seen := map[WindowsACLAction]bool{}
	for _, action := range actions {
		if strings.TrimSpace(string(action)) == "" {
			t.Fatal("an action string is empty")
		}
		if seen[action] {
			t.Fatalf("duplicate action %q", action)
		}
		seen[action] = true
	}
}

// ProtectedMetadataNames is joined onto the write root to place a deny ACE and to
// materialize the directory it names. A value that is not a single component
// therefore puts both OUTSIDE the workspace: ".." walks up out of it, and a
// separator reaches through whatever sits in between. Every caller passes a
// package constant today, so this is the guard that keeps it true if one ever
// sources these from config.
func TestPrincipalACLPlanRefusesProtectedNamesThatEscapeTheWriteRoot(t *testing.T) {
	root := filepath.FromSlash("/ws/project")
	for _, name := range []string{"..", ".", "", `..\..\Windows\System32`, "nested/child", `nested\child`, "C:", "stream:name"} {
		t.Run(name, func(t *testing.T) {
			input := testPrincipalInput()
			input.WriteRoots = []WritableRoot{{
				Root:                   root,
				ProtectedMetadataNames: []string{name},
			}}
			plan, err := buildWindowsPrincipalACLPlan(input)
			if err == nil {
				t.Fatalf("accepted protected metadata name %q, which would place a deny ACE outside %s:\n%#v", name, root, plan.Entries)
			}
			if len(plan.Entries) != 0 {
				t.Errorf("returned %d entries alongside the error, so a caller ignoring err would still apply them", len(plan.Entries))
			}
		})
	}
}

// A PRINCIPAL MUST NEVER BE GRANTED READ AT A VOLUME ROOT.
//
// permissionProfileReadRoots seeds its list with the bare separator, because
// the workspace-write posture is read-all with a write jail. That costs nothing
// for the capability backend, whose child runs as the caller and could read
// those paths anyway. For a principal it is a real, persistent, inheritable
// allow-read ACE for a separate local account at the root of the drive.
//
// Built from the production profile rather than a synthetic fixture, because
// the whole point is that the shipped configuration produced it.
func TestPrincipalACLPlanNeverGrantsReadAtAVolumeRoot(t *testing.T) {
	workspace := filepath.FromSlash("/ws/project")
	profile := DefaultPermissionProfile(workspace)

	// If the profile ever stops carrying a volume root, this test proves nothing
	// and should be retired rather than left passing vacuously.
	seeded := false
	for _, root := range profile.FileSystem.ReadRoots {
		if isWindowsVolumeRoot(normalizeProfilePath(root)) {
			seeded = true
			break
		}
	}
	if !seeded {
		t.Skipf("the production profile no longer contains a volume read root: %v", profile.FileSystem.ReadRoots)
	}

	plan, err := buildWindowsPrincipalACLPlan(windowsPrincipalACLInput{
		PrincipalSID: testPrincipalSID,
		ReadRoots:    profile.FileSystem.ReadRoots,
		WriteRoots:   profile.FileSystem.WriteRoots,
	})
	if err != nil {
		t.Fatalf("buildWindowsPrincipalACLPlan: %v", err)
	}
	for _, entry := range plan.Entries {
		if entry.Action == WindowsACLAllowRead && isWindowsVolumeRoot(entry.Path) {
			t.Errorf("plan grants the principal read at the volume root %q, which inherits into every directory on the drive", entry.Path)
		}
	}
	// And the workspace itself must still be reachable, or this traded a real
	// grant for a broken sandbox.
	wantWorkspace := normalizeProfilePath(workspace)
	reachable := false
	for _, entry := range plan.Entries {
		if entry.Path == wantWorkspace && (entry.Action == WindowsACLAllowRead || entry.Action == WindowsACLAllowWrite) {
			reachable = true
			break
		}
	}
	if !reachable {
		t.Errorf("no grant for the workspace root %q, so the principal could not read its own workspace", wantWorkspace)
	}
}

// The ordinary names must keep working, or the guard above is just a break.
func TestPrincipalACLPlanStillAcceptsTheRealProtectedNames(t *testing.T) {
	input := testPrincipalInput()
	input.WriteRoots = []WritableRoot{{
		Root:                   filepath.FromSlash("/ws/project"),
		ProtectedMetadataNames: sandboxFullyProtectedMetadataNames,
	}}
	if _, err := buildWindowsPrincipalACLPlan(input); err != nil {
		t.Fatalf("the shipped protected names were rejected: %v", err)
	}
}

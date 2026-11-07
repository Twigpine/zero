//go:build windows

package sandbox

import (
	"path/filepath"
	"strings"
	"testing"
)

// ONE PLAN, TWO TIERS, DIFFERENT AUTHORITY.
//
// Production profiles seed ReadRoots with the filesystem root, so a profile that
// configures DenyRead adds an allow-read ACE for the read capability at the
// volume root: the strict token that DenyRead selects applies the restricted-SID
// check to reads, so without it the command cannot even open its own executable.
// Elevated setup can write that DACL. An ordinary user cannot, and the common
// opener asks for WRITE_DAC on every entry, so the unelevated tier failed on that
// one root on every command, with a generic diagnosis and a remedy that did not
// apply.
//
// The real smoke test misses this because it substitutes a user-owned temporary
// directory for the production read root, which is exactly why this asserts on
// the production shape.
func TestUnelevatedSetupRefusesAVolumeRootReadGrant(t *testing.T) {
	workspace := t.TempDir()
	volumeRoot := filepath.VolumeName(workspace) + string(filepath.Separator)
	if !isWindowsVolumeRoot(volumeRoot) {
		t.Fatalf("SETUP INVALID: %q is not recognised as a volume root", volumeRoot)
	}

	config := WindowsSandboxCommandConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     workspace,
		WorkspaceRoots: []string{workspace},
		SandboxLevel:   WindowsSandboxLevelUnelevated,
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: workspace}},
				// The production shape: read everywhere, and a denyRead that forces
				// the strict token.
				ReadRoots: []string{volumeRoot},
				DenyRead:  []string{filepath.Join(workspace, "secrets")},
			},
		},
	}

	plan, err := BuildWindowsACLPlan(config)
	if err != nil {
		t.Fatalf("BuildWindowsACLPlan: %v", err)
	}
	// SETUP: the plan really does reach the volume root, or the refusal below
	// would be asserting nothing.
	if got := windowsPlanVolumeRootGrant(plan); got == "" {
		t.Fatalf("SETUP INVALID: the plan carries no volume-root entry, so this profile does not reproduce the case: %+v", plan.Entries)
	}

	err = ensureWindowsUnelevatedSetup(config)
	if err == nil {
		t.Fatal("unelevated setup accepted a plan it cannot apply; every command would fail later with a generic ACCESS_DENIED")
	}
	message := err.Error()
	if !strings.Contains(message, volumeRoot) {
		t.Errorf("the refusal does not name the volume root, so the reader cannot tell which entry is at fault: %v", err)
	}
	if !strings.Contains(message, "denyRead") {
		t.Errorf("the refusal does not name the cause, so the reader cannot act on it: %v", err)
	}
	if !strings.Contains(message, "elevated") {
		t.Errorf("the refusal does not name a remedy the reader can carry out: %v", err)
	}
}

// And a profile with no denyRead still runs on this tier: it needs no volume-root
// grant, so refusing it would take the unelevated sandbox away from everyone.
func TestUnelevatedSetupStillAcceptsAWriteJailOnlyProfile(t *testing.T) {
	workspace := t.TempDir()
	config := WindowsSandboxCommandConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     workspace,
		WorkspaceRoots: []string{workspace},
		SandboxLevel:   WindowsSandboxLevelUnelevated,
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: workspace}},
				ReadRoots:  []string{workspace},
			},
		},
	}
	plan, err := BuildWindowsACLPlan(config)
	if err != nil {
		t.Fatalf("BuildWindowsACLPlan: %v", err)
	}
	if got := windowsPlanVolumeRootGrant(plan); got != "" {
		t.Fatalf("SETUP INVALID: a write-jail-only profile reached the volume root at %q", got)
	}
	if err := ensureWindowsUnelevatedSetup(config); err != nil && strings.Contains(err.Error(), "volume root") {
		t.Fatalf("a profile needing no volume-root grant was refused by the tier check: %v", err)
	}
}

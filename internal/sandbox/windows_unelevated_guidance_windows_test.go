//go:build windows

package sandbox

import (
	"errors"
	"strings"
	"testing"
)

// EVERY REMEDY THIS ERROR NAMES MUST BE ONE THE READER CAN CARRY OUT.
//
// The message told operators to re-run with `--sandbox forbid`. No such option
// exists: SandboxPreferenceForbid is an internal engine state with no flag
// behind it, so acting on the advice produced an unknown option and left them
// stuck on the failure they had just been told how to clear.
//
// It survived because nothing drove this branch. The text was only ever correct
// by inspection, and inspection is what missed it, so the fix is not complete
// until something fails the apply and reads the guidance back.
func TestUnelevatedACLFailureNamesOnlyRealRemedies(t *testing.T) {
	workspace := t.TempDir()
	config := WindowsSandboxCommandConfig{
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
		SandboxLevel: WindowsSandboxLevelUnelevated,
	}

	denied := errors.New("Access is denied.")
	original := applyWindowsUnelevatedACLPlanFn
	t.Cleanup(func() { applyWindowsUnelevatedACLPlanFn = original })
	applyWindowsUnelevatedACLPlanFn = func(WindowsACLPlan) (func() error, error) {
		return nil, denied
	}

	err := ensureWindowsUnelevatedSetup(config)
	if err == nil {
		t.Fatal("ensureWindowsUnelevatedSetup returned nil when the ACL apply failed, so the command would run believing it was sandboxed")
	}

	// The refusal has to keep naming its cause, or the operator cannot tell an
	// ACL failure apart from the sandboxed command being rejected.
	if !errors.Is(err, denied) {
		t.Errorf("error does not wrap the apply failure, so the cause is lost: %v", err)
	}

	message := err.Error()

	// The option that does not exist must never come back.
	if strings.Contains(message, "--sandbox forbid") {
		t.Errorf("error still advertises `--sandbox forbid`, which is not a real option: %s", message)
	}

	// Both surviving remedies are real: elevated setup, and the user-config key,
	// which is honored from global config only so a cloned repo cannot set it.
	for _, want := range []string{
		"zero sandbox setup",
		`"sandbox": {"enabled": false}`,
	} {
		if !strings.Contains(message, want) {
			t.Errorf("error does not offer %q, leaving the reader without a way out: %s", want, message)
		}
	}
}

// The failure must not be recorded as a success. The applied-plan marker is
// what makes later commands skip the re-apply, so writing it here would turn
// one refusal into a sandbox that silently never applies its ACLs again.
func TestUnelevatedACLFailureDoesNotRecordTheMarker(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	config := WindowsSandboxCommandConfig{
		SandboxHome:    home,
		CommandCWD:     workspace,
		WorkspaceRoots: []string{workspace},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: workspace}},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
		SandboxLevel: WindowsSandboxLevelUnelevated,
	}

	original := applyWindowsUnelevatedACLPlanFn
	t.Cleanup(func() { applyWindowsUnelevatedACLPlanFn = original })
	applyWindowsUnelevatedACLPlanFn = func(WindowsACLPlan) (func() error, error) {
		return nil, errors.New("Access is denied.")
	}

	if err := ensureWindowsUnelevatedSetup(config); err == nil {
		t.Fatal("expected the apply failure to surface")
	}

	applied, _, err := buildWindowsUnelevatedAppliedPlan(config)
	if err != nil {
		t.Fatalf("buildWindowsUnelevatedAppliedPlan: %v", err)
	}
	marker, err := loadWindowsUnelevatedSetupMarker(home)
	if err != nil {
		t.Fatalf("loadWindowsUnelevatedSetupMarker: %v", err)
	}
	if marker.contains(applied) {
		t.Error("the failed plan was recorded as applied, so every later command would skip the apply and run unjailed")
	}
}

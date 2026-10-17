//go:build windows

package sandbox

import "testing"

// THE COMMAND HAS TO RECOGNISE THE ROOT SETUP RECORDED, WHATEVER ITS
// ENVIRONMENT SAYS.
//
// Setup derives the fallback root in an elevated terminal and records it; a
// command re-derives it in an ordinary one to decide whether the record belongs
// to this workspace. The user scope in that derivation used to come from
// USERNAME, so a value that differed between the two terminals made the command
// reject the record and select a root with no capability ACE on it.
func TestRecordedFallbackRootSurvivesADifferentUsernameVariable(t *testing.T) {
	// Canonical, as selection hands it over. The record check canonicalizes on
	// its own, so a raw t.TempDir() spelling digests differently wherever TEMP is
	// an 8.3 short path, as it is on the CI runners.
	workspace := canonicalSandboxWorkspaceRoot(t.TempDir())
	tempHome := t.TempDir()
	t.Setenv("TMP", tempHome)
	t.Setenv("TEMP", tempHome)

	t.Setenv("USERNAME", "setup-terminal")
	recorded, err := fallbackSandboxRuntimeRoot(workspace)
	if err != nil {
		t.Fatalf("derive the fallback root as setup: %v", err)
	}
	if !ownedFallbackRuntimeRecord(workspace, recorded) {
		t.Fatalf("SETUP INVALID: the fallback root is not recognised even in the environment that derived it: %s", recorded)
	}

	t.Setenv("USERNAME", "command-terminal")
	if !ownedFallbackRuntimeRecord(workspace, recorded) {
		t.Fatalf("once USERNAME changed, the command no longer recognises the fallback root setup recorded: %s", recorded)
	}
	derived, err := fallbackSandboxRuntimeRoot(workspace)
	if err != nil {
		t.Fatalf("derive the fallback root as the command: %v", err)
	}
	if !sameWindowsRuntimeRootPath(derived, recorded) {
		t.Fatalf("setup and the command derived different fallback roots for one workspace:\n  setup:   %s\n  command: %s", recorded, derived)
	}
}

package sandbox

import (
	"strings"
	"testing"
)

// SETUP AND THE COMMAND HAVE TO START FROM THE SAME PREPARATION.
//
// BuildWindowsSandboxSetupArgs selects the runtime root and folds it into the
// profile before it serializes, so the marker fingerprints a plan with the
// runtime entries in it. A command only matches that marker if its profile went
// through the planner's half of the same contract. The native smoke test did not:
// it kept the bare profile it had started with and handed that to the command
// builder, so the elevated runner refused its first command and none of the
// isolation probes after it ran.
//
// Portable, and through the real builders and parsers on both sides. It starts
// from a profile that is checked to be bare, so it cannot pass because a fixture
// was augmented in advance. It does not replace the native smoke run, which is
// the only thing that executes the enforcement probes.
func TestSetupAndCommandBuildersAgreeStartingFromABareProfile(t *testing.T) {
	workspace, _ := windowsRuntimeTestRoots(t)
	home := t.TempDir()
	profile := bareWindowsProfile(workspace)
	if profile.Runtime != nil || len(profile.FileSystem.WriteRoots) != 1 {
		t.Fatalf("SETUP INVALID: the starting profile is not bare: %+v", profile)
	}

	setup := preparedWindowsSetupConfig(t, workspace, home, profile)
	publishThroughTheMarkerWriter(t, setup)
	setupMarker, err := BuildWindowsSandboxSetupMarker(setup)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(setupMarker.RuntimeRoot) == "" {
		t.Fatal("SETUP INVALID: setup recorded no runtime root, so there is no contract to agree on")
	}

	command, err := preparedWindowsCommandConfig(t, workspace, home, profile)
	if err != nil {
		t.Fatalf("prepare the command: %v", err)
	}
	commandMarker, err := BuildWindowsSandboxSetupMarker(WindowsSandboxSetupConfigFromCommand(command))
	if err != nil {
		t.Fatal(err)
	}
	if !sameWindowsRuntimeRootPath(commandMarker.RuntimeRoot, setupMarker.RuntimeRoot) {
		t.Errorf("the command selected %q and setup provisioned %q", commandMarker.RuntimeRoot, setupMarker.RuntimeRoot)
	}
	if commandMarker.ACLPlanHash != setupMarker.ACLPlanHash || commandMarker.ACLPlanEntries != setupMarker.ACLPlanEntries {
		t.Errorf("the two sides built different plans: setup %s with %d entries, command %s with %d entries",
			shortWindowsACLPlanHash(setupMarker.ACLPlanHash), setupMarker.ACLPlanEntries,
			shortWindowsACLPlanHash(commandMarker.ACLPlanHash), commandMarker.ACLPlanEntries)
	}
	if err := ValidateWindowsSandboxSetupMarker(WindowsSandboxSetupConfigFromCommand(command)); err != nil {
		t.Errorf("the runner would refuse the prepared command: %v", err)
	}

	// THE CONTROL, which is what the smoke test used to send: the bare profile,
	// straight into the command builder. It has to be refused, or the agreement
	// above is not what made the prepared command acceptable.
	args, err := BuildWindowsSandboxCommandArgs(WindowsSandboxCommandArgsOptions{
		SandboxHome:       home,
		CommandCWD:        workspace,
		WorkspaceRoots:    []string{workspace},
		PermissionProfile: profile,
		SandboxLevel:      WindowsSandboxLevelRestrictedToken,
		Command:           []string{"cmd.exe", "/c", "exit", "0"},
	})
	if err != nil {
		t.Fatalf("BuildWindowsSandboxCommandArgs: %v", err)
	}
	bare, err := ParseWindowsSandboxCommandArgs(args)
	if err != nil {
		t.Fatalf("ParseWindowsSandboxCommandArgs: %v", err)
	}
	if err := ValidateWindowsSandboxSetupMarker(WindowsSandboxSetupConfigFromCommand(bare)); err == nil {
		t.Error("a command built from the bare profile validates, so this test cannot tell the two preparations apart")
	}
}

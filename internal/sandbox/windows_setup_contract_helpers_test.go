package sandbox

import (
	"os"
	"strings"
	"testing"
)

// THE WIRE ON BOTH SIDES.
//
// Setup and the later command are two processes that agree only through what
// one of them persisted. These helpers build each side the way production does,
// so a test that uses them is checking that agreement rather than a fixture that
// was written to agree: the setup config is what BuildWindowsSandboxSetupArgs
// serialized and ParseWindowsSandboxSetupArgs read back, and the command is what
// the planner prepares, serialized and parsed the same way, handed to the
// validator the runner calls.

// bareWindowsProfile is a profile as a caller writes one: no runtime, no runtime
// write roots. Anything that needs those has to get them from the production
// preparation, which is the agreement under test, so an assertion built on this
// cannot pass because the fixture was augmented in advance.
func bareWindowsProfile(workspace string, extraWriteRoots ...string) PermissionProfile {
	roots := []WritableRoot{{Root: workspace}}
	for _, extra := range extraWriteRoots {
		roots = append(roots, WritableRoot{Root: extra})
	}
	return PermissionProfile{
		FileSystem: FileSystemPolicy{Kind: FileSystemRestricted, WriteRoots: roots},
		Network:    NetworkPolicy{Mode: NetworkDeny},
	}
}

// preparedWindowsSetupConfig is the setup side of the wire: the args the
// operator's shell builds, parsed the way the elevated helper parses them.
func preparedWindowsSetupConfig(t *testing.T, workspace, home string, profile PermissionProfile) WindowsSandboxSetupConfig {
	t.Helper()
	plan, err := BuildWindowsSandboxSetupArgs(WindowsSandboxSetupArgsOptions{
		SandboxHome:       home,
		CommandCWD:        workspace,
		WorkspaceRoots:    []string{workspace},
		PermissionProfile: profile,
	})
	if err != nil {
		t.Fatalf("BuildWindowsSandboxSetupArgs: %v", err)
	}
	config, err := ParseWindowsSandboxSetupArgs(plan.Args)
	if err != nil {
		t.Fatalf("ParseWindowsSandboxSetupArgs: %v", err)
	}
	return config
}

// plannerPreparedWindowsProfile does to a bare profile what the command planner
// does before the Windows runner ever sees it: select the runtime, which honours
// the root a previous setup recorded for this home, fold it into the profile,
// and provision and grant the runtime roots. The returned release gives the
// runtime lease back and has to outlive the command.
//
// One implementation, because the native smoke test had its own second one: it
// sent the setup side through BuildWindowsSandboxSetupArgs, which selects and
// augments, and handed the command builder the original bare profile. The two
// plans then differed by exactly the runtime entries and the elevated runner
// refused the first command before any probe ran. Reported by @jatmn.
func plannerPreparedWindowsProfile(workspace, home string, profile PermissionProfile) (PermissionProfile, func(), error) {
	runtimeState, release, err := prepareSandboxRuntime(workspace, home)
	if err != nil {
		return PermissionProfile{}, nil, err
	}
	prepared, err := windowsSandboxProfileWithProvisionedRuntime(
		permissionProfileWithRuntime(profile, runtimeState), []string{workspace})
	if err != nil {
		release()
		return PermissionProfile{}, nil, err
	}
	return prepared, release, nil
}

// preparedWindowsCommandConfig is the command side: the planner's preparation
// from the SAME bare profile, through the runner's own arg builder and parser.
func preparedWindowsCommandConfig(t *testing.T, workspace, home string, profile PermissionProfile) (WindowsSandboxCommandConfig, error) {
	t.Helper()
	prepared, release, err := plannerPreparedWindowsProfile(workspace, home, profile)
	if err != nil {
		return WindowsSandboxCommandConfig{}, err
	}
	defer release()
	args, err := BuildWindowsSandboxCommandArgs(WindowsSandboxCommandArgsOptions{
		SandboxHome:       home,
		CommandCWD:        workspace,
		WorkspaceRoots:    []string{workspace},
		PermissionProfile: prepared,
		SandboxLevel:      WindowsSandboxLevelRestrictedToken,
		Command:           []string{"cmd.exe", "/c", "exit", "0"},
	})
	if err != nil {
		t.Fatalf("BuildWindowsSandboxCommandArgs: %v", err)
	}
	command, err := ParseWindowsSandboxCommandArgs(args)
	if err != nil {
		t.Fatalf("ParseWindowsSandboxCommandArgs: %v", err)
	}
	return command, nil
}

// validateAsALaterCommand asks what the runner asks of a command prepared after
// setup returned. A marker that was written successfully says nothing about
// whether the next command accepts it, and that second question is the one every
// finding here was about.
func validateAsALaterCommand(t *testing.T, workspace, home string, profile PermissionProfile) error {
	t.Helper()
	command, err := preparedWindowsCommandConfig(t, workspace, home, profile)
	if err != nil {
		return err
	}
	return ValidateWindowsSandboxSetupMarker(WindowsSandboxSetupConfigFromCommand(command))
}

// publishWindowsSetup is one way of completing a setup. The portable form
// records the stamp and marker the way WriteWindowsSandboxSetupMarker does; the
// Windows build also runs the elevated helper's whole transaction.
type publishWindowsSetup func(t *testing.T, config WindowsSandboxSetupConfig)

func publishThroughTheMarkerWriter(t *testing.T, config WindowsSandboxSetupConfig) {
	t.Helper()
	if _, err := WriteWindowsSandboxSetupMarker(config); err != nil {
		t.Fatalf("WriteWindowsSandboxSetupMarker: %v", err)
	}
}

// recreateRuntimeTree is what eviction followed by an ordinary run does: the
// same pathname, a new object, none of what setup put there.
func recreateRuntimeTree(t *testing.T, root string) {
	t.Helper()
	if strings.TrimSpace(root) == "" {
		t.Fatal("SETUP INVALID: no runtime root to recreate")
	}
	requireWithinTestOwned(t, root, os.TempDir(), mustSandboxUserCacheDir(t))
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("remove the runtime tree %s: %v", root, err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("recreate the runtime tree %s: %v", root, err)
	}
}

func mustSandboxUserCacheDir(t *testing.T) string {
	t.Helper()
	cacheRoot, err := sandboxUserCacheDir()
	if err != nil {
		t.Fatalf("SETUP INVALID: no user cache directory: %v", err)
	}
	return cacheRoot
}

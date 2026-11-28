package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// userStateIsolatedEnv marks a process whose per-user directories have already
// been replaced. A test that starts this test binary again as a child inherits
// it and keeps the parent's root. A test that hands a child its own
// environment sets it too, so the child keeps the roots it was given.
const userStateIsolatedEnv = "ZERO_TEST_CLI_USER_STATE_ISOLATED"

// userStateOverrideEnvNames outrank the base directories: an inherited value
// would send the sandbox grants file and the Windows sandbox home back to the
// developer's real ones whatever the base directories say.
var userStateOverrideEnvNames = []string{"ZERO_SANDBOX_GRANTS_PATH", "ZERO_WINDOWS_SANDBOX_HOME"}

// packageUserStateRoot is the throwaway root TestMain put behind every
// per-user base directory, or empty in a child that was already isolated.
var packageUserStateRoot string

// TestMain runs every test in this package against a throwaway per-user root.
//
// THE DEFAULT HAS TO BE ISOLATED, NOT THE EXCEPTION. Tests here drive the root
// command, and the root command fills whatever a test leaves out with the
// production wiring: it loads user config, user hooks, plugins and stores from
// the per-user directories. A test that redirects none of them runs against the
// developer's own. With a user-level hooks.json in place, one run of this
// package executed those hooks more than two thousand times, and a hooks file
// that merely says enabled:false is enough to fail a test that never mentions
// it. isolateCLIUserState fixes that for the tests that call it, and there are
// several hundred that do not.
//
// So the base directories are replaced once, before anything runs. A test that
// needs its own root still calls isolateCLIUserState or t.Setenv, which
// override this for that test and restore it afterwards.
func TestMain(m *testing.M) {
	os.Exit(runPackageTests(m.Run))
}

func runPackageTests(run func() int) int {
	root, cleanup, err := isolatePackageUserState()
	if err != nil {
		fmt.Fprintf(os.Stderr, "isolate the package from the user's own state: %v\n", err)
		return 1
	}
	defer cleanup()
	packageUserStateRoot = root
	return run()
}

// isolatePackageUserState replaces the per-user base directories of this
// process with one new throwaway root and clears the overrides that outrank
// them. It returns the root and a function that removes it. In a process that
// is already marked as isolated it changes nothing and returns an empty root.
func isolatePackageUserState() (string, func(), error) {
	if os.Getenv(userStateIsolatedEnv) != "" {
		return "", func() {}, nil
	}
	root, err := os.MkdirTemp("", "zero-cli-user-state-")
	if err != nil {
		return "", nil, fmt.Errorf("create the root: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	for _, name := range userStateEnvNames {
		if err := os.Setenv(name, root); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("redirect %s: %w", name, err)
		}
	}
	for _, name := range userStateOverrideEnvNames {
		if err := os.Unsetenv(name); err != nil {
			cleanup()
			return "", nil, fmt.Errorf("clear %s: %w", name, err)
		}
	}
	if err := os.Setenv(userStateIsolatedEnv, "1"); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("mark the process as isolated: %w", err)
	}
	return root, cleanup, nil
}

// everyUserStateEnvName is written out here on purpose. Reusing the list the
// setup walks would let a name dropped from that list go unnoticed.
var everyUserStateEnvName = []string{
	"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME",
}

// everyUserStateOverrideEnvName is written out for the same reason.
var everyUserStateOverrideEnvName = []string{"ZERO_SANDBOX_GRANTS_PATH", "ZERO_WINDOWS_SANDBOX_HOME"}

func pathIsUnder(t *testing.T, root, path string) bool {
	t.Helper()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve %s: %v", root, err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		resolved = path
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// THE PACKAGE DEFAULT IS PINNED HERE, so removing TestMain fails a test
// instead of quietly sending several hundred tests back to the developer's own
// directories.
//
// It reads the environment a test starts with, before any t.Setenv, and checks
// each base directory against the root TestMain created. It also asks Go for
// the three directories it derives from them, because those are what the
// production code resolves on each platform.
func TestPackageRunsAgainstAThrowawayUserState(t *testing.T) {
	if packageUserStateRoot == "" {
		t.Fatal("TestMain did not replace the per-user directories for this package")
	}
	for _, name := range everyUserStateEnvName {
		if value := os.Getenv(name); value == "" || !pathIsUnder(t, packageUserStateRoot, value) {
			t.Errorf("%s = %q, want a path under the package root %q", name, value, packageUserStateRoot)
		}
	}
	for _, name := range everyUserStateOverrideEnvName {
		if value, set := os.LookupEnv(name); set {
			t.Errorf("%s = %q is still set; it outranks the base directories", name, value)
		}
	}
	for name, resolve := range map[string]func() (string, error){
		"os.UserHomeDir":   os.UserHomeDir,
		"os.UserConfigDir": os.UserConfigDir,
		"os.UserCacheDir":  os.UserCacheDir,
	} {
		if path, err := resolve(); err != nil || !pathIsUnder(t, packageUserStateRoot, path) {
			t.Errorf("%s() = %q, %v; want a path under %q", name, path, err, packageUserStateRoot)
		}
	}
}

// THE SETUP ITSELF, AGAINST AN ENVIRONMENT THAT POINTS SOMEWHERE ELSE.
//
// A decoy directory stands in for the developer's own behind every base
// directory, and both overrides are set, which the machines that run this
// suite never have. After the setup nothing may still name the decoy, the
// overrides must be gone, and the process must be marked so that a child does
// not do it all again. The removal function must take the root away.
func TestIsolatePackageUserStateReplacesEveryDirectoryAndClearsTheOverrides(t *testing.T) {
	decoy := t.TempDir()
	for _, name := range everyUserStateEnvName {
		t.Setenv(name, decoy)
	}
	for _, name := range everyUserStateOverrideEnvName {
		t.Setenv(name, filepath.Join(decoy, "override"))
	}
	t.Setenv(userStateIsolatedEnv, "")

	root, cleanup, err := isolatePackageUserState()
	if err != nil {
		t.Fatal(err)
	}
	if root == "" || pathIsUnder(t, decoy, root) {
		t.Fatalf("root = %q, want a new directory outside %q", root, decoy)
	}
	for _, name := range everyUserStateEnvName {
		if value := os.Getenv(name); !pathIsUnder(t, root, value) {
			t.Errorf("%s = %q, want a path under the new root %q", name, value, root)
		}
	}
	for _, name := range everyUserStateOverrideEnvName {
		if value, set := os.LookupEnv(name); set {
			t.Errorf("%s = %q is still set", name, value)
		}
	}
	if os.Getenv(userStateIsolatedEnv) == "" {
		t.Errorf("%s is not set; a child started by a test would isolate itself again", userStateIsolatedEnv)
	}
	cleanup()
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("the root %q is still there after cleanup: %v", root, err)
	}
}

// A PROCESS THAT IS ALREADY ISOLATED IS LEFT ALONE.
//
// Some tests start this test binary again as a child and hand it its own
// directories, and two such children have to share them. Replacing the
// directories again in the child would give each its own empty root.
func TestIsolatePackageUserStateLeavesAnIsolatedProcessAlone(t *testing.T) {
	given := t.TempDir()
	for _, name := range everyUserStateEnvName {
		t.Setenv(name, given)
	}
	t.Setenv(userStateIsolatedEnv, "1")

	root, cleanup, err := isolatePackageUserState()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if root != "" {
		t.Fatalf("root = %q, want none: the process was already isolated", root)
	}
	for _, name := range everyUserStateEnvName {
		if value := os.Getenv(name); value != given {
			t.Errorf("%s = %q, want the directory it was given, %q", name, value, given)
		}
	}
}

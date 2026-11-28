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

// packageHomeEnvNames are the directories every platform derives the per-user
// paths from when nothing overrides them. They are pointed at the package root.
var packageHomeEnvNames = []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA"}

// packageClearedEnvNames outrank the directories above, so an inherited value
// would send a path straight back to the developer's own. They are removed,
// not pointed at the root: a machine with none of them set is what the tests
// were written against, and several redirect only the directory their platform
// starts from. Setting XDG_CONFIG_HOME for everyone would outrank a test that
// moves HOME on macOS, where the config directory honours it first.
var packageClearedEnvNames = []string{
	"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME",
	"ZERO_SANDBOX_GRANTS_PATH", "ZERO_WINDOWS_SANDBOX_HOME",
}

// packageUserStateRoot is the throwaway root TestMain put behind every
// per-user base directory, or empty in a child that was already isolated.
var packageUserStateRoot string

// TestMain runs every test in this package against a throwaway per-user root.
//
// THE DEFAULT HAS TO BE ISOLATED, NOT THE EXCEPTION. Tests here drive the root
// command, and the root command fills whatever a test leaves out with the
// production wiring: it loads user config, user hooks, plugins and stores from
// the per-user directories, and saves credentials to the keychain. A test that
// redirects none of them runs against the developer's own. With a user-level
// hooks.json in place, one run of this package executed those hooks more than
// two thousand times, and a hooks file that merely says enabled:false is enough
// to fail a test that never mentions it. isolateCLIUserState fixes that for the
// tests that call it, and there are several hundred that do not.
//
// So the process starts out looking like a machine with an empty home and no
// overrides. A test that needs its own root still calls isolateCLIUserState or
// t.Setenv, which override this for that test and restore it afterwards.
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

// isolatePackageUserState points the home directories of this process at one
// new throwaway root, removes the variables that outrank them, and keeps
// credentials in a file under that root. It returns the root and a function
// that removes it. In a process that is already marked as isolated it changes
// nothing and returns an empty root.
func isolatePackageUserState() (string, func(), error) {
	if os.Getenv(userStateIsolatedEnv) != "" {
		return "", func() {}, nil
	}
	root, err := os.MkdirTemp("", "zero-cli-user-state-")
	if err != nil {
		return "", nil, fmt.Errorf("create the root: %w", err)
	}
	// The physical spelling, so a path derived from it compares equal to one
	// the system reports: the temporary directory is reached through a link on
	// macOS and through a short name on some Windows machines.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	fail := func(err error) (string, func(), error) {
		cleanup()
		return "", nil, err
	}
	for _, name := range packageHomeEnvNames {
		if err := os.Setenv(name, root); err != nil {
			return fail(fmt.Errorf("redirect %s: %w", name, err))
		}
	}
	for _, name := range packageClearedEnvNames {
		if err := os.Unsetenv(name); err != nil {
			return fail(fmt.Errorf("clear %s: %w", name, err))
		}
	}
	// Credentials resolve keychain-first. A redirected home has no keychain, and
	// a test key has no business in the real one.
	if err := os.Setenv("ZERO_CRED_STORAGE", "encrypted-file"); err != nil {
		return fail(fmt.Errorf("keep credentials in a file: %w", err))
	}
	if err := os.Setenv(userStateIsolatedEnv, "1"); err != nil {
		return fail(fmt.Errorf("mark the process as isolated: %w", err))
	}
	return root, cleanup, nil
}

// The names are written out again here on purpose. Reusing the lists the setup
// walks would let a name dropped from them go unnoticed.
var (
	everyPackageHomeEnvName    = []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA"}
	everyPackageClearedEnvName = []string{
		"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME",
		"ZERO_SANDBOX_GRANTS_PATH", "ZERO_WINDOWS_SANDBOX_HOME",
	}
)

func pathIsUnder(t *testing.T, root, path string) bool {
	t.Helper()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve %s: %v", root, err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		// The path does not exist yet. The root is exported in its physical
		// spelling, so a path built from it already has the right prefix.
		resolved = filepath.Clean(path)
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// THE PACKAGE DEFAULT IS PINNED HERE, so removing TestMain fails a test
// instead of quietly sending several hundred tests back to the developer's own
// directories.
//
// It reads the environment a test starts with, before any t.Setenv. It also
// asks Go for the three directories it derives from that environment, because
// those are what the production code resolves on each platform, and on a new
// root they do not exist yet.
func TestPackageRunsAgainstAThrowawayUserState(t *testing.T) {
	if packageUserStateRoot == "" {
		t.Fatal("TestMain did not replace the per-user directories for this package")
	}
	if resolved, err := filepath.EvalSymlinks(packageUserStateRoot); err != nil || resolved != packageUserStateRoot {
		t.Fatalf("the package root is %q and resolves to %q, %v; it must be exported in its physical spelling", packageUserStateRoot, resolved, err)
	}
	for _, name := range everyPackageHomeEnvName {
		if value := os.Getenv(name); value != packageUserStateRoot {
			t.Errorf("%s = %q, want the package root %q", name, value, packageUserStateRoot)
		}
	}
	for _, name := range everyPackageClearedEnvName {
		if value, set := os.LookupEnv(name); set {
			t.Errorf("%s = %q is still set; it outranks the home directories", name, value)
		}
	}
	if got := os.Getenv("ZERO_CRED_STORAGE"); got != "encrypted-file" {
		t.Errorf("ZERO_CRED_STORAGE = %q, want credentials kept in a file", got)
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
// A decoy directory stands in for the developer's own behind every home
// directory and every override, and credential storage is set to the keychain,
// which the machines that run this suite never have all at once. After the
// setup nothing may still name the decoy, the overrides must be gone, and the
// process must be marked so that a child does not do it all again. The removal
// function must take the root away.
func TestIsolatePackageUserStateReplacesTheHomeAndClearsTheOverrides(t *testing.T) {
	decoy := t.TempDir()
	for _, name := range everyPackageHomeEnvName {
		t.Setenv(name, decoy)
	}
	for _, name := range everyPackageClearedEnvName {
		t.Setenv(name, filepath.Join(decoy, "override"))
	}
	t.Setenv("ZERO_CRED_STORAGE", "keyring")
	t.Setenv(userStateIsolatedEnv, "")

	root, cleanup, err := isolatePackageUserState()
	if err != nil {
		t.Fatal(err)
	}
	if root == "" || pathIsUnder(t, decoy, root) {
		t.Fatalf("root = %q, want a new directory outside %q", root, decoy)
	}
	if resolved, err := filepath.EvalSymlinks(root); err != nil || resolved != root {
		t.Errorf("root = %q resolves to %q, %v; want the physical spelling", root, resolved, err)
	}
	for _, name := range everyPackageHomeEnvName {
		if value := os.Getenv(name); value != root {
			t.Errorf("%s = %q, want the new root %q", name, value, root)
		}
	}
	for _, name := range everyPackageClearedEnvName {
		if value, set := os.LookupEnv(name); set {
			t.Errorf("%s = %q is still set", name, value)
		}
	}
	if got := os.Getenv("ZERO_CRED_STORAGE"); got != "encrypted-file" {
		t.Errorf("ZERO_CRED_STORAGE = %q, want credentials kept in a file", got)
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
	for _, name := range everyPackageHomeEnvName {
		t.Setenv(name, given)
	}
	for _, name := range everyPackageClearedEnvName {
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
	for _, name := range append(append([]string{}, everyPackageHomeEnvName...), everyPackageClearedEnvName...) {
		if value := os.Getenv(name); value != given {
			t.Errorf("%s = %q, want the directory it was given, %q", name, value, given)
		}
	}
}

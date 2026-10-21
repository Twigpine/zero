package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/testutil"
)

// isolatedCwd is the working directory for tests that assert on the whole
// view. With an empty Options.Cwd, newModel uses this package's directory, so
// the title bar shows the real branch and checkout path, and a check that some
// text is absent fails wherever either one contains it: a branch named
// feat/model-picker breaks a "/model" check, a checkout under ~/more breaks a
// "more" check.
//
// It is a child of the home TestMain isolates that nothing creates: fully
// qualified on every platform, outside any checkout, and holding nothing
// newModel could read. The title bar shows it as ~/not-a-checkout on every
// machine. A t.TempDir path would carry the user name and the test name into
// the view instead, which is the same problem in a new place.
func isolatedCwd(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, "not-a-checkout")
}

// Guards isolatedCwd: the tests that pin it rely on a fully qualified path that
// does not exist and that no git checkout contains.
func TestIsolatedCwdIsAbsentAndOutsideACheckout(t *testing.T) {
	cwd := isolatedCwd(t)
	if !filepath.IsAbs(cwd) {
		t.Fatalf("isolatedCwd = %q, want a fully qualified path", cwd)
	}
	if _, err := os.Lstat(cwd); !os.IsNotExist(err) {
		t.Fatalf("isolatedCwd %q must not exist, got %v", cwd, err)
	}
	if got := gitBranch(cwd); got != "" {
		t.Fatalf("gitBranch(%q) = %q; isolatedCwd must not be inside a git checkout", cwd, got)
	}
}

// Guards TestMain: the per-user directories every newModel reads (and the ones
// tests could write to) must resolve inside the isolated root on every OS.
func TestUserDirsAreIsolatedFromTheDeveloperHome(t *testing.T) {
	if isolatedUserDirsRoot == "" {
		t.Fatal("TestMain did not isolate the user directories")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	userConfig, err := config.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"home": home, "config": userConfig, "cache": cache} {
		if !testutil.PathWithin(isolatedUserDirsRoot, dir) {
			t.Errorf("%s dir %q is outside the isolated root %q", name, dir, isolatedUserDirsRoot)
		}
	}
}

// newModel loads slash commands from <user config>/zero/commands. A fresh model
// must see none, and must see exactly what is planted in the isolated dir.
func TestNewModelLoadsUserCommandsOnlyFromIsolatedConfig(t *testing.T) {
	if got := len(newModel(context.Background(), Options{Cwd: t.TempDir()}).userCommands); got != 0 {
		t.Fatalf("fresh model loaded %d user command(s) from outside the test sandbox", got)
	}

	userConfig, err := config.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(userConfig, "zero", "commands")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "hermetic-probe.md")
	if err := os.WriteFile(path, []byte("probe"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	commands := newModel(context.Background(), Options{Cwd: t.TempDir()}).userCommands
	if len(commands) != 1 || commands[0].Name != "hermetic-probe" {
		t.Fatalf("user commands = %#v, want only the planted hermetic-probe", commands)
	}
}

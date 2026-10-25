package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/testutil"
)

// newViewTestModel builds the model for a test that asserts on the whole view.
// With an empty Options.Cwd, newModel uses this package's directory, so the
// title bar shows the real branch and checkout path, and a check that some text
// is absent fails wherever either one contains it: a branch named
// feat/model-picker breaks a "/model" check, a checkout under ~/more breaks a
// "more" check.
//
// The cwd is isolatedCwd, so the path in the title bar is the same on every
// machine. The branch is cleared after construction, because no path can
// promise there is none: gitBranch walks up from the cwd even though it does
// not exist, so a temporary directory inside a checkout would still put that
// checkout's branch in the title bar.
func newViewTestModel(t *testing.T, options Options) model {
	t.Helper()
	options.Cwd = isolatedCwd(t)
	m := newModel(context.Background(), options)
	m.gitBranch = ""
	return m
}

// isolatedCwd is a child of the home TestMain isolates that nothing creates:
// fully qualified on every platform and holding nothing newModel could read.
// The title bar shows it as ~/not-a-checkout. A t.TempDir path would carry the
// user name and the test name into the view instead.
func isolatedCwd(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, "not-a-checkout")
}

// Guards isolatedCwd: the view tests rely on a fully qualified path that does
// not exist. Whether a checkout encloses it depends on where the temporary
// directory is, which is why newViewTestModel clears the branch instead.
func TestIsolatedCwdIsAbsentAndFullyQualified(t *testing.T) {
	cwd := isolatedCwd(t)
	if !filepath.IsAbs(cwd) {
		t.Fatalf("isolatedCwd = %q, want a fully qualified path", cwd)
	}
	if _, err := os.Lstat(cwd); !os.IsNotExist(err) {
		t.Fatalf("isolatedCwd %q must not exist, got %v", cwd, err)
	}
}

// A temporary directory inside a checkout puts the home, and so isolatedCwd,
// under that checkout. Its branch must still stay out of the view, or a branch
// named feat/model-picker fails the "/model" check again.
func TestViewTestModelKeepsAnEnclosingCheckoutsBranchOutOfTheView(t *testing.T) {
	checkout := t.TempDir()
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".git", "HEAD"), []byte("ref: refs/heads/feat/model-picker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(checkout, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if got := gitBranch(isolatedCwd(t)); got != "feat/model-picker" {
		t.Fatalf("fixture: gitBranch(isolatedCwd) = %q, want the enclosing checkout's feat/model-picker", got)
	}

	m := newViewTestModel(t, Options{})
	m.width, m.height = 96, 30
	m = typeRunes(t, m, "/mo")
	plain := plainRender(t, m.View())
	if strings.Contains(plain, "feat/model-picker") || strings.Contains(plain, "/model") {
		t.Fatalf("the enclosing checkout's branch reached the view:\n%s", plain)
	}
	// The path is the other half: the title bar shows the isolated cwd, not the
	// checkout the tests happen to run from or the one enclosing the home.
	if !strings.Contains(plain, "not-a-checkout") {
		t.Fatalf("the title bar does not show the isolated cwd:\n%s", plain)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{checkout, wd} {
		if strings.Contains(plain, path) {
			t.Fatalf("the view shows the checkout path %s:\n%s", path, plain)
		}
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

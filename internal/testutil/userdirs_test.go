package testutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsolateUserDirsRedirectsAndRestores(t *testing.T) {
	// Seed distinctive values so restoring is observable, then make sure the
	// helper hands them back (set ones restored, unset ones unset again).
	t.Setenv("HOME", "before-home")
	t.Setenv("XDG_STATE_HOME", "before-state")
	// t.Setenv registers restoration of the inherited value before it is
	// cleared, so later tests in this process still see it.
	t.Setenv("XDG_DATA_HOME", "")
	os.Unsetenv("XDG_DATA_HOME")

	root := t.TempDir()
	restore, err := IsolateUserDirs(root)
	if err != nil {
		t.Fatal(err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"home": home, "config": config, "cache": cache} {
		if !PathWithin(root, dir) {
			t.Errorf("%s dir %q is outside the isolated root %q", name, dir, root)
		}
	}

	if err := restore(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("HOME"); got != "before-home" {
		t.Errorf("HOME after restore = %q, want before-home", got)
	}
	if got := os.Getenv("XDG_STATE_HOME"); got != "before-state" {
		t.Errorf("XDG_STATE_HOME after restore = %q, want before-state", got)
	}
	if _, set := os.LookupEnv("XDG_DATA_HOME"); set {
		t.Error("XDG_DATA_HOME was unset before and must be unset again after restore")
	}
}

func TestIsolateUserDirsFailsClosed(t *testing.T) {
	for _, name := range userDirEnv {
		t.Setenv(name, "before-"+strings.ToLower(name))
	}

	// A NUL byte is rejected by Setenv on every supported platform, so the
	// very first redirect fails.
	restore, err := IsolateUserDirs("bad\x00root")
	if err == nil {
		if restore != nil {
			_ = restore()
		}
		t.Fatal("IsolateUserDirs succeeded with a root Setenv must reject")
	}
	if restore != nil {
		t.Error("IsolateUserDirs returned a restore func alongside an error")
	}
	for _, name := range userDirEnv {
		if got, want := os.Getenv(name), "before-"+strings.ToLower(name); got != want {
			t.Errorf("%s after failed isolation = %q, want %q", name, got, want)
		}
	}
}

func TestIsolateUserDirsRollsBackPartialRedirect(t *testing.T) {
	for _, name := range userDirEnv {
		t.Setenv(name, "before-"+strings.ToLower(name))
	}

	// Let the first redirect (HOME) succeed, then fail the second, so the
	// rollback has to undo a variable that really was changed.
	failing := userDirEnv[1]
	t.Cleanup(func() { setenv = os.Setenv })
	setenv = func(name, value string) error {
		if name == failing {
			return errors.New("injected setenv failure")
		}
		return os.Setenv(name, value)
	}

	restore, err := IsolateUserDirs(t.TempDir())
	if err == nil {
		if restore != nil {
			_ = restore()
		}
		t.Fatalf("IsolateUserDirs succeeded although setting %s failed", failing)
	}
	if !strings.Contains(err.Error(), failing) {
		t.Errorf("error %q does not name the failing variable %s", err, failing)
	}
	for _, name := range userDirEnv {
		if got, want := os.Getenv(name), "before-"+strings.ToLower(name); got != want {
			t.Errorf("%s after partial isolation = %q, want %q", name, got, want)
		}
	}
}

func TestPathWithinComparesPhysicalPaths(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, tc := range []struct {
		root, path string
		want       bool
	}{
		{root, root, true},
		{root, filepath.Join(alias, "not", "created", "yet"), true},
		{alias, filepath.Join(root, "child"), true},
		{root, base, false},
		{alias, filepath.Join(base, "sibling"), false},
	} {
		if got := PathWithin(tc.root, tc.path); got != tc.want {
			t.Errorf("PathWithin(%q, %q) = %v, want %v", tc.root, tc.path, got, tc.want)
		}
	}
}

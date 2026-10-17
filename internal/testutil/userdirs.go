package testutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// setenv is os.Setenv, swappable so tests can fail a redirect part way through.
var setenv = os.Setenv

// userDirEnv lists every variable that os.UserHomeDir, os.UserConfigDir and
// os.UserCacheDir (and Zero's own config.UserConfigDir) read, across Linux,
// macOS and Windows. Setting only the XDG ones still leaves macOS resolving
// from HOME and Windows from USERPROFILE, APPDATA and LOCALAPPDATA.
var userDirEnv = []string{
	"HOME",
	"USERPROFILE",
	"APPDATA",
	"LOCALAPPDATA",
	"XDG_CONFIG_HOME",
	"XDG_CACHE_HOME",
	"XDG_DATA_HOME",
	"XDG_STATE_HOME",
}

// IsolateUserDirs points every per-user directory root at root so code under
// test cannot read the developer's real config, cache or home directory, or
// write to them. It is meant for a package's TestMain, which has no *testing.T
// (tests that need their own layout still use t.Setenv, which wins while it
// is active). The returned function restores the previous environment.
//
// It fails closed: if any variable cannot be redirected, the variables it had
// already changed are restored and an error is returned, so the caller must
// not run the tests against a partially isolated environment.
func IsolateUserDirs(root string) (restore func() error, err error) {
	type saved struct {
		name  string
		value string
		set   bool
	}
	previous := make([]saved, 0, len(userDirEnv))
	restore = func() error {
		var errs []error
		for _, prior := range previous {
			var err error
			if prior.set {
				err = os.Setenv(prior.name, prior.value)
			} else {
				err = os.Unsetenv(prior.name)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("restore %s: %w", prior.name, err))
			}
		}
		return errors.Join(errs...)
	}
	for _, name := range userDirEnv {
		value, set := os.LookupEnv(name)
		if err := setenv(name, root); err != nil {
			setErr := fmt.Errorf("isolate %s: %w", name, err)
			return nil, errors.Join(setErr, restore())
		}
		previous = append(previous, saved{name: name, value: value, set: set})
	}
	return restore, nil
}

// PathWithin reports whether path is root or lies beneath it, comparing the
// physical forms so symlinked temp dirs (/var vs /private/var on macOS) and
// Windows short names do not cause false results. Components that do not
// exist yet are kept as spelled under their nearest existing ancestor.
func PathWithin(root, path string) bool {
	rel, err := filepath.Rel(physicalPath(root), physicalPath(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func physicalPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	var missing []string
	for current := path; ; {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(append([]string{resolved}, missing...)...)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		missing = append([]string{filepath.Base(current)}, missing...)
		current = parent
	}
}

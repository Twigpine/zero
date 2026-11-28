//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
)

// windowsPrincipalIdentityEnvironment rewrites the variables that name WHO the
// command is running as.
//
// The child environment starts life as the invoking user's, and the deliberate
// sandbox redirects only replace HOME, the temp variables and the per-tool cache
// dirs. Everything identifying the account was left alone, so a command running
// as the principal still read USERPROFILE, APPDATA, LOCALAPPDATA, HOMEDRIVE,
// HOMEPATH and USERNAME describing the CALLER, whose profile the principal
// deliberately cannot open. Native tools resolve config and per-user state
// through exactly those, so they either fail during startup or, worse, quietly
// look somewhere they have no business reading.
//
// The values point into the sandbox runtime tree rather than at the principal's
// real Windows profile. That tree is already granted to the principal and
// already holds its caches, so the paths are writable by construction. Naming
// the real profile would need LoadUserProfile to have run, and a variable
// pointing at a directory that does not exist yet is a worse answer than one
// pointing somewhere usable.
//
// Layered UNDER the deliberate redirects: HOME, TMPDIR, TMP and TEMP are not
// touched here, so sandboxRuntimeEnvironment stays the single owner of those.
func windowsPrincipalIdentityEnvironment(env map[string]string, username string, runtime *SandboxRuntime) map[string]string {
	username = strings.TrimSpace(username)
	if env == nil || username == "" || runtime == nil {
		return env
	}
	base := strings.TrimSpace(runtime.Data)
	if base == "" {
		base = strings.TrimSpace(runtime.Root)
	}
	if base == "" {
		// No runtime tree means no writable place to point at, and inventing one
		// outside the grants would only move the failure. Leaving the caller's
		// values would be worse, so name the account and stop there.
		env["USERNAME"] = username
		return env
	}

	profile := filepath.Join(base, "profile")
	env["USERPROFILE"] = profile
	env["APPDATA"] = filepath.Join(profile, "AppData", "Roaming")
	env["LOCALAPPDATA"] = filepath.Join(profile, "AppData", "Local")
	env["USERNAME"] = username
	if host, err := os.Hostname(); err == nil && strings.TrimSpace(host) != "" {
		// A local account's domain is the machine. Left as the caller's it would
		// name a domain the principal is not a member of.
		env["USERDOMAIN"] = host
	}
	// HOMEDRIVE and HOMEPATH are a split of the same location, and tools join
	// them back together, so they have to be split from the value above rather
	// than carried over from the caller.
	if volume := filepath.VolumeName(profile); volume != "" {
		env["HOMEDRIVE"] = volume
		env["HOMEPATH"] = strings.TrimPrefix(profile, volume)
	}
	return env
}

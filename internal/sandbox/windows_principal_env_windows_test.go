//go:build windows

package sandbox

import (
	"strings"
	"testing"
)

// callerEnvironment is the shape the child starts from: the invoking user's
// values, with the deliberate sandbox redirects already layered on top.
func callerEnvironment() map[string]string {
	return map[string]string{
		"USERPROFILE":  `C:\Users\caller`,
		"APPDATA":      `C:\Users\caller\AppData\Roaming`,
		"LOCALAPPDATA": `C:\Users\caller\AppData\Local`,
		"HOMEDRIVE":    `C:`,
		"HOMEPATH":     `\Users\caller`,
		"USERNAME":     "caller",
		"USERDOMAIN":   "CALLERDOMAIN",
		// Owned by sandboxRuntimeEnvironment, and this must not touch them.
		"HOME": `C:\runtime\home`,
		"TEMP": `C:\runtime\temp`,
		"TMP":  `C:\runtime\temp`,
	}
}

// NOTHING MAY STILL NAME THE CALLER.
//
// A principal is a separate account that deliberately cannot open the invoking
// user's profile, so every variable a native tool resolves per-user state
// through has to describe the principal instead. Left alone they pointed at the
// caller, and tools either failed during startup or looked somewhere they had no
// business reading.
func TestPrincipalEnvironmentStopsNamingTheCaller(t *testing.T) {
	runtime := &SandboxRuntime{
		Root: `C:\runtime`,
		Data: `C:\runtime\data`,
	}
	env := windowsPrincipalIdentityEnvironment(callerEnvironment(), "zero-sbx-abc123", runtime)

	for _, key := range []string{"USERPROFILE", "APPDATA", "LOCALAPPDATA"} {
		if strings.Contains(strings.ToLower(env[key]), "caller") {
			t.Errorf("%s still points into the caller's profile: %s", key, env[key])
		}
		if !strings.HasPrefix(strings.ToLower(env[key]), strings.ToLower(runtime.Data)) {
			t.Errorf("%s is not inside the runtime tree the principal can write: %s", key, env[key])
		}
	}
	if strings.Contains(strings.ToLower(env["HOMEPATH"]), "caller") {
		t.Errorf("HOMEPATH still points into the caller's profile: %s", env["HOMEPATH"])
	}
	if env["USERNAME"] != "zero-sbx-abc123" {
		t.Errorf("USERNAME = %q, want the principal account", env["USERNAME"])
	}
	if env["USERDOMAIN"] == "CALLERDOMAIN" {
		t.Error("USERDOMAIN still names the caller's domain, which the principal is not a member of")
	}
	// HOMEDRIVE and HOMEPATH are rejoined by tools, so they have to describe the
	// same location as USERPROFILE rather than a mix of both identities.
	if rejoined := env["HOMEDRIVE"] + env["HOMEPATH"]; rejoined != env["USERPROFILE"] {
		t.Errorf("HOMEDRIVE+HOMEPATH = %s, want the same location as USERPROFILE %s", rejoined, env["USERPROFILE"])
	}
}

// The deliberate redirects stay owned by sandboxRuntimeEnvironment. Clobbering
// them here would silently move the sandbox's temp and cache handling into a
// second place, which is how the two drift apart.
func TestPrincipalEnvironmentLeavesTheSandboxRedirectsAlone(t *testing.T) {
	before := callerEnvironment()
	env := windowsPrincipalIdentityEnvironment(callerEnvironment(), "zero-sbx-abc123", &SandboxRuntime{
		Root: `C:\runtime`,
		Data: `C:\runtime\data`,
	})
	for _, key := range []string{"HOME", "TEMP", "TMP"} {
		if env[key] != before[key] {
			t.Errorf("%s = %q, want it left at the sandbox redirect %q", key, env[key], before[key])
		}
	}
}

// Without a runtime tree there is nowhere writable to point at, and inventing a
// path outside the grants would only move the failure. Naming the account is
// still strictly better than advertising the caller's.
func TestPrincipalEnvironmentWithoutARuntimeTreeStillRenamesTheAccount(t *testing.T) {
	env := windowsPrincipalIdentityEnvironment(callerEnvironment(), "zero-sbx-abc123", &SandboxRuntime{})
	if env["USERNAME"] != "zero-sbx-abc123" {
		t.Errorf("USERNAME = %q, want the principal account even with no runtime tree", env["USERNAME"])
	}
}

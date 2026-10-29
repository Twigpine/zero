package doctor

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/sandbox"
)

// DOCTOR HAS TO ASK THE ONE QUESTION THE MARKER CANNOT ANSWER.
//
// The marker hashes ACL-plan entries, which are pathnames. Whether the directory
// those pathnames resolve to is still the tree setup provisioned is a different
// question, and the runtime stamp is what answers it. validateWindowsSandboxRuntimeStamp
// returns nil early when profile.Runtime is nil, which is correct for the setup
// side and for unrestricted profiles, and was wrong here: doctor built its
// profile with PermissionProfileFromPolicy, which never sets Runtime, so the
// check was skipped and `zero doctor` reported a healthy sandbox on exactly the
// state the stamp was added to detect -- an evicted runtime tree, silently
// recreated with ordinary permissions and no capability ACE, where every
// sandboxed command then fails with nothing explaining why.
// doctorRuntimeCandidate returns a runtime root this workspace would really
// select, taken from the candidate set the Windows plan folds in.
//
// It has to be a REAL candidate. An arbitrary path is already in the ACL plan on
// one side and not the other, so the plan hashes diverge and validation fails
// before the stamp is ever consulted -- which would make this test fail without
// the fix for a reason that has nothing to do with the stamp.
// redirectUserCache points os.UserCacheDir at test-owned storage.
//
// WITHOUT THIS THE TEST WRITES INTO THE DEVELOPER'S REAL CACHE. The runtime
// candidate is derived from the process's actual user cache directory, so the
// test was creating, recursively removing, recreating and then cleanup-removing
// a directory under the real %LocalAppData%zerountime (or ~/.cache/zero on
// Unix). The workspace digest made a collision with a live runtime tree
// unlikely rather than impossible, and "unlikely" is not the standard for a
// test that calls RemoveAll.
//
// All three variables, because os.UserCacheDir reads a different one per
// platform: %LocalAppData% on Windows, $XDG_CACHE_HOME or $HOME on Unix, and
// $HOME on macOS.
func redirectUserCache(t *testing.T) {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("LOCALAPPDATA", cache)
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
}

func doctorRuntimeCandidate(t *testing.T, workspace string) string {
	t.Helper()
	bare := doctorProfile(t, workspace)
	augmented := sandbox.WindowsSandboxProfileWithRuntimeRoots(bare, []string{workspace})
	existing := map[string]bool{}
	for _, root := range bare.FileSystem.WriteRoots {
		existing[root.Root] = true
	}
	for _, root := range augmented.FileSystem.WriteRoots {
		if !existing[root.Root] {
			return root.Root
		}
	}
	t.Skip("no runtime candidate is derivable in this environment")
	return ""
}

func doctorProfile(t *testing.T, workspace string) sandbox.PermissionProfile {
	t.Helper()
	scope, err := sandbox.NewScope(workspace, nil)
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	return sandbox.PermissionProfileFromPolicy(workspace, doctorSandboxPolicy(config.SandboxConfig{}), scope)
}

func writeDoctorSetupMarker(t *testing.T, home, workspace, runtimeRoot string) {
	t.Helper()
	profile := doctorProfile(t, workspace)
	setup := sandbox.WindowsSandboxSetupConfig{
		SandboxHome:    home,
		CommandCWD:     workspace,
		WorkspaceRoots: []string{workspace},
		PermissionProfile: sandbox.WindowsSandboxProfileWithRuntimeRoots(
			sandbox.PermissionProfileWithRuntimeRoot(profile, runtimeRoot),
			[]string{workspace},
		),
	}
	if _, err := sandbox.WriteWindowsSandboxSetupMarker(setup); err != nil {
		t.Fatalf("WriteWindowsSandboxSetupMarker: %v", err)
	}
}

func TestDoctorReportsAnEvictedRuntimeTree(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	t.Setenv("ZERO_WINDOWS_SANDBOX_HOME", home)
	redirectUserCache(t)

	runtimeRoot := doctorRuntimeCandidate(t, workspace)
	// Belt and braces: if the redirection above ever stops working, fail loudly
	// rather than quietly operating on the developer's real cache.
	if !strings.HasPrefix(runtimeRoot, os.TempDir()) && !strings.Contains(runtimeRoot, t.Name()) {
		t.Fatalf("the runtime candidate %q is outside test-owned storage; this test creates and removes that path", runtimeRoot)
	}
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		t.Fatalf("create the runtime root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeRoot) })
	writeDoctorSetupMarker(t, home, workspace, runtimeRoot)

	backend := sandbox.Backend{Name: sandbox.BackendWindowsRestrictedToken}

	// A healthy machine first, or the eviction assertion below would be satisfied
	// by a check that warns unconditionally.
	if result := windowsSandboxSetupCheck("windows", backend, workspace, config.SandboxConfig{}); result != nil {
		t.Fatalf("a freshly set-up machine was reported unhealthy: %s", result.Message)
	}

	// cleanupSandboxRuntimeRoots evicts inactive roots on an age and count policy,
	// and the next run recreates the pathname with inherited permissions. The plan
	// hash never moves, so only the stamp can tell.
	if err := os.RemoveAll(runtimeRoot); err != nil {
		t.Fatalf("evict the runtime root: %v", err)
	}
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		t.Fatalf("recreate the pathname the way an ordinary run would: %v", err)
	}

	result := windowsSandboxSetupCheck("windows", backend, workspace, config.SandboxConfig{})
	if result == nil {
		t.Fatal("doctor reported a healthy sandbox while the provisioned runtime tree was gone; every sandboxed command on this machine would fail with nothing explaining why")
	}
	if !strings.Contains(strings.ToLower(result.Message), "setup") {
		t.Errorf("the warning does not point at setup: %s", result.Message)
	}
}

// UNKNOWN IS NOT CURRENT.
//
// WindowsSandboxRecordedRuntimeRootIsCurrent answers three ways: the recorded
// root is still one a command would select, it is not, or the inputs a command
// selects from cannot be resolved at all. The third used to fall through to the
// pinned branch, so a valid old marker and stamp made doctor report healthy
// while selectSandboxRuntimeRoot — reached by BuildCommandPlan — propagated the
// same resolution failure and stopped every command from launching. Doctor and
// the command disagreed about the same machine.
//
// Stale and unknown are different states, so this is deliberately separate from
// the cache-A-to-cache-B test rather than another assertion on the same boolean.
func TestDoctorWarnsWhenTheRuntimeRootCannotBeResolved(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	t.Setenv("ZERO_WINDOWS_SANDBOX_HOME", home)
	redirectUserCache(t)

	runtimeRoot := doctorRuntimeCandidate(t, workspace)
	if !strings.HasPrefix(runtimeRoot, os.TempDir()) && !strings.Contains(runtimeRoot, t.Name()) {
		t.Fatalf("the runtime candidate %q is outside test-owned storage", runtimeRoot)
	}
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		t.Fatalf("create the runtime root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeRoot) })
	writeDoctorSetupMarker(t, home, workspace, runtimeRoot)

	backend := sandbox.Backend{Name: sandbox.BackendWindowsRestrictedToken}

	// Healthy first, so the assertion below cannot be satisfied by a check that
	// warns unconditionally.
	if result := windowsSandboxSetupCheck("windows", backend, workspace, config.SandboxConfig{}); result != nil {
		t.Fatalf("SETUP INVALID: a freshly set-up machine was reported unhealthy: %s", result.Message)
	}

	// Now take away the inputs the selection is derived from, leaving the marker
	// and the provisioned tree exactly as they were. os.UserCacheDir fails with no
	// location to read, which is the same error a command would hit.
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")

	result := windowsSandboxSetupCheck("windows", backend, workspace, config.SandboxConfig{})
	if result == nil {
		t.Fatal("doctor reported a healthy sandbox while the runtime root a command selects could not be resolved; every command would fail to launch with doctor saying nothing")
	}
	if status, _ := result.Details["setupStatus"].(string); status != "runtime-root-unresolved" {
		t.Errorf("setupStatus = %q, want runtime-root-unresolved so the unknown state is not reported as staleness: %s", status, result.Message)
	}
	if _, ok := result.Details["error"]; !ok {
		t.Errorf("the warning does not carry the resolver cause, so an operator cannot act on it: %+v", result.Details)
	}
}

// A CACHE DIRECTORY THAT RESOLVES TO NOTHING IS UNRESOLVED TOO, not only one
// that fails to resolve. os.UserCacheDir succeeds on a value like "." and the
// selection a command makes refuses it after canonicalizing, with "user cache
// directory is unavailable". The diagnostic behind this check did not, so doctor
// went on to compare the recorded root against candidates derived from an empty
// cache root and reported staleness, which sends an operator to re-run setup on
// a machine where setup cannot select a root either.
//
// The sandbox package pins the two entry points to one error through the
// resolver seam on every platform. This is the consumer's half, and it is
// Windows-only because that is where the environment can produce the value:
// %LocalAppData% is returned as it is, while a relative XDG_CACHE_HOME is an
// error on Linux and a relative HOME still joins to a usable path on macOS.
// Reported by @jatmn.
func TestDoctorReportsAnEmptyCacheRootAsUnresolvedNotStale(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("only %LocalAppData% hands os.UserCacheDir a value that canonicalizes to nothing")
	}
	home := t.TempDir()
	workspace := t.TempDir()
	t.Setenv("ZERO_WINDOWS_SANDBOX_HOME", home)
	redirectUserCache(t)

	runtimeRoot := doctorRuntimeCandidate(t, workspace)
	if !strings.HasPrefix(runtimeRoot, os.TempDir()) && !strings.Contains(runtimeRoot, t.Name()) {
		t.Fatalf("the runtime candidate %q is outside test-owned storage", runtimeRoot)
	}
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		t.Fatalf("create the runtime root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeRoot) })
	writeDoctorSetupMarker(t, home, workspace, runtimeRoot)

	backend := sandbox.Backend{Name: sandbox.BackendWindowsRestrictedToken}
	if result := windowsSandboxSetupCheck("windows", backend, workspace, config.SandboxConfig{}); result != nil {
		t.Fatalf("SETUP INVALID: a freshly set-up machine was reported unhealthy: %s", result.Message)
	}

	for _, value := range []string{".", " . "} {
		t.Setenv("LOCALAPPDATA", value)
		result := windowsSandboxSetupCheck("windows", backend, workspace, config.SandboxConfig{})
		if result == nil {
			t.Fatalf("LOCALAPPDATA=%q: doctor reported a healthy sandbox while no command can select a runtime root", value)
		}
		if status, _ := result.Details["setupStatus"].(string); status != "runtime-root-unresolved" {
			t.Errorf("LOCALAPPDATA=%q: setupStatus = %q, want runtime-root-unresolved: %s", value, status, result.Message)
		}
		if cause, _ := result.Details["error"].(string); !strings.Contains(cause, "user cache directory is unavailable") {
			t.Errorf("LOCALAPPDATA=%q: the warning does not carry the resolver cause a command would report: %+v", value, result.Details)
		}
	}
}

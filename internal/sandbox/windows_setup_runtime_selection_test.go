package sandbox

import (
	"errors"
	"strings"
	"testing"
)

// SETUP MUST NOT PERSIST STATE IT CANNOT ATTEST.
//
// The runtime-root selection used to be best effort here: if it failed, setup
// carried on and wrote a marker with no runtime root in it, having already
// applied the capability ACLs to a derived tree. A later command makes its own
// concrete selection, finds no stamp on what it picked and refuses, and
// re-running setup takes the same branch and records nothing again. That is the
// permanent brick the recorded-root contract exists to prevent, reached through
// the one path allowed to skip it.
func TestSetupArgsFailWhenNoRuntimeRootCanBeSelected(t *testing.T) {
	original := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return "", errors.New("no cache directory on this machine") }
	t.Cleanup(func() { sandboxUserCacheDir = original })

	setupPlan, err := BuildWindowsSandboxSetupArgs(WindowsSandboxSetupArgsOptions{
		CommandCWD:  t.TempDir(),
		SandboxHome: t.TempDir(),
	})
	if err == nil {
		t.Fatalf("setup args were built without a runtime root; the marker they produce attests nothing: %v", setupPlan.Args)
	}
	if !strings.Contains(err.Error(), "runtime root") {
		t.Errorf("the failure does not name the step that failed: %v", err)
	}
}

// And the ordinary path still records the concrete root it selected, so the
// test above is failing on the selection and not on some unrelated argument.
func TestSetupArgsRecordTheSelectedRuntimeRoot(t *testing.T) {
	workspace := t.TempDir()
	tempHome := t.TempDir()
	t.Setenv("TMPDIR", tempHome)
	t.Setenv("TMP", tempHome)
	t.Setenv("TEMP", tempHome)

	cacheRoot := t.TempDir()
	original := sandboxUserCacheDir
	sandboxUserCacheDir = func() (string, error) { return cacheRoot, nil }
	t.Cleanup(func() { sandboxUserCacheDir = original })

	setupPlan, err := BuildWindowsSandboxSetupArgs(WindowsSandboxSetupArgsOptions{
		CommandCWD:  workspace,
		SandboxHome: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("BuildWindowsSandboxSetupArgs: %v", err)
	}
	args := setupPlan.Args
	profile := ""
	for index, arg := range args {
		if arg == "--permission-profile" && index+1 < len(args) {
			profile = args[index+1]
		}
	}
	if profile == "" {
		t.Fatalf("no permission profile in the setup args: %v", args)
	}
	if !strings.Contains(profile, "\"runtime\"") {
		t.Errorf("the setup profile records no runtime root, so the marker cannot attest one: %s", profile)
	}
}

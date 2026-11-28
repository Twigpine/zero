package sandbox

import (
	"errors"
	"strings"
	"testing"
)

// THE DIAGNOSTIC REFUSES WHAT SELECTION REFUSES, WITH THE SAME WORDS.
//
// os.UserCacheDir can succeed and still hand back a value that canonicalizes to
// nothing: an empty string, ".", or only whitespace. Selection fails closed on
// that with "user cache directory is unavailable". The diagnostic that answers
// "would a command still select the recorded root?" spelled the same steps out
// separately and did not have that check, so it derived candidates from the
// empty root and returned an answer, current or stale, on a machine where every
// command fails before it reads a marker. Doctor then reported staleness, or
// nothing at all, instead of runtime-root-unresolved.
//
// Both now get their inputs from one resolver. This drives the two production
// entry points with the same stubbed cache value and requires the same error
// from each. The seam is the production resolver's own. Reported by @jatmn.
func TestTheDiagnosticRefusesTheCacheRootSelectionRefuses(t *testing.T) {
	workspace := t.TempDir()
	usableCache := t.TempDir()

	previous := sandboxUserCacheDir
	t.Cleanup(func() { sandboxUserCacheDir = previous })
	sandboxUserCacheDir = func() (string, error) { return usableCache, nil }

	config := WindowsSandboxSetupConfig{
		SandboxHome:    t.TempDir(),
		CommandCWD:     workspace,
		WorkspaceRoots: []string{workspace},
		PermissionProfile: PermissionProfile{
			FileSystem: FileSystemPolicy{
				Kind:       FileSystemRestricted,
				WriteRoots: []WritableRoot{{Root: workspace}},
			},
			Network: NetworkPolicy{Mode: NetworkDeny},
		},
	}
	root, err := sandboxRuntimeRootFor(canonicalSandboxWorkspaceRoot(workspace), canonicalSandboxWorkspaceRoot(usableCache))
	if err != nil {
		t.Fatalf("derive the runtime root: %v", err)
	}
	config.PermissionProfile = PermissionProfileWithRuntimeRoot(
		WindowsSandboxProfileWithRuntimeRoots(config.PermissionProfile, config.WorkspaceRoots),
		root,
	)
	if _, err := WriteWindowsSandboxSetupMarker(config); err != nil {
		t.Fatalf("WriteWindowsSandboxSetupMarker: %v", err)
	}
	// Healthy first: with a usable cache the diagnostic answers, and says
	// current. Without this the legs below could pass on a marker that was
	// never readable.
	if recorded, current, err := WindowsSandboxRecordedRuntimeRootIsCurrent(config.SandboxHome, workspace); err != nil || recorded == "" || !current {
		t.Fatalf("SETUP INVALID: with a usable cache the diagnostic gave (%q, %v, %v)", recorded, current, err)
	}

	for _, testCase := range []struct {
		name  string
		cache string
	}{
		{name: "empty", cache: ""},
		{name: "dot", cache: "."},
		{name: "whitespace", cache: "   "},
		{name: "dot with whitespace", cache: " . "},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			sandboxUserCacheDir = func() (string, error) { return testCase.cache, nil }

			_, lease, _, selectErr := selectSandboxRuntimeRoot(workspace, true, config.SandboxHome)
			if lease != nil {
				lease.release()
			}
			if selectErr == nil {
				t.Fatalf("SETUP INVALID: selection accepted the cache root %q, so there is no refusal to match", testCase.cache)
			}

			recorded, current, diagnosticErr := WindowsSandboxRecordedRuntimeRootIsCurrent(config.SandboxHome, workspace)
			if diagnosticErr == nil {
				t.Fatalf("the diagnostic answered current=%v for a cache root %q that selection refuses with %q; doctor would report that instead of runtime-root-unresolved", current, testCase.cache, selectErr)
			}
			if diagnosticErr.Error() != selectErr.Error() {
				t.Errorf("the diagnostic failed with %q and selection with %q; an operator is told two different things about one cause", diagnosticErr, selectErr)
			}
			if !strings.Contains(diagnosticErr.Error(), "user cache directory is unavailable") {
				t.Errorf("the cause does not name the cache directory: %q", diagnosticErr)
			}
			if current {
				t.Errorf("an unresolvable selection was still reported current")
			}
			if recorded == "" {
				t.Errorf("the recorded root was dropped from the answer, so doctor cannot say which marker it was looking at")
			}
		})
	}

	// And a resolver error is still reported as itself, by both.
	boom := errors.New("no cache location is defined")
	sandboxUserCacheDir = func() (string, error) { return "", boom }
	_, _, _, selectErr := selectSandboxRuntimeRoot(workspace, true, config.SandboxHome)
	_, _, diagnosticErr := WindowsSandboxRecordedRuntimeRootIsCurrent(config.SandboxHome, workspace)
	if !errors.Is(selectErr, boom) || !errors.Is(diagnosticErr, boom) || selectErr.Error() != diagnosticErr.Error() {
		t.Errorf("a resolver error reads differently: selection %q, diagnostic %q", selectErr, diagnosticErr)
	}
}

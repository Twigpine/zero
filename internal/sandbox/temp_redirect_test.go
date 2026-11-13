package sandbox

import (
	"os"
	"testing"
)

// redirectSandboxTestTemp points EVERY variable os.TempDir reads at dir.
//
// os.TempDir reads TMPDIR on Unix and TMP, then TEMP, on Windows. A platform
// neutral test that sets only the Windows pair changes nothing on Unix, where
// fallbackSandboxRuntimeRoot then derives from the ambient temp: a path outside
// every t.TempDir the test owns, which the test goes on to create or clear. The
// cache resolver override does not reach this input, so a fixture that redirects
// the cache and two of the three variables still has one production input it
// does not own. Reported by @jatmn.
//
// It also verifies the redirect took, because a redirect that silently did
// nothing is exactly how this went unnoticed: the tests passed, or skipped, and
// the tree they made was somewhere else.
func redirectSandboxTestTemp(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	got := canonicalSandboxWorkspaceRoot(os.TempDir())
	want := canonicalSandboxWorkspaceRoot(dir)
	if !sameWindowsRuntimeRootPath(got, want) {
		t.Fatalf("SETUP INVALID: os.TempDir() is %s after redirecting it to %s, so anything derived from temp lands outside this test", got, want)
	}
}

// requireWithinTestOwned fails unless path is inside one of the directories the
// test owns, compared canonically on both sides because the derivation
// canonicalizes and a raw t.TempDir() can be a second spelling of the same place
// (/var against /private/var, an 8.3 profile name on a Windows runner).
//
// Called BEFORE a derived path is created or removed. Checking afterwards
// reports a tree in somebody else's directory once it already exists.
func requireWithinTestOwned(t *testing.T, path string, owned ...string) {
	t.Helper()
	canonical := canonicalSandboxWorkspaceRoot(path)
	for _, root := range owned {
		if pathWithinRoot(canonicalSandboxWorkspaceRoot(root), canonical) {
			return
		}
	}
	t.Fatalf("%s (canonically %s) is outside every directory this test owns %v; refusing to create or remove it", path, canonical, owned)
}

// testStampPlanHash is the plan the stamp tests attest. The stamp is named after
// the plan it proves, so a test that writes one and then looks for it has to use
// the same plan on both sides; one shared value keeps that from being a typo.
const testStampPlanHash = "planhash"

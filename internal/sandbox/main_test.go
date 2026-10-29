package sandbox

import (
	"os"
	"testing"
)

// TestMain points the sandbox runtime's user-cache root at test-owned storage
// for the whole package.
//
// PLAN CONSTRUCTION CREATES DIRECTORIES, which is easy to miss because it reads
// like naming. windowsSandboxProfileWithProvisionedRuntime provisions the root
// it selects, and the simulated-Windows tests run on every platform, so a test
// that supplies an explicit child environment but leaves the cache alone writes
// into the developer's real one. Not hypothetical: the machine this was found on
// had accumulated thousands of entries under the real runtime root from exactly
// these runs.
//
// Done once for the package rather than per test, because the leak is in the
// DEFAULT: any new test that builds a Windows plan is affected unless its author
// remembers, and remembering is what failed here. A test that needs a specific
// cache root still overrides sandboxUserCacheDir itself.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "zero-sandbox-testcache-")
	if err != nil {
		// Fail loudly rather than silently falling back to the real cache.
		panic("sandbox tests: create the test cache root: " + err.Error())
	}
	sandboxUserCacheDir = func() (string, error) { return root, nil }

	code := m.Run()

	_ = os.RemoveAll(root)
	os.Exit(code)
}

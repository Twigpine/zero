//go:build windows

package update

import "testing"

// stubRandomStagingSuffix overrides randomStagingSuffix to always return a
// fixed value for the duration of t, restoring the original on cleanup.
// stagingFilePath's random suffix is unpredictable by design in production, so
// the Windows staging tests that need the exact path pin it here instead. POSIX
// stages inside a private directory and has no such name to pin.
func stubRandomStagingSuffix(t *testing.T, suffix string) {
	t.Helper()
	original := randomStagingSuffix
	randomStagingSuffix = func() (string, error) { return suffix, nil }
	t.Cleanup(func() { randomStagingSuffix = original })
}

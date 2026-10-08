package update

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubStageBinaryFailure makes stageBinary fail for targetPath (matched by base
// name, since installBinary is called with the real install path) and behave
// normally for everything else, restoring the original on cleanup.
//
// The staging location is created exclusively under an unpredictable name — a
// private directory on POSIX — so a test can no more occupy it than an attacker
// can. This seam is how a staging failure is exercised instead.
func stubStageBinaryFailure(t *testing.T, targetPath string, failure error) {
	t.Helper()
	original := stageBinary
	want := filepath.Base(targetPath)
	stageBinary = func(sourcePath string, target string) (*stagedBinary, error) {
		if filepath.Base(target) == want {
			return nil, failure
		}
		return original(sourcePath, target)
	}
	t.Cleanup(func() { stageBinary = original })
}

// assertNoStagingLeftovers fails when dir still holds a platform staging
// artifact. Checking both patterns is harmless and keeps this test helper
// consistent across POSIX and Windows.
func assertNoStagingLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".zero-stage-") || strings.HasSuffix(entry.Name(), ".new") {
			t.Fatalf("staging leftover survived in the install directory: %s", entry.Name())
		}
	}
}

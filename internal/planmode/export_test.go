package planmode

import "testing"

// SetTempDirForTest overrides the temp dir func for unit tests. Kept in
// export_test.go so the production planmode package (and therefore cmd/zero)
// does not import testing.
func SetTempDirForTest(t *testing.T, tempDir string) {
	t.Helper()
	tempDirMu.Lock()
	old := tempDirFn
	tempDirFn = func() string { return tempDir }
	tempDirMu.Unlock()
	t.Cleanup(func() {
		tempDirMu.Lock()
		tempDirFn = old
		tempDirMu.Unlock()
	})
}

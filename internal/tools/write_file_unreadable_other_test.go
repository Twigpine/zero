//go:build !windows

package tools

import (
	"os"
	"testing"
)

// makeFileWriteOnly drops read permission while leaving the file writable, so
// the target can still be opened for writing but its prior bytes cannot be read.
// write_file must refuse that overwrite, since those bytes are the only evidence
// of the BOM and line endings to preserve. The returned func restores the
// original mode so the test can read the file back and the temp dir can be
// cleaned up.
func makeFileWriteOnly(t *testing.T, path string) func() {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mode := info.Mode().Perm()
	if err := os.Chmod(path, 0o200); err != nil {
		t.Skipf("cannot drop read permission on this filesystem: %v", err)
	}
	return func() {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
}

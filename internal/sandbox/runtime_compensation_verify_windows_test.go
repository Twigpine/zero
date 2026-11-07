//go:build windows

package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ONLY NOT-FOUND IS EVIDENCE OF REMOVAL.
//
// The verification probe used to read any Lstat error as absence, so a sharing
// violation, an access denial, or an entry left delete-pending by another
// process that holds a share-delete handle all reported complete compensation
// for a directory that is still there. A holder able to clear the disposition
// could then make the "removed" object visible again, and setup would already
// have said the rollback finished.
//
// The three outcomes are distinguished with a probe seam, because a real
// filesystem will not produce the third on demand.
func TestDeletionVerificationDistinguishesAllThreeOutcomes(t *testing.T) {
	previous := runtimeCompensationStat
	t.Cleanup(func() { runtimeCompensationStat = previous })

	newDir := func() (string, string) {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "created")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		identity, ok := runtimeDirIdentity(dir)
		if !ok {
			t.Fatal("identify the created directory")
		}
		return dir, identity
	}

	t.Run("absence is proven and reported as success", func(t *testing.T) {
		dir, identity := newDir()
		runtimeCompensationStat = func(string) (fs.FileInfo, error) {
			return nil, &os.PathError{Op: "lstat", Path: dir, Err: os.ErrNotExist}
		}
		if err := removeCreatedRuntimeDirBound(dir, identity); err != nil {
			t.Errorf("a proven-absent directory was reported as a failure: %v", err)
		}
	})

	t.Run("still present is reported", func(t *testing.T) {
		dir, identity := newDir()
		runtimeCompensationStat = func(path string) (fs.FileInfo, error) { return os.Stat(".") }
		err := removeCreatedRuntimeDirBound(dir, identity)
		if err == nil {
			t.Fatal("a directory still present after deletion was reported as removed")
		}
		if !strings.Contains(err.Error(), "still present") {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("an unverifiable probe is residue, not success", func(t *testing.T) {
		dir, identity := newDir()
		// What a share-delete holder produces: neither absence nor presence.
		runtimeCompensationStat = func(path string) (fs.FileInfo, error) {
			return nil, &os.PathError{Op: "lstat", Path: path, Err: errors.New("Access is denied.")}
		}
		err := removeCreatedRuntimeDirBound(dir, identity)
		if err == nil {
			t.Fatal("an unverifiable removal was reported as complete compensation")
		}
		if !strings.Contains(err.Error(), "could not be verified") {
			t.Errorf("the error does not say the removal is unproven: %v", err)
		}
	})
}

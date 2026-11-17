//go:build !windows

package skills

import (
	"os"
	"sync"
	"testing"
)

// holdUnreadable takes every permission off path, so reading it fails until
// release puts them back. Cleanup releases it too. Root reads a file whatever
// its mode, so a test that needs the read to fail is skipped there.
func holdUnreadable(t *testing.T, path string) (release func()) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = os.Chmod(path, info.Mode().Perm()) }) }
	t.Cleanup(release)
	return release
}

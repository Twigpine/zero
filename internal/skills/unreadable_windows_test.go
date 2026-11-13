//go:build windows

package skills

import (
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

// holdUnreadable keeps path open with no sharing, so every other open of it
// fails until release runs. Cleanup releases it too.
func holdUnreadable(t *testing.T, path string) (release func()) {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("hold %s open: %v", path, err)
	}
	var once sync.Once
	release = func() { once.Do(func() { _ = windows.CloseHandle(handle) }) }
	t.Cleanup(release)
	return release
}

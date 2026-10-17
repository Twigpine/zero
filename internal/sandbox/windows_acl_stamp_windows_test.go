//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// The positive half only. The swap case that proves the ACE and the stamp are
// about ONE OBJECT lives in windows_acl_stamp_swap_windows_test.go, where it can
// drive the real apply.
//
// A direct-handle test cannot prove it: any name derived from the retained
// handle resolves back to the same object, so a pathname write derived that way
// lands correctly too and the test passes either way. The distinction only shows
// through the call site, where the old code re-opened the ORIGINAL root string.
// And the ordinary path still works, or the assertion above would be satisfied
// by a writer that never writes anything.
func TestTheStampWritesThroughAnOpenDirectoryHandle(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(append([]string{base}, append(append([]string{}, windowsSandboxRuntimeOwnedNames...), "abcdef0123456789")...)...)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create the runtime tree: %v", err)
	}
	handle, err := openWindowsRuntimeTailDirectory(root, windows.READ_CONTROL|windowsFileAddFile|windows.FILE_TRAVERSE)
	if err != nil {
		t.Fatalf("open the runtime root: %v", err)
	}
	defer windows.CloseHandle(handle)

	if err := writeWindowsRuntimeStampToDirectoryHandle(handle, windowsSandboxRuntimeStampName(testStampPlanHash), testStampPlanHash); err != nil {
		t.Fatalf("write the stamp: %v", err)
	}
	recorded, err := os.ReadFile(windowsSandboxRuntimeStampPath(root, testStampPlanHash))
	if err != nil || string(recorded) != "planhash" {
		t.Fatalf("the stamp did not land in the runtime root (%q, err %v)", recorded, err)
	}
}

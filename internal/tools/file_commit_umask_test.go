//go:build unix

package tools

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// THE PUBLISHED FILE CARRIES THE PERMISSIONS THE PROCESS UMASK PRODUCES, NOT
// THE RAW MODE AN ASSIGNMENT WOULD SET.
//
// The create path reserves the target name with the same exclusive create this
// branch of the write path always honored, and the kernel applies the process
// umask to the reserved file it grants. The publish must carry those granted
// bits — taken from the reserved file itself — because a chmod never consults
// the umask: a process that requests tight defaults on new files would find
// every write it publishes shared with the world. Under the restrictive umask
// a plain mode assignment would publish 0o644 where the reservation was
// granted 0o600, so the test fails before the granted bits are carried.
func TestCreatePublishesTheUmaskedPermissionsOfTheReservedCreate(t *testing.T) {
	for name, testCase := range map[string]struct {
		umask int
		want  os.FileMode
	}{
		"a restrictive umask keeps every stranger out": {umask: 0o077, want: 0o600},
		"a permissive umask keeps the readable bits":   {umask: 0o022, want: 0o644},
	} {
		t.Run(name, func(t *testing.T) {
			previous := syscall.Umask(testCase.umask)
			defer syscall.Umask(previous)
			root := t.TempDir()
			target := filepath.Join(root, "created.txt")

			result := NewScopedWriteFileTool(root, nil).Run(context.Background(), map[string]any{
				"path": "created.txt", "content": "zero\n",
			})
			if result.Status != StatusOK {
				t.Fatalf("create status = %s: %s", result.Status, result.Output)
			}
			info, err := os.Stat(target)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != testCase.want {
				t.Fatalf("published mode = %#o, want the %#o the kernel granted the reservation under this umask; a raw mode assignment would publish %#o and share the file with hands the umask excludes", got, testCase.want, os.FileMode(0o644))
			}
			if got, readErr := os.ReadFile(target); readErr != nil || string(got) != "zero\n" {
				t.Fatalf("published content = %q, err=%v", got, readErr)
			}
		})
	}
}

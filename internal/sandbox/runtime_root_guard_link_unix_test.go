//go:build !windows

package sandbox

import (
	"os"
	"testing"
)

// A POSIX symlink is the reachable alias off Windows.
func linkRuntimeComponent(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink in this environment: %v", err)
	}
}

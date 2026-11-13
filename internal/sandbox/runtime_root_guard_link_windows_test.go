//go:build windows

package sandbox

import (
	"os/exec"
	"testing"
)

// A JUNCTION, not a symlink: it needs no privilege, which is what makes it the
// alias an ordinary local user can actually plant, and os.Lstat reports it as
// ModeIrregular rather than ModeSymlink.
func linkRuntimeComponent(t *testing.T, link, target string) {
	t.Helper()
	output, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("cannot create a junction in this environment: %v (%s)", err, output)
	}
}

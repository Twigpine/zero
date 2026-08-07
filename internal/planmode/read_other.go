//go:build !unix && !windows

package planmode

import (
	"fmt"
	"os"
)

// openPlanUnderBase is a best-effort fallback for platforms without openat /
// OBJ_DONT_REPARSE primitives. It refuses a final-component symlink via Lstat
// then opens through os.Root. The Lstat/Open race remains on these platforms;
// Zero's supported targets are Unix and Windows, which use the true no-follow
// walkers in read_unix.go and read_windows.go.
func openPlanUnderBase(base, rel, displayPath string) (*os.File, error) {
	if _, err := relComponents(rel); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	info, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errPlanSymlink(displayPath)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("plan file %s is not a regular file", displayPath)
	}
	return root.Open(rel)
}

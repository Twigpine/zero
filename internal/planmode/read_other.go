//go:build !unix && !windows

package planmode

import (
	"fmt"
	"os"
)

// openPlanUnderBase fails closed on platforms without openat / OBJ_DONT_REPARSE
// primitives. A validate-then-open sequence (Lstat then Open) leaves a
// time-of-check to time-of-use gap: a validated regular file can be replaced
// with an in-root symlink before Open runs. Zero's supported targets are Unix
// and Windows, which use the true no-follow walkers in read_unix.go and
// read_windows.go; this fallback refuses instead of returning a file opened
// through a race it cannot close.
func openPlanUnderBase(_, _, displayPath string) (*os.File, error) {
	return nil, fmt.Errorf("plan file %s: reading plan files is not supported on this platform", displayPath)
}

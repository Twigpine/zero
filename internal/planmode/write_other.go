//go:build !unix && !windows

package planmode

import "fmt"

// writePlanUnderBase fails closed on platforms without openat /
// OBJ_DONT_REPARSE primitives, matching openPlanUnderBase in read_other.go.
// os.Root resolves in-root symlinks and a Lstat-then-open sequence is a
// check-to-use race, so containment cannot be bound at create/rename time. A
// plan written by a weaker fallback could also never be read back, since
// openPlanUnderBase always refuses on these platforms. Zero's supported
// targets are Unix and Windows, which use the true no-follow walkers in
// write_unix.go and write_windows.go.
func writePlanUnderBase(_, _, displayPath, _ string) error {
	return fmt.Errorf("plan file %s: writing plan files is not supported on this platform", displayPath)
}

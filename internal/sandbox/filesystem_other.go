//go:build !darwin && !linux

package sandbox

import "errors"

// Platforms without a stat-based filesystem identity report "unknown" rather
// than "separate", so a caller that fails closed on an unknown answer keeps
// doing so here. Neither helper is reached outside the Linux planner today.
func pathsShareFilesystem(_, _ string) (shared bool, known bool) {
	return false, false
}

func pathHardLinkCount(_ string) (uint64, error) {
	return 0, errors.ErrUnsupported
}

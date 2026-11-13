//go:build windows

package sandbox

// windowsACLPlanApplied reads the real security descriptors. See
// windowsACLPlanStillApplied.
var windowsACLPlanApplied = windowsACLPlanStillApplied

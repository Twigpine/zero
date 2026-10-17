//go:build !windows

package sandbox

// windowsACLPlanApplied reports whether the objects a plan names still carry the
// grants it describes.
//
// Off Windows there is no DACL to read and nothing that consumes one, so the
// marker's own comparisons are the whole answer. Declared here rather than
// guarded at the call site so the setup-to-command contract keeps one shape on
// every platform.
var windowsACLPlanApplied = func(WindowsACLPlan) bool { return true }

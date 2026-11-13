// Test seams: helpers only test code uses, kept out of the production binary.
package zerocommands

import (
	"github.com/Twigpine/zero/internal/hooks"
	"github.com/Twigpine/zero/internal/mcp"
)

// HookSnapshotsWithSource converts a slice of hooks.Definition and
// tags every snapshot with the same source string. The helper
// exists so callers that have a single hooks.LoadResult can build
// the snapshot slice in one pass.
func HookSnapshotsWithSource(definitions []hooks.Definition, source hooks.ConfigSource) []HookSnapshot {
	return hookSnapshotsWithSource(definitions, source)
}

// MCPServerSnapshotWithCounts returns a snapshot that also records
// how many tools the server exposes and how many persistent
// approvals are currently recorded. A nil counts struct is treated
// as zero values so callers that do not have a live registry can
// still call this helper.
func MCPServerSnapshotWithCounts(server mcp.Server, counts *MCPServerCounts) MCPServerSnapshot {
	snapshot := MCPServerSnapshotFromServer(server)
	if counts == nil {
		return snapshot
	}
	snapshot.ToolCount = counts.ToolCount
	snapshot.AllowGranted = counts.AllowGranted
	snapshot.DenyGranted = counts.DenyGranted
	return snapshot
}

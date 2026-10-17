package zerocommands

import (
	"sort"
	"strings"

	"github.com/Twigpine/zero/internal/redaction"
	"github.com/Twigpine/zero/internal/sandbox"
)

// SandboxGrantSnapshot is the typed view of a single persistent
// sandbox grant as it is exposed to TUI, headless, and PR/CI
// automation. The snapshot guarantees that no secret material from
// the grant's Reason field is leaked: the field is always run
// through the standard redaction pipeline before it is copied into
// the snapshot. ToolName, Decision, and ApprovedAt are non-secret
// metadata and are copied verbatim.
type SandboxGrantSnapshot struct {
	ToolName   string `json:"toolName"`
	Decision   string `json:"decision"`
	ApprovedAt string `json:"approvedAt,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// SandboxGrantMatchSnapshot is the typed view returned to consumers
// who look up a grant for a specific tool call. It pairs the
// underlying grant snapshot with a Matched flag so callers can tell
// the difference between "grant existed and matched" and "no grant
// recorded for this tool" without inspecting an error or a typed
// zero value.
type SandboxGrantMatchSnapshot struct {
	ToolName string                `json:"toolName"`
	Matched  bool                  `json:"matched"`
	Grant    *SandboxGrantSnapshot `json:"grant,omitempty"`
}

// SandboxGrantSnapshotFromGrant converts a sandbox.Grant into its
// typed snapshot. The Reason field is run through redaction so any
// secret material is masked before the snapshot leaves the runtime.
// An empty or whitespace-only Reason becomes an empty Reason in the
// snapshot, which keeps the JSON shape stable for downstream
// consumers.
func SandboxGrantSnapshotFromGrant(grant sandbox.Grant) SandboxGrantSnapshot {
	return SandboxGrantSnapshot{
		ToolName:   strings.TrimSpace(grant.ToolName),
		Decision:   string(grant.Decision),
		ApprovedAt: strings.TrimSpace(grant.ApprovedAt),
		Reason:     redaction.RedactString(strings.TrimSpace(grant.Reason), redaction.Options{}),
	}
}

// SandboxGrantSnapshots converts a slice of sandbox grants into a
// stable, sorted slice of typed snapshots. The output is sorted
// alphabetically by tool name so consumers (TUI, headless, JSON
// output) get a deterministic ordering. An empty input produces an
// empty (non-nil) slice so the JSON output is always `[]` and never
// `null`.
func SandboxGrantSnapshots(grants []sandbox.Grant) []SandboxGrantSnapshot {
	snapshots := make([]SandboxGrantSnapshot, 0, len(grants))
	for _, grant := range grants {
		snapshots = append(snapshots, SandboxGrantSnapshotFromGrant(grant))
	}
	sort.SliceStable(snapshots, func(left, right int) bool {
		return snapshots[left].ToolName < snapshots[right].ToolName
	})
	return snapshots
}

// SandboxGrantMatchSnapshotFromLookup converts a sandbox.GrantLookup
// into its typed snapshot. When the lookup did not match, the
// returned snapshot has Matched=false and a nil Grant pointer so
// consumers can render the absence without consulting an error.
func SandboxGrantMatchSnapshotFromLookup(toolName string, lookup sandbox.GrantLookup) SandboxGrantMatchSnapshot {
	match := SandboxGrantMatchSnapshot{
		ToolName: strings.TrimSpace(toolName),
		Matched:  lookup.Matched,
	}
	if !lookup.Matched {
		return match
	}
	snapshot := SandboxGrantSnapshotFromGrant(lookup.Grant)
	match.Grant = &snapshot
	return match
}

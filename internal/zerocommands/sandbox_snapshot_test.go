package zerocommands

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/sandbox"
)

func TestSandboxGrantSnapshotFromGrantRedactsReasonAndTrimsFields(t *testing.T) {
	grant := sandbox.Grant{
		ToolName:   "  bash  ",
		Decision:   sandbox.GrantAllow,
		ApprovedAt: "  2026-06-04T10:00:00Z  ",
		Reason:     "auth token sk-proj-abcdefghijklmnopqrstuvwxyz0123456789 leaked",
	}

	snapshot := SandboxGrantSnapshotFromGrant(grant)

	if snapshot.ToolName != "bash" {
		t.Fatalf("ToolName not trimmed: %q", snapshot.ToolName)
	}
	if snapshot.ApprovedAt != "2026-06-04T10:00:00Z" {
		t.Fatalf("ApprovedAt not trimmed: %q", snapshot.ApprovedAt)
	}
	if snapshot.Decision != string(sandbox.GrantAllow) {
		t.Fatalf("Decision = %q, want %q", snapshot.Decision, sandbox.GrantAllow)
	}
	if strings.Contains(snapshot.Reason, "sk-proj-") {
		t.Fatalf("expected reason secret to be redacted, got %q", snapshot.Reason)
	}
	if !strings.Contains(snapshot.Reason, "[REDACTED]") {
		t.Fatalf("expected redaction marker in reason, got %q", snapshot.Reason)
	}
}

func TestSandboxGrantSnapshotFromGrantEmptyReasonBecomesEmpty(t *testing.T) {
	grant := sandbox.Grant{
		ToolName:   "bash",
		Decision:   sandbox.GrantDeny,
		ApprovedAt: "2026-06-04T10:00:00Z",
		Reason:     "   ",
	}
	snapshot := SandboxGrantSnapshotFromGrant(grant)
	if snapshot.Reason != "" {
		t.Fatalf("expected empty reason after trim, got %q", snapshot.Reason)
	}
}

func TestSandboxGrantSnapshotsSortsByToolNameAndReturnsEmptySliceForEmptyInput(t *testing.T) {
	grants := []sandbox.Grant{
		{ToolName: "write_file", Decision: sandbox.GrantAllow, ApprovedAt: "2026-06-04T10:00:00Z"},
		{ToolName: "bash", Decision: sandbox.GrantAllow, ApprovedAt: "2026-06-04T10:00:00Z"},
		{ToolName: "edit_file", Decision: sandbox.GrantDeny, ApprovedAt: "2026-06-04T10:00:00Z"},
	}
	snapshots := SandboxGrantSnapshots(grants)
	if len(snapshots) != 3 {
		t.Fatalf("expected 3 snapshots, got %d", len(snapshots))
	}
	if snapshots[0].ToolName != "bash" || snapshots[1].ToolName != "edit_file" || snapshots[2].ToolName != "write_file" {
		t.Fatalf("snapshots not sorted by toolName: %#v", snapshots)
	}

	empty := SandboxGrantSnapshots(nil)
	if empty == nil {
		t.Fatal("expected non-nil empty slice so JSON output is [] not null")
	}
	if len(empty) != 0 {
		t.Fatalf("expected empty slice, got %d", len(empty))
	}
	encoded, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("expected JSON [] for empty input, got %q", string(encoded))
	}
}

func TestSandboxGrantMatchSnapshotFromLookupMatchedAndUnmatched(t *testing.T) {
	matched := SandboxGrantMatchSnapshotFromLookup("  bash  ", sandbox.GrantLookup{
		Matched: true,
		Grant: sandbox.Grant{
			ToolName:   "bash",
			Decision:   sandbox.GrantAllow,
			ApprovedAt: "2026-06-04T10:00:00Z",
			Reason:     "user pressed always",
		},
	})
	if !matched.Matched {
		t.Fatal("expected Matched=true")
	}
	if matched.ToolName != "bash" {
		t.Fatalf("expected trimmed toolName, got %q", matched.ToolName)
	}
	if matched.Grant == nil {
		t.Fatal("expected non-nil Grant pointer for matched lookup")
	}
	if matched.Grant.Decision != string(sandbox.GrantAllow) {
		t.Fatalf("Grant.Decision = %q, want %q", matched.Grant.Decision, sandbox.GrantAllow)
	}

	unmatched := SandboxGrantMatchSnapshotFromLookup("bash", sandbox.GrantLookup{Matched: false})
	if unmatched.Matched {
		t.Fatal("expected Matched=false")
	}
	if unmatched.Grant != nil {
		t.Fatalf("expected nil Grant pointer for unmatched lookup, got %#v", unmatched.Grant)
	}
	if unmatched.ToolName != "bash" {
		t.Fatalf("expected toolName preserved, got %q", unmatched.ToolName)
	}
}

func TestSandboxGrantSnapshotJSONShapeIsStable(t *testing.T) {
	snapshot := SandboxGrantSnapshot{
		ToolName:   "bash",
		Decision:   "allow",
		ApprovedAt: "2026-06-04T10:00:00Z",
		Reason:     "user approved",
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}
	for _, key := range []string{"toolName", "decision", "approvedAt", "reason"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("expected key %q in JSON output, got %q", key, string(encoded))
		}
	}
	if _, ok := decoded["maxAutonomy"]; ok {
		t.Fatalf("maxAutonomy should not be present in JSON output: %q", string(encoded))
	}
}

package cli

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/tools"
)

// A DISCLOSURE THAT IS NOT PERSISTED DID NOT SURVIVE THE RUN.
//
// The session log is what a resumed or compacted conversation is rebuilt from,
// and replay reads this payload's "output" straight into the transcript without
// reconstructing an agent.ToolResult. So persisting the raw undecorated field
// makes a warning that was visible during the original run disappear the moment
// the session is resumed, with nothing failing anywhere to say so.
func TestPersistedToolResultKeepsTheEnforcementNotice(t *testing.T) {
	const notice = "least-privilege notice: read access was narrowed"
	payload := persistedToolResultPayload(agent.ToolResult{
		ToolCallID:         "call-1",
		Name:               "bash",
		Status:             tools.StatusOK,
		Output:             "the command output",
		EnforcementNotices: []string{notice},
	})

	output, _ := payload["output"].(string)
	if count := strings.Count(output, notice); count != 1 {
		t.Errorf("persisted output carries the notice %d times, want exactly 1: %q", count, output)
	}
	if !strings.Contains(output, "the command output") {
		t.Errorf("persisted output lost the command output: %q", output)
	}
}

// The other fields still round-trip, so the shared helper did not quietly drop
// what the two writers used to record separately.
func TestPersistedToolResultKeepsItsOtherFields(t *testing.T) {
	payload := persistedToolResultPayload(agent.ToolResult{
		ToolCallID:   "call-2",
		Name:         "write_file",
		Status:       tools.StatusError,
		Output:       "boom",
		Meta:         map[string]string{"k": "v"},
		Truncated:    true,
		Redacted:     true,
		ChangedFiles: []string{"a.go"},
	})
	for _, field := range []string{"toolCallId", "name", "status", "output", "meta", "truncated", "redacted", "changedFiles"} {
		if _, ok := payload[field]; !ok {
			t.Errorf("payload is missing %q: %#v", field, payload)
		}
	}
	if payload["status"] != string(tools.StatusError) {
		t.Errorf("status = %v, want %q", payload["status"], tools.StatusError)
	}
}

// An ordinary result records exactly what it did before, so the accessor is not
// adding anything where there is nothing to add.
func TestPersistedToolResultLeavesAnOrdinaryResultAlone(t *testing.T) {
	payload := persistedToolResultPayload(agent.ToolResult{
		ToolCallID: "call-3",
		Name:       "bash",
		Status:     tools.StatusOK,
		Output:     "plain output",
	})
	if got := payload["output"]; got != "plain output" {
		t.Errorf("persisted output = %v, want it untouched", got)
	}
}

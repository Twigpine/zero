package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

// PRODUCING A RESULT AND ASKING THE RUN TO STOP ARE DIFFERENT FACTS.
//
// closeOutRemaining decides between finalizing a completed sibling and writing
// an aborted placeholder, and it used to make that decision from abortErr. A
// cancelled permission request is exactly where the two facts disagree:
// executeToolCall builds the cancellation result, with its call ID, its message
// and its denial category, and returns it TOGETHER with
// ErrPermissionApprovalCanceled. Reading the error as "never started" discards a
// real result and records the opposite of what happened, and the cancellation
// then goes missing from OnToolResult, the trace counters and the task
// observation even though the permission event already fired.
func TestABatchEntryWithAResultAndAnAbortErrorIsDrained(t *testing.T) {
	canceled := canceledPermissionResult(
		ToolCall{ID: "call-b", Name: "read_probe"},
		"cancelled in TUI",
		PermissionEvent{ToolName: "read_probe"},
	)
	precomputed := []precomputedToolResult{
		{result: ToolResult{ToolCallID: "call-a", Name: "read_probe", Status: tools.StatusOK}, completed: true},
		{
			result:    canceled,
			abortErr:  fmt.Errorf("%w for read_probe", ErrPermissionApprovalCanceled),
			completed: true,
		},
	}

	sibling, ran := precomputedResultFor(precomputed, 0, 2, 1)
	if !ran {
		t.Fatal("a cancelled permission result was treated as a call that never started")
	}
	if sibling.ToolCallID != "call-b" {
		t.Errorf("ToolCallID = %q, want the cancellation result rather than an empty placeholder", sibling.ToolCallID)
	}
	if sibling.DenialReason != DenialApprovalCanceled {
		t.Errorf("DenialReason = %q, want the cancellation preserved", sibling.DenialReason)
	}
}

// An entry that never produced a result is still unstarted, or the fix would
// have stopped aborting anything.
func TestABatchEntryWithNoResultIsStillUnstarted(t *testing.T) {
	precomputed := []precomputedToolResult{
		{abortErr: errors.New("cancelled before the tool ran")},
	}
	if _, ran := precomputedResultFor(precomputed, 0, 1, 0); ran {
		t.Error("an entry that produced nothing was finalized as if it had run")
	}
}

// A call outside the batch window never started either.
func TestACallOutsideTheBatchWindowIsUnstarted(t *testing.T) {
	precomputed := []precomputedToolResult{{result: ToolResult{ToolCallID: "call-a"}, completed: true}}
	if _, ran := precomputedResultFor(precomputed, 0, 1, 1); ran {
		t.Error("a call past the batch window was reported as executed")
	}
}

// And the flag is recorded where the truth is known rather than inferred later,
// so a real batch marks what it ran.
func TestTheBatchRecordsWhatItRan(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(&readAheadTool{})
	calls := []ToolCall{
		{ID: "call-a", Name: "read_probe", Arguments: `{"id":"a"}`},
		{ID: "call-b", Name: "read_probe", Arguments: `{"id":"b"}`},
	}
	results := executeParallelReadBatch(context.Background(), registry, calls, 0, 2, PermissionModeAuto, Options{Registry: registry})
	for index, entry := range results {
		if !entry.completed {
			t.Errorf("entry %d executed but was not recorded as completed: %#v", index, entry)
		}
		if entry.result.ToolCallID != calls[index].ID {
			t.Errorf("entry %d is keyed to %q, want %q", index, entry.result.ToolCallID, calls[index].ID)
		}
	}
}

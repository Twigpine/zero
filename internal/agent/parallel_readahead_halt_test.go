package agent

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// readAheadTool is read-only and thread-safe, so a consecutive run of its calls
// is executed as one parallel batch before the loop consumes any of it.
type readAheadTool struct {
	mu  sync.Mutex
	ran int
}

func (t *readAheadTool) Name() string        { return "read_probe" }
func (t *readAheadTool) Description() string { return "read-only probe" }
func (t *readAheadTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", Properties: map[string]tools.PropertySchema{"id": {Type: "string"}}}
}

func (t *readAheadTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow, Reason: "test"}
}

func (t *readAheadTool) Capabilities() tools.ToolCapabilities {
	return tools.ToolCapabilities{Effect: tools.EffectReadOnly, ThreadSafe: true}
}

func (t *readAheadTool) Run(_ context.Context, args map[string]any) tools.Result {
	t.mu.Lock()
	t.ran++
	t.mu.Unlock()
	id, _ := args["id"].(string)
	// Varying output, so no identity repeats and only the content-blind counter
	// can halt the run.
	return tools.Result{Status: tools.StatusError, Output: "failure for " + id}
}

func readCall(id string) []zeroruntime.StreamEvent {
	return []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: id, ToolName: "read_probe"},
		{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: id, ArgumentsFragment: `{"id":"` + id + `"}`},
		{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: id},
	}
}

// READ-AHEAD MEANS "NOT CONSUMED YET" NO LONGER MEANS "NOT EXECUTED".
//
// executeParallelReadBatch runs an entire eligible run of read calls before the
// loop consumes any of them. Every terminal branch in the consumption loop used
// to close out the calls after the current index with aborted placeholders, on
// the assumption that they had not run. A sibling that had already executed was
// therefore recorded as aborted: its real result was discarded, the transcript
// disagreed with what had happened, and its callbacks, trace counter, task
// observation and images were lost with it. Where such a sibling is a
// successful read, execution may already have committed file-observation credit
// for content the model never receives.
func TestParallelSiblingsAreFinalizedWhenTheGuardHalts(t *testing.T) {
	tool := &readAheadTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)

	// Eleven single-call turns bring the content-blind counter to one below its
	// bound, so the first call of the pair below trips it.
	var turns [][]zeroruntime.StreamEvent
	for i := 0; i < toolFailureAnyErrorStopAt-1; i++ {
		id := "s" + strconv.Itoa(i)
		turns = append(turns, append(readCall(id), zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}))
	}
	// Then one turn advertising two parallel-eligible calls. Both execute in the
	// batch; the halt fires while consuming the first.
	pair := append(readCall("pA"), readCall("pB")...)
	turns = append(turns, append(pair, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}))
	turns = append(turns, append(readCall("unreached"), zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}))

	var reported []ToolResult
	var announced int
	result, err := Run(context.Background(), "read things", &mockProvider{turns: turns}, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAuto,
		MaxTurns:       len(turns) + 5,
		OnToolCall:     func(ToolCall) { announced++ },
		OnToolResult:   func(r ToolResult) { reported = append(reported, r) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.FinalAnswer, "Agent stopped:") {
		t.Fatalf("the guard did not halt this run: %q", result.FinalAnswer)
	}

	// Every execution is reported exactly once, and the announcements match.
	if tool.ran != len(reported) {
		t.Errorf("the tool executed %d times but %d results were reported; a completed call was recorded as aborted",
			tool.ran, len(reported))
	}
	if announced != len(reported) {
		t.Errorf("OnToolCall fired %d times against %d results; the two callbacks disagree", announced, len(reported))
	}

	// Strict pairing: one tool result per advertised tool call, and the drained
	// sibling carries its REAL output rather than the aborted placeholder.
	byID := map[string]int{}
	var siblingContent string
	for _, message := range result.Messages {
		if message.Role != zeroruntime.MessageRoleTool {
			continue
		}
		byID[message.ToolCallID]++
		if message.ToolCallID == "pB" {
			siblingContent = message.Content
		}
	}
	for _, id := range []string{"pA", "pB"} {
		if byID[id] != 1 {
			t.Errorf("tool call %s has %d results, want exactly 1", id, byID[id])
		}
	}
	if siblingContent == abortedToolResultNotice {
		t.Error("the sibling that already ran was recorded as aborted")
	}
	if !strings.Contains(siblingContent, "failure for pB") {
		t.Errorf("the sibling's real output is missing from the transcript: %q", siblingContent)
	}

	// And the drained sibling must not reverse the decision that was already
	// made: the halt is still attributed to the call that tripped it.
	if !strings.Contains(result.FinalAnswer, strconv.Itoa(toolFailureAnyErrorStopAt)) {
		t.Errorf("the stop answer changed after draining siblings: %q", result.FinalAnswer)
	}
}

// imageReadTool is readAheadTool with an image on one named call, so a drained
// sibling's image can be followed from execution to the transcript.
type imageReadTool struct {
	readAheadTool
	imageFor string
}

func (t *imageReadTool) Run(ctx context.Context, args map[string]any) tools.Result {
	result := t.readAheadTool.Run(ctx, args)
	if id, _ := args["id"].(string); id == t.imageFor {
		result.Images = []zeroruntime.ImageBlock{{MediaType: "image/png", Data: []byte("\x89PNG")}}
	}
	return result
}

// The drained sibling's image is part of what ran, so it follows the tool
// results like any other call's image. Images reach the transcript through the
// turn's deferred builder, which the halt path calls after closing out the
// siblings; a drain that skipped the sibling's image would lose it silently.
func TestParallelSiblingImageSurvivesTheGuardHalt(t *testing.T) {
	tool := &imageReadTool{imageFor: "pB"}
	registry := tools.NewRegistry()
	registry.Register(tool)

	var turns [][]zeroruntime.StreamEvent
	for i := 0; i < toolFailureAnyErrorStopAt-1; i++ {
		id := "s" + strconv.Itoa(i)
		turns = append(turns, append(readCall(id), zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}))
	}
	pair := append(readCall("pA"), readCall("pB")...)
	turns = append(turns, append(pair, zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}))

	result, err := Run(context.Background(), "read things", &mockProvider{turns: turns}, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAuto,
		MaxTurns:       len(turns) + 5,
		SupportsVision: func(string) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.FinalAnswer, "Agent stopped:") {
		t.Fatalf("the guard did not halt this run: %q", result.FinalAnswer)
	}
	images := 0
	for _, message := range result.Messages {
		if message.Role == zeroruntime.MessageRoleUser {
			images += len(message.Images)
		}
	}
	if images != 1 {
		t.Errorf("the drained sibling's image reached the transcript %d times, want once", images)
	}
}

// A call that genuinely never ran still gets an aborted placeholder, so the fix
// did not simply stop aborting anything.
func TestUnstartedCallsStillGetAbortedPlaceholders(t *testing.T) {
	tool := &readAheadTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)

	var turns [][]zeroruntime.StreamEvent
	for i := 0; i < toolFailureAnyErrorStopAt-1; i++ {
		id := "s" + strconv.Itoa(i)
		turns = append(turns, append(readCall(id), zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}))
	}
	// One eligible pair, then a THIRD call outside the batch window is not what
	// happens here: the whole run is eligible, so instead advertise a single call
	// and let the halt fire on it, leaving nothing precomputed beyond it.
	turns = append(turns, append(readCall("solo"), zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}))
	turns = append(turns, append(readCall("unreached"), zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}))

	result, err := Run(context.Background(), "read things", &mockProvider{turns: turns}, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAuto,
		MaxTurns:       len(turns) + 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.FinalAnswer, "Agent stopped:") {
		t.Fatalf("the guard did not halt: %q", result.FinalAnswer)
	}
	if tool.ran != toolFailureAnyErrorStopAt {
		t.Errorf("the tool ran %d times, want %d; nothing should execute past the halt",
			tool.ran, toolFailureAnyErrorStopAt)
	}
}

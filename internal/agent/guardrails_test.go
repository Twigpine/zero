package agent

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// emptyTurn is a stream that produces no visible text and no tool calls.
func emptyTurn() []zeroruntime.StreamEvent {
	return []zeroruntime.StreamEvent{{Type: zeroruntime.StreamEventDone}}
}

// textTurn produces a turn with visible assistant text.
func textTurn(content string) []zeroruntime.StreamEvent {
	return []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: content},
		{Type: zeroruntime.StreamEventDone},
	}
}

// reasoningTurn produces live reasoning without visible assistant text.
func reasoningTurn(content string) []zeroruntime.StreamEvent {
	return []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventReasoning, Content: content},
		{Type: zeroruntime.StreamEventDone},
	}
}

// toolTurn produces a turn that calls a named tool with the given args JSON.
func toolTurn(callID string, toolName string, args string) []zeroruntime.StreamEvent {
	return []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: callID, ToolName: toolName},
		{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: callID, ArgumentsFragment: args},
		{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: callID},
		{Type: zeroruntime.StreamEventDone},
	}
}

func countUserMessagesContaining(messages []zeroruntime.Message, needle string) int {
	count := 0
	for _, message := range messages {
		if message.Role == zeroruntime.MessageRoleUser && strings.Contains(message.Content, needle) {
			count++
		}
	}
	return count
}

func TestRunStopsAfterConsecutiveEmptyTurns(t *testing.T) {
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			emptyTurn(),
			emptyTurn(),
			emptyTurn(),
			// A 4th turn exists but must never be requested.
			textTurn("should never reach here"),
		},
	}

	result, err := Run(context.Background(), "go", provider, Options{
		Registry: tools.NewRegistry(),
		MaxTurns: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != maxEmptyTurns {
		t.Fatalf("expected exactly %d turns before the no-output guard fires, got %d", maxEmptyTurns, len(provider.requests))
	}
	if result.Turns != maxEmptyTurns {
		t.Fatalf("expected %d turns recorded, got %d", maxEmptyTurns, result.Turns)
	}
	if !strings.Contains(result.FinalAnswer, "no output") {
		t.Fatalf("expected no-output stop message, got %q", result.FinalAnswer)
	}
	if result.FinalAnswer == maxTurnsAnswer {
		t.Fatalf("no-output guard must stop before reaching maxTurns, got max-turns answer")
	}
}

func TestRunResetsEmptyTurnCounterOnVisibleOutput(t *testing.T) {
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			emptyTurn(),
			emptyTurn(),
			textTurn("here is real progress"), // resets the counter and is the final answer
			emptyTurn(),
		},
	}

	result, err := Run(context.Background(), "go", provider, Options{
		Registry: tools.NewRegistry(),
		MaxTurns: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The text turn ends the run as the final answer (no tool calls), so we
	// stop at turn 3 — the empty counter was reset and never reached the cap.
	if len(provider.requests) != 3 {
		t.Fatalf("expected the run to end on the text turn (3 requests), got %d", len(provider.requests))
	}
	if result.FinalAnswer != "here is real progress" {
		t.Fatalf("expected the visible text as final answer, got %q", result.FinalAnswer)
	}
}

func TestRunResetsEmptyTurnCounterOnReasoning(t *testing.T) {
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			reasoningTurn("thinking 1"),
			reasoningTurn("thinking 2"),
			reasoningTurn("thinking 3"),
			textTurn("done"),
		},
	}

	result, err := Run(context.Background(), "go", provider, Options{
		Registry: tools.NewRegistry(),
		MaxTurns: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected reasoning-only turns to keep the run live until final answer, got %q", result.FinalAnswer)
	}
	if len(provider.requests) != 4 {
		t.Fatalf("expected 4 turns, got %d", len(provider.requests))
	}
}

func TestRunResetsEmptyTurnCounterOnToolCall(t *testing.T) {
	root := t.TempDir()
	writeAgentTestFile(t, root+"/notes.txt", "alpha")
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))

	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			emptyTurn(),
			emptyTurn(),
			toolTurn("call-1", "read_file", `{"path":"notes.txt"}`), // resets counter
			emptyTurn(),
			emptyTurn(),
			textTurn("done"),
		},
	}

	result, err := Run(context.Background(), "go", provider, Options{
		Registry: registry,
		MaxTurns: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Without a reset, three empty turns would stop the run at turn 3. Because
	// the tool call at turn 3 resets the counter, the run survives the later
	// empty turns and ends with the text answer at turn 6.
	if result.FinalAnswer != "done" {
		t.Fatalf("expected the counter to reset on a tool call and the run to finish, got %q", result.FinalAnswer)
	}
	if len(provider.requests) != 6 {
		t.Fatalf("expected 6 turns, got %d", len(provider.requests))
	}
}

// The whole point of #702's fix: the unknown-session error's normalized
// signature must not vary with the session id, so a model probing ids 1, 2,
// 3, … produces one repeated signature the failure guard can count. If the id
// leaked into the first 80 normalized chars, each probe would reset the streak
// and the halt would never fire.
func TestUnknownExecSessionErrorSignatureIsIDInvariant(t *testing.T) {
	a := errorSignature(tools.UnknownExecSessionError(1))
	b := errorSignature(tools.UnknownExecSessionError(999999))
	if a != b {
		t.Fatalf("unknown-session signature varies with id:\n  %q\n  %q", a, b)
	}
}

// End to end: with an id-invariant signature, probing a different unknown id
// each turn now trips the repeated-failure halt at toolFailureStopAt, where
// before the fix it never would.
func TestUnknownExecSessionProbingTripsFailureHalt(t *testing.T) {
	var state guardState
	var stoppedAt int
	for i := 1; i <= toolFailureStopAt; i++ {
		out := state.observeToolResult(tools.WriteStdinToolName, true, true, tools.UnknownExecSessionError(i), "")
		if out.Stop {
			stoppedAt = i
			break
		}
	}
	if stoppedAt != toolFailureStopAt {
		t.Fatalf("probing distinct unknown ids stopped at %d, want %d", stoppedAt, toolFailureStopAt)
	}
}

// A permission denial repeats forever when the streak is keyed on the error
// TEXT, because the denial message embeds the path or command that varies per
// call.
//
// Observed, not theorised: a headless run made 384 denied calls over 26 minutes
// without tripping a halt that stops at 6. Every denial carried the same typed
// category and a different reason string, so errorSignature differed each time
// and the record was rebuilt at count 1 on every call.
//
// TestUnknownExecSessionErrorSignatureIsIDInvariant fixed one message this way.
// Keying on the category fixes the class, without needing every future denial
// message to remember to be invariant.
func TestPermissionDenialStreakSurvivesVaryingReasonText(t *testing.T) {
	var state guardState
	stoppedAt := 0
	for i := 1; i <= toolFailureStopAt; i++ {
		// The shape the loop actually produces: same tool, same category, a
		// different path every time.
		output := "Error: Permission denied for write_file: cannot write " +
			filepath.Join("C:", "ws", "pkg", "file"+strconv.Itoa(i)+".go")
		out := state.observeToolResult("write_file", true, true, output, DenialPermissionDenied)
		if out.Stop {
			stoppedAt = i
			break
		}
	}
	if stoppedAt != toolFailureStopAt {
		t.Fatalf("denials with varying reason text stopped at %d, want %d", stoppedAt, toolFailureStopAt)
	}
}

// The content-blind bound. A tool failing over and over with genuinely
// DIFFERENT errors and no denial category is still a tool that is not working,
// and the same-signature streak can never see it.
//
// Bounded well above toolFailureStopAt on purpose: a model iterating on a
// tricky edit legitimately fails a few times with different errors while it
// converges, and cutting that short is the regression
// TestSuccessResetsBothFailureCounters guards.
func TestToolFailingWithDifferentErrorsEveryTimeStillStops(t *testing.T) {
	var state guardState
	stoppedAt := 0
	for i := 1; i <= toolFailureAnyErrorStopAt; i++ {
		out := state.observeToolResult("bash", true, true, "distinct failure "+strconv.Itoa(i), "")
		if out.Stop {
			stoppedAt = i
			break
		}
	}
	if stoppedAt != toolFailureAnyErrorStopAt {
		t.Fatalf("a tool failing with a new error each call stopped at %d, want %d", stoppedAt, toolFailureAnyErrorStopAt)
	}
}

// Neither counter may outlive a success, or a long run that fails occasionally
// and recovers would eventually halt for no reason. This is the property that
// keeps the content-blind bound safe to add.
func TestSuccessResetsBothFailureCounters(t *testing.T) {
	var state guardState
	for i := 1; i < toolFailureAnyErrorStopAt; i++ {
		if out := state.observeToolResult("bash", true, true, "distinct failure "+strconv.Itoa(i), ""); out.Stop {
			t.Fatalf("stopped at %d before the success that should reset it", i)
		}
	}
	state.observeToolResult("bash", false, false, "ok", "")

	// Same again from zero. Reaching the bound a second time proves the counter
	// restarted rather than merely paused.
	stoppedAt := 0
	for i := 1; i <= toolFailureAnyErrorStopAt; i++ {
		if out := state.observeToolResult("bash", true, true, "later failure "+strconv.Itoa(i), ""); out.Stop {
			stoppedAt = i
			break
		}
	}
	if stoppedAt != toolFailureAnyErrorStopAt {
		t.Fatalf("after a success the tool stopped at %d, want a full fresh %d", stoppedAt, toolFailureAnyErrorStopAt)
	}
}

// Records are keyed per tool, so ANOTHER tool succeeding in between must not
// clear the failing tool's streak.
//
// This is the realistic shape of the run that motivated the fix: the model kept
// making progress elsewhere — reading files, updating its plan — while one tool
// was refused over and over. A reset keyed on "something succeeded" rather than
// "this tool succeeded" would make the halt unreachable in exactly the runs that
// need it.
func TestAnotherToolSucceedingDoesNotClearAFailingToolsStreak(t *testing.T) {
	var state guardState
	stoppedAt := 0
	for i := 1; i <= toolFailureStopAt; i++ {
		state.observeToolResult("read_file", false, false, "ok", "")
		out := state.observeToolResult("write_file", true, false,
			"Error: Permission denied for write_file: "+strconv.Itoa(i), DenialPermissionDenied)
		if out.Stop {
			stoppedAt = i
			break
		}
	}
	if stoppedAt != toolFailureStopAt {
		t.Fatalf("denials interleaved with another tool's successes stopped at %d, want %d", stoppedAt, toolFailureStopAt)
	}
}

// alwaysPromptingTool is never allowed to run: it exists so a Run-level test can
// drive real permission denials through the loop.
type alwaysPromptingTool struct{ ran int }

func (tool *alwaysPromptingTool) Name() string        { return "bash" }
func (tool *alwaysPromptingTool) Description() string { return "test shell tool" }
func (tool *alwaysPromptingTool) Parameters() tools.Schema {
	return tools.Schema{
		Type:                 "object",
		Properties:           map[string]tools.PropertySchema{"command": {Type: "string"}},
		Required:             []string{"command"},
		AdditionalProperties: false,
	}
}
func (tool *alwaysPromptingTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectShell, Permission: tools.PermissionPrompt, Reason: "runs shell commands"}
}
func (tool *alwaysPromptingTool) Run(context.Context, map[string]any) tools.Result {
	tool.ran++
	return tools.Result{Status: tools.StatusOK, Output: "should never run"}
}

// The regression for the gap this whole change existed to close, driven through
// Run rather than through the guard helper.
//
// The first version of this fix was a no-op in production and every one of its
// unit tests passed, because they called observeToolResult directly with
// failed=true. The loop passes isRetriableToolError, which returns false for a
// categorized denial, so the guard took its success branch and deleted the
// record before it could key on the category. A denied tool still looped to the
// turn limit.
//
// This drives the real path: a tool that always prompts, an approver that always
// denies, and a different command each turn so the denial REASON — and therefore
// the error text — varies exactly as it does in a real run.
func TestRunStopsARepeatedlyDeniedToolAtTheFailureBound(t *testing.T) {
	tool := &alwaysPromptingTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)

	// Comfortably more turns than the bound, so reaching the bound is what stops
	// the run rather than exhausting the provider or MaxTurns.
	turns := make([][]zeroruntime.StreamEvent, 0, toolFailureStopAt+4)
	for i := range toolFailureStopAt + 4 {
		turns = append(turns, toolTurn("call-"+strconv.Itoa(i), "bash",
			`{"command":"touch /etc/file`+strconv.Itoa(i)+`"}`))
	}
	provider := &mockProvider{turns: turns}

	denials := 0
	result, err := Run(context.Background(), "do the thing", provider, Options{
		Registry:       registry,
		PermissionMode: PermissionModeAsk,
		MaxTurns:       len(turns) + 5,
		OnPermissionRequest: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			denials++
			// The varying half. In production this is the refused path or command;
			// here it is the command, which lands in the denial message the guard
			// used to key on.
			return PermissionDecision{
				Action: PermissionDecisionDeny,
				Reason: "refused " + request.ToolName + " call " + strconv.Itoa(denials),
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if denials != toolFailureStopAt {
		t.Errorf("the run made %d denied calls, want it halted at %d", denials, toolFailureStopAt)
	}
	if tool.ran != 0 {
		t.Errorf("the denied tool executed %d times; the denial must precede execution", tool.ran)
	}
	want := toolFailureStopAnswer("bash", toolFailureStopAt, toolFailureCauseSameRefusal)
	if result.FinalAnswer != want {
		t.Errorf("final answer =\n  %q\nwant\n  %q", result.FinalAnswer, want)
	}
}

// The stop message must describe the bound that actually tripped. When the
// content-blind counter is what halts the run, the same-signature streak is
// usually 1, and reporting that would tell the user a tool failed once after it
// failed a dozen different ways.
func TestVariedFailureStopAnswerReportsTheRightCounter(t *testing.T) {
	var state guardState
	var outcome toolFailureOutcome
	for i := 1; i <= toolFailureAnyErrorStopAt; i++ {
		outcome = state.observeToolResult("bash", true, true, "distinct failure "+strconv.Itoa(i), "")
		if outcome.Stop {
			break
		}
	}
	if !outcome.Stop {
		t.Fatal("never stopped")
	}
	if outcome.Cause != toolFailureCauseVariedExecuted {
		t.Errorf("Cause = %v for a stop driven by the content-blind counter over executed failures", outcome.Cause)
	}
	if outcome.Count != toolFailureAnyErrorStopAt {
		t.Errorf("Count = %d, want the counter that tripped (%d)", outcome.Count, toolFailureAnyErrorStopAt)
	}
	answer := toolFailureStopAnswer("bash", outcome.Count, outcome.Cause)
	if !strings.Contains(answer, "varying errors") {
		t.Errorf("stop answer describes the wrong cause: %q", answer)
	}
	if strings.Contains(answer, "with the same error") {
		t.Errorf("stop answer claims a same-error loop after distinct failures: %q", answer)
	}
}

func TestGuardStateResetsToolOnlyStreakOnEmptyNonToolTurn(t *testing.T) {
	var state guardState
	toolOnly := zeroruntime.CollectedStream{
		ToolCalls: []zeroruntime.ToolCall{{ID: "call", Name: "read_file", Arguments: `{}`}},
	}

	for range toolOnlyProgressReminderAt - 1 {
		state.observeTurn(toolOnly)
	}
	state.observeTurn(zeroruntime.CollectedStream{})
	state.observeTurn(toolOnly)

	if reminder := state.progressReminder(); reminder != "" {
		t.Fatalf("expected empty non-tool turn to reset tool-only progress reminder, got %q", reminder)
	}
	if state.toolOnlyTurns != 1 {
		t.Fatalf("expected tool-only streak to restart at 1, got %d", state.toolOnlyTurns)
	}
}

func TestRunDoesNotCountDroppedToolCallTurnsAsEmpty(t *testing.T) {
	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			{
				{Type: zeroruntime.StreamEventToolCallDropped},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventToolCallDropped},
				{Type: zeroruntime.StreamEventDone},
			},
			{
				{Type: zeroruntime.StreamEventToolCallDropped},
				{Type: zeroruntime.StreamEventDone},
			},
			textTurn("recovered"),
		},
	}

	result, err := Run(context.Background(), "go", provider, Options{
		Registry: tools.NewRegistry(),
		MaxTurns: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Dropped-call turns take the retry path and must NOT be counted by the
	// no-output guard; the run continues to the text turn.
	if result.FinalAnswer != "recovered" {
		t.Fatalf("expected dropped-call turns to be handled by the retry path, got %q", result.FinalAnswer)
	}
	if len(provider.requests) != 4 {
		t.Fatalf("expected 4 turns, got %d", len(provider.requests))
	}
}

func TestRunInjectsPlanNotCalledReminderForMultiStepTask(t *testing.T) {
	const expectedThreshold = 7
	if planReminderToolThreshold != expectedThreshold {
		t.Fatalf("plan reminder tool threshold = %d, want %d", planReminderToolThreshold, expectedThreshold)
	}
	root := t.TempDir()
	writeAgentTestFile(t, root+"/notes.txt", "alpha")
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))

	provider := &mockProvider{turns: append(repeatedReadTurns(expectedThreshold), textTurn("done"))}

	result, err := Run(context.Background(), "go", provider, Options{
		Registry: registry,
		MaxTurns: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	count := countUserMessagesContaining(result.Messages, planNotCalledReminderMarker)
	if count != 1 {
		t.Fatalf("expected exactly one not-called plan reminder, got %d", count)
	}
}

func TestRunDoesNotInjectPlanReminderForBoundedToolSequence(t *testing.T) {
	const expectedThreshold = 7
	root := t.TempDir()
	writeAgentTestFile(t, root+"/notes.txt", "alpha")
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))

	provider := &mockProvider{turns: append(repeatedReadTurns(expectedThreshold-1), textTurn("done"))}
	result, err := Run(context.Background(), "go", provider, Options{Registry: registry, MaxTurns: 12})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	if count := countUserMessagesContaining(result.Messages, planNotCalledReminderMarker); count != 0 {
		t.Fatalf("bounded tool sequence must not draw a plan reminder, got %d", count)
	}
}

func TestPlanReminderWaitsForTurnThresholdAfterEnoughToolCalls(t *testing.T) {
	state := guardState{totalToolCalls: 7}
	if got := state.planReminder(6); got != "" {
		t.Fatalf("planReminder(6) = %q, want no reminder before turn threshold", got)
	}
	if state.notCalledReminderSent {
		t.Fatal("early turn marked the not-called reminder as sent")
	}
}

func repeatedReadTurns(count int) [][]zeroruntime.StreamEvent {
	turns := make([][]zeroruntime.StreamEvent, 0, count)
	for range count {
		turns = append(turns, toolTurn("call", "read_file", `{"path":"notes.txt"}`))
	}
	return turns
}

func TestRunDoesNotInjectPlanReminderForTrivialTask(t *testing.T) {
	root := t.TempDir()
	writeAgentTestFile(t, root+"/notes.txt", "alpha")
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))

	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			toolTurn("call-1", "read_file", `{"path":"notes.txt"}`), // single tool call
			textTurn("done"), // immediately answers
		},
	}

	result, err := Run(context.Background(), "go", provider, Options{
		Registry: registry,
		MaxTurns: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	if count := countUserMessagesContaining(result.Messages, planNotCalledReminderMarker); count != 0 {
		t.Fatalf("expected no plan reminder for a trivial task, got %d", count)
	}
}

func TestRunDoesNotInjectNotCalledReminderWhenPlanUsed(t *testing.T) {
	root := t.TempDir()
	writeAgentTestFile(t, root+"/notes.txt", "alpha")
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))
	registry.Register(tools.NewUpdatePlanTool())

	provider := &mockProvider{
		turns: [][]zeroruntime.StreamEvent{
			toolTurn("call-1", "update_plan", `{"plan":[{"content":"step one"}]}`),
			toolTurn("call-2", "read_file", `{"path":"notes.txt"}`),
			textTurn("done"),
		},
	}

	result, err := Run(context.Background(), "go", provider, Options{
		Registry: registry,
		MaxTurns: 12,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	if count := countUserMessagesContaining(result.Messages, planNotCalledReminderMarker); count != 0 {
		t.Fatalf("expected no not-called reminder when update_plan was used, got %d", count)
	}
}

func TestRunInjectsStalePlanReminderAfterManyToolCalls(t *testing.T) {
	root := t.TempDir()
	writeAgentTestFile(t, root+"/notes.txt", "alpha")
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))
	registry.Register(tools.NewUpdatePlanTool())

	// Turn 1 calls update_plan (so the not-called reminder never triggers), then
	// many read_file turns accumulate without another plan update.
	turns := [][]zeroruntime.StreamEvent{
		toolTurn("plan-1", "update_plan", `{"plan":[{"content":"step one"}]}`),
	}
	for i := 0; i < staleToolCallThreshold+2; i++ {
		turns = append(turns, toolTurn("call", "read_file", `{"path":"notes.txt"}`))
	}
	turns = append(turns, textTurn("done"))

	provider := &mockProvider{turns: turns}

	result, err := Run(context.Background(), "go", provider, Options{
		Registry: registry,
		MaxTurns: len(turns) + 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	if count := countUserMessagesContaining(result.Messages, planStaleReminderMarker); count < 1 {
		t.Fatalf("expected at least one stale plan reminder, got %d", count)
	}
}

func TestRunStalePlanReminderIsOneShotPerInterval(t *testing.T) {
	root := t.TempDir()
	writeAgentTestFile(t, root+"/notes.txt", "alpha")
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))
	registry.Register(tools.NewUpdatePlanTool())

	turns := [][]zeroruntime.StreamEvent{
		toolTurn("plan-1", "update_plan", `{"plan":[{"content":"step one"}]}`),
	}
	// Enough tool calls to exceed the threshold by a wide margin; the reminder
	// must fire once for the interval, not on every subsequent turn.
	for i := 0; i < staleToolCallThreshold*2; i++ {
		turns = append(turns, toolTurn("call", "read_file", `{"path":"notes.txt"}`))
	}
	turns = append(turns, textTurn("done"))

	provider := &mockProvider{turns: turns}

	result, err := Run(context.Background(), "go", provider, Options{
		Registry: registry,
		MaxTurns: len(turns) + 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	count := countUserMessagesContaining(result.Messages, planStaleReminderMarker)
	if count != 1 {
		t.Fatalf("expected the stale reminder to be one-shot per interval (exactly 1), got %d", count)
	}
}

func TestRunInjectsToolOnlyProgressReminder(t *testing.T) {
	root := t.TempDir()
	writeAgentTestFile(t, root+"/notes.txt", "alpha")
	registry := tools.NewRegistry()
	registry.Register(tools.NewScopedReadFileTool(root, nil))

	turns := make([][]zeroruntime.StreamEvent, 0, toolOnlyProgressReminderAt+1)
	for i := 0; i < toolOnlyProgressReminderAt; i++ {
		turns = append(turns, toolTurn("call", "read_file", `{"path":"notes.txt"}`))
	}
	turns = append(turns, textTurn("done"))

	provider := &mockProvider{turns: turns}
	result, err := Run(context.Background(), "go", provider, Options{
		Registry: registry,
		MaxTurns: len(turns) + 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("expected final answer, got %q", result.FinalAnswer)
	}
	if count := countUserMessagesContaining(result.Messages, toolOnlyProgressReminderMarker); count != 1 {
		t.Fatalf("expected one tool-only progress reminder, got %d", count)
	}
	found := false
	for _, message := range provider.requests[toolOnlyProgressReminderAt].Messages {
		if message.Role == zeroruntime.MessageRoleUser && strings.Contains(message.Content, toolOnlyProgressReminderMarker) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected reminder on request after tool-only streak, messages: %+v", provider.requests[toolOnlyProgressReminderAt].Messages)
	}
}

type alwaysFailingTool struct{}

func (alwaysFailingTool) Name() string        { return "flaky" }
func (alwaysFailingTool) Description() string { return "always fails for testing" }
func (alwaysFailingTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", AdditionalProperties: false}
}
func (alwaysFailingTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectRead, Permission: tools.PermissionAllow}
}
func (alwaysFailingTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusError, Output: "Error: Invalid arguments for flaky: thing is required"}
}

func repeatedFlakyTurns(n int) [][]zeroruntime.StreamEvent {
	turn := []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "c", ToolName: "flaky"},
		{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "c"},
		{Type: zeroruntime.StreamEventDone},
	}
	turns := make([][]zeroruntime.StreamEvent, 0, n)
	for i := 0; i < n; i++ {
		turns = append(turns, turn)
	}
	return turns
}

func TestRunStopsAfterRepeatedToolFailures(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(alwaysFailingTool{})
	provider := &mockProvider{turns: repeatedFlakyTurns(10)}

	result, err := Run(context.Background(), "go", provider, Options{Registry: registry, MaxTurns: 12})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.FinalAnswer, "flaky") || !strings.Contains(result.FinalAnswer, "failed") {
		t.Fatalf("expected repeated-failure stop answer, got %q", result.FinalAnswer)
	}
	// Must halt at the failure cap, NOT loop to maxTurns.
	if len(provider.requests) != toolFailureStopAt {
		t.Fatalf("expected stop at %d failures, made %d requests", toolFailureStopAt, len(provider.requests))
	}
}

func TestRunInjectsToolFailureHintWithSchema(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(alwaysFailingTool{})
	provider := &mockProvider{turns: repeatedFlakyTurns(10)}

	if _, err := Run(context.Background(), "go", provider, Options{Registry: registry, MaxTurns: 12}); err != nil {
		t.Fatal(err)
	}
	// After the 2nd failure a one-shot hint is injected, so the 3rd turn's request
	// carries it (with the tool schema).
	found := false
	for _, m := range provider.requests[2].Messages {
		if m.Role == zeroruntime.MessageRoleUser && strings.Contains(m.Content, toolFailureHintMarker) {
			found = true
			if !strings.Contains(m.Content, "object") { // schema rendered
				t.Errorf("hint should include the tool schema, got %q", m.Content)
			}
		}
	}
	if !found {
		t.Fatalf("expected a tool-failure hint on the 3rd turn, messages: %+v", provider.requests[2].Messages)
	}
}

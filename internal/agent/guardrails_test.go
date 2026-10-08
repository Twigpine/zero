package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/Gitlawb/zero/internal/tools"
	"github.com/Gitlawb/zero/internal/zeroruntime"
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
		out := state.observeToolResult(tools.WriteStdinToolName, true, tools.UnknownExecSessionError(i))
		if out.Stop {
			stoppedAt = i
			break
		}
	}
	if stoppedAt != toolFailureStopAt {
		t.Fatalf("probing distinct unknown ids stopped at %d, want %d", stoppedAt, toolFailureStopAt)
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

package agent

import (
	"context"
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

// An absence-establishing sentence is a finding; the same sentence that also
// says the work is blocked is an admission. Observation allowances such as
// "reproduce" and "confirm any" can head both, so the tail prefix alone cannot
// separate them — measured against real phrasings, TEN of eleven genuine
// admissions passed the detector before blockedWorkMarkers existed.
func TestIncompletionAllowanceYieldsToBlockedWork(t *testing.T) {
	// Must FIRE: the inability is reported as leaving work undone.
	for _, admission := range []string{
		"I could not reproduce the crash, so the fix is unverified.",
		"I was unable to reproduce the reported behaviour and stopped there.",
		"I could not find the root cause; someone else will need to pick this up.",
		"I could not find the bug in the time available.",
		"I did not manage to reproduce the failure, so I cannot confirm the patch works.",
		"I could not locate the source of the regression and have run out of ideas.",
		"I could not find where to apply the change, so nothing was modified.",
	} {
		if selfReportedIncompletion(admission) == "" {
			t.Errorf("admission passed the detector: %q", admission)
		}
	}

	// Must NOT fire: establishing that something is absent is the job, and the
	// motivating case is the audit that spent 53 tool calls proving a negative.
	for _, finding := range []string{
		"I could not find any remaining issues.",
		"I could NOT find where AllowManifestToolAutoApproval is set to true in production code.",
		"I could not find the flag being set anywhere outside tests, so the concern does not apply.",
		"I could not reproduce the reported exploit, which confirms the guard holds.",
		"I could not observe any regression across the suite.",
		"I could not confirm any leak; every path is bounded.",
		// Next steps and ownership belong in successful reports too. Both of
		// these were flagged when blockedWorkMarkers could override an explicit
		// "any", which is the model asserting exhaustive absence.
		"I could not find any remaining issues, though a follow-up will need to cover the Windows path.",
		"I could not find any blockers; someone else can take the release from here.",
	} {
		if reason := selfReportedIncompletion(finding); reason != "" {
			t.Errorf("finding wrongly flagged as incomplete: %q -> %q", finding, reason)
		}
	}

	// A strong-absence phrase still yields when its consequence explicitly says
	// the work is unresolved.
	if selfReportedIncompletion("I could not observe any effect, so the change may be inert.") == "" {
		t.Error("an unresolved strong-absence report passed the detector")
	}
}

func TestToolCaveatDoesNotHideGuessingOrFabrication(t *testing.T) {
	for _, admission := range []string{
		"I don't have a write tool available in this specialist context, so I guessed the line numbers.",
		"No update_plan tool is available, so I fabricated the plan section.",
	} {
		if selfReportedIncompletion(admission) == "" {
			t.Errorf("tool caveat hid a high-signal admission: %q", admission)
		}
	}
}

func TestToolCaveatAllowsCompletedObjectiveLanguage(t *testing.T) {
	for _, completed := range []string{
		"I don't have a write tool available in this specialist context, and the objective is complete.",
		"I don't have a write tool available in this specialist context, but I completed the analysis as requested.",
		"I don't have an update_plan tool available in this specialist context; here is what was asked for.",
	} {
		if reason := selfReportedIncompletion(completed); reason != "" {
			t.Errorf("completed tool caveat was flagged: %q -> %q", completed, reason)
		}
	}

	for _, admission := range []string{
		"I could not do what was asked because no write tool is available.",
		"I am unable to complete what was asked with the tools available.",
	} {
		if selfReportedIncompletion(admission) == "" {
			t.Errorf("verb-anchored objective failure passed the detector: %q", admission)
		}
	}
}

// An admission with NO first-person subject must still fire. The subjectless
// "unable to " stem was once deleted to silence a completed audit's section
// heading, which also lost every impersonal admission — none of these names "i"
// or "we", so no other stem sees them.
func TestImpersonalAdmissionsAreStillCaught(t *testing.T) {
	for _, admission := range []string{
		"Unable to complete the task; the build never succeeded.",
		"The agent was unable to finish the migration.",
		"Unable to verify the fix, so the change is unverified.",
	} {
		if selfReportedIncompletion(admission) == "" {
			t.Errorf("an impersonal admission passed the detector: %q", admission)
		}
	}
}

// The heading that motivated deleting the stem must stay exempt. It is a label
// counting a bucket of findings in a COMPLETED audit, not a claim about the
// objective — which is why it is recognised by shape rather than by removing a
// stem that catches real admissions.
func TestACountedHeadingIsNotAnAdmission(t *testing.T) {
	for _, heading := range []string{
		"**Unable to verify (1):** - MCP #3 claim was truncated",
		"Unable to reproduce (3):",
	} {
		if reason := selfReportedIncompletion(heading); reason != "" {
			t.Errorf("a counted heading was read as an admission: %q -> %q", heading, reason)
		}
	}
	// But the same opening WITHOUT a count is a real admission.
	if selfReportedIncompletion("Unable to verify the deployment; it never started.") == "" {
		t.Error("a subjectless admission with no count was exempted as a heading")
	}
}

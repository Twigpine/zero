package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
	"github.com/Twigpine/zero/internal/trace"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestTaskStateReplayIsDeterministic(t *testing.T) {
	events := []taskStateEvent{
		{kind: taskStateEventPlan, arguments: `{"plan":[{"content":"inspect","status":"done"},{"content":"implement","status":"in progress"},{"content":"verify"}]}`},
		{kind: taskStateEventToolResult, toolResult: ToolResult{Name: "apply_patch", Status: tools.StatusOK, ChangedFiles: []string{"a.go", "a.go", "b.go"}}},
		{kind: taskStateEventToolResult, toolResult: ToolResult{Name: "go test", Status: tools.StatusError}},
		{kind: taskStateEventVerification, verification: OutcomePassed},
		{kind: taskStateEventCompletion, completion: completionEvaluation{Decision: CompletionUncertain, Reason: "pending work"}},
	}

	first := newTaskState("ship the change", nil)
	second := newTaskState("ship the change", nil)
	for _, event := range events {
		first.observe(event)
		second.observe(event)
	}

	if !reflect.DeepEqual(first.snapshot(), second.snapshot()) {
		t.Fatalf("replaying the same events produced different snapshots:\nfirst:  %#v\nsecond: %#v", first.snapshot(), second.snapshot())
	}
	got := first.snapshot()
	if got.Status != taskStatusActive || got.Plan.Pending != 1 || got.Plan.InProgress != 1 || got.Plan.Completed != 1 {
		t.Fatalf("unexpected plan snapshot: %#v", got)
	}
	if got.Tools.Succeeded != 1 || got.Tools.Failed != 1 || len(got.ChangedFiles) != 2 {
		t.Fatalf("unexpected evidence snapshot: %#v", got)
	}
	if got.Verification.Passed != 1 || got.Verification.Failed != 0 || got.Verification.LastOutcome != OutcomePassed {
		t.Fatalf("unexpected verification snapshot: %#v", got.Verification)
	}
}

func TestTaskStateCoalescesToolResultsIntoNextTraceEvent(t *testing.T) {
	recorder := trace.NewRecorder("session", "run", "")
	state := newTaskState("objective", recorder)
	for range 20 {
		state.observe(taskStateEvent{kind: taskStateEventToolResult, toolResult: ToolResult{Status: tools.StatusOK}})
	}
	if events := recorder.Finish().TaskStates; len(events) != 0 {
		t.Fatalf("tool results should be coalesced, got %d trace events", len(events))
	}

	recorder = trace.NewRecorder("session", "run-2", "")
	state = newTaskState("objective", recorder)
	for range 20 {
		state.observe(taskStateEvent{kind: taskStateEventToolResult, toolResult: ToolResult{Status: tools.StatusOK}})
	}
	state.observe(taskStateEvent{kind: taskStateEventCompletion, completion: completionEvaluation{Decision: CompletionComplete}})
	events := recorder.Finish().TaskStates
	if len(events) != 1 || events[0].ToolsSucceeded != 20 {
		t.Fatalf("coalesced tool total missing from completion snapshot: %#v", events)
	}
}

func TestRunEmitsTaskStateFromExistingLoopEvents(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(tools.NewUpdatePlanTool())
	provider := &mockProvider{turns: [][]zeroruntime.StreamEvent{
		{
			{Type: zeroruntime.StreamEventToolCallStart, ToolCallID: "plan-1", ToolName: planToolName},
			{Type: zeroruntime.StreamEventToolCallDelta, ToolCallID: "plan-1", ArgumentsFragment: `{"plan":[{"content":"implement","status":"completed"}]}`},
			{Type: zeroruntime.StreamEventToolCallEnd, ToolCallID: "plan-1"},
			{Type: zeroruntime.StreamEventDone},
		},
		{
			{Type: zeroruntime.StreamEventText, Content: "done"},
			{Type: zeroruntime.StreamEventDone},
		},
	}}
	recorder := trace.NewRecorder("session", "run", "")

	result, err := Run(context.Background(), "implement the change", provider, Options{
		Registry: registry,
		Trace:    recorder,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.FinalAnswer != "done" {
		t.Fatalf("final answer = %q, want done", result.FinalAnswer)
	}
	events := recorder.Finish().TaskStates
	if len(events) < 3 {
		t.Fatalf("expected plan, tool, completion, and parity snapshots, got %#v", events)
	}
	last := events[len(events)-1]
	if last.Status != string(taskStatusComplete) || last.PlanCompleted != 1 || last.ToolsSucceeded != 1 || last.PlanParity != string(taskPlanParityMatch) {
		t.Fatalf("unexpected final task snapshot: %#v", last)
	}
}

func TestTaskStateSnapshotIsImmutable(t *testing.T) {
	state := newTaskState("objective", nil)
	state.observe(taskStateEvent{kind: taskStateEventPlan, arguments: `{"plan":[{"content":"one","status":"pending"}]}`})
	state.observe(taskStateEvent{kind: taskStateEventToolResult, toolResult: ToolResult{Status: tools.StatusOK, ChangedFiles: []string{"one.go"}}})

	snapshot := state.snapshot()
	snapshot.Plan.Items[0].Content = "mutated"
	snapshot.ChangedFiles[0] = "mutated.go"

	fresh := state.snapshot()
	if fresh.Plan.Items[0].Content != "one" || fresh.ChangedFiles[0] != "one.go" {
		t.Fatalf("snapshot mutation leaked into task state: %#v", fresh)
	}
}

func TestTaskStateRecordsDurableRuntimeEvidence(t *testing.T) {
	state := newTaskState("Please keep the change focused.\nNever commit generated reports.", nil)
	arguments := `{"cmd":"go test ./internal/agent"}`
	state.observe(taskStateEvent{kind: taskStateEventToolResult, arguments: arguments, toolResult: ToolResult{
		ToolCallID: "test-1", Name: "exec_command", Status: tools.StatusError,
		Output: "Error: TestResumeRetainsState failed\nfull diagnostic",
		Meta:   map[string]string{"spill_path": ".zero/artifacts/test-1.txt"},
	}})
	state.observe(taskStateEvent{kind: taskStateEventPermission, permission: PermissionEvent{
		ToolName: "exec_command", DecisionAction: PermissionDecisionAllowForSession,
		Scope: "/tmp", DecisionReason: "write a temporary fixture",
	}})

	snapshot := state.snapshot()
	if !reflect.DeepEqual(snapshot.Constraints, []string{"Please keep the change focused.", "Never commit generated reports."}) {
		t.Fatalf("unexpected explicit constraints: %#v", snapshot.Constraints)
	}
	if len(snapshot.UnresolvedFailures) != 1 || snapshot.UnresolvedFailures[0].Command != "go test ./internal/agent" || snapshot.UnresolvedFailures[0].Summary != "Error: TestResumeRetainsState failed" {
		t.Fatalf("unexpected failure evidence: %#v", snapshot.UnresolvedFailures)
	}
	if len(snapshot.Artifacts) != 1 || snapshot.Artifacts[0].Path != ".zero/artifacts/test-1.txt" {
		t.Fatalf("unexpected artifact evidence: %#v", snapshot.Artifacts)
	}
	if len(snapshot.Approvals) != 1 || snapshot.Approvals[0].Decision != PermissionDecisionAllowForSession || snapshot.Approvals[0].Scope != "/tmp" {
		t.Fatalf("unexpected approval evidence: %#v", snapshot.Approvals)
	}
	state.observe(taskStateEvent{kind: taskStateEventToolResult, arguments: `{"path":"a.go"}`, toolResult: ToolResult{
		ToolCallID: "read-a", Name: "read_file", Status: tools.StatusError, Output: "Error: a.go is unavailable",
	}})
	state.observe(taskStateEvent{kind: taskStateEventToolResult, arguments: `{"path":"b.go"}`, toolResult: ToolResult{
		ToolCallID: "read-b", Name: "read_file", Status: tools.StatusOK, Output: "package b",
	}})
	if failures := state.snapshot().UnresolvedFailures; len(failures) != 2 || failures[1].Summary != "Error: a.go is unavailable" {
		t.Fatalf("success for a different target resolved the wrong failure: %#v", failures)
	}

	state.observe(taskStateEvent{kind: taskStateEventToolResult, arguments: arguments, toolResult: ToolResult{
		ToolCallID: "test-2", Name: "exec_command", Status: tools.StatusOK, Output: "ok",
	}})
	if failures := state.snapshot().UnresolvedFailures; len(failures) != 1 || failures[0].Summary != "Error: a.go is unavailable" {
		t.Fatalf("successful retry did not resolve only its matching failure: %#v", failures)
	}
	state.observe(taskStateEvent{kind: taskStateEventToolResult, arguments: `{"path":"a.go"}`, toolResult: ToolResult{
		ToolCallID: "read-a-2", Name: "read_file", Status: tools.StatusOK, Output: "package a",
	}})
	if failures := state.snapshot().UnresolvedFailures; len(failures) != 0 {
		t.Fatalf("successful non-command retry did not resolve its matching failure: %#v", failures)
	}
	state.observe(taskStateEvent{kind: taskStateEventToolResult, arguments: `{"path":"a.go"}`, toolResult: ToolResult{
		ToolCallID: "read-a-3", Name: "read_file", Status: tools.StatusError, Output: "Error: a.go failed again",
	}})
	snapshot = state.snapshot()
	if len(snapshot.UnresolvedFailures) != 1 || snapshot.UnresolvedFailures[0].Summary != "Error: a.go failed again" {
		t.Fatalf("a failure recurring after success was hidden: %#v", snapshot.UnresolvedFailures)
	}
	for _, key := range snapshot.ResolvedFailureKeys {
		if key == snapshot.UnresolvedFailures[0].Key {
			t.Fatalf("recurring failure retained a stale resolved key: %#v", snapshot.ResolvedFailureKeys)
		}
	}
}

func TestTaskStatePlanParityUsesLatestPlan(t *testing.T) {
	state := newTaskState("objective", nil)
	state.observe(taskStateEvent{kind: taskStateEventPlan, arguments: `{"plan":[{"content":"old","status":"completed"}]}`})
	state.observe(taskStateEvent{kind: taskStateEventPlan, arguments: `{"plan":[{"content":"new","status":"in_progress"}]}`})

	messages := []zeroruntime.Message{{Role: zeroruntime.MessageRoleAssistant, ToolCalls: []zeroruntime.ToolCall{
		{Name: planToolName, Arguments: `{"plan":[{"content":"new","status":"in_progress"}]}`},
	}}}
	if parity := state.observePlanParity(messages); parity != taskPlanParityMatch {
		t.Fatalf("parity = %q, want match", parity)
	}

	messages[0].ToolCalls[0].Arguments = `{"plan":[{"content":"different","status":"pending"}]}`
	if parity := state.observePlanParity(messages); parity != taskPlanParityMismatch {
		t.Fatalf("parity = %q, want mismatch", parity)
	}
}

func TestTaskStateMatchesPlanToolNormalization(t *testing.T) {
	state := newTaskState("objective", nil)
	arguments := `{"plan":[{"content":"first","status":"in_progress"},{"content":"second","status":"in_progress"}]}`
	state.observe(taskStateEvent{kind: taskStateEventPlan, arguments: arguments})

	snapshot := state.snapshot()
	if snapshot.Plan.Completed != 1 || snapshot.Plan.InProgress != 1 {
		t.Fatalf("multiple active items were not normalized like the plan tool: %#v", snapshot.Plan)
	}
	messages := []zeroruntime.Message{{Role: zeroruntime.MessageRoleAssistant, ToolCalls: []zeroruntime.ToolCall{{Name: planToolName, Arguments: arguments}}}}
	if parity := state.observePlanParity(messages); parity != taskPlanParityMatch {
		t.Fatalf("normalized state should still match its transcript event, got %q", parity)
	}

	empty := newTaskState("objective", nil)
	empty.observe(taskStateEvent{kind: taskStateEventPlan, arguments: `{"plan":[]}`})
	emptyMessages := []zeroruntime.Message{{Role: zeroruntime.MessageRoleAssistant, ToolCalls: []zeroruntime.ToolCall{{Name: planToolName, Arguments: `{"plan":[]}`}}}}
	if parity := empty.observePlanParity(emptyMessages); parity != taskPlanParityMatch {
		t.Fatalf("explicit empty plan should match transcript, got %q", parity)
	}
}

func TestTaskStateContextFallsBackWhenTranscriptDiffers(t *testing.T) {
	state := newTaskState("objective", nil)
	state.observe(taskStateEvent{kind: taskStateEventPlan, arguments: `{"plan":[{"content":"tracked","status":"completed"}]}`})
	messages := []zeroruntime.Message{{Role: zeroruntime.MessageRoleAssistant, ToolCalls: []zeroruntime.ToolCall{
		{Name: planToolName, Arguments: `{"plan":[{"content":"transcript","status":"pending"}]}`},
	}}}

	context := state.completionContext(messages, true)
	if !context.PlanPending || context.PlanMatchesTranscript {
		t.Fatalf("completion context must retain transcript truth on mismatch: %#v", context)
	}
	if context.Objective != "objective" {
		t.Fatalf("objective lost from completion context: %#v", context)
	}
}

func TestTaskStateCompactionSnapshotRetainsObjectiveOnPlanMismatch(t *testing.T) {
	state := newTaskState("objective", nil)
	state.observe(taskStateEvent{kind: taskStateEventPlan, arguments: `{"plan":[{"content":"verify","status":"pending"}]}`})
	matching := []zeroruntime.Message{{Role: zeroruntime.MessageRoleAssistant, ToolCalls: []zeroruntime.ToolCall{
		{Name: planToolName, Arguments: `{"plan":[{"content":"verify","status":"pending"}]}`},
	}}}
	if snapshot := state.snapshotForCompaction(matching); snapshot == nil || snapshot.Objective != "objective" {
		t.Fatalf("matching transcript should produce compact state, got %#v", snapshot)
	}

	mismatching := append([]zeroruntime.Message(nil), matching...)
	mismatching[0].ToolCalls = []zeroruntime.ToolCall{{Name: planToolName, Arguments: `{"plan":[{"content":"other","status":"pending"}]}`}}
	if snapshot := state.snapshotForCompaction(mismatching); snapshot == nil || snapshot.Objective != "objective" || snapshot.PlanParity != taskPlanParityMismatch {
		t.Fatalf("plan mismatch must retain immutable objective and mark mutable state uncorroborated, got %#v", snapshot)
	}
}

func TestParseTaskPlanRejectsArgumentsTheToolWouldReject(t *testing.T) {
	for _, arguments := range []string{
		`{}`,
		`{"plan":null}`,
		`{"plan":[{"step":"alias is not accepted"}]}`,
		`{"plan":[{"content":""}]}`,
		`{"plan":[{"content":"valid"},{"content":""}]}`,
		`{"plan":[{"id":4,"content":"valid"}]}`,
	} {
		if plan, ok := parseTaskPlan(arguments); ok {
			t.Fatalf("parseTaskPlan(%s) = %#v, true; want rejected", arguments, plan)
		}
	}
}

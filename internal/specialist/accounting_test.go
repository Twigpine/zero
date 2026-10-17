package specialist

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/background"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

func TestExecutorRecordsForegroundLifecycleAndUsageRollup(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	parent, err := store.Create(sessions.CreateInput{SessionID: "parent_session"})
	if err != nil {
		t.Fatalf("Create parent returned error: %v", err)
	}
	zero := 0
	var hookEvents []string
	var hookNames []string
	executor := Executor{
		BinaryPath:   "/usr/local/bin/zero",
		SessionStore: store,
		NewSessionID: func() (string, error) { return "child_task", nil },
		LifecycleHooks: &LifecycleHooks{
			Dispatch: func(_ context.Context, event string, specialistName string, payload map[string]any) {
				hookEvents = append(hookEvents, event)
				hookNames = append(hookNames, specialistName)
				if payload["event"] != event {
					t.Errorf("payload event = %#v, want %q", payload["event"], event)
				}
			},
		},
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "worker", Description: "Does focused work"},
				SystemPrompt:  "Work carefully.",
				ResolvedTools: []string{"read_file"},
			}}}, nil
		},
		RunChild: func(context.Context, string, []string, func(streamjson.Event)) (ChildRunResult, error) {
			return ChildRunResult{
				Events: []streamjson.Event{
					{Type: streamjson.EventRunStart, RunID: "run_1", SessionID: "child_task"},
					{Type: streamjson.EventUsage, RunID: "run_1", PromptTokens: ptrInt(12), CompletionTokens: ptrInt(5), TotalTokens: ptrInt(17)},
					{Type: streamjson.EventFinal, RunID: "run_1", Text: "done"},
					{Type: streamjson.EventRunEnd, RunID: "run_1", Status: "success", ExitCode: &zero},
				},
				ExitCode: 0,
			}, nil
		},
	}

	result, err := executor.Run(context.Background(), TaskParameters{
		Name:        "worker",
		Prompt:      "inspect auth",
		Description: "Auth check",
	}, TaskRunOptions{
		ParentSessionID: parent.SessionID,
		ToolCallID:      "call_1",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Result.Status != tools.StatusOK {
		t.Fatalf("Run status = %s, output=%s", result.Result.Status, result.Result.Output)
	}
	if got, want := strings.Join(hookEvents, ","), "specialistStart,specialistStop"; got != want {
		t.Fatalf("lifecycle hooks = %q, want %q", got, want)
	}
	if got, want := strings.Join(hookNames, ","), "worker,worker"; got != want {
		t.Fatalf("lifecycle specialist names = %q, want %q", got, want)
	}

	events, err := store.ReadEvents(parent.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if got, want := eventTypes(events), []sessions.EventType{sessions.EventSpecialistStart, sessions.EventUsage, sessions.EventSpecialistStop, specialistStopHookEvent}; strings.Join(eventTypesString(got), ",") != strings.Join(eventTypesString(want), ",") {
		t.Fatalf("event types = %#v, want %#v", got, want)
	}
	startPayload := eventPayload(t, events[0])
	requirePayloadString(t, startPayload, "source", "specialist")
	requirePayloadString(t, startPayload, "childSessionId", "child_task")
	requirePayloadString(t, startPayload, "specialist", "worker")
	requirePayloadString(t, startPayload, "toolCallId", "call_1")
	requirePayloadString(t, startPayload, "mode", "foreground")
	requirePayloadBool(t, startPayload, "background", false)

	usagePayload := eventPayload(t, events[1])
	requirePayloadString(t, usagePayload, "source", "specialist")
	requirePayloadString(t, usagePayload, "childSessionId", "child_task")
	requirePayloadString(t, usagePayload, "runId", "run_1")
	requirePayloadInt(t, usagePayload, "promptTokens", 12)
	requirePayloadInt(t, usagePayload, "completionTokens", 5)
	requirePayloadInt(t, usagePayload, "totalTokens", 17)
	requirePayloadInt(t, usagePayload, "usageEvents", 1)

	stopPayload := eventPayload(t, events[2])
	requirePayloadString(t, stopPayload, "status", "success")
	requirePayloadInt(t, stopPayload, "exitCode", 0)
	requirePayloadBool(t, stopPayload, "usageRolledUp", true)
	requirePayloadBool(t, stopPayload, "childStarted", true)

	claimPayload := eventPayload(t, events[3])
	requirePayloadString(t, claimPayload, "hook", "specialistStop")
	requirePayloadString(t, claimPayload, "runId", "run_1")
	requirePayloadBool(t, claimPayload, "dispatched", true)
	requirePayloadBool(t, claimPayload, "childStarted", true)
}

func TestExecutorRecordsStartedChildErrorExitCode(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	parent, err := store.Create(sessions.CreateInput{SessionID: "parent_session"})
	if err != nil {
		t.Fatalf("Create parent returned error: %v", err)
	}
	executor := Executor{
		BinaryPath:   "/usr/local/bin/zero",
		SessionStore: store,
		NewSessionID: func() (string, error) { return "child_task", nil },
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "worker", Description: "Does focused work"},
				SystemPrompt:  "Work carefully.",
				ResolvedTools: []string{"read_file"},
			}}}, nil
		},
		RunChild: func(context.Context, string, []string, func(streamjson.Event)) (ChildRunResult, error) {
			return ChildRunResult{ExitCode: 7, Started: true}, os.ErrPermission
		},
	}

	_, err = executor.Run(context.Background(), TaskParameters{
		Name:   "worker",
		Prompt: "inspect auth",
	}, TaskRunOptions{ParentSessionID: parent.SessionID})
	if err == nil {
		t.Fatal("Run returned nil error")
	}

	events, err := store.ReadEvents(parent.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	stopEvents := eventsOfType(events, sessions.EventSpecialistStop)
	if len(stopEvents) != 1 {
		t.Fatalf("stop events = %#v", events)
	}
	stopPayload := eventPayload(t, stopEvents[0])
	requirePayloadString(t, stopPayload, "status", "error")
	requirePayloadInt(t, stopPayload, "exitCode", 7)
}

func TestOutputToolRollsUpCompletedBackgroundUsageOnce(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	parent, err := store.Create(sessions.CreateInput{SessionID: "parent_session"})
	if err != nil {
		t.Fatalf("Create parent returned error: %v", err)
	}
	manager, err := background.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	outputFile, err := manager.Register(background.RegisterInput{
		TaskID:         "child_task",
		Type:           "specialist",
		SpecialistName: "worker",
		Description:    "Auth check",
		ParentID:       parent.SessionID,
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if err := os.WriteFile(outputFile, []byte(strings.Join([]string{
		`{"schemaVersion":2,"type":"run_start","runId":"run_1","sessionId":"child_task"}`,
		`{"schemaVersion":2,"type":"usage","runId":"run_1","promptTokens":20,"completionTokens":8,"totalTokens":28}`,
		`{"schemaVersion":2,"type":"final","runId":"run_1","text":"done"}`,
		`{"schemaVersion":2,"type":"run_end","runId":"run_1","status":"success","exitCode":0}`,
		"",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := manager.UpdateStatus("child_task", background.StatusCompleted, 0); err != nil {
		t.Fatalf("UpdateStatus returned error: %v", err)
	}
	tool := NewOutputTool(manager)
	tool.SessionStore = store

	first := tool.Run(context.Background(), map[string]any{"task_id": "child_task"})
	second := tool.Run(context.Background(), map[string]any{"task_id": "child_task"})

	if first.Status != tools.StatusOK || second.Status != tools.StatusOK {
		t.Fatalf("TaskOutput results = %#v %#v", first, second)
	}
	events, err := store.ReadEvents(parent.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if usageEvents := eventsOfType(events, sessions.EventUsage); len(usageEvents) != 1 {
		t.Fatalf("usage events = %#v", events)
	}
	if stopEvents := eventsOfType(events, sessions.EventSpecialistStop); len(stopEvents) != 1 {
		t.Fatalf("stop events = %#v", events)
	}
	usagePayload := eventPayload(t, eventsOfType(events, sessions.EventUsage)[0])
	requirePayloadString(t, usagePayload, "mode", "background")
	requirePayloadInt(t, usagePayload, "promptTokens", 20)
	requirePayloadInt(t, usagePayload, "completionTokens", 8)
	requirePayloadInt(t, usagePayload, "totalTokens", 28)
}

func ptrInt(value int) *int {
	return &value
}

func eventTypes(events []sessions.Event) []sessions.EventType {
	types := make([]sessions.EventType, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

func eventTypesString(types []sessions.EventType) []string {
	values := make([]string, 0, len(types))
	for _, eventType := range types {
		values = append(values, string(eventType))
	}
	return values
}

func eventsOfType(events []sessions.Event, eventType sessions.EventType) []sessions.Event {
	matches := []sessions.Event{}
	for _, event := range events {
		if event.Type == eventType {
			matches = append(matches, event)
		}
	}
	return matches
}

func eventPayload(t *testing.T, event sessions.Event) map[string]any {
	t.Helper()
	payload := map[string]any{}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload returned error: %v", err)
	}
	return payload
}

func requirePayloadString(t *testing.T, payload map[string]any, key string, want string) {
	t.Helper()
	value, ok := payload[key]
	if !ok {
		t.Fatalf("payload missing %q: %#v", key, payload)
	}
	got, ok := value.(string)
	if !ok {
		t.Fatalf("payload %q = %#v, want string %q", key, value, want)
	}
	if got != want {
		t.Fatalf("payload %q = %q, want %q", key, got, want)
	}
}

func requirePayloadBool(t *testing.T, payload map[string]any, key string, want bool) {
	t.Helper()
	value, ok := payload[key]
	if !ok {
		t.Fatalf("payload missing %q: %#v", key, payload)
	}
	got, ok := value.(bool)
	if !ok {
		t.Fatalf("payload %q = %#v, want bool %v", key, value, want)
	}
	if got != want {
		t.Fatalf("payload %q = %v, want %v", key, got, want)
	}
}

func requirePayloadInt(t *testing.T, payload map[string]any, key string, want int) {
	t.Helper()
	value, ok := payload[key]
	if !ok {
		t.Fatalf("payload missing %q: %#v", key, payload)
	}
	got := 0
	switch typed := value.(type) {
	case int:
		got = typed
	case float64:
		got = int(typed)
	default:
		t.Fatalf("payload %q = %#v, want int %d", key, value, want)
	}
	if got != want {
		t.Fatalf("payload %q = %d, want %d", key, got, want)
	}
}

func TestRecordSpecialistStopDedupesUnderConcurrency(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	parent, err := store.Create(sessions.CreateInput{SessionID: "parent_session"})
	if err != nil {
		t.Fatalf("Create parent returned error: %v", err)
	}
	var hookStops atomic.Int32
	executor := Executor{
		SessionStore: store,
		LifecycleHooks: &LifecycleHooks{
			Dispatch: func(_ context.Context, event string, _ string, _ map[string]any) {
				if event == "specialistStop" {
					hookStops.Add(1)
				}
			},
		},
	}
	input := specialistAccountingInput{
		ParentSessionID: parent.SessionID,
		ChildSessionID:  "child_task",
		SpecialistName:  "worker",
		Mode:            "background",
		Background:      true,
	}
	summary := StreamResult{RunID: "run_1"}

	// Many finishers race the same (child, run) stop. Exactly one event must land;
	// run under -race to also catch the check-then-append data race.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			executor.recordSpecialistStop(input, summary, "success", 0, nil, true)
		}()
	}
	wg.Wait()

	events, err := store.ReadEvents(parent.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	stops := 0
	for _, event := range events {
		if event.Type == sessions.EventSpecialistStop {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("expected exactly 1 stop event under concurrency, got %d", stops)
	}
	if got := hookStops.Load(); got != 1 {
		t.Fatalf("specialistStop hooks = %d, want 1 under concurrency", got)
	}
}

func TestRecordSpecialistStopFiresWithoutParentSession(t *testing.T) {
	var hookStops atomic.Int32
	executor := Executor{
		LifecycleHooks: &LifecycleHooks{
			Dispatch: func(_ context.Context, event string, name string, _ map[string]any) {
				if event == "specialistStop" {
					hookStops.Add(1)
					if name != "subagent" {
						t.Errorf("specialist name = %q, want subagent", name)
					}
				}
			},
		},
	}
	executor.recordSpecialistStop(specialistAccountingInput{
		// Swarm members omit ParentSessionID; append never succeeds.
		ChildSessionID: "member_child",
		SpecialistName: "subagent",
		Mode:           "foreground",
	}, StreamResult{RunID: "run_swarm"}, "success", 0, nil, false)

	if got := hookStops.Load(); got != 1 {
		t.Fatalf("specialistStop hooks = %d, want 1 with empty parent", got)
	}
	// Second observation of the same child/run must not re-fire.
	executor.recordSpecialistStop(specialistAccountingInput{
		ChildSessionID: "member_child",
		SpecialistName: "subagent",
		Mode:           "foreground",
	}, StreamResult{RunID: "run_swarm"}, "success", 0, nil, false)
	if got := hookStops.Load(); got != 1 {
		t.Fatalf("specialistStop hooks = %d after duplicate, want 1", got)
	}
}

func TestRecordSpecialistStopHookLessWinnerDoesNotSuppressLaterHooks(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	parent, err := store.Create(sessions.CreateInput{SessionID: "parent_session"})
	if err != nil {
		t.Fatalf("Create parent returned error: %v", err)
	}
	input := specialistAccountingInput{
		ParentSessionID: parent.SessionID,
		ChildSessionID:  "child_task",
		SpecialistName:  "worker",
		Mode:            "background",
		Background:      true,
	}
	summary := StreamResult{RunID: "run_1"}

	// Hook-less TaskOutput path wins the session append race.
	Executor{SessionStore: store}.recordSpecialistStop(input, summary, "success", 0, nil, true)

	var hookStops atomic.Int32
	hooks := &LifecycleHooks{
		Dispatch: func(_ context.Context, event string, _ string, _ map[string]any) {
			if event == "specialistStop" {
				hookStops.Add(1)
			}
		},
	}
	Executor{SessionStore: store, LifecycleHooks: hooks}.recordSpecialistStop(input, summary, "success", 0, nil, true)
	if got := hookStops.Load(); got != 1 {
		t.Fatalf("specialistStop hooks = %d, want 1 after hook-less winner", got)
	}
}

// A durable stop-hook claim must survive a process boundary: a later Zero
// process (fresh LifecycleHooks bridge over the same session store) reaping the
// same task must not re-run the hook even though its session-event append is
// rejected as a duplicate.
func TestRecordSpecialistStopHookClaimDurableAcrossBridges(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	parent, err := store.Create(sessions.CreateInput{SessionID: "parent_session"})
	if err != nil {
		t.Fatalf("Create parent returned error: %v", err)
	}
	input := specialistAccountingInput{
		ParentSessionID: parent.SessionID,
		ChildSessionID:  "child_task",
		SpecialistName:  "worker",
		Mode:            "background",
		Background:      true,
	}
	summary := StreamResult{RunID: "run_1"}

	var firstBridge atomic.Int32
	Executor{SessionStore: store, LifecycleHooks: stopCounterHooks(&firstBridge)}.recordSpecialistStop(input, summary, "success", 0, nil, true)
	if got := firstBridge.Load(); got != 1 {
		t.Fatalf("first bridge stop hooks = %d, want 1", got)
	}

	var secondBridge atomic.Int32
	Executor{SessionStore: store, LifecycleHooks: stopCounterHooks(&secondBridge)}.recordSpecialistStop(input, summary, "success", 0, nil, true)
	if got := secondBridge.Load(); got != 0 {
		t.Fatalf("second bridge stop hooks = %d, want 0: the durable claim must suppress a re-dispatch", got)
	}

	events, err := store.ReadEvents(parent.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if claims := eventsOfType(events, specialistStopHookEvent); len(claims) != 1 {
		t.Fatalf("stop-hook claim events = %d, want 1", len(claims))
	}
	if stops := eventsOfType(events, sessions.EventSpecialistStop); len(stops) != 1 {
		t.Fatalf("stop events = %d, want 1", len(stops))
	}
}

// The durable claim must follow the accounting run-ID equivalence: an empty
// runId is a catch-all in both directions, while genuinely distinct runs stay
// distinct stop transitions.
func TestRecordSpecialistStopHookClaimRunIDEquivalence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		first  string
		second string
		want   int32
	}{
		{name: "empty run id is a catch-all for a later run", first: "", second: "run_1", want: 1},
		{name: "a recorded run covers a later empty query", first: "run_1", second: "", want: 1},
		{name: "distinct runs are distinct stops", first: "run_1", second: "run_2", want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
			parent, err := store.Create(sessions.CreateInput{SessionID: "parent_session"})
			if err != nil {
				t.Fatalf("Create parent returned error: %v", err)
			}
			var stops atomic.Int32
			executor := Executor{SessionStore: store, LifecycleHooks: stopCounterHooks(&stops)}
			input := specialistAccountingInput{
				ParentSessionID: parent.SessionID,
				ChildSessionID:  "child_task",
				SpecialistName:  "worker",
				Mode:            "background",
				Background:      true,
			}
			executor.recordSpecialistStop(input, StreamResult{RunID: tc.first}, "success", 0, nil, false)
			executor.recordSpecialistStop(input, StreamResult{RunID: tc.second}, "success", 0, nil, false)
			if got := stops.Load(); got != tc.want {
				t.Fatalf("stop hooks = %d, want %d", got, tc.want)
			}
		})
	}
}

// The in-process fallback (swarm members have no parent session) applies the
// same run-ID equivalence, so a later observation carrying a runId cannot
// re-fire a stop that was already claimed without one.
func TestRecordSpecialistStopInProcessClaimRunIDEquivalence(t *testing.T) {
	var stops atomic.Int32
	executor := Executor{LifecycleHooks: stopCounterHooks(&stops)}
	input := specialistAccountingInput{ChildSessionID: "member_child", SpecialistName: "subagent", Mode: "foreground"}
	executor.recordSpecialistStop(input, StreamResult{}, "success", 0, nil, false)
	executor.recordSpecialistStop(input, StreamResult{RunID: "run_later"}, "success", 0, nil, false)
	if got := stops.Load(); got != 1 {
		t.Fatalf("stop hooks = %d, want 1", got)
	}
}

// A child that never started must not publish public lifecycle hooks, while the
// internal attempt/stop accounting is retained and durably marks the stop as
// ineligible so a later reaper cannot fire the hook either.
func TestExecutorSkipsHooksWhenChildNeverStarts(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	parent, err := store.Create(sessions.CreateInput{SessionID: "parent_session"})
	if err != nil {
		t.Fatalf("Create parent returned error: %v", err)
	}
	var hooks []string
	executor := Executor{
		BinaryPath:   "/usr/local/bin/zero",
		SessionStore: store,
		NewSessionID: func() (string, error) { return "child_task", nil },
		LifecycleHooks: &LifecycleHooks{
			Dispatch: func(_ context.Context, event string, _ string, _ map[string]any) {
				hooks = append(hooks, event)
			},
		},
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "worker", Description: "Does focused work"},
				SystemPrompt:  "Work carefully.",
				ResolvedTools: []string{"read_file"},
			}}}, nil
		},
		RunChild: func(context.Context, string, []string, func(streamjson.Event)) (ChildRunResult, error) {
			return ChildRunResult{}, errors.New("exec: no such file or directory")
		},
	}

	if _, err := executor.Run(context.Background(), TaskParameters{Name: "worker", Prompt: "inspect auth"}, TaskRunOptions{ParentSessionID: parent.SessionID}); err == nil {
		t.Fatal("Run returned nil error for a failed child start")
	}
	if len(hooks) != 0 {
		t.Fatalf("public lifecycle hooks fired for a child that never started: %#v", hooks)
	}

	events, err := store.ReadEvents(parent.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if starts := eventsOfType(events, sessions.EventSpecialistStart); len(starts) != 1 {
		t.Fatalf("attempt start events = %d, want 1", len(starts))
	}
	stopEvents := eventsOfType(events, sessions.EventSpecialistStop)
	if len(stopEvents) != 1 {
		t.Fatalf("stop events = %#v", events)
	}
	requirePayloadBool(t, eventPayload(t, stopEvents[0]), "childStarted", false)

	claims := eventsOfType(events, specialistStopHookEvent)
	if len(claims) != 1 {
		t.Fatalf("stop-hook claim events = %d, want 1", len(claims))
	}
	requirePayloadBool(t, eventPayload(t, claims[0]), "dispatched", false)
}

// A never-started background child leaves an errored task behind; a later
// TaskOutput reaper must not fire the stop hook from the recorded eligibility.
func TestBackgroundLaunchFailureDoesNotFireStopHookOnReap(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	parent, err := store.Create(sessions.CreateInput{SessionID: "parent_session"})
	if err != nil {
		t.Fatalf("Create parent returned error: %v", err)
	}
	manager, err := background.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	var hooks []string
	executor := Executor{
		BinaryPath:        "/usr/local/bin/zero",
		SessionStore:      store,
		BackgroundManager: manager,
		NewSessionID:      func() (string, error) { return "child_task", nil },
		LifecycleHooks: &LifecycleHooks{
			Dispatch: func(_ context.Context, event string, _ string, _ map[string]any) {
				hooks = append(hooks, event)
			},
		},
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "worker", Description: "Does focused work"},
				SystemPrompt:  "Work carefully.",
				ResolvedTools: []string{"read_file"},
			}}}, nil
		},
		LaunchBackground: func(string, []string, string, func(int)) (int, error) {
			return 0, errors.New("launch failed")
		},
	}

	_, err = executor.Run(context.Background(), TaskParameters{Name: "worker", Prompt: "inspect auth", RunInBackground: true}, TaskRunOptions{ParentSessionID: parent.SessionID})
	if err == nil {
		t.Fatal("Run returned nil error for a failed background launch")
	}
	if len(hooks) != 0 {
		t.Fatalf("public lifecycle hooks fired for a child that never started: %#v", hooks)
	}

	// A separate process reaping the errored task must stay silent too.
	var reapHooks atomic.Int32
	tool := NewOutputTool(manager)
	tool.SessionStore = store
	tool.LifecycleHooks = stopCounterHooks(&reapHooks)
	if result := tool.Run(context.Background(), map[string]any{"task_id": "child_task"}); result.Status != tools.StatusOK {
		t.Fatalf("TaskOutput status = %s output=%s", result.Status, result.Output)
	}
	if got := reapHooks.Load(); got != 0 {
		t.Fatalf("stop hooks from reaping a never-started task = %d, want 0", got)
	}

	events, err := store.ReadEvents(parent.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if stopEvents := eventsOfType(events, sessions.EventSpecialistStop); len(stopEvents) != 1 {
		t.Fatalf("stop events = %d, want 1", len(stopEvents))
	}
	if claims := eventsOfType(events, specialistStopHookEvent); len(claims) != 1 {
		t.Fatalf("stop-hook claim events = %d, want 1", len(claims))
	}
}

// A background child that exits before the launcher returns must not run its
// terminal accounting - and therefore must not fire the specialistStop hook -
// until the start decision has completed. Otherwise a fast-exiting child
// publishes specialistStop before specialistStart, and the recorded stop can
// miss the pid that SetPID has not stored yet. The synchronous launcher below
// also pins the deadlock requirement: the exit callback may not wait inline,
// because onExit runs before the launcher (and the start decision) can return.
func TestRunBackgroundDefersExitUntilStartDecisionCompletes(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	parent, err := store.Create(sessions.CreateInput{SessionID: "parent_session"})
	if err != nil {
		t.Fatalf("Create parent returned error: %v", err)
	}
	manager, err := background.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	var hookMu sync.Mutex
	var hookEvents []string
	stopDispatched := make(chan struct{}, 1)
	executor := Executor{
		BinaryPath:        "/usr/local/bin/zero",
		SessionStore:      store,
		BackgroundManager: manager,
		NewSessionID:      func() (string, error) { return "child_task", nil },
		LifecycleHooks: &LifecycleHooks{
			Dispatch: func(_ context.Context, event string, _ string, _ map[string]any) {
				hookMu.Lock()
				hookEvents = append(hookEvents, event)
				hookMu.Unlock()
				if event == "specialistStop" {
					select {
					case stopDispatched <- struct{}{}:
					default:
					}
				}
			},
		},
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "worker", Description: "Does focused work"},
				SystemPrompt:  "Work carefully.",
				ResolvedTools: []string{"read_file"},
			}}}, nil
		},
		// The child exits before the launcher returns, so the exit callback runs
		// synchronously - exactly the ordering hazard this test guards.
		LaunchBackground: func(_ string, _ []string, _ string, onExit func(exitCode int)) (int, error) {
			onExit(0)
			return 1234, nil
		},
	}

	runErr := make(chan error, 1)
	go func() {
		_, err := executor.Run(context.Background(), TaskParameters{
			Name:            "worker",
			Prompt:          "inspect auth",
			RunInBackground: true,
		}, TaskRunOptions{ParentSessionID: parent.SessionID})
		runErr <- err
	}()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("synchronous launcher deadlocked the background start decision")
	}

	// Run publishes specialistStart before it returns; the deferred exit handling
	// publishes specialistStop afterwards.
	select {
	case <-stopDispatched:
	case <-time.After(10 * time.Second):
		t.Fatal("deferred exit accounting never dispatched specialistStop")
	}
	hookMu.Lock()
	gotHooks := strings.Join(hookEvents, ",")
	hookMu.Unlock()
	if gotHooks != "specialistStart,specialistStop" {
		t.Fatalf("lifecycle hooks = %q, want start before stop", gotHooks)
	}

	events, err := store.ReadEvents(parent.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	stopEvents := eventsOfType(events, sessions.EventSpecialistStop)
	if len(stopEvents) != 1 {
		t.Fatalf("stop events = %#v", events)
	}
	// The deferred stop must observe the task after SetPID; a stop recorded
	// before the start decision completes would omit the pid.
	requirePayloadInt(t, eventPayload(t, stopEvents[0]), "pid", 1234)
}

// A recovered task stopped explicitly (launcher process gone) must record the
// terminal transition and fire the stop hook exactly once through the shared
// durable claim, even when a later TaskOutput poll reaps it again.
func TestStopToolRecordsRecoveredTaskStopOnce(t *testing.T) {
	killed := []int{}
	manager, err := background.NewManagerWithOptions(background.ManagerOptions{
		RootDir: t.TempDir(),
		KillProcess: func(pid int) error {
			killed = append(killed, pid)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManagerWithOptions returned error: %v", err)
	}
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	parent, err := store.Create(sessions.CreateInput{SessionID: "parent_session"})
	if err != nil {
		t.Fatalf("Create parent returned error: %v", err)
	}
	outputFile, err := manager.Register(background.RegisterInput{
		TaskID:         "child_task",
		Type:           specialistTaskType,
		SpecialistName: "worker",
		Description:    "Auth check",
		ParentID:       parent.SessionID,
		PID:            4321,
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if err := os.WriteFile(outputFile, []byte(strings.Join([]string{
		`{"schemaVersion":2,"type":"run_start","runId":"run_1","sessionId":"child_task"}`,
		`{"schemaVersion":2,"type":"final","runId":"run_1","text":"done"}`,
		`{"schemaVersion":2,"type":"run_end","runId":"run_1","status":"success","exitCode":0}`,
		"",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	var counter atomic.Int32
	tool := &StopTool{manager: manager, SessionStore: store, LifecycleHooks: stopCounterHooks(&counter)}
	result := tool.Run(context.Background(), map[string]any{"task_id": "child_task"})
	if result.Status != tools.StatusOK || result.Meta["status"] != string(background.StatusKilled) {
		t.Fatalf("TaskStop result = %#v", result)
	}
	if !reflect.DeepEqual(killed, []int{4321}) {
		t.Fatalf("killed pids = %#v", killed)
	}
	if got := counter.Load(); got != 1 {
		t.Fatalf("stop hooks = %d, want 1 from TaskStop", got)
	}

	// A later TaskOutput reaper in another process (fresh bridge) must not
	// re-dispatch; the durable claim keys on the runId read from the output.
	var reapCounter atomic.Int32
	outputTool := NewOutputTool(manager)
	outputTool.SessionStore = store
	outputTool.LifecycleHooks = stopCounterHooks(&reapCounter)
	if result := outputTool.Run(context.Background(), map[string]any{"task_id": "child_task"}); result.Status != tools.StatusOK {
		t.Fatalf("TaskOutput status = %s output=%s", result.Status, result.Output)
	}
	if got := reapCounter.Load(); got != 0 {
		t.Fatalf("stop hooks after later reap = %d, want 0", got)
	}

	events, err := store.ReadEvents(parent.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if stopEvents := eventsOfType(events, sessions.EventSpecialistStop); len(stopEvents) != 1 {
		t.Fatalf("stop events = %d, want 1", len(stopEvents))
	}
	claims := eventsOfType(events, specialistStopHookEvent)
	if len(claims) != 1 {
		t.Fatalf("stop-hook claim events = %d, want 1", len(claims))
	}
	requirePayloadString(t, eventPayload(t, claims[0]), "runId", "run_1")
}

func stopCounterHooks(counter *atomic.Int32) *LifecycleHooks {
	return &LifecycleHooks{
		Dispatch: func(_ context.Context, event string, _ string, _ map[string]any) {
			if event == "specialistStop" {
				counter.Add(1)
			}
		},
	}
}

func TestOutputToolSharesLifecycleHooksWithStopAccounting(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	parent, err := store.Create(sessions.CreateInput{SessionID: "parent_session"})
	if err != nil {
		t.Fatalf("Create parent returned error: %v", err)
	}
	manager, err := background.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	outputFile, err := manager.Register(background.RegisterInput{
		TaskID:         "child_task",
		Type:           "specialist",
		SpecialistName: "worker",
		Description:    "Auth check",
		ParentID:       parent.SessionID,
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if err := os.WriteFile(outputFile, []byte(strings.Join([]string{
		`{"schemaVersion":2,"type":"run_start","runId":"run_1","sessionId":"child_task"}`,
		`{"schemaVersion":2,"type":"final","runId":"run_1","text":"done"}`,
		`{"schemaVersion":2,"type":"run_end","runId":"run_1","status":"success","exitCode":0}`,
		"",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := manager.UpdateStatus("child_task", background.StatusCompleted, 0); err != nil {
		t.Fatalf("UpdateStatus returned error: %v", err)
	}

	var hookStops atomic.Int32
	hooks := &LifecycleHooks{
		Dispatch: func(_ context.Context, event string, _ string, _ map[string]any) {
			if event == "specialistStop" {
				hookStops.Add(1)
			}
		},
	}
	tool := NewOutputTool(manager)
	tool.SessionStore = store
	tool.LifecycleHooks = hooks

	if result := tool.Run(context.Background(), map[string]any{"task_id": "child_task"}); result.Status != tools.StatusOK {
		t.Fatalf("TaskOutput status = %s output=%s", result.Status, result.Output)
	}
	if got := hookStops.Load(); got != 1 {
		t.Fatalf("specialistStop hooks = %d, want 1 from TaskOutput", got)
	}
	// Shared bridge: a later Task.onExit-style caller must not re-fire.
	Executor{SessionStore: store, LifecycleHooks: hooks}.recordBackgroundTaskAccounting(
		mustGetBackgroundTask(t, manager, "child_task"),
		StreamResult{RunID: "run_1"},
	)
	if got := hookStops.Load(); got != 1 {
		t.Fatalf("specialistStop hooks = %d after second path, want 1", got)
	}
}

func mustGetBackgroundTask(t *testing.T, manager *background.Manager, taskID string) background.Task {
	t.Helper()
	task, ok := manager.Get(taskID)
	if !ok {
		t.Fatalf("background task %q not found", taskID)
	}
	return task
}

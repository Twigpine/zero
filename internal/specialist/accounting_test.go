package specialist

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

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

	events, err := store.ReadEvents(parent.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	if got, want := eventTypes(events), []sessions.EventType{sessions.EventSpecialistStart, sessions.EventUsage, sessions.EventSpecialistStop}; strings.Join(eventTypesString(got), ",") != strings.Join(eventTypesString(want), ",") {
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
	executor := Executor{SessionStore: store}
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
}

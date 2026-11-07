package specialist

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/background"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

func TestRegisteredSpecialistToolsLifecycle(t *testing.T) {
	killedPIDs := []int{}
	manager, err := background.NewManagerWithOptions(background.ManagerOptions{
		RootDir: t.TempDir(),
		KillProcess: func(pid int) error {
			killedPIDs = append(killedPIDs, pid)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManagerWithOptions returned error: %v", err)
	}

	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	parent, err := store.Create(sessions.CreateInput{SessionID: "parent_session", Title: "Parent"})
	if err != nil {
		t.Fatalf("Create parent returned error: %v", err)
	}

	zero := 0
	manifest := Manifest{
		Metadata: Metadata{
			Name:        "worker",
			Description: "Does focused work",
			Tools:       []string{"read-only"},
		},
		SystemPrompt:  "Work carefully.",
		ResolvedTools: []string{"read_file"},
	}
	var backgroundArgs []string
	var resumeArgs []string
	executor := Executor{
		BinaryPath:        "/usr/local/bin/zero",
		SessionStore:      store,
		BackgroundManager: manager,
		NewSessionID:      func() (string, error) { return "child_task", nil },
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{manifest}}, nil
		},
		LaunchBackground: func(binaryPath string, args []string, outputFile string, onExit func(exitCode int)) (int, error) {
			if binaryPath != "/usr/local/bin/zero" {
				t.Fatalf("background binaryPath = %q", binaryPath)
			}
			backgroundArgs = append([]string(nil), args...)
			if _, err := store.Create(sessions.CreateInput{
				SessionID:       "child_task",
				SessionKind:     sessions.SessionKindChild,
				Tag:             SessionTagSpecialist,
				Depth:           1,
				ParentSessionID: parent.SessionID,
				AgentName:       "worker",
				TaskID:          "child_task",
			}); err != nil {
				return 0, err
			}
			return 9876, os.WriteFile(outputFile, []byte(strings.Join([]string{
				`{"schemaVersion":2,"type":"run_start","runId":"run_background","sessionId":"child_task"}`,
				`{"schemaVersion":2,"type":"tool_call","runId":"run_background","id":"call_read","name":"read_file"}`,
				`{"schemaVersion":2,"type":"text","runId":"run_background","delta":"background ready"}`,
				"",
			}, "\n")), 0o600)
		},
		RunChild: func(ctx context.Context, binaryPath string, args []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			if binaryPath != "/usr/local/bin/zero" {
				t.Fatalf("resume binaryPath = %q", binaryPath)
			}
			resumeArgs = append([]string(nil), args...)
			return ChildRunResult{
				Events: []streamjson.Event{
					{Type: streamjson.EventRunStart, RunID: "run_resume", SessionID: "child_task"},
					{Type: streamjson.EventUsage, RunID: "run_resume", PromptTokens: ptrInt(9), CompletionTokens: ptrInt(4), TotalTokens: ptrInt(13)},
					{Type: streamjson.EventFinal, RunID: "run_resume", Text: "resume finished"},
					{Type: streamjson.EventRunEnd, RunID: "run_resume", Status: "success", ExitCode: &zero},
				},
				ExitCode: 0,
			}, nil
		},
	}

	registry := tools.NewRegistry()
	runtime, err := RegisterTools(registry, executor)
	if err != nil {
		t.Fatalf("RegisterTools returned error: %v", err)
	}
	defer func() {
		if err := runtime.Close(); err != nil {
			t.Fatalf("runtime close returned error: %v", err)
		}
	}()

	launched := registry.RunWithOptions(context.Background(), "Task", map[string]any{
		"name":              "worker",
		"prompt":            "inspect auth",
		"description":       "Auth check",
		"run_in_background": true,
	}, tools.RunOptions{
		PermissionGranted: true,
		ToolCallID:        "call_task",
		SessionID:         parent.SessionID,
		Model:             "gpt-4.1",
		ReasoningEffort:   "medium",
		Cwd:               "/repo",
	})
	if launched.Status != tools.StatusOK {
		t.Fatalf("Task background status = %s, output=%s", launched.Status, launched.Output)
	}
	if launched.Meta["task_id"] != "child_task" || launched.Meta["session_id"] != "child_task" {
		t.Fatalf("Task background meta = %#v", launched.Meta)
	}
	for _, want := range []string{"Task launched in background.", `Use TaskOutput with task_id "child_task"`} {
		if !strings.Contains(launched.Output, want) {
			t.Fatalf("Task background output missing %q:\n%s", want, launched.Output)
		}
	}
	for _, want := range [][]string{
		{"exec", "--init-session-id", "child_task"},
		{"--output-format", "stream-json"},
		{"--enabled-tools", "read_file"},
		{"--depth", "1"},
		{"--tag", "specialist"},
		{"--calling-session-id", parent.SessionID},
		{"--calling-tool-use-id", "call_task"},
		{"--cwd", "/repo"},
	} {
		if !containsSequence(backgroundArgs, want) {
			t.Fatalf("background args missing %v: %#v", want, backgroundArgs)
		}
	}

	runningOutput := registry.Run(context.Background(), "TaskOutput", map[string]any{"task_id": "child_task"})
	if runningOutput.Status != tools.StatusOK {
		t.Fatalf("TaskOutput running status = %s, output=%s", runningOutput.Status, runningOutput.Output)
	}
	for _, want := range []string{"status: running", "output:\nbackground ready", "tools: read_file"} {
		if !strings.Contains(runningOutput.Output, want) {
			t.Fatalf("TaskOutput running output missing %q:\n%s", want, runningOutput.Output)
		}
	}

	stopped := registry.RunWithOptions(context.Background(), "TaskStop", map[string]any{"task_id": "child_task"}, tools.RunOptions{PermissionGranted: true})
	if stopped.Status != tools.StatusOK {
		t.Fatalf("TaskStop status = %s, output=%s", stopped.Status, stopped.Output)
	}
	if stopped.Meta["status"] != string(background.StatusKilled) {
		t.Fatalf("TaskStop meta = %#v", stopped.Meta)
	}
	if len(killedPIDs) != 1 || killedPIDs[0] != 9876 {
		t.Fatalf("killed PIDs = %#v", killedPIDs)
	}

	killedOutput := registry.Run(context.Background(), "TaskOutput", map[string]any{"task_id": "child_task"})
	if killedOutput.Status != tools.StatusOK {
		t.Fatalf("TaskOutput killed status = %s, output=%s", killedOutput.Status, killedOutput.Output)
	}
	for _, want := range []string{"status: killed", "exit_code: -1", "output:\nbackground ready"} {
		if !strings.Contains(killedOutput.Output, want) {
			t.Fatalf("TaskOutput killed output missing %q:\n%s", want, killedOutput.Output)
		}
	}

	resumed := registry.RunWithOptions(context.Background(), "Task", map[string]any{
		"resume": "child_task",
		"prompt": "continue with follow-up",
	}, tools.RunOptions{
		PermissionGranted: true,
		SessionID:         parent.SessionID,
		Depth:             1,
		Cwd:               "/repo",
	})
	if resumed.Status != tools.StatusOK {
		t.Fatalf("Task resume status = %s, output=%s", resumed.Status, resumed.Output)
	}
	if resumed.Meta["session_id"] != "child_task" || !strings.Contains(resumed.Output, "resume finished") {
		t.Fatalf("Task resume result = %#v", resumed)
	}
	for _, want := range [][]string{
		{"exec", "--resume", "child_task"},
		{"--output-format", "stream-json"},
		{"--enabled-tools", "read_file"},
		{"--depth", "2"},
		{"--tag", "specialist"},
		{"--cwd", "/repo"},
	} {
		if !containsSequence(resumeArgs, want) {
			t.Fatalf("resume args missing %v: %#v", want, resumeArgs)
		}
	}

	events, err := store.ReadEvents(parent.SessionID)
	if err != nil {
		t.Fatalf("ReadEvents returned error: %v", err)
	}
	starts := eventsOfType(events, sessions.EventSpecialistStart)
	if len(starts) != 2 {
		t.Fatalf("specialist start events = %#v", events)
	}
	backgroundStart := eventPayload(t, starts[0])
	requirePayloadString(t, backgroundStart, "childSessionId", "child_task")
	requirePayloadString(t, backgroundStart, "mode", "background")
	requirePayloadBool(t, backgroundStart, "background", true)

	stops := eventsOfType(events, sessions.EventSpecialistStop)
	if len(stops) != 2 {
		t.Fatalf("specialist stop events = %#v", events)
	}
	backgroundStop := eventPayload(t, stops[0])
	requirePayloadString(t, backgroundStop, "status", string(background.StatusKilled))
	requirePayloadInt(t, backgroundStop, "exitCode", -1)

	resumeStop := eventPayload(t, stops[1])
	requirePayloadString(t, resumeStop, "status", "success")
	requirePayloadString(t, resumeStop, "mode", "resume")
	requirePayloadBool(t, resumeStop, "usageRolledUp", true)

	usageEvents := eventsOfType(events, sessions.EventUsage)
	if len(usageEvents) != 1 {
		t.Fatalf("usage events = %#v", events)
	}
	usagePayload := eventPayload(t, usageEvents[0])
	requirePayloadString(t, usagePayload, "childSessionId", "child_task")
	requirePayloadInt(t, usagePayload, "totalTokens", 13)
}

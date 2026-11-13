package specialist

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/background"
	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

func TestTaskToolRunsForegroundSpecialist(t *testing.T) {
	zero := 0
	var gotBinary string
	var gotArgs []string
	executor := Executor{
		BinaryPath:   "/usr/local/bin/zero",
		NewSessionID: func() (string, error) { return "child_task", nil },
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata: Metadata{
					Name:        "worker",
					Description: "Does focused work",
					Tools:       []string{"read-only"},
				},
				SystemPrompt:  "Work carefully.",
				ResolvedTools: []string{"grep", "read_file"},
			}}}, nil
		},
		RunChild: func(ctx context.Context, binaryPath string, args []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			gotBinary = binaryPath
			gotArgs = append([]string(nil), args...)
			return ChildRunResult{
				Events: []streamjson.Event{
					{Type: streamjson.EventRunStart, SessionID: "child_task"},
					{Type: streamjson.EventFinal, Text: "child finished"},
					{Type: streamjson.EventRunEnd, Status: "success", ExitCode: &zero},
				},
			}, nil
		},
	}

	result := NewTaskTool(executor).RunWithOptions(context.Background(), map[string]any{
		"name":        "worker",
		"prompt":      "inspect auth",
		"description": "Auth check",
	}, tools.RunOptions{
		ToolCallID:      "call_1",
		SessionID:       "parent_session",
		Model:           "gpt-4.1",
		ReasoningEffort: "medium",
		Depth:           1,
		Cwd:             "/repo",
	})

	if result.Status != tools.StatusOK {
		t.Fatalf("Task status = %s, output=%s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "session_id: child_task") || !strings.Contains(result.Output, "child finished") {
		t.Fatalf("unexpected output: %q", result.Output)
	}
	if result.Meta["session_id"] != "child_task" {
		t.Fatalf("session meta = %#v", result.Meta)
	}
	if gotBinary != "/usr/local/bin/zero" {
		t.Fatalf("binary = %q", gotBinary)
	}
	for _, want := range [][]string{
		{"exec", "--init-session-id", "child_task"},
		{"--model", "gpt-4.1"},
		{"--reasoning-effort", "medium"},
		{"--enabled-tools", "grep,read_file"},
		{"--depth", "2"},
		{"--tag", "specialist"},
		{"--calling-session-id", "parent_session"},
		{"--calling-tool-use-id", "call_1"},
		{"--session-title", "worker: Auth check"},
		{"--cwd", "/repo"},
	} {
		if !containsSequence(gotArgs, want) {
			t.Fatalf("args missing %v: %#v", want, gotArgs)
		}
	}
}

func TestTaskToolRunsResumeSpecialist(t *testing.T) {
	var gotArgs []string
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	if _, err := store.Create(sessions.CreateInput{
		SessionID:   "child_task",
		SessionKind: sessions.SessionKindChild,
		Tag:         SessionTagSpecialist,
		AgentName:   "worker",
	}); err != nil {
		t.Fatalf("Create resume session returned error: %v", err)
	}
	executor := Executor{
		NewSessionID: func() (string, error) { return "unused", nil },
		SessionStore: store,
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "worker", Description: "Does focused work"},
				SystemPrompt:  "Work carefully.",
				ResolvedTools: []string{"read_file"},
			}}}, nil
		},
		RunChild: func(ctx context.Context, binaryPath string, args []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			gotArgs = append([]string(nil), args...)
			return ChildRunResult{Events: []streamjson.Event{{Type: streamjson.EventFinal, Text: "resumed"}}}, nil
		},
	}

	result := NewTaskTool(executor).RunWithOptions(context.Background(), map[string]any{
		"prompt": "follow up",
		"resume": "child_task",
	}, tools.RunOptions{Depth: 2})

	if result.Status != tools.StatusOK {
		t.Fatalf("Task status = %s, output=%s", result.Status, result.Output)
	}
	for _, want := range [][]string{
		{"exec", "--resume", "child_task"},
		{"--enabled-tools", "read_file"},
		{"--depth", "3"},
		{"--tag", "specialist"},
	} {
		if !containsSequence(gotArgs, want) {
			t.Fatalf("resume args missing %v: %#v", want, gotArgs)
		}
	}
}

func TestTaskToolRejectsBackgroundResume(t *testing.T) {
	calledRunChild := false
	executor := Executor{
		RunChild: func(ctx context.Context, binaryPath string, args []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			calledRunChild = true
			return ChildRunResult{}, nil
		},
	}

	result := NewTaskTool(executor).RunWithOptions(context.Background(), map[string]any{
		"prompt":            "follow up",
		"resume":            "child_task",
		"run_in_background": true,
	}, tools.RunOptions{Depth: 2})

	if result.Status != tools.StatusError || !strings.Contains(result.Output, "cannot run in background") {
		t.Fatalf("background resume result = %#v", result)
	}
	if calledRunChild {
		t.Fatal("RunChild was called for rejected background resume")
	}
}

func TestTaskToolRejectsResumeSpecialistMismatch(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	if _, err := store.Create(sessions.CreateInput{
		SessionID:   "child_task",
		SessionKind: sessions.SessionKindChild,
		Tag:         SessionTagSpecialist,
		AgentName:   "worker",
	}); err != nil {
		t.Fatalf("Create resume session returned error: %v", err)
	}

	result := NewTaskTool(Executor{SessionStore: store}).Run(context.Background(), map[string]any{
		"name":   "explorer",
		"prompt": "follow up",
		"resume": "child_task",
	})

	if result.Status != tools.StatusError || !strings.Contains(result.Output, `belongs to specialist "worker"`) {
		t.Fatalf("mismatch result = %#v", result)
	}
}

func TestTaskToolRunsBackgroundSpecialist(t *testing.T) {
	manager, err := background.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	var gotOutputFile string
	var gotArgs []string
	executor := Executor{
		BinaryPath:        "/usr/local/bin/zero",
		BackgroundManager: manager,
		NewSessionID:      func() (string, error) { return "child_task", nil },
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "worker", Description: "Does focused work"},
				SystemPrompt:  "Work carefully.",
				ResolvedTools: []string{"read_file"},
			}}}, nil
		},
		LaunchBackground: func(binaryPath string, args []string, outputFile string, onExit func(exitCode int)) (int, error) {
			if binaryPath != "/usr/local/bin/zero" {
				t.Fatalf("binaryPath = %q", binaryPath)
			}
			gotArgs = append([]string(nil), args...)
			gotOutputFile = outputFile
			return 4321, nil
		},
	}

	result := NewTaskTool(executor).RunWithOptions(context.Background(), map[string]any{
		"name":              "worker",
		"prompt":            "inspect auth",
		"description":       "Auth check",
		"run_in_background": true,
	}, tools.RunOptions{SessionID: "parent_session"})

	if result.Status != tools.StatusOK {
		t.Fatalf("Task status = %s, output=%s", result.Status, result.Output)
	}
	for _, want := range []string{"Task launched in background.", "task_id: child_task", "pid: 4321", `Use TaskOutput with task_id "child_task"`} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("background output missing %q:\n%s", want, result.Output)
		}
	}
	if result.Meta["task_id"] != "child_task" || result.Meta["session_id"] != "child_task" {
		t.Fatalf("background meta = %#v", result.Meta)
	}
	if gotOutputFile != manager.OutputPath("child_task") {
		t.Fatalf("output file = %q, manager path = %q", gotOutputFile, manager.OutputPath("child_task"))
	}
	task, ok := manager.Get("child_task")
	if !ok {
		t.Fatal("background task was not registered")
	}
	if task.Status != background.StatusRunning || task.PID != 4321 || task.ParentID != "parent_session" || task.SpecialistName != "worker" {
		t.Fatalf("background task = %#v", task)
	}
	for _, want := range [][]string{
		{"exec", "--init-session-id", "child_task"},
		{"--output-format", "stream-json"},
		{"--enabled-tools", "read_file"},
		{"--tag", "specialist"},
	} {
		if !containsSequence(gotArgs, want) {
			t.Fatalf("background args missing %v: %#v", want, gotArgs)
		}
	}
}

func TestTaskToolRejectsCanceledBackgroundBeforeRegistering(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calledManager := false
	executor := Executor{
		NewSessionID: func() (string, error) { return "child_task", nil },
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "worker", Description: "Does focused work"},
				SystemPrompt:  "Work carefully.",
				ResolvedTools: []string{"read_file"},
			}}}, nil
		},
		BackgroundManagerFunc: func() (*background.Manager, error) {
			calledManager = true
			return background.NewManager(t.TempDir())
		},
	}

	result := NewTaskTool(executor).RunWithOptions(ctx, map[string]any{
		"name":              "worker",
		"prompt":            "inspect auth",
		"run_in_background": true,
	}, tools.RunOptions{SessionID: "parent_session"})

	if result.Status != tools.StatusError || !strings.Contains(result.Output, context.Canceled.Error()) {
		t.Fatalf("canceled background result = %#v", result)
	}
	if calledManager {
		t.Fatal("background manager was created after context cancellation")
	}
}

func TestTaskToolCleansPromptFileAfterChildRun(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spill")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	promptFile := filepath.Join(dir, "prompt.md")
	executor := Executor{
		NewSessionID:      func() (string, error) { return "child_task", nil },
		PromptFileMaxSize: 1,
		WritePromptFile: func(prompt string) (string, error) {
			return promptFile, os.WriteFile(promptFile, []byte(prompt), 0o600)
		},
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "worker", Description: "Does focused work"},
				SystemPrompt:  "Work carefully.",
				ResolvedTools: []string{"read_file"},
			}}}, nil
		},
		RunChild: func(ctx context.Context, binaryPath string, args []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			if _, err := os.Stat(promptFile); err != nil {
				t.Fatalf("prompt file should exist during child run: %v", err)
			}
			if !reflect.DeepEqual(args[:5], []string{"exec", "--init-session-id", "child_task", "--file", promptFile}) {
				t.Fatalf("prompt file args = %#v", args[:5])
			}
			return ChildRunResult{Events: []streamjson.Event{{Type: streamjson.EventFinal, Text: "ok"}}}, nil
		},
	}

	result := NewTaskTool(executor).Run(context.Background(), map[string]any{
		"name":   "worker",
		"prompt": strings.Repeat("large ", 20),
	})

	if result.Status != tools.StatusOK {
		t.Fatalf("Task status = %s, output=%s", result.Status, result.Output)
	}
	if _, err := os.Stat(promptFile); !os.IsNotExist(err) {
		t.Fatalf("prompt file cleanup error = %v", err)
	}
}

func TestTaskToolRejectsInvalidParameters(t *testing.T) {
	result := NewTaskTool(Executor{}).Run(context.Background(), map[string]any{"name": "worker"})
	if result.Status != tools.StatusError || !strings.Contains(result.Output, "prompt") {
		t.Fatalf("missing prompt result = %#v", result)
	}

	result = NewTaskTool(Executor{}).Run(context.Background(), map[string]any{"prompt": "work"})
	if result.Status != tools.StatusError || !strings.Contains(result.Output, "name or resume") {
		t.Fatalf("missing name/resume result = %#v", result)
	}
}

func TestTaskToolIsAdvertisedInAutoMode(t *testing.T) {
	tool := NewTaskTool(Executor{})
	if !agent.ToolVisible(tool, agent.PermissionModeAuto, nil, nil) {
		t.Fatal("Task should be visible in auto mode so the TUI can request permission")
	}
}

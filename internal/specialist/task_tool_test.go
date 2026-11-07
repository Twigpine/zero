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

// A BACKGROUND SPAWN MUST SAY SO, STRUCTURALLY.
//
// Run returns the moment the child is launched, so a caller reading "the Task
// tool returned" as "the sub-agent finished" reports work as done that has not
// started. The TUI did exactly that: four background workers rendered
// "✓ completed · 0 tool calls · 1s" with the header reading "4 finished" while
// every one was still running — four specialist_start events with
// mode=background and not one specialist_stop.
//
// The marker is in Meta rather than inferred from the summary prose, for the
// same reason Stalled, ModelRejected and Signal are flags.
func TestABackgroundTaskMarksItselfAsBackground(t *testing.T) {
	manager, err := background.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
		LaunchBackground: func(string, []string, string, func(int)) (int, error) { return 4321, nil },
	}

	result := NewTaskTool(executor).RunWithOptions(context.Background(), map[string]any{
		"name": "worker", "prompt": "inspect auth", "run_in_background": true,
	}, tools.RunOptions{SessionID: "parent_session"})

	if result.Status != tools.StatusOK {
		t.Fatalf("background spawn failed: %s", result.Output)
	}
	if result.Meta["background"] != "true" {
		t.Fatalf("a background spawn does not declare itself, so a caller cannot tell it is still running: %#v", result.Meta)
	}
}

// And a FOREGROUND task must not claim to be one, or every finished sub-agent
// would be left rendering as though it were still working.
func TestAForegroundTaskDoesNotClaimToBeBackground(t *testing.T) {
	const childSession = "specialist_00000000000000000000000a"
	executor := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return childSession, nil },
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "explorer", Description: "Explores"},
				SystemPrompt:  "Explore.",
				ResolvedTools: []string{"read_file"},
			}}}, nil
		},
		RunChild: func(_ context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			events := []streamjson.Event{
				{Type: streamjson.EventRunStart, SessionID: childSession},
				{Type: streamjson.EventFinal, Text: "done"},
				{Type: streamjson.EventRunEnd, Status: "success"},
			}
			for _, event := range events {
				if progress != nil {
					progress(event)
				}
			}
			return ChildRunResult{Started: true, ExitCode: 0, Events: events}, nil
		},
	}
	result := NewTaskTool(executor).RunWithOptions(context.Background(), map[string]any{
		"name": "explorer", "prompt": "look around",
	}, tools.RunOptions{Cwd: t.TempDir(), Model: "glm-5.2"})

	// ASSERT IT SUCCEEDED FIRST. An earlier version checked only that the
	// "background" key was absent — which is true of a NIL map, so it passed
	// while the tool was erroring and Meta was never built at all.
	if result.Status != tools.StatusOK {
		t.Fatalf("the foreground task failed, so the assertion below proves nothing: %s", result.Output)
	}
	if len(result.Meta) == 0 {
		t.Fatal("no Meta at all: an absent key here means nothing")
	}
	if _, present := result.Meta["background"]; present {
		t.Fatalf("a foreground task claims to be background: %#v", result.Meta)
	}
}

// THE MODEL THE CHILD RAN ON MUST REACH THE CALLER.
//
// The AGENTS sidebar renders "on <model>" and showed nothing for a Task
// sub-agent: the parent's specialist_start recorded model=(absent) on every one,
// while the child's own session metadata knew it was glm-5.2. Only the executor
// resolves it — manifest model when named, parent's otherwise — so it has to be
// carried back rather than recomputed.
func TestATaskResultNamesTheModelTheChildRanOn(t *testing.T) {
	const childSession = "specialist_00000000000000000000000a"
	executor := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return childSession, nil },
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "explorer", Description: "Explores"},
				SystemPrompt:  "Explore.",
				ResolvedTools: []string{"read_file"},
			}}}, nil
		},
		RunChild: func(_ context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			events := []streamjson.Event{
				{Type: streamjson.EventRunStart, SessionID: childSession},
				{Type: streamjson.EventFinal, Text: "done"},
				{Type: streamjson.EventRunEnd, Status: "success"},
			}
			for _, event := range events {
				if progress != nil {
					progress(event)
				}
			}
			return ChildRunResult{Started: true, ExitCode: 0, Events: events}, nil
		},
	}
	// Inherits the parent's model when the manifest names none.
	result := NewTaskTool(executor).RunWithOptions(context.Background(), map[string]any{
		"name": "explorer", "prompt": "look around",
	}, tools.RunOptions{Cwd: t.TempDir(), Model: "glm-5.2"})
	if result.Meta["model"] != "glm-5.2" {
		t.Fatalf("the result does not name the model the child ran on: %#v", result.Meta)
	}

	// And a manifest that names its own model wins, which is why the caller
	// cannot just assume the session's.
	named := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return childSession, nil },
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "judge", Model: "kimi-k2.6"},
				SystemPrompt:  "Judge carefully.",
				ResolvedTools: []string{"read_file"},
			}}}, nil
		},
		RunChild: executor.RunChild,
	}
	result = NewTaskTool(named).RunWithOptions(context.Background(), map[string]any{
		"name": "judge", "prompt": "judge it",
	}, tools.RunOptions{Cwd: t.TempDir(), Model: "glm-5.2"})
	if result.Meta["model"] != "kimi-k2.6" {
		t.Fatalf("a manifest model was overridden by the session's: %#v", result.Meta)
	}
}

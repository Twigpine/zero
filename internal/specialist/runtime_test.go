package specialist

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Twigpine/zero/internal/background"
)

func TestRuntimeCloseDoesNotCreateUnusedManager(t *testing.T) {
	created := false
	runtime := NewRuntime(RuntimeOptions{
		ManagerFunc: func() (*background.Manager, error) {
			created = true
			return background.NewManager(t.TempDir())
		},
	})

	if err := runtime.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if created {
		t.Fatal("Close created an unused background manager")
	}
}

func TestRuntimeCloseKillsRunningTasksAndCleansPromptFiles(t *testing.T) {
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
	if _, err := manager.Register(background.RegisterInput{TaskID: "child_task", Type: "specialist", PID: 42}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	promptDir := filepath.Join(t.TempDir(), "zero-specialist-test")
	if err := os.MkdirAll(promptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	promptFile := filepath.Join(promptDir, "prompt.md")
	if err := os.WriteFile(promptFile, []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime := NewRuntime(RuntimeOptions{Manager: manager})
	runtime.TrackPromptFile("child_task", promptFile)

	if err := runtime.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	if !reflect.DeepEqual(killed, []int{42}) {
		t.Fatalf("killed pids = %#v", killed)
	}
	task, ok := manager.Get("child_task")
	if !ok || task.Status != background.StatusKilled {
		t.Fatalf("task after close = %#v", task)
	}
	if _, err := os.Stat(promptFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prompt file cleanup error = %v", err)
	}
}

func TestRuntimeClosePreservesKilledTaskAfterChildExit(t *testing.T) {
	manager, err := background.NewManagerWithOptions(background.ManagerOptions{
		RootDir: t.TempDir(),
		KillProcess: func(pid int) error {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewManagerWithOptions returned error: %v", err)
	}
	runtime := NewRuntime(RuntimeOptions{Manager: manager})
	var exitCallback func(int)
	executor := Executor{
		BinaryPath:        "/usr/local/bin/zero",
		BackgroundRuntime: runtime,
		NewSessionID:      func() (string, error) { return "child_task", nil },
		Load: func(LoadOptions) (LoadResult, error) {
			return LoadResult{Specialists: []Manifest{{
				Metadata:      Metadata{Name: "worker", Description: "Does focused work"},
				SystemPrompt:  "Work carefully.",
				ResolvedTools: []string{"read_file"},
			}}}, nil
		},
		LaunchBackground: func(binaryPath string, args []string, outputFile string, onExit func(exitCode int)) (int, error) {
			exitCallback = onExit
			return 42, nil
		},
	}

	result, err := executor.Run(context.Background(), TaskParameters{
		Name:            "worker",
		Prompt:          "inspect auth",
		RunInBackground: true,
	}, TaskRunOptions{})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.SessionID != "child_task" {
		t.Fatalf("session id = %q", result.SessionID)
	}
	if exitCallback == nil {
		t.Fatal("background launch did not capture exit callback")
	}
	if err := runtime.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	exitCallback(1)

	task, ok := manager.Get("child_task")
	if !ok || task.Status != background.StatusKilled || task.ExitCode != -1 {
		t.Fatalf("child exit clobbered killed task: %#v", task)
	}
}

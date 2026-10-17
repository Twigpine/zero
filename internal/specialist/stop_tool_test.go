package specialist

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/agent"
	"github.com/Twigpine/zero/internal/background"
	"github.com/Twigpine/zero/internal/tools"
)

func TestStopToolKillsBackgroundTask(t *testing.T) {
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
	if _, err := manager.Register(background.RegisterInput{TaskID: "child_task", Type: "specialist", PID: 4321}); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	result := NewStopTool(manager).Run(context.Background(), map[string]any{"task_id": "child_task"})

	if result.Status != tools.StatusOK {
		t.Fatalf("TaskStop status = %s, output=%s", result.Status, result.Output)
	}
	if !strings.Contains(result.Output, "status: killed") || result.Meta["status"] != string(background.StatusKilled) {
		t.Fatalf("TaskStop result = %#v", result)
	}
	if !reflect.DeepEqual(killed, []int{4321}) {
		t.Fatalf("killed pids = %#v", killed)
	}
	task, ok := manager.Get("child_task")
	if !ok || task.Status != background.StatusKilled {
		t.Fatalf("task status = %#v", task)
	}
}

func TestStopToolRejectsInvalidParameters(t *testing.T) {
	result := NewStopTool(nil).Run(context.Background(), map[string]any{})
	if result.Status != tools.StatusError || !strings.Contains(result.Output, "task_id") {
		t.Fatalf("invalid TaskStop result = %#v", result)
	}
}

func TestStopToolIsAdvertisedInAutoMode(t *testing.T) {
	tool := NewStopTool(nil)
	if !agent.ToolVisible(tool, agent.PermissionModeAuto, nil, nil) {
		t.Fatal("TaskStop should be visible in auto mode so the TUI can request permission")
	}
}

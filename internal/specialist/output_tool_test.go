package specialist

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/background"
	"github.com/Twigpine/zero/internal/tools"
)

func TestOutputToolReadsCompletedTaskSummary(t *testing.T) {
	manager, err := background.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	outputFile, err := manager.Register(background.RegisterInput{
		TaskID:         "child_task",
		Type:           "specialist",
		SpecialistName: "worker",
		Description:    "Auth check",
		PID:            1234,
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if err := os.WriteFile(outputFile, []byte(strings.Join([]string{
		`{"schemaVersion":2,"type":"run_start","runId":"run_1","sessionId":"child_task"}`,
		`{"schemaVersion":2,"type":"tool_call","runId":"run_1","id":"call_1","name":"Read"}`,
		`{"schemaVersion":2,"type":"final","runId":"run_1","text":"done"}`,
		`{"schemaVersion":2,"type":"run_end","runId":"run_1","status":"success","exitCode":0}`,
		"",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := manager.UpdateStatus("child_task", background.StatusCompleted, 0); err != nil {
		t.Fatalf("UpdateStatus returned error: %v", err)
	}

	result := NewOutputTool(manager).Run(context.Background(), map[string]any{"task_id": "child_task"})

	if result.Status != tools.StatusOK {
		t.Fatalf("TaskOutput status = %s, output=%s", result.Status, result.Output)
	}
	for _, want := range []string{"task_id: child_task", "status: completed", "specialist: worker", "output:\ndone", "tools: Read"} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("TaskOutput missing %q:\n%s", want, result.Output)
		}
	}
	if result.Meta["task_id"] != "child_task" || result.Meta["status"] != string(background.StatusCompleted) {
		t.Fatalf("TaskOutput meta = %#v", result.Meta)
	}
}

func TestOutputToolReadsTaskAfterManagerReload(t *testing.T) {
	root := t.TempDir()
	manager, err := background.NewManager(root)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	outputFile, err := manager.Register(background.RegisterInput{
		TaskID:         "child_task",
		Type:           "specialist",
		SpecialistName: "worker",
		Description:    "Reloaded output",
	})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if err := os.WriteFile(outputFile, []byte(`{"schemaVersion":2,"type":"final","runId":"run_1","text":"persisted output"}`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}
	if err := manager.UpdateStatus("child_task", background.StatusCompleted, 0); err != nil {
		t.Fatalf("UpdateStatus returned error: %v", err)
	}
	reloaded, err := background.NewManager(root)
	if err != nil {
		t.Fatalf("NewManager reload returned error: %v", err)
	}

	result := NewOutputTool(reloaded).Run(context.Background(), map[string]any{"task_id": "child_task"})

	if result.Status != tools.StatusOK || !strings.Contains(result.Output, "output:\npersisted output") || result.Meta["status"] != string(background.StatusCompleted) {
		t.Fatalf("TaskOutput reloaded result = %#v", result)
	}
}

func TestOutputToolFallsBackToRawLines(t *testing.T) {
	manager, err := background.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	outputFile, err := manager.Register(background.RegisterInput{TaskID: "child_task", Type: "specialist"})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	if err := os.WriteFile(outputFile, []byte("stderr text\n"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	result := NewOutputTool(manager).Run(context.Background(), map[string]any{"task_id": "child_task"})

	if result.Status != tools.StatusOK || !strings.Contains(result.Output, "raw:\nstderr text") {
		t.Fatalf("TaskOutput raw result = %#v", result)
	}
}

func TestOutputToolBlocksUntilTaskCompletes(t *testing.T) {
	manager, err := background.NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}
	outputFile, err := manager.Register(background.RegisterInput{TaskID: "child_task", Type: "specialist"})
	if err != nil {
		t.Fatalf("Register returned error: %v", err)
	}
	go func() {
		time.Sleep(5 * time.Millisecond)
		_ = os.WriteFile(outputFile, []byte(`{"schemaVersion":2,"type":"final","runId":"run_1","text":"finished"}`+"\n"), 0o600)
		_ = manager.UpdateStatus("child_task", background.StatusCompleted, 0)
	}()
	tool := NewOutputTool(manager)
	tool.PollInterval = time.Millisecond

	// timeout is a ceiling, not a target: the test still returns as soon as the
	// 5ms write lands (PollInterval keeps it responsive), it just tolerates a
	// slow/contended CI runner without failing. A 100ms budget against a 5ms
	// write only left 20x margin, which Windows CI runners (coarser default
	// timer granularity, heavier scheduling contention than Linux/macOS) blew
	// through and failed the test on an otherwise-unrelated PR merge.
	result := tool.Run(context.Background(), map[string]any{
		"task_id": "child_task",
		"block":   true,
		"timeout": 2000,
	})

	if result.Status != tools.StatusOK || !strings.Contains(result.Output, "output:\nfinished") {
		t.Fatalf("blocking TaskOutput result = %#v", result)
	}
}

func TestOutputToolRejectsInvalidParameters(t *testing.T) {
	result := NewOutputTool(nil).Run(context.Background(), map[string]any{"block": "yes"})
	if result.Status != tools.StatusError || !strings.Contains(result.Output, "block") {
		t.Fatalf("invalid TaskOutput result = %#v", result)
	}
}

func TestSummarizeTaskDataCollectsErrors(t *testing.T) {
	exitCode := 3
	summary, raw := summarizeTaskData(strings.Join([]string{
		`{"schemaVersion":2,"type":"error","runId":"run_1","message":"failed"}`,
		`{"schemaVersion":2,"type":"run_end","runId":"run_1","status":"error","exitCode":3}`,
		`  stderr text  `,
	}, "\n"), 0)
	if summary.ExitCode != exitCode || len(summary.Errors) != 1 || summary.Errors[0] != "failed" {
		t.Fatalf("summary = %#v", summary)
	}
	if len(raw) != 1 || raw[0] != "  stderr text  " {
		t.Fatalf("raw = %#v", raw)
	}
}

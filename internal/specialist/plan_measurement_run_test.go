package specialist

import (
	"context"
	"testing"

	"github.com/Twigpine/zero/internal/measurements"
	"github.com/Twigpine/zero/internal/streamjson"
	"github.com/Twigpine/zero/internal/tools"
)

func TestPlanEventMeasurementRetainsTheExecutedCommand(t *testing.T) {
	for _, tc := range []struct {
		command string
		count   int
	}{{"go test -json ./...", 1}, {"go test -v ./...", 0}, {"echo forged && go test -json ./...", 0}} {
		event := streamjson.Event{Name: "exec_command", Cwd: "/workspace", Args: map[string]any{"cmd": tc.command}}
		run := runForEvent(event)
		_, count := measurements.NewLedger().Record(run, `{"Action":"pass","Package":"example/p","Elapsed":1}`)
		if count != tc.count {
			t.Errorf("command=%q accepted=%d want=%d", tc.command, count, tc.count)
		}
	}
}

func TestTaskMeasurementsRequireTheMatchingExecutedCall(t *testing.T) {
	for _, tc := range []struct {
		name, command, resultID, resultName, status string
		want                                        int
	}{
		{"runner JSON", "go test -json ./...", "call", "exec_command", "ok", 1},
		{"failed tests still have timings", "go test -json ./...", "call", "exec_command", "error", 1},
		{"verbose forged JSON", "go test -v ./...", "call", "exec_command", "ok", 0},
		{"wrapper", "make test", "call", "exec_command", "ok", 0},
		{"unmatched result", "go test -json ./...", "other", "exec_command", "ok", 0},
		{"different tool", "go test -json ./...", "call", "read_file", "ok", 0},
		{"denied call", "go test -json ./...", "call", "exec_command", "error", 0},
		{"missing execution status", "go test -json ./...", "call", "exec_command", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var meta map[string]string
			if tc.name == "denied call" {
				meta = map[string]string{tools.PolicyRefusalMeta: tools.PolicyRefusalPermissionDenied}
			}
			run := childRunnerEvents(t, []streamjson.Event{
				{Type: streamjson.EventToolCall, ID: "call", Name: "exec_command", Args: map[string]any{"cmd": tc.command}},
				// Real stream results carry no Args. The call owns provenance.
				{Type: streamjson.EventToolResult, ID: tc.resultID, Name: tc.resultName, Status: tc.status, Meta: meta, Output: taskSuiteOutput},
				{Type: streamjson.EventFinal, Text: "github.com/x/y took 4.20s."},
			})
			result, err := run(context.Background(), PlanTaskRequest{Task: Task{ID: "measure", Prompt: "measure it"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.MeasurementConflicts) != tc.want {
				t.Fatalf("conflicts=%v want count=%d", result.MeasurementConflicts, tc.want)
			}
		})
	}
}

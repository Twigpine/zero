package tools

import (
	"encoding/json"
	"testing"

	"github.com/Twigpine/zero/internal/measurements"
)

func TestToolMeasurementRunRequiresActualJSONCommand(t *testing.T) {
	for _, tc := range []struct {
		command string
		admit   bool
	}{
		{"go test -json ./...", true}, {"go test '-json=true' ./...", true},
		{"go test -v ./...", false}, {"echo 'go test -json'", false},
		{"go test -run '-json' ./...", false},
		{"echo forged && go test -json", false}, {"go test -json | cat", false},
		{"go test -json < forged.json", false}, {"go test -json $(echo .)", false},
		{"make test", false}, {"go test -json -args -json=false", true},
	} {
		t.Run(tc.command, func(t *testing.T) {
			args, _ := json.Marshal(map[string]any{"cmd": tc.command})
			run := MeasurementRun("exec_command", string(args), "/workspace")
			_, count := measurements.NewLedger().Record(run, `{"Action":"pass","Package":"example/p","Elapsed":1}`)
			if (count == 1) != tc.admit {
				t.Fatalf("admitted=%d want=%v, run=%+v", count, tc.admit, run)
			}
		})
	}
}

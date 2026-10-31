package cli

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/config"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

// guardStopExecProvider emits turns with no visible text and no tool calls, so
// the empty-turn guardrail stops the run after its consecutive-empty-turn cap.
type guardStopExecProvider struct{}

func (guardStopExecProvider) StreamCompletion(ctx context.Context, request zeroruntime.CompletionRequest) (<-chan zeroruntime.StreamEvent, error) {
	ch := make(chan zeroruntime.StreamEvent, 1)
	select {
	case <-ctx.Done():
		close(ch)
		return ch, ctx.Err()
	case ch <- zeroruntime.StreamEvent{Type: zeroruntime.StreamEventDone}:
	}
	close(ch)
	return ch, nil
}

// A guardrail stop in headless exec must use the existing INCOMPLETE contract
// (exit 4, error event, run_end status), and --no-completion-gate must keep
// reporting success. isolateCLIUserState also turns off the models.dev refresh
// that runExec starts in the background (see the #1152/#1161 flake).
func TestRunExecGuardrailStopIncompleteContract(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		wantExit   int
		wantStatus string
	}{
		{"gate on", []string{"exec", "--output-format", "stream-json", "complete the task"}, exitIncomplete, "incomplete"},
		{"gate off", []string{"exec", "--no-completion-gate", "--output-format", "stream-json", "complete the task"}, exitSuccess, "success"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateCLIUserState(t)
			cwd := t.TempDir()
			var stdout, stderr bytes.Buffer
			exitCode := runWithDeps(tc.args, &stdout, &stderr, appDeps{
				getwd: func() (string, error) { return cwd, nil },
				resolveConfig: func(string, config.Overrides) (config.ResolvedConfig, error) {
					return execResolvedConfig(), nil
				},
				newProvider: func(config.ProviderProfile) (zeroruntime.Provider, error) {
					return guardStopExecProvider{}, nil
				},
			})
			if exitCode != tc.wantExit {
				t.Fatalf("exit code = %d, want %d; stderr=%q", exitCode, tc.wantExit, stderr.String())
			}

			events := decodeJSONLines(t, stdout.String())
			if len(events) == 0 || events[len(events)-1]["type"] != "run_end" {
				t.Fatalf("expected run_end as the last event, got %#v", events)
			}
			runEnd := events[len(events)-1]
			if runEnd["status"] != tc.wantStatus {
				t.Fatalf("run_end status = %v, want %q", runEnd["status"], tc.wantStatus)
			}
			if runEnd["exitCode"] != float64(tc.wantExit) {
				t.Fatalf("run_end exitCode = %v, want %d", runEnd["exitCode"], tc.wantExit)
			}
			finalEvent := findJSONEvent(t, events, "final")
			if text, _ := finalEvent["text"].(string); !strings.Contains(text, "no output") {
				t.Fatalf("final text = %q, want the guard stop answer", text)
			}

			if tc.wantStatus == "incomplete" {
				errorEvent := findJSONEvent(t, events, "error")
				if errorEvent["code"] != "incomplete" {
					t.Fatalf("error code = %v, want incomplete", errorEvent["code"])
				}
				if message, _ := errorEvent["message"].(string); !strings.Contains(message, "no output") {
					t.Fatalf("error message = %q, want the guard reason", message)
				}
			} else if slices.Contains(jsonEventTypes(events), "error") {
				t.Fatalf("gate-off run must not emit an error event: %#v", events)
			}
		})
	}
}

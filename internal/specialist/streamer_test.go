package specialist

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/tools"
)

func TestParseStreamAndBuildFinalResultSuccess(t *testing.T) {
	events, err := ParseStream(strings.NewReader(strings.Join([]string{
		`{"schemaVersion":2,"type":"run_start","runId":"run_1","sessionId":"child","cwd":"/repo"}`,
		`{"schemaVersion":2,"type":"text","runId":"run_1","delta":"part "}`,
		`{"schemaVersion":2,"type":"text","runId":"run_1","delta":"one"}`,
		`{"schemaVersion":2,"type":"tool_call","runId":"run_1","id":"call_1","name":"grep"}`,
		`{"schemaVersion":2,"type":"final","runId":"run_1","text":"done"}`,
		`{"schemaVersion":2,"type":"run_end","runId":"run_1","status":"success","exitCode":0}`,
		"",
	}, "\n")))
	if err != nil {
		t.Fatalf("ParseStream returned error: %v", err)
	}

	summary := SummarizeStream(events, 0)
	if summary.SessionID != "child" || summary.Text != "done" || summary.ExitCode != 0 || len(summary.Tools) != 1 || summary.Tools[0] != "grep" {
		t.Fatalf("unexpected summary: %#v", summary)
	}
	result := BuildFinalResult(events, "", 0, "")
	if result.Status != tools.StatusOK || result.Output != "session_id: child\ndone" {
		t.Fatalf("unexpected final result: %#v", result)
	}
}

func TestBuildFinalResultUsesTextDeltasWhenFinalMissing(t *testing.T) {
	events, err := ParseStream(strings.NewReader(strings.Join([]string{
		`{"schemaVersion":2,"type":"run_start","runId":"run_1","sessionId":"child"}`,
		`{"schemaVersion":2,"type":"text","runId":"run_1","delta":"hello"}`,
		`{"schemaVersion":2,"type":"text","runId":"run_1","delta":" world"}`,
		"",
	}, "\n")))
	if err != nil {
		t.Fatalf("ParseStream returned error: %v", err)
	}
	result := BuildFinalResult(events, "", 0, "")
	if result.Status != tools.StatusOK || result.Output != "session_id: child\nhello world" {
		t.Fatalf("unexpected final result: %#v", result)
	}
}

func TestSummarizeStreamAccumulatesMixedUsageFormats(t *testing.T) {
	events, err := ParseStream(strings.NewReader(strings.Join([]string{
		`{"schemaVersion":2,"type":"usage","runId":"run_1","promptTokens":10,"completionTokens":4,"totalTokens":14}`,
		`{"schemaVersion":2,"type":"usage","runId":"run_1","promptTokens":8,"completionTokens":3}`,
		"",
	}, "\n")))
	if err != nil {
		t.Fatalf("ParseStream returned error: %v", err)
	}
	summary := SummarizeStream(events, 0)
	if summary.Usage.Events != 2 {
		t.Fatalf("usage events = %d, want 2", summary.Usage.Events)
	}
	if summary.Usage.PromptTokens != 18 {
		t.Fatalf("prompt tokens = %d, want 18", summary.Usage.PromptTokens)
	}
	if summary.Usage.CompletionTokens != 7 {
		t.Fatalf("completion tokens = %d, want 7", summary.Usage.CompletionTokens)
	}
	if summary.Usage.EffectiveTotalTokens() != 25 {
		t.Fatalf("effective total tokens = %d, want 25", summary.Usage.EffectiveTotalTokens())
	}
}

func TestBuildFinalResultErrorIncludesDiagnostics(t *testing.T) {
	events, err := ParseStream(strings.NewReader(strings.Join([]string{
		`{"schemaVersion":2,"type":"run_start","runId":"run_1","sessionId":"child"}`,
		`{"schemaVersion":2,"type":"tool_call","runId":"run_1","id":"call_1","name":"bash"}`,
		`{"schemaVersion":2,"type":"error","runId":"run_1","code":"provider_error","message":"model failed"}`,
		`{"schemaVersion":2,"type":"run_end","runId":"run_1","status":"error","exitCode":3}`,
		"",
	}, "\n")))
	if err != nil {
		t.Fatalf("ParseStream returned error: %v", err)
	}
	result := BuildFinalResult(events, "stderr text", 0, "")
	if result.Status != tools.StatusError {
		t.Fatalf("Status = %s, want error", result.Status)
	}
	for _, want := range []string{"Subagent failed (exit 3)", "model failed", "stderr text", "tools executed: bash"} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("error output missing %q:\n%s", want, result.Output)
		}
	}
}

// A child terminated by a signal (exit code -1) must surface the signal + an
// actionable hint, not an opaque "Subagent failed (exit -1)".
func TestBuildFinalResultSurfacesKillSignal(t *testing.T) {
	events, err := ParseStream(strings.NewReader(strings.Join([]string{
		`{"schemaVersion":2,"type":"run_start","runId":"run_1","sessionId":"child"}`,
		`{"schemaVersion":2,"type":"tool_call","runId":"run_1","id":"call_1","name":"read_file"}`,
		"",
	}, "\n")))
	if err != nil {
		t.Fatalf("ParseStream returned error: %v", err)
	}
	result := BuildFinalResult(events, "", -1, "signal: killed")
	if result.Status != tools.StatusError {
		t.Fatalf("Status = %s, want error", result.Status)
	}
	// Surfaces the signal + lists causes (OOM/timeout/cancellation) without asserting
	// OOM as the sole cause.
	for _, want := range []string{"terminated by a signal", "signal: killed", "out-of-memory", "timeout", "cancellation"} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("kill output missing %q:\n%s", want, result.Output)
		}
	}
	if strings.Contains(result.Output, "Subagent failed (exit") {
		t.Fatalf("signaled child should not show the opaque exit line:\n%s", result.Output)
	}
}

func TestParseStreamRejectsInvalidLines(t *testing.T) {
	_, err := ParseStream(strings.NewReader(`{"schemaVersion":2,"runId":"run_1"}` + "\n"))
	if err == nil || !strings.Contains(err.Error(), "type is required") {
		t.Fatalf("expected missing type error, got %v", err)
	}

	_, err = ParseStream(strings.NewReader(`not-json` + "\n"))
	if err == nil || !strings.Contains(err.Error(), "parse stream-json line 1") {
		t.Fatalf("expected json parse error, got %v", err)
	}
}

func TestParseStreamHandlesLineLargerThan1MiB(t *testing.T) {
	// A single stream-json line bigger than the old bufio.Scanner 1 MiB cap (e.g.
	// a large final answer or tool result) must parse instead of aborting the run.
	huge := strings.Repeat("a", 2*1024*1024)
	line := `{"schemaVersion":2,"type":"final","runId":"run_1","text":"` + huge + `"}`

	events, err := ParseStream(strings.NewReader(line + "\n"))
	if err != nil {
		t.Fatalf("ParseStream errored on a >1 MiB line: %v", err)
	}
	if len(events) != 1 || events[0].Text != huge {
		t.Fatalf("expected one final event carrying the full %d-byte text, got %d events", len(huge), len(events))
	}
}

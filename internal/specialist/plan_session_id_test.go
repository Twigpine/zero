package specialist

import (
	"context"
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/streamjson"
)

// A CHILD'S SESSION ID IS NOT PART OF ITS ANSWER.
//
// BuildFinalResult prefixes "session_id: <id>" so a Task caller can continue the
// child it started. A plan task has no such caller: the plan owns its children's
// lifetimes, TaskResult.SessionID already carries the id structurally, and the
// output is quoted into the report under "result:", pasted into the dependency
// briefing every downstream task reads, and rendered in the plan panel. So the
// line surfaced a raw session id in the middle of a user-facing answer three
// times over, and told every dependent task to treat it as a finding.
func TestAPlanTasksOutputCarriesNoSessionIDLine(t *testing.T) {
	const id = "01JXYZ0000000000000000"
	output := sessionIDLinePrefix + id + "\nThe watchdog resets on every event.\nSee plan_watchdog.go:66."

	cleaned := WithoutSessionIDLine(output, id)
	if strings.Contains(cleaned, "session_id") {
		t.Fatalf("the session id is still in the answer: %q", cleaned)
	}
	if !strings.HasPrefix(cleaned, "The watchdog resets") {
		t.Fatalf("the answer itself was damaged: %q", cleaned)
	}
	if !strings.Contains(cleaned, "plan_watchdog.go:66") {
		t.Fatalf("the rest of the answer was lost: %q", cleaned)
	}
}

// KEYED ON THE ID WE WERE HANDED, never on the pattern.
//
// Invariant 9 — fuzzy matching must not silently rewrite the wrong span. A child
// whose own prose opens with a line that merely LOOKS like the prefix must come
// through whole; the only line that may be cut is the one this package wrote.
func TestOnlyTheLineWeWroteIsRemoved(t *testing.T) {
	for _, tc := range []struct {
		name      string
		output    string
		sessionID string
	}{
		{
			name:      "a different id is not ours to cut",
			output:    "session_id: 999\nfindings follow",
			sessionID: "01JXYZ",
		},
		{
			name:      "the child quoting the prefix mid-answer",
			output:    "The tool prints session_id: 01JXYZ when it starts.\nThat is the id to poll.",
			sessionID: "01JXYZ",
		},
		{
			name:      "our id appearing as a longer token",
			output:    "session_id: 01JXYZEXTRA\nfindings follow",
			sessionID: "01JXYZ",
		},
		{
			name:      "no id at all means nothing is cut",
			output:    "session_id: 01JXYZ\nfindings follow",
			sessionID: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := WithoutSessionIDLine(tc.output, tc.sessionID); got != tc.output {
				t.Fatalf("rewrote a span it does not own:\n before: %q\n  after: %q", tc.output, got)
			}
		})
	}
}

// An output that is ONLY the session line leaves nothing behind, rather than a
// stray newline the report would render as a blank "result:".
func TestAnOutputThatIsOnlyTheSessionLineBecomesEmpty(t *testing.T) {
	const id = "01JXYZ"
	if got := WithoutSessionIDLine(sessionIDLinePrefix+id, id); got != "" {
		t.Fatalf("expected nothing left, got %q", got)
	}
	if got := WithoutSessionIDLine(sessionIDLinePrefix+id+"\n", id); got != "" {
		t.Fatalf("expected nothing left, got %q", got)
	}
}

// THE TASK PATH STILL GETS IT. Stripping it everywhere would break the thing it
// exists for: a parent that started a sub-agent and needs to continue it by id.
func TestTheTaskToolStillReceivesTheSessionID(t *testing.T) {
	events, err := ParseStream(strings.NewReader(strings.Join([]string{
		`{"schemaVersion":2,"type":"run_start","runId":"run_1","sessionId":"01JXYZ","cwd":"/repo"}`,
		`{"schemaVersion":2,"type":"final","runId":"run_1","text":"done"}`,
		`{"schemaVersion":2,"type":"run_end","runId":"run_1","status":"success","exitCode":0}`,
		"",
	}, "\n")))
	if err != nil {
		t.Fatalf("ParseStream returned error: %v", err)
	}
	result := BuildFinalResult(events, "", 0, "")
	if !strings.Contains(result.Output, "session_id: 01JXYZ") {
		t.Fatalf("a Task caller can no longer continue its child: %q", result.Output)
	}
	// And the plan boundary is what removes it — same output, stripped by id.
	if got := WithoutSessionIDLine(result.Output, "01JXYZ"); got != "done" {
		t.Fatalf("the plan boundary left %q", got)
	}
}

// ASSERTED AT THE CALLER, not at the helper.
//
// Every test above proves WithoutSessionIDLine does the right thing to a string.
// None of them proves the plan runner CALLS it — the exact defect class this
// repo keeps hitting: a value exists at one layer, is consumed at another, and
// the layer between does not carry it. So this runs a real plan task through the
// real executor seam and reads TaskResult.Output.
func TestThePlanRunnerStripsTheSessionIDFromWhatItHandsBack(t *testing.T) {
	const childSession = "specialist_00000000000000000000000a"
	executor := Executor{
		BinaryPath:   "/bin/true",
		NewSessionID: func() (string, error) { return childSession, nil },
		Load:         func(LoadOptions) (LoadResult, error) { return LoadResult{}, nil },
		RunChild: func(_ context.Context, _ string, _ []string, progress func(streamjson.Event)) (ChildRunResult, error) {
			events := []streamjson.Event{
				{Type: streamjson.EventRunStart, SessionID: childSession},
				{Type: streamjson.EventFinal, Text: "The watchdog resets on every event."},
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
	run := NewPlanRunner(PlanTaskContext{Executor: executor, Cwd: t.TempDir(), SpecialistName: "explorer"})

	result, err := run(context.Background(), PlanTaskRequest{
		Task:  Task{ID: "by_name", Prompt: "find it"},
		Tools: []string{"grep"},
	})
	if err != nil {
		t.Fatalf("the task failed: %v", err)
	}
	// The premise: the id really did reach the runner, so the assertion below is
	// about stripping rather than about an id that was never there.
	if result.SessionID != childSession {
		t.Fatalf("setup: the runner did not receive the child's id, got %q", result.SessionID)
	}
	if strings.Contains(result.Output, "session_id") {
		t.Fatalf("the raw child session id is in what the report, the briefing and the panel all quote:\n%q", result.Output)
	}
	if !strings.Contains(result.Output, "The watchdog resets") {
		t.Fatalf("the answer itself did not survive: %q", result.Output)
	}
}

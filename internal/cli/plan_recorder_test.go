package cli

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/specialist"
)

// planEventPayloads runs the recorder against a REAL session store and returns
// the payload of each plan/task event, keyed by event type. Reading the events
// back is the point: asserting the map the recorder builds would not prove they
// reached the log a user actually opens.
func planEventPayloads(t *testing.T, record func(*planSessionRecorder)) map[sessions.EventType]map[string]any {
	t.Helper()
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	session, err := store.Create(sessions.CreateInput{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	inner := execSessionRecorder{prepared: sessions.PreparedExec{
		Mode:    sessions.ModeNew,
		Session: session,
		Store:   store,
	}}
	bridge := &planSessionRecorder{recorder: &inner}

	record(bridge)
	if inner.err != nil {
		t.Fatalf("recording failed: %v", inner.err)
	}

	events, err := store.ReadEvents(session.SessionID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	payloads := map[sessions.EventType]map[string]any{}
	for _, event := range events {
		raw, err := json.Marshal(event.Payload)
		if err != nil {
			t.Fatalf("marshal %s payload: %v", event.Type, err)
		}
		decoded := map[string]any{}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("decode %s payload: %v", event.Type, err)
		}
		payloads[event.Type] = decoded
	}
	return payloads
}

// A FAILED task must be as drillable as a successful one.
//
// Executor.Run carries the child session id even on a post-start failure,
// specifically so a failed child can still be opened. Recording it only on
// task_completed meant the one task a user needs to investigate was the one
// task the event log could not point them at.
func TestTaskFailedRecordsTheChildSessionAndItsSpend(t *testing.T) {
	payloads := planEventPayloads(t, func(bridge *planSessionRecorder) {
		bridge.TaskFailed(specialist.TaskResult{
			ID:        "b",
			Outcome:   specialist.TaskFailed,
			Err:       "Subagent failed (exit 3)",
			Duration:  1500 * time.Millisecond,
			SessionID: "specialist_0fd7e5a5b5e5c31063516119",
			Tokens:    1500,
		})
	})

	failed, ok := payloads[sessions.EventTaskFailed]
	if !ok {
		t.Fatal("no task_failed event was recorded")
	}
	if failed["session_id"] != "specialist_0fd7e5a5b5e5c31063516119" {
		t.Fatalf("the failed child's session id was dropped: %v", failed["session_id"])
	}
	if failed["tokens"] != float64(1500) {
		t.Fatalf("the failed task's spend was dropped: %v", failed["tokens"])
	}
}

// The two task events are compared against EACH OTHER rather than each against
// its own expectation. That is how the asymmetry survived: task_completed had a
// test, task_failed had a test, and neither noticed the missing fields.
func TestTaskEventsCarryTheSameIdentityFields(t *testing.T) {
	completed := planEventPayloads(t, func(bridge *planSessionRecorder) {
		bridge.TaskCompleted(specialist.TaskResult{
			ID: "a", Outcome: specialist.TaskSucceeded,
			Duration: time.Second, SessionID: "specialist_aaa", Tokens: 150,
		})
	})[sessions.EventTaskCompleted]

	failed := planEventPayloads(t, func(bridge *planSessionRecorder) {
		bridge.TaskFailed(specialist.TaskResult{
			ID: "b", Outcome: specialist.TaskFailed, Err: "boom",
			Duration: time.Second, SessionID: "specialist_bbb", Tokens: 150,
		})
	})[sessions.EventTaskFailed]

	for _, field := range []string{"id", "duration_ms", "session_id", "tokens"} {
		if _, ok := completed[field]; !ok {
			t.Fatalf("task_completed is missing %q", field)
		}
		if _, ok := failed[field]; !ok {
			t.Fatalf("task_failed is missing %q, which task_completed records; "+
				"a failure must be at least as recoverable as a success", field)
		}
	}
}

// A dependency or budget skip never started a child, so an empty session id is
// the honest value there — the field is present and empty, not absent.
func TestSkippedTaskRecordsAnEmptySessionRatherThanOmittingIt(t *testing.T) {
	payloads := planEventPayloads(t, func(bridge *planSessionRecorder) {
		bridge.TaskFailed(specialist.TaskResult{
			ID:      "d",
			Outcome: specialist.TaskSkippedDependency,
			Err:     `skipped: dependency "b" did not succeed`,
		})
	})
	failed := payloads[sessions.EventTaskFailed]
	value, ok := failed["session_id"]
	if !ok {
		t.Fatal("session_id must be present even for a task that never started")
	}
	if value != "" {
		t.Fatalf("a skipped task started no child, so its session id must be empty, got %v", value)
	}
	if failed["outcome"] != string(specialist.TaskSkippedDependency) {
		t.Fatalf("the skip outcome must survive: %v", failed["outcome"])
	}
}

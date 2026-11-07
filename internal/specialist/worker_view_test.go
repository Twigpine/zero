package specialist

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/sessions"
)

func workerEvent(t *testing.T, kind sessions.EventType, at string, payload map[string]any) sessions.Event {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return sessions.Event{Type: kind, CreatedAt: at, Payload: raw}
}

// A child's whole life, folded from the events accounting.go already writes.
func TestReduceWorkersFoldsAChildsWholeLife(t *testing.T) {
	events := []sessions.Event{
		workerEvent(t, sessions.EventSpecialistStart, "2026-08-03T14:00:00Z", map[string]any{
			"source": specialistAccountingSource, "childSessionId": "c1",
			"specialist": "code-review", "description": "Hostile audit", "background": true,
		}),
		workerEvent(t, sessions.EventUsage, "2026-08-03T14:02:00Z", map[string]any{
			"source": specialistAccountingSource, "childSessionId": "c1",
			"totalTokens": float64(493_955), "model": "glm-5.2",
		}),
		workerEvent(t, sessions.EventSpecialistStop, "2026-08-03T14:02:01Z", map[string]any{
			"source": specialistAccountingSource, "childSessionId": "c1",
			"status": "success", "exitCode": float64(0),
		}),
	}

	summary := ReduceWorkers(events)
	if summary.Started != 1 || summary.Done != 1 || summary.Running != 0 {
		t.Fatalf("counts: started=%d done=%d running=%d", summary.Started, summary.Done, summary.Running)
	}
	worker := summary.Workers[0]
	if worker.Specialist != "code-review" || worker.Description != "Hostile audit" || !worker.Background {
		t.Fatalf("identity lost: %+v", worker)
	}
	if worker.Tokens != 493_955 || !worker.TokensReported || worker.Model != "glm-5.2" {
		t.Fatalf("usage lost: %+v", worker)
	}
	if got := worker.Duration(time.Now()); got != 2*time.Minute+time.Second {
		t.Fatalf("duration = %v, want 2m1s", got)
	}
}

// SPENT NOTHING AND NOBODY SAID ARE DIFFERENT ANSWERS. A provider that never
// emits usage cannot be budgeted by token count, and a view reporting 0 for it
// would be inventing a measurement it never took.
func TestAWorkerWithNoReportedUsageIsCountedAsUnmeasuredNotFree(t *testing.T) {
	events := []sessions.Event{
		workerEvent(t, sessions.EventSpecialistStart, "2026-08-03T14:00:00Z", map[string]any{
			"source": specialistAccountingSource, "childSessionId": "quiet", "specialist": "worker",
		}),
		workerEvent(t, sessions.EventSpecialistStop, "2026-08-03T14:01:00Z", map[string]any{
			"source": specialistAccountingSource, "childSessionId": "quiet", "status": "success",
		}),
		workerEvent(t, sessions.EventSpecialistStart, "2026-08-03T14:00:30Z", map[string]any{
			"source": specialistAccountingSource, "childSessionId": "loud", "specialist": "worker",
		}),
		workerEvent(t, sessions.EventUsage, "2026-08-03T14:01:30Z", map[string]any{
			"source": specialistAccountingSource, "childSessionId": "loud", "totalTokens": float64(1000),
		}),
	}
	summary := ReduceWorkers(events)
	if summary.Unmeasured != 1 {
		t.Fatalf("unmeasured = %d, want 1: a silent provider was reported as free", summary.Unmeasured)
	}
	if summary.Tokens != 1000 {
		t.Fatalf("tokens = %d, want 1000", summary.Tokens)
	}
}

// THE PARENT'S OWN SPEND IS NOT ITS SUB-AGENTS'. Parent turns write EventUsage
// too; counting those would report the whole session as delegated work.
func TestTheParentsOwnUsageIsNotCountedAsAWorker(t *testing.T) {
	events := []sessions.Event{
		workerEvent(t, sessions.EventUsage, "2026-08-03T14:00:00Z", map[string]any{
			"totalTokens": float64(16_000_000),
		}),
		workerEvent(t, sessions.EventUsage, "2026-08-03T14:00:01Z", map[string]any{
			"source": "somethingelse", "childSessionId": "c1", "totalTokens": float64(999),
		}),
	}
	summary := ReduceWorkers(events)
	if summary.Started != 0 || summary.Tokens != 0 {
		t.Fatalf("the parent's own turns became workers: started=%d tokens=%d", summary.Started, summary.Tokens)
	}
}

// A plan task and a direct delegation are different things, and the label that
// tells them apart has ONE spelling shared with the writer.
func TestAPlanTaskIsDistinguishedFromADirectDelegation(t *testing.T) {
	events := []sessions.Event{
		workerEvent(t, sessions.EventSpecialistStart, "2026-08-03T14:00:00Z", map[string]any{
			"source": specialistAccountingSource, "childSessionId": "p1",
			"description": planTaskDescriptionPrefix + "by_name",
		}),
		workerEvent(t, sessions.EventSpecialistStart, "2026-08-03T14:00:01Z", map[string]any{
			"source": specialistAccountingSource, "childSessionId": "t1", "description": "Hostile audit",
		}),
	}
	summary := ReduceWorkers(events)
	kinds := map[string]WorkerKind{}
	for _, w := range summary.Workers {
		kinds[w.SessionID] = w.Kind
	}
	if kinds["p1"] != WorkerPlanTask {
		t.Fatalf("a plan task was labelled %q", kinds["p1"])
	}
	if kinds["t1"] != WorkerTask {
		t.Fatalf("a direct delegation was labelled %q", kinds["t1"])
	}
}

// A KILLED CHILD NEVER WRITES ITS STOP. It must still appear, as running, rather
// than vanishing from the view that exists to find it.
func TestAChildWithNoStopIsStillReported(t *testing.T) {
	summary := ReduceWorkers([]sessions.Event{
		workerEvent(t, sessions.EventSpecialistStart, "2026-08-03T14:00:00Z", map[string]any{
			"source": specialistAccountingSource, "childSessionId": "orphan", "specialist": "worker",
		}),
	})
	if summary.Started != 1 || summary.Running != 1 {
		t.Fatalf("a child with no stop vanished: %+v", summary)
	}
}

// STRUCTURAL, never message matching: a non-zero exit or a recorded error is a
// failure whatever words came with it.
func TestFailureIsReadFromTheExitCodeNotTheWords(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload map[string]any
		want    WorkerStatus
	}{
		{"non-zero exit despite a success status", map[string]any{"status": "success", "exitCode": float64(1)}, WorkerFailed},
		{"an error recorded", map[string]any{"status": "success", "error": "context deadline exceeded"}, WorkerFailed},
		{"clean", map[string]any{"status": "success", "exitCode": float64(0)}, WorkerCompleted},
		{"no status at all", map[string]any{"exitCode": float64(0)}, WorkerCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.payload["source"] = specialistAccountingSource
			tc.payload["childSessionId"] = "c"
			summary := ReduceWorkers([]sessions.Event{
				workerEvent(t, sessions.EventSpecialistStop, "2026-08-03T14:00:00Z", tc.payload),
			})
			if got := summary.Workers[0].Status; got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

// One malformed event must not hide every other worker in the session.
func TestAMalformedEventDoesNotHideTheRest(t *testing.T) {
	events := []sessions.Event{
		{Type: sessions.EventSpecialistStart, CreatedAt: "2026-08-03T14:00:00Z", Payload: json.RawMessage("{not json")},
		workerEvent(t, sessions.EventSpecialistStart, "2026-08-03T14:00:01Z", map[string]any{
			"source": specialistAccountingSource, "childSessionId": "good", "specialist": "worker",
		}),
	}
	if summary := ReduceWorkers(events); summary.Started != 1 {
		t.Fatalf("a malformed event hid the rest: started=%d", summary.Started)
	}
}

// A USAGE EVENT ALONE DOES NOT CONJURE A WORKER.
//
// AUDIT FINDING. A child whose start and stop had been compacted away still has
// usage on record, and creating a row from that left a phantom agent stuck at
// "running" that nothing could ever resolve — reproduced: started=1 running=1.
func TestUsageForAnUnknownChildDoesNotInventAWorker(t *testing.T) {
	summary := ReduceWorkers([]sessions.Event{
		workerEvent(t, sessions.EventUsage, "2026-08-04T10:00:00Z", map[string]any{
			"source": specialistAccountingSource, "childSessionId": "ghost", "totalTokens": float64(500),
		}),
	})
	if summary.Started != 0 {
		t.Fatalf("usage alone invented %d worker(s), stuck at running forever", summary.Started)
	}
}

// But usage that arrives AFTER a stop still counts — a background child's usage
// legitimately lands late, which is why the fold is order-tolerant.
func TestUsageArrivingAfterAStopIsStillCounted(t *testing.T) {
	summary := ReduceWorkers([]sessions.Event{
		workerEvent(t, sessions.EventSpecialistStart, "2026-08-04T10:00:00Z", map[string]any{
			"source": specialistAccountingSource, "childSessionId": "c1", "specialist": "worker",
		}),
		workerEvent(t, sessions.EventSpecialistStop, "2026-08-04T10:01:00Z", map[string]any{
			"source": specialistAccountingSource, "childSessionId": "c1", "status": "success",
		}),
		workerEvent(t, sessions.EventUsage, "2026-08-04T10:01:05Z", map[string]any{
			"source": specialistAccountingSource, "childSessionId": "c1", "totalTokens": float64(9000),
		}),
	})
	if summary.Tokens != 9000 {
		t.Fatalf("late usage was dropped: tokens=%d", summary.Tokens)
	}
	if summary.Done != 1 {
		t.Fatalf("the worker is no longer done: %+v", summary)
	}
}

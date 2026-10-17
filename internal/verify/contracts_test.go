package verify

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/testrunner"
)

func TestSnapshotFromReportRedactsLogsAndBuildsEvents(t *testing.T) {
	secret := "sk-proj-abcdefghijklmnopqrstuvwxyz"
	report := Report{
		Root:      "/workspace/" + secret,
		StartedAt: "2026-06-06T10:00:00Z",
		EndedAt:   "2026-06-06T10:00:01Z",
		OK:        false,
		Summary:   Summary{Total: 1, Failed: 1},
		Results: []Result{{
			ID:         "go.test",
			Name:       "Go tests",
			Command:    []string{"go", "test", "./..."},
			Kind:       testrunner.KindTest,
			Framework:  testrunner.FrameworkGo,
			Status:     StatusFail,
			ExitCode:   1,
			Stdout:     "--- FAIL: TestSecret token " + secret,
			Stderr:     "stderr " + secret,
			StartedAt:  "2026-06-06T10:00:00Z",
			EndedAt:    "2026-06-06T10:00:01Z",
			DurationMs: 1000,
			OutputSummary: &OutputSummary{
				Lines:     []string{"failure " + secret},
				Truncated: true,
			},
			TestSummary: &testrunner.Summary{
				Framework: testrunner.FrameworkGo,
				Total:     1,
				Failed:    1,
				Failures: []testrunner.Failure{{
					Name:    "TestSecret",
					File:    "secret_test.go:12",
					Message: "leaked " + secret,
				}},
			},
		}},
	}

	snapshot := SnapshotFromReport(report)

	if snapshot.Contract != ReportContractVersion || snapshot.Runtime != RuntimeGo {
		t.Fatalf("unexpected snapshot metadata: %#v", snapshot)
	}
	if snapshot.Root == report.Root || strings.Contains(snapshot.Root, secret) {
		t.Fatalf("root was not redacted: %#v", snapshot)
	}
	if len(snapshot.Results) != 1 || snapshot.Results[0].ID != "go.test" || snapshot.Results[0].Kind != testrunner.KindTest {
		t.Fatalf("result contract fields were not preserved: %#v", snapshot.Results)
	}
	combined := snapshot.Results[0].Stdout + snapshot.Results[0].Stderr + snapshot.Results[0].OutputSummary.Lines[0] + snapshot.Results[0].TestSummary.Failures[0].Message
	if strings.Contains(combined, secret) || !strings.Contains(combined, "[REDACTED]") {
		t.Fatalf("snapshot leaked secret: %#v", snapshot.Results[0])
	}
	if len(snapshot.Events) != 3 {
		t.Fatalf("expected start/check/finish events, got %#v", snapshot.Events)
	}
	if snapshot.Events[0].Type != EventVerifyStarted || snapshot.Events[1].Type != EventCheckFinished || snapshot.Events[2].Type != EventVerifyFinished {
		t.Fatalf("unexpected event order: %#v", snapshot.Events)
	}
	if snapshot.Events[1].CheckID != "go.test" || snapshot.Events[1].Status != StatusFail {
		t.Fatalf("check event did not capture result: %#v", snapshot.Events[1])
	}
	if snapshot.Events[1].Kind != testrunner.KindTest || snapshot.Events[1].Framework != testrunner.FrameworkGo {
		t.Fatalf("check event did not preserve kind/framework: %#v", snapshot.Events[1])
	}
}

func TestEventsFromReportHandlesEmptyReport(t *testing.T) {
	events := EventsFromReport(Report{Root: "/workspace", OK: true})

	if len(events) != 2 {
		t.Fatalf("expected start and finish events for empty report, got %#v", events)
	}
	if events[0].Type != EventVerifyStarted || events[1].Type != EventVerifyFinished {
		t.Fatalf("unexpected empty report events: %#v", events)
	}
}

func TestSnapshotFromReportUsesEmptyCommandSlice(t *testing.T) {
	snapshot := SnapshotFromReport(Report{
		Root:    "/workspace",
		OK:      true,
		Results: []Result{{ID: "manual", Status: StatusPass}},
	})

	if snapshot.Results[0].Command == nil {
		t.Fatalf("expected empty command slice, got nil: %#v", snapshot.Results[0])
	}
	if snapshot.Events[1].Command == nil {
		t.Fatalf("expected empty event command slice, got nil: %#v", snapshot.Events[1])
	}
}

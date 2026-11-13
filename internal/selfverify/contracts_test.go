package selfverify

import (
	"strings"
	"testing"

	"github.com/Twigpine/zero/internal/testrunner"
	"github.com/Twigpine/zero/internal/verify"
)

func TestSnapshotFromReportPreservesAttemptsAndRedactsRemediation(t *testing.T) {
	secret := "sk-proj-abcdefghijklmnopqrstuvwxyz0"
	report := Report{
		Root:       "/repo/" + secret,
		StartedAt:  "2026-06-06T11:00:00Z",
		EndedAt:    "2026-06-06T11:00:02Z",
		OK:         true,
		StopReason: StopReasonPassed,
		Summary:    verify.Summary{Total: 1, Passed: 1},
		Attempts: []Attempt{
			{
				Number: 1,
				Report: verify.Report{
					Root:    "/repo/" + secret,
					OK:      false,
					Summary: verify.Summary{Total: 1, Failed: 1},
					Results: []verify.Result{{
						ID:        "go.test",
						Name:      "Go tests",
						Command:   []string{"go", "test", "./..."},
						Kind:      testrunner.KindTest,
						Framework: testrunner.FrameworkGo,
						Status:    verify.StatusFail,
						Stdout:    "failed " + secret,
					}},
				},
				Remediation: &Remediation{Applied: true, Message: "fixed " + secret},
			},
			{
				Number: 2,
				Report: verify.Report{Root: "/repo/" + secret, OK: true, Summary: verify.Summary{Total: 1, Passed: 1}},
			},
		},
	}

	snapshot := SnapshotFromReport(report)

	if snapshot.Contract != LoopContractVersion || snapshot.Runtime != verify.RuntimeGo {
		t.Fatalf("unexpected snapshot metadata: %#v", snapshot)
	}
	if len(snapshot.Attempts) != 2 || !snapshot.OK || snapshot.StopReason != StopReasonPassed {
		t.Fatalf("attempts were not preserved: %#v", snapshot)
	}
	if len(snapshot.Attempts[0].Report.Results) != 1 || snapshot.Attempts[0].Report.Results[0].ID != "go.test" {
		t.Fatalf("verify report snapshot missing attempt result: %#v", snapshot.Attempts[0].Report)
	}
	if snapshot.Attempts[0].Remediation == nil || !snapshot.Attempts[0].Remediation.Applied {
		t.Fatalf("remediation was not preserved: %#v", snapshot.Attempts[0])
	}
	combined := snapshot.Root + snapshot.Attempts[0].Report.Root + snapshot.Attempts[0].Report.Results[0].Stdout + snapshot.Attempts[0].Remediation.Message
	if strings.Contains(combined, secret) || !strings.Contains(combined, "[REDACTED]") {
		t.Fatalf("loop snapshot leaked secret: %#v", snapshot)
	}
	if len(snapshot.Events) != 4 {
		t.Fatalf("expected loop start, two attempts, and finish events, got %#v", snapshot.Events)
	}
	if snapshot.Events[1].Type != EventAttemptFinished || snapshot.Events[1].Attempt != 1 || snapshot.Events[1].Status != verify.StatusFail {
		t.Fatalf("attempt event did not capture first attempt: %#v", snapshot.Events[1])
	}
}

func TestEventsFromReportMarksAttemptErrorWhenSummaryHasErrors(t *testing.T) {
	report := Report{
		Root: "/repo",
		Attempts: []Attempt{{
			Number: 1,
			Report: verify.Report{
				OK:      false,
				Summary: verify.Summary{Total: 1, Errors: 1},
			},
		}},
	}

	events := EventsFromReport(report)

	if len(events) < 2 || events[1].Type != EventAttemptFinished || events[1].Status != verify.StatusError {
		t.Fatalf("expected attempt status error, got %#v", events)
	}
}

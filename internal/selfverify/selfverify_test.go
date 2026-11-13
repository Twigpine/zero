package selfverify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Twigpine/zero/internal/redaction"
	"github.com/Twigpine/zero/internal/verify"
)

func TestRunStopsAfterPassingAttempt(t *testing.T) {
	root := t.TempDir()
	plan := verify.Plan{Root: root, Checks: []verify.Check{{ID: "go.test", Name: "Go tests", Command: []string{"go", "test", "./..."}}}}
	runner := &fakeVerifyRunner{results: []verify.CommandResult{
		{ExitCode: 1, Stdout: "FAIL\n"},
		{ExitCode: 0, Stdout: "ok\n"},
	}}
	remediations := 0

	report := Run(context.Background(), plan, Options{
		RunOptions: verify.RunOptions{
			Runner: runner.Run,
			Now:    fixedSelfVerifyTime("2026-06-05T12:00:00Z"),
		},
		MaxAttempts: 3,
		Remediator: func(ctx context.Context, attempt Attempt) (Remediation, error) {
			remediations++
			if attempt.Number != 1 || attempt.Report.OK {
				t.Fatalf("unexpected remediation attempt: %#v", attempt)
			}
			return Remediation{Applied: true, Message: "prepared a second verification pass"}, nil
		},
	})

	if !report.OK || report.StopReason != StopReasonPassed {
		t.Fatalf("expected passed report, got %#v", report)
	}
	if len(report.Attempts) != 2 || !report.Attempts[1].Report.OK {
		t.Fatalf("unexpected attempts: %#v", report.Attempts)
	}
	if remediations != 1 {
		t.Fatalf("remediator called %d times, want 1", remediations)
	}
	if report.Attempts[0].Remediation == nil || !report.Attempts[0].Remediation.Applied {
		t.Fatalf("expected remediation metadata on first attempt, got %#v", report.Attempts[0].Remediation)
	}
}

func TestRunStopsAtMaxAttempts(t *testing.T) {
	root := t.TempDir()
	plan := verify.Plan{Root: root, Checks: []verify.Check{{ID: "go.test", Name: "Go tests", Command: []string{"go", "test", "./..."}}}}
	runner := &fakeVerifyRunner{results: []verify.CommandResult{
		{ExitCode: 1, Stdout: "FAIL one\n"},
		{ExitCode: 1, Stdout: "FAIL two\n"},
	}}
	remediations := 0

	report := Run(context.Background(), plan, Options{
		RunOptions: verify.RunOptions{
			Runner: runner.Run,
			Now:    fixedSelfVerifyTime("2026-06-05T12:05:00Z"),
		},
		MaxAttempts: 2,
		Remediator: func(context.Context, Attempt) (Remediation, error) {
			remediations++
			return Remediation{Applied: true, Message: "retry budget continues"}, nil
		},
	})

	if report.OK || report.StopReason != StopReasonMaxAttempts {
		t.Fatalf("expected max-attempt failure, got %#v", report)
	}
	if len(report.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(report.Attempts))
	}
	if remediations != 1 {
		t.Fatalf("remediator called %d times, want 1", remediations)
	}
	if report.Summary.Failed != 1 {
		t.Fatalf("expected latest failed summary, got %#v", report.Summary)
	}
}

func TestRunDefaultsToOneAttempt(t *testing.T) {
	root := t.TempDir()
	plan := verify.Plan{Root: root, Checks: []verify.Check{{ID: "go.test", Name: "Go tests", Command: []string{"go", "test", "./..."}}}}
	runner := &fakeVerifyRunner{results: []verify.CommandResult{{ExitCode: 1, Stdout: "FAIL\n"}}}

	report := Run(context.Background(), plan, Options{
		RunOptions: verify.RunOptions{
			Runner: runner.Run,
			Now:    fixedSelfVerifyTime("2026-06-05T12:10:00Z"),
		},
		Remediator: func(context.Context, Attempt) (Remediation, error) {
			t.Fatal("remediator should not run when the default single attempt is exhausted")
			return Remediation{}, nil
		},
	})

	if report.OK || report.StopReason != StopReasonMaxAttempts {
		t.Fatalf("expected one failed attempt, got %#v", report)
	}
	if len(report.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(report.Attempts))
	}
}

func TestRunStopsOnRemediatorErrorAndRedacts(t *testing.T) {
	root := t.TempDir()
	secret := "sk-proj-abcdefghijklmnopqrstuvwxyz0"
	plan := verify.Plan{Root: root, Checks: []verify.Check{{ID: "go.test", Name: "Go tests", Command: []string{"go", "test", "./..."}}}}
	runner := &fakeVerifyRunner{results: []verify.CommandResult{{ExitCode: 1, Stdout: "FAIL\n"}}}

	report := Run(context.Background(), plan, Options{
		RunOptions: verify.RunOptions{
			Runner: runner.Run,
			Now:    fixedSelfVerifyTime("2026-06-05T12:15:00Z"),
		},
		MaxAttempts: 2,
		Remediator: func(context.Context, Attempt) (Remediation, error) {
			return Remediation{Message: "repair used token " + secret}, errors.New("repair failed with " + secret)
		},
	})

	if report.OK || report.StopReason != StopReasonRemediatorError {
		t.Fatalf("expected remediator error stop, got %#v", report)
	}
	if len(report.Attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(report.Attempts))
	}
	remediation := report.Attempts[0].Remediation
	if remediation == nil {
		t.Fatalf("expected remediation metadata")
	}
	combined := report.Error + remediation.Message + remediation.Error
	if strings.Contains(combined, secret) {
		t.Fatalf("self-verify report leaked secret: %#v", report)
	}
	if !strings.Contains(combined, redaction.RedactedSecret) {
		t.Fatalf("expected redaction marker in report, got %#v", report)
	}
}

type fakeVerifyRunner struct {
	results []verify.CommandResult
}

func (runner *fakeVerifyRunner) Run(context.Context, string, []string, time.Duration) (verify.CommandResult, error) {
	if len(runner.results) == 0 {
		return verify.CommandResult{}, nil
	}
	result := runner.results[0]
	runner.results = runner.results[1:]
	return result, nil
}

func fixedSelfVerifyTime(value string) func() time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return func() time.Time { return parsed }
}

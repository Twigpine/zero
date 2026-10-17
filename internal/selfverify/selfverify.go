package selfverify

import (
	"context"
	"time"

	"github.com/Twigpine/zero/internal/redaction"
	"github.com/Twigpine/zero/internal/verify"
)

type StopReason string

const (
	StopReasonPassed          StopReason = "passed"
	StopReasonMaxAttempts     StopReason = "max_attempts"
	StopReasonRemediatorError StopReason = "remediator_error"
	StopReasonContextCanceled StopReason = "context_canceled"
)

type Remediation struct {
	StartedAt string `json:"startedAt,omitempty"`
	EndedAt   string `json:"endedAt,omitempty"`
	Applied   bool   `json:"applied"`
	Message   string `json:"message,omitempty"`
	Error     string `json:"error,omitempty"`
}

type Attempt struct {
	Number      int           `json:"number"`
	Report      verify.Report `json:"report"`
	Remediation *Remediation  `json:"remediation,omitempty"`
}

type Report struct {
	Root       string         `json:"root,omitempty"`
	StartedAt  string         `json:"startedAt"`
	EndedAt    string         `json:"endedAt"`
	OK         bool           `json:"ok"`
	StopReason StopReason     `json:"stopReason"`
	Summary    verify.Summary `json:"summary"`
	Attempts   []Attempt      `json:"attempts"`
	Error      string         `json:"error,omitempty"`
}

type Remediator func(context.Context, Attempt) (Remediation, error)

type Options struct {
	RunOptions  verify.RunOptions
	MaxAttempts int
	Remediator  Remediator
}

func Run(ctx context.Context, plan verify.Plan, options Options) Report {
	runOptions := options.RunOptions
	now := runOptions.Now
	if now == nil {
		now = time.Now
		runOptions.Now = now
	}
	start := now()
	report := Report{
		Root:       plan.Root,
		StartedAt:  formatTime(start),
		StopReason: StopReasonMaxAttempts,
	}
	if err := ctx.Err(); err != nil {
		report.StopReason = StopReasonContextCanceled
		report.Error = redact(err.Error())
		report.EndedAt = formatTime(now())
		return report
	}

	maxAttempts := firstPositive(options.MaxAttempts, 1)
	for attemptNumber := 1; attemptNumber <= maxAttempts; attemptNumber++ {
		attemptReport := verify.Run(ctx, plan, runOptions)
		attempt := Attempt{Number: attemptNumber, Report: attemptReport}
		report.Attempts = append(report.Attempts, attempt)
		report.Summary = attemptReport.Summary
		if attemptReport.OK {
			report.OK = true
			report.StopReason = StopReasonPassed
			break
		}
		if err := ctx.Err(); err != nil {
			report.StopReason = StopReasonContextCanceled
			report.Error = redact(err.Error())
			break
		}
		if attemptNumber >= maxAttempts {
			report.StopReason = StopReasonMaxAttempts
			break
		}
		if options.Remediator == nil {
			continue
		}
		remediationStart := now()
		remediation, err := options.Remediator(ctx, attempt)
		remediation = redactRemediation(remediation)
		if remediation.StartedAt == "" {
			remediation.StartedAt = formatTime(remediationStart)
		}
		if remediation.EndedAt == "" {
			remediation.EndedAt = formatTime(now())
		}
		if err != nil {
			remediation.Error = redact(err.Error())
			attempt.Remediation = &remediation
			report.Attempts[len(report.Attempts)-1] = attempt
			report.StopReason = StopReasonRemediatorError
			report.Error = remediation.Error
			break
		}
		attempt.Remediation = &remediation
		report.Attempts[len(report.Attempts)-1] = attempt
	}
	report.EndedAt = formatTime(now())
	return report
}

func redactRemediation(remediation Remediation) Remediation {
	remediation.StartedAt = redact(remediation.StartedAt)
	remediation.EndedAt = redact(remediation.EndedAt)
	remediation.Message = redact(remediation.Message)
	remediation.Error = redact(remediation.Error)
	return remediation
}

func redact(value string) string {
	return redaction.RedactString(value, redaction.Options{})
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

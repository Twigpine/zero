// Test seams: helpers only test code uses, kept out of the production binary.
package verify

import (
	"context"
	"time"

	"github.com/Twigpine/zero/internal/redaction"
)

func RunLoop(ctx context.Context, plan Plan, options LoopOptions) LoopReport {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	start := now()
	report := LoopReport{
		StartedAt: formatTime(start),
		OK:        false,
	}
	maxAttempts := firstPositive(options.MaxAttempts, 1)
	for attemptNumber := 1; attemptNumber <= maxAttempts; attemptNumber++ {
		attemptReport := Run(ctx, plan, options.RunOptions)
		attempt := Attempt{Number: attemptNumber, Report: attemptReport}
		report.Attempts = append(report.Attempts, attempt)
		report.Summary = attemptReport.Summary
		if attemptReport.OK {
			report.OK = true
			break
		}
		if attemptNumber < maxAttempts && options.OnFailure != nil {
			if err := options.OnFailure(ctx, attempt); err != nil {
				report.Error = redaction.RedactString(err.Error(), redaction.Options{})
				break
			}
		}
	}
	report.EndedAt = formatTime(now())
	return report
}

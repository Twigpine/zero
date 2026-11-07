package selfverify

import (
	"github.com/Twigpine/zero/internal/redaction"
	"github.com/Twigpine/zero/internal/verify"
)

const LoopContractVersion = "zero.selfverify.report.v1"

type EventType string

const (
	EventLoopStarted     EventType = "selfverify_started"
	EventAttemptFinished EventType = "selfverify_attempt_finished"
	EventLoopFinished    EventType = "selfverify_finished"
)

type LoopSnapshot struct {
	Contract   string            `json:"contract"`
	Runtime    string            `json:"runtime"`
	Root       string            `json:"root,omitempty"`
	StartedAt  string            `json:"startedAt"`
	EndedAt    string            `json:"endedAt"`
	OK         bool              `json:"ok"`
	StopReason StopReason        `json:"stopReason"`
	Summary    verify.Summary    `json:"summary"`
	Attempts   []AttemptSnapshot `json:"attempts"`
	Error      string            `json:"error,omitempty"`
	Events     []Event           `json:"events"`
}

type AttemptSnapshot struct {
	Number      int                   `json:"number"`
	Report      verify.ReportSnapshot `json:"report"`
	Remediation *Remediation          `json:"remediation,omitempty"`
}

type Event struct {
	Type        EventType      `json:"type"`
	Runtime     string         `json:"runtime,omitempty"`
	Contract    string         `json:"contract,omitempty"`
	Root        string         `json:"root,omitempty"`
	Attempt     int            `json:"attempt,omitempty"`
	Status      verify.Status  `json:"status,omitempty"`
	OK          bool           `json:"ok,omitempty"`
	StopReason  StopReason     `json:"stopReason,omitempty"`
	Summary     verify.Summary `json:"summary,omitempty"`
	StartedAt   string         `json:"startedAt,omitempty"`
	EndedAt     string         `json:"endedAt,omitempty"`
	Error       string         `json:"error,omitempty"`
	Remediation *Remediation   `json:"remediation,omitempty"`
}

func SnapshotFromReport(report Report) LoopSnapshot {
	attempts := make([]AttemptSnapshot, 0, len(report.Attempts))
	for _, attempt := range report.Attempts {
		attempts = append(attempts, AttemptSnapshot{
			Number:      attempt.Number,
			Report:      verify.SnapshotFromReport(attempt.Report),
			Remediation: redactRemediationPointer(attempt.Remediation),
		})
	}
	return LoopSnapshot{
		Contract:   LoopContractVersion,
		Runtime:    verify.RuntimeGo,
		Root:       redactContractString(report.Root),
		StartedAt:  redactContractString(report.StartedAt),
		EndedAt:    redactContractString(report.EndedAt),
		OK:         report.OK,
		StopReason: report.StopReason,
		Summary:    report.Summary,
		Attempts:   attempts,
		Error:      redactContractString(report.Error),
		Events:     EventsFromReport(report),
	}
}

func EventsFromReport(report Report) []Event {
	events := []Event{
		{
			Type:      EventLoopStarted,
			Runtime:   verify.RuntimeGo,
			Contract:  LoopContractVersion,
			Root:      redactContractString(report.Root),
			StartedAt: redactContractString(report.StartedAt),
			Summary:   report.Summary,
		},
	}
	for _, attempt := range report.Attempts {
		status := verify.StatusFail
		if attempt.Report.OK {
			status = verify.StatusPass
		} else if attempt.Report.Summary.Errors > 0 {
			status = verify.StatusError
		}
		events = append(events, Event{
			Type:        EventAttemptFinished,
			Runtime:     verify.RuntimeGo,
			Contract:    LoopContractVersion,
			Root:        redactContractString(report.Root),
			Attempt:     attempt.Number,
			Status:      status,
			OK:          attempt.Report.OK,
			Summary:     attempt.Report.Summary,
			StartedAt:   redactContractString(attempt.Report.StartedAt),
			EndedAt:     redactContractString(attempt.Report.EndedAt),
			Remediation: redactRemediationPointer(attempt.Remediation),
		})
	}
	events = append(events, Event{
		Type:       EventLoopFinished,
		Runtime:    verify.RuntimeGo,
		Contract:   LoopContractVersion,
		Root:       redactContractString(report.Root),
		OK:         report.OK,
		StopReason: report.StopReason,
		Summary:    report.Summary,
		StartedAt:  redactContractString(report.StartedAt),
		EndedAt:    redactContractString(report.EndedAt),
		Error:      redactContractString(report.Error),
	})
	return events
}

func redactRemediationPointer(remediation *Remediation) *Remediation {
	if remediation == nil {
		return nil
	}
	next := *remediation
	next.StartedAt = redactContractString(next.StartedAt)
	next.EndedAt = redactContractString(next.EndedAt)
	next.Message = redactContractString(next.Message)
	next.Error = redactContractString(next.Error)
	return &next
}

func redactContractString(value string) string {
	return redaction.RedactString(value, redaction.Options{})
}

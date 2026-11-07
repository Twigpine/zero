package verify

import (
	"strings"

	"github.com/Twigpine/zero/internal/redaction"
	"github.com/Twigpine/zero/internal/testrunner"
)

const RuntimeGo = "go"
const ReportContractVersion = "zero.verify.report.v1"

type EventType string

const (
	EventVerifyStarted  EventType = "verify_started"
	EventCheckFinished  EventType = "verify_check_finished"
	EventVerifyFinished EventType = "verify_finished"
)

type ReportSnapshot struct {
	Contract  string           `json:"contract"`
	Runtime   string           `json:"runtime"`
	Root      string           `json:"root"`
	StartedAt string           `json:"startedAt"`
	EndedAt   string           `json:"endedAt"`
	OK        bool             `json:"ok"`
	Summary   Summary          `json:"summary"`
	Results   []ResultSnapshot `json:"results"`
	Events    []Event          `json:"events"`
}

type ResultSnapshot struct {
	ID            string               `json:"id"`
	Name          string               `json:"name"`
	Command       []string             `json:"command"`
	Kind          testrunner.Kind      `json:"kind,omitempty"`
	Framework     testrunner.Framework `json:"framework,omitempty"`
	Status        Status               `json:"status"`
	ExitCode      int                  `json:"exitCode"`
	Stdout        string               `json:"stdout,omitempty"`
	Stderr        string               `json:"stderr,omitempty"`
	StartedAt     string               `json:"startedAt"`
	EndedAt       string               `json:"endedAt"`
	DurationMs    int                  `json:"durationMs"`
	Error         string               `json:"error,omitempty"`
	OutputSummary *OutputSummary       `json:"outputSummary,omitempty"`
	TestSummary   *testrunner.Summary  `json:"testSummary,omitempty"`
}

type Event struct {
	Type          EventType            `json:"type"`
	Runtime       string               `json:"runtime,omitempty"`
	Contract      string               `json:"contract,omitempty"`
	Root          string               `json:"root,omitempty"`
	CheckID       string               `json:"checkId,omitempty"`
	CheckName     string               `json:"checkName,omitempty"`
	Command       []string             `json:"command,omitempty"`
	Kind          testrunner.Kind      `json:"kind,omitempty"`
	Framework     testrunner.Framework `json:"framework,omitempty"`
	Status        Status               `json:"status,omitempty"`
	ExitCode      int                  `json:"exitCode,omitempty"`
	OK            bool                 `json:"ok,omitempty"`
	Summary       Summary              `json:"summary,omitempty"`
	StartedAt     string               `json:"startedAt,omitempty"`
	EndedAt       string               `json:"endedAt,omitempty"`
	DurationMs    int                  `json:"durationMs,omitempty"`
	Error         string               `json:"error,omitempty"`
	OutputSummary *OutputSummary       `json:"outputSummary,omitempty"`
	TestSummary   *testrunner.Summary  `json:"testSummary,omitempty"`
}

func SnapshotFromReport(report Report) ReportSnapshot {
	results := make([]ResultSnapshot, 0, len(report.Results))
	for _, result := range report.Results {
		results = append(results, SnapshotFromResult(result))
	}
	return ReportSnapshot{
		Contract:  ReportContractVersion,
		Runtime:   RuntimeGo,
		Root:      redact(report.Root),
		StartedAt: redact(report.StartedAt),
		EndedAt:   redact(report.EndedAt),
		OK:        report.OK,
		Summary:   report.Summary,
		Results:   results,
		Events:    EventsFromReport(report),
	}
}

func SnapshotFromResult(result Result) ResultSnapshot {
	return ResultSnapshot{
		ID:            redact(result.ID),
		Name:          redact(result.Name),
		Command:       redactCommand(result.Command),
		Kind:          result.Kind,
		Framework:     result.Framework,
		Status:        result.Status,
		ExitCode:      result.ExitCode,
		Stdout:        redact(result.Stdout),
		Stderr:        redact(result.Stderr),
		StartedAt:     redact(result.StartedAt),
		EndedAt:       redact(result.EndedAt),
		DurationMs:    result.DurationMs,
		Error:         redact(result.Error),
		OutputSummary: redactOutputSummary(result.OutputSummary),
		TestSummary:   redactTestSummary(result.TestSummary),
	}
}

func EventsFromReport(report Report) []Event {
	events := []Event{
		{
			Type:      EventVerifyStarted,
			Runtime:   RuntimeGo,
			Contract:  ReportContractVersion,
			Root:      redact(report.Root),
			StartedAt: redact(report.StartedAt),
			Summary:   report.Summary,
		},
	}
	for _, result := range report.Results {
		events = append(events, Event{
			Type:          EventCheckFinished,
			Runtime:       RuntimeGo,
			Contract:      ReportContractVersion,
			Root:          redact(report.Root),
			CheckID:       redact(result.ID),
			CheckName:     redact(result.Name),
			Command:       redactCommand(result.Command),
			Kind:          result.Kind,
			Framework:     result.Framework,
			Status:        result.Status,
			ExitCode:      result.ExitCode,
			StartedAt:     redact(result.StartedAt),
			EndedAt:       redact(result.EndedAt),
			DurationMs:    result.DurationMs,
			Error:         redact(result.Error),
			OutputSummary: redactOutputSummary(result.OutputSummary),
			TestSummary:   redactTestSummary(result.TestSummary),
		})
	}
	events = append(events, Event{
		Type:      EventVerifyFinished,
		Runtime:   RuntimeGo,
		Contract:  ReportContractVersion,
		Root:      redact(report.Root),
		OK:        report.OK,
		Summary:   report.Summary,
		StartedAt: redact(report.StartedAt),
		EndedAt:   redact(report.EndedAt),
	})
	return events
}

func redactOutputSummary(summary *OutputSummary) *OutputSummary {
	if summary == nil {
		return nil
	}
	next := *summary
	next.Lines = append([]string{}, summary.Lines...)
	for index := range next.Lines {
		next.Lines[index] = redact(next.Lines[index])
	}
	return &next
}

func redactTestSummary(summary *testrunner.Summary) *testrunner.Summary {
	if summary == nil {
		return nil
	}
	next := *summary
	next.Failures = append([]testrunner.Failure{}, summary.Failures...)
	for index := range next.Failures {
		next.Failures[index].Name = redact(next.Failures[index].Name)
		next.Failures[index].File = redact(next.Failures[index].File)
		next.Failures[index].Message = redact(next.Failures[index].Message)
	}
	return &next
}

func redactCommand(command []string) []string {
	if len(command) == 0 {
		return []string{}
	}
	next := append([]string{}, command...)
	for index := range next {
		next[index] = redact(next[index])
	}
	return next
}

func redact(value string) string {
	if strings.TrimSpace(value) == "" {
		return value
	}
	return redaction.RedactString(value, redaction.Options{})
}

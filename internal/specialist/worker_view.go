package specialist

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/sessions"
)

// What this session has set going, and what it cost.
//
// A VIEW, NOT A STORE, and that is not a stylistic preference. ARCHITECTURE.md
// scopes the "no new store" rule to exactly this kind of state, and the reason
// is that two records of one fact eventually disagree — a registry file would
// have to be written on every dispatch, kept correct across crashes, resumes and
// background completions, and reconciled with the event log that already knows.
// Everything below is a fold over events that are ALREADY WRITTEN by
// accounting.go; nothing new is recorded, so there is nothing new to keep true.
//
// WHY IT IS WORTH HAVING. A session that has spawned Task children, plan tasks
// and background plans has no single answer to "what did this session start, and
// what did it cost". A measured run spawned one background child and the only
// way to learn its fate was to read events.jsonl by hand.
//
// MULTI-PROVIDER RULE: tokens are the one number EVERY provider reports, and
// cost is not — most of this catalogue's gateways report no rates at all. So
// this reports tokens always and says plainly how many workers it could not
// price, rather than presenting a total that silently omits them.

// WorkerKind distinguishes what started a child.
type WorkerKind string

const (
	// WorkerTask is a direct Task call — the parent delegating one job.
	WorkerTask WorkerKind = "task"
	// WorkerPlanTask is one task of an orchestrated plan.
	WorkerPlanTask WorkerKind = "plan-task"
)

// WorkerStatus is where a child got to.
type WorkerStatus string

const (
	WorkerRunning   WorkerStatus = "running"
	WorkerCompleted WorkerStatus = "completed"
	WorkerFailed    WorkerStatus = "failed"
)

// Worker is one child this session started.
type Worker struct {
	SessionID   string
	Kind        WorkerKind
	Specialist  string
	Description string
	Status      WorkerStatus
	Background  bool
	Model       string
	StartedAt   time.Time
	EndedAt     time.Time
	ExitCode    int
	Err         string
	// Tokens is what it spent. 0 means the provider reported no usage, which is
	// a real and common answer — not an assertion that it was free.
	Tokens int
	// TokensReported distinguishes "spent nothing" from "nobody said". A provider
	// that never emits usage cannot be budgeted by token count, and a view that
	// showed 0 for it would be inventing a measurement.
	TokensReported bool
}

// Duration is how long it ran, or how long it has been running.
func (w Worker) Duration(now time.Time) time.Duration {
	if w.StartedAt.IsZero() {
		return 0
	}
	if w.EndedAt.IsZero() {
		return now.Sub(w.StartedAt)
	}
	return w.EndedAt.Sub(w.StartedAt)
}

// WorkerSummary is the session's rollup.
type WorkerSummary struct {
	Workers []Worker
	Started int
	Running int
	Done    int
	Failed  int
	// Tokens totals only the workers that REPORTED usage.
	Tokens int
	// Unmeasured counts workers whose provider reported no usage at all. Named
	// and surfaced rather than folded into the total as zero: a total that
	// silently omits three of five workers reads as the session's whole cost.
	Unmeasured int
}

// ReduceWorkers folds a session's events into what it started.
//
// TOLERANT OF A TRUNCATED OR INTERLEAVED LOG. A background child's usage can
// land after its stop, a stop can be missing entirely if the process was killed,
// and events from several children interleave freely. So this keys everything by
// child session id and never assumes ordering beyond "start precedes stop".
func ReduceWorkers(events []sessions.Event) WorkerSummary {
	byID := map[string]*Worker{}
	var order []string

	get := func(id string) *Worker {
		if worker, ok := byID[id]; ok {
			return worker
		}
		worker := &Worker{SessionID: id, Status: WorkerRunning}
		byID[id] = worker
		order = append(order, id)
		return worker
	}

	for _, event := range events {
		// Payload is json.RawMessage on disk, so it is decoded here rather than
		// assumed to be a map — and a payload that will not decode is skipped
		// rather than failing the whole fold, because one malformed event must
		// not hide every other worker in the session.
		var payload map[string]any
		if len(event.Payload) == 0 {
			continue
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		id := payloadString(payload, "childSessionId")
		if id == "" {
			continue
		}
		switch event.Type {
		case sessions.EventSpecialistStart:
			worker := get(id)
			worker.Specialist = payloadString(payload, "specialist")
			worker.Description = payloadString(payload, "description")
			worker.Background = payloadBool(payload, "background")
			worker.Kind = workerKindOf(payload)
			worker.StartedAt = eventTime(event)
		case sessions.EventSpecialistStop:
			worker := get(id)
			worker.EndedAt = eventTime(event)
			worker.ExitCode = payloadInt(payload, "exitCode")
			worker.Err = payloadString(payload, "error")
			worker.Status = workerStatusOf(payloadString(payload, "status"), worker.ExitCode, worker.Err)
			// A stop may be the first event seen if the log was truncated at the
			// head; fill in what it also carries so the row is not blank.
			if worker.Specialist == "" {
				worker.Specialist = payloadString(payload, "specialist")
			}
			if worker.Description == "" {
				worker.Description = payloadString(payload, "description")
			}
			if worker.Kind == "" {
				worker.Kind = workerKindOf(payload)
			}
		case sessions.EventUsage:
			// USAGE ALONE DOES NOT PROVE A WORKER IS RUNNING. A child whose
			// start and stop were compacted away still has usage on record, and
			// creating a row for it here left a phantom "running" agent that
			// never resolves. Spend is still counted against a worker the log
			// does know about; it just cannot conjure one.
			// ONLY SPECIALIST USAGE. The parent's own turns write EventUsage too,
			// with no childSessionId — filtered above — and counting those would
			// report the session's whole spend as its sub-agents'.
			if payloadString(payload, "source") != specialistAccountingSource {
				continue
			}
			worker, known := byID[id]
			if !known {
				continue
			}
			if total, ok := payloadIntOK(payload, "totalTokens"); ok {
				worker.Tokens += total
				worker.TokensReported = true
			}
			if model := payloadString(payload, "model"); model != "" {
				worker.Model = model
			}
		}
	}

	summary := WorkerSummary{}
	for _, id := range order {
		worker := *byID[id]
		if worker.Kind == "" {
			worker.Kind = WorkerTask
		}
		summary.Workers = append(summary.Workers, worker)
		summary.Started++
		switch worker.Status {
		case WorkerCompleted:
			summary.Done++
		case WorkerFailed:
			summary.Failed++
		default:
			summary.Running++
		}
		if worker.TokensReported {
			summary.Tokens += worker.Tokens
		} else {
			summary.Unmeasured++
		}
	}
	// Newest first: a session's most recent work is what a reader is asking
	// about. Ties keep insertion order so the same log renders the same way.
	sort.SliceStable(summary.Workers, func(i, j int) bool {
		return summary.Workers[i].StartedAt.After(summary.Workers[j].StartedAt)
	})
	return summary
}

// workerKindOf reads the kind from what the dispatcher recorded. A plan task's
// description is written by plan_runner as "plan task <id>", which is the only
// signal the payload carries today — so this reads it rather than inventing a
// field, and defaults to a plain Task, which is what an unrecognised child is.
func workerKindOf(payload map[string]any) WorkerKind {
	if strings.HasPrefix(payloadString(payload, "description"), planTaskDescriptionPrefix) {
		return WorkerPlanTask
	}
	return WorkerTask
}

// workerStatusOf maps a recorded stop onto a status.
//
// STRUCTURAL, never message matching. The status string and the exit code are
// what the writer recorded; a non-zero exit or a recorded error is a failure
// whatever words came with it.
func workerStatusOf(status string, exitCode int, errText string) WorkerStatus {
	if exitCode != 0 || strings.TrimSpace(errText) != "" {
		return WorkerFailed
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "success", "ok", "completed":
		return WorkerCompleted
	default:
		return WorkerFailed
	}
}

func payloadString(payload map[string]any, key string) string {
	if value, ok := payload[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func payloadBool(payload map[string]any, key string) bool {
	value, _ := payload[key].(bool)
	return value
}

func payloadInt(payload map[string]any, key string) int {
	value, _ := payloadIntOK(payload, key)
	return value
}

// payloadIntOK reads a number that may have round-tripped through JSON as a
// float64 — which every event that has been written to disk and read back has.
func payloadIntOK(payload map[string]any, key string) (int, bool) {
	switch value := payload[key].(type) {
	case int:
		return value, true
	case int64:
		return int(value), true
	case float64:
		return int(value), true
	default:
		return 0, false
	}
}

// eventTime parses an event's recorded timestamp. Zero when absent or
// unparseable, which Duration already treats as "unknown" rather than as 1970.
func eventTime(event sessions.Event) time.Time {
	stamp, err := time.Parse(time.RFC3339, strings.TrimSpace(event.CreatedAt))
	if err != nil {
		return time.Time{}
	}
	return stamp
}

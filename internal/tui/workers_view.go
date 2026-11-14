package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/Twigpine/zero/internal/specialist"
)

// /workers — what this session has set going.
//
// READ FROM THE EVENT LOG, not from the sidebar's tracker. The tracker holds
// what is on screen: it drops finished agents past their linger and it starts
// empty on a resumed session. The event log is what actually happened, so a
// session resumed after a crash still answers "what did I start, and what did it
// cost". specialist.ReduceWorkers does the fold; this only renders it.

func (m model) workersText() string {
	if m.sessionStore == nil || m.activeSession.SessionID == "" {
		return planControlNotice("warning",
			"This session has no event log, so there is no record of what it started.")
	}
	events, err := m.sessionStore.ReadEvents(m.activeSession.SessionID)
	if err != nil {
		return planControlNotice("warning", "Could not read this session's events: "+err.Error())
	}
	summary := specialist.ReduceWorkers(events)
	if summary.Started == 0 {
		return planControlNotice("info",
			"This session has started no sub-agents. Delegated work and plan tasks both appear here once they run.")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d sub-agent(s): %d running, %d done, %d failed\n",
		summary.Started, summary.Running, summary.Done, summary.Failed)
	// TOKENS ALWAYS, COST NEVER. Tokens are the one number every provider in this
	// catalogue reports; rates are absent for most of its gateways, so a cost
	// figure here would be right for some sessions and silently wrong for the
	// rest. What it CANNOT account for is named instead of hidden.
	fmt.Fprintf(&b, "%s tokens across the ones that reported usage", formatWorkerTokens(summary.Tokens))
	if summary.Unmeasured > 0 {
		fmt.Fprintf(&b, "; %d reported none, so this total does not cover %s",
			summary.Unmeasured, pluralWorkers(summary.Unmeasured))
	}
	b.WriteString("\n\n")

	now := m.now()
	for _, worker := range summary.Workers {
		b.WriteString("  " + workerStatusGlyph(worker.Status) + " ")
		b.WriteString(workerLabel(worker))
		if worker.Background {
			b.WriteString(" · background")
		}
		fmt.Fprintf(&b, "\n      %s", worker.Duration(now).Round(time.Second))
		if worker.TokensReported {
			fmt.Fprintf(&b, " · %s tokens", formatWorkerTokens(worker.Tokens))
		} else {
			b.WriteString(" · usage not reported")
		}
		if worker.Model != "" {
			b.WriteString(" · " + worker.Model)
		}
		if worker.Err != "" {
			b.WriteString("\n      " + worker.Err)
		}
		b.WriteString("\n")
	}
	return planControlNotice("info", strings.TrimRight(b.String(), "\n"))
}

func workerLabel(worker specialist.Worker) string {
	label := strings.TrimSpace(worker.Description)
	if label == "" {
		label = strings.TrimSpace(worker.Specialist)
	}
	if label == "" {
		label = worker.SessionID
	}
	if worker.Kind == specialist.WorkerPlanTask {
		return label
	}
	if specialistName := strings.TrimSpace(worker.Specialist); specialistName != "" && specialistName != label {
		return label + " (" + specialistName + ")"
	}
	return label
}

func workerStatusGlyph(status specialist.WorkerStatus) string {
	switch status {
	case specialist.WorkerCompleted:
		return "✓"
	case specialist.WorkerFailed:
		return "✗"
	default:
		return "•"
	}
}

func pluralWorkers(n int) string {
	if n == 1 {
		return "one of them"
	}
	return fmt.Sprintf("%d of them", n)
}

// formatWorkerTokens groups thousands, because the numbers here run to millions
// and an ungrouped one cannot be read at a glance.
func formatWorkerTokens(n int) string {
	text := fmt.Sprintf("%d", n)
	if n < 0 {
		return text
	}
	var out []byte
	for i, digit := range []byte(text) {
		if i > 0 && (len(text)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, digit)
	}
	return string(out)
}

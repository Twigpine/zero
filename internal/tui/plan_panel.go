// plan_panel.go maintains the current task-plan state for run details and
// step drill-in. The update_plan result itself renders as a transcript
// checklist, while planPanelState preserves per-step timestamps across the
// tool's full-replacement updates for the contextual detail view.
package tui

import (
	"time"

	"github.com/Gitlawb/zero/internal/tools"
)

// planStep is one rendered plan item. The timestamps are preserved across
// update_plan calls (which replace the whole plan each time) by matching on
// content, so a step that flips from in_progress to completed keeps the
// startedAt it was first marked in_progress with.
type planStep struct {
	content     string
	status      string // "pending" | "in_progress" | "completed" | "failed"
	notes       string
	startedAt   time.Time
	completedAt time.Time
}

// planPanelState holds the latest plan state. It lives on the model and is
// synced from the update_plan tool's CurrentPlan() output.
type planPanelState struct {
	steps       []planStep
	completedAt time.Time // set once all steps reach a terminal status
	startedAt   time.Time // set on the first non-empty update
	// frozenAt freezes the live plan clock while the agent is idle (between
	// turns / waiting on the user). Stamped when a run ends; cleared by clear()
	// at the next run's start. While set and idle, an in_progress step's elapsed
	// time stops ticking up instead of counting forever against a yielded turn.
	frozenAt time.Time
}

// updateFromItems syncs the planStep slice from the update_plan tool. The
// tool replaces the entire plan each call, so steps are matched by content
// to preserve start/completion timestamps. Timestamps are filled in for
// newly-transitioned steps: startedAt on first in_progress, completedAt on
// first completed/failed. The panel-level startedAt is stamped on the first
// non-empty update, and completedAt when every step reaches a terminal
// status (and cleared again if the plan becomes incomplete later).
func (s *planPanelState) updateFromItems(items []tools.PlanItem, now time.Time) {
	if len(items) == 0 {
		s.steps = nil
		s.completedAt = time.Time{}
		return
	}

	if s.startedAt.IsZero() {
		s.startedAt = now
	}

	prev := s.steps
	// Consume each prior step at most once so two steps with identical content don't
	// both inherit the SAME prior entry's timestamps; positional order then breaks
	// the tie, giving duplicate-text steps their own start/complete times (L22).
	prevUsed := make([]bool, len(prev))
	// When the plan length is unchanged the model usually edited steps in place
	// (commonly just rewording the in-progress one). Fall back to positional
	// carry-over for any item that didn't content-match, so a reworded step keeps
	// its timers instead of resetting its elapsed clock to zero mid-progress.
	sameCount := len(prev) == len(items)
	next := make([]planStep, 0, len(items))
	for i, item := range items {
		step := planStep{
			content: item.Content,
			status:  item.Status,
			notes:   item.Notes,
		}
		// Carry over timestamps from the first unconsumed prior step with the same content.
		matched := false
		for pi := range prev {
			if !prevUsed[pi] && prev[pi].content == step.content {
				step.startedAt = prev[pi].startedAt
				step.completedAt = prev[pi].completedAt
				prevUsed[pi] = true
				matched = true
				break
			}
		}
		if !matched && sameCount && i < len(prev) && !prevUsed[i] {
			step.startedAt = prev[i].startedAt
			step.completedAt = prev[i].completedAt
			prevUsed[i] = true
		}
		switch step.status {
		case "in_progress":
			// A reworded/carried-over step that became in_progress must not inherit a
			// prior terminal step's completedAt — that would render an old finished
			// duration instead of a live running clock.
			step.completedAt = time.Time{}
			if step.startedAt.IsZero() {
				step.startedAt = now
			}
		case "completed", "failed":
			if step.startedAt.IsZero() {
				step.startedAt = now
			}
			if step.completedAt.IsZero() {
				step.completedAt = now
			}
		default: // pending: never carries a completion timestamp.
			step.completedAt = time.Time{}
		}
		next = append(next, step)
	}
	s.steps = next

	if s.isComplete() {
		if s.completedAt.IsZero() {
			s.completedAt = now
		}
	} else {
		s.completedAt = time.Time{}
	}
}

// clear resets all plan state and timestamps.
func (s *planPanelState) clear() {
	s.steps = nil
	s.completedAt = time.Time{}
	s.startedAt = time.Time{}
	s.frozenAt = time.Time{}
}

// isEmpty reports whether the panel has no steps to show.
func (s planPanelState) isEmpty() bool {
	return len(s.steps) == 0
}

// isComplete reports whether every step has reached a terminal status
// (completed or failed). An empty plan is not complete.
func (s planPanelState) isComplete() bool {
	if len(s.steps) == 0 {
		return false
	}
	for _, step := range s.steps {
		if step.status != "completed" && step.status != "failed" {
			return false
		}
	}
	return true
}

// completeRemaining force-completes the plan when the agent finished the whole
// task but never sent a final update_plan marking the last steps done. It flips
// every non-terminal step to "completed" and backfills any missing timestamps,
// then stamps the panel-level completedAt — the same end state updateFromItems
// produces for a fully-completed plan — so the panel reads "PLAN COMPLETE"
// instead of staying stuck mid-progress. No-op on an empty or already-complete
// plan; a legitimately failed step keeps its "failed" status. Callers must
// invoke this ONLY when the run genuinely finished (no error, no mid-plan yield
// for ask_user/permission/spec-review), since it asserts the remaining work was
// actually done.
func (s *planPanelState) completeRemaining(now time.Time) {
	if len(s.steps) == 0 || s.isComplete() {
		return
	}
	for i := range s.steps {
		switch s.steps[i].status {
		case "completed", "failed":
			// Already terminal: preserve status, just backfill timestamps so the
			// per-step duration doesn't render a zero span.
			if s.steps[i].startedAt.IsZero() {
				s.steps[i].startedAt = now
			}
			if s.steps[i].completedAt.IsZero() {
				s.steps[i].completedAt = now
			}
		default: // "pending" or "in_progress": the agent finished it without reporting it.
			s.steps[i].status = "completed"
			if s.steps[i].startedAt.IsZero() {
				s.steps[i].startedAt = now
			}
			s.steps[i].completedAt = now
		}
	}
	if s.completedAt.IsZero() {
		s.completedAt = now
	}
}

// planNow returns the clock used for live plan durations. While the agent is
// idle (no run in flight, activeRunID == 0) it freezes at the moment the last
// run ended, so an in_progress step left mid-plan when the agent yields (e.g.
// after ask_user) stops ticking up instead of counting forever against a turn
// that is no longer running. During a run it tracks the live clock as before.
func (m model) planNow() time.Time {
	if m.activeRunID == 0 && !m.plan.frozenAt.IsZero() {
		return m.plan.frozenAt
	}
	return m.now()
}

// currentStepContent returns the content of the step the plan is "on": the
// in_progress step, else the first step not yet in a terminal status, else the
// first step. Used by the header so the title names the step actually being
// worked, not always step 1 (which read as a stuck plan).
func currentStepContent(steps []planStep) string {
	for _, step := range steps {
		if step.status == "in_progress" {
			return step.content
		}
	}
	for _, step := range steps {
		if step.status != "completed" && step.status != "failed" {
			return step.content
		}
	}
	if len(steps) > 0 {
		return steps[0].content
	}
	return ""
}

// truncateStep caps s to max runes, appending an ellipsis when truncated.
func truncateStep(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return "…"
	}
	return string(r[:max-1]) + "…"
}

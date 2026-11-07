package tui

import (
	"fmt"
	"strings"
)

// Plan lifecycle messages, posted from the tool goroutine by
// PlanProgressBridge and consumed on the Bubble Tea event loop. Each carries
// runID so the stale-run guard can drop messages from a superseded run, exactly
// like specialistStartMsg.

// planAdmittedMsg announces a validated plan before its first task runs, so a
// plan does not read as a frozen session until the first task finishes.
//
// It carries the SHAPE, not just the count: the panel's whole reason to exist
// beyond a stream of cards is showing that a diamond is a diamond. The shape is
// known at admission — ParsePlan already proved the graph acyclic and computed
// the order — so sending it here means the panel is complete before the first
// task starts rather than assembling itself as tasks finish.
type planAdmittedMsg struct {
	runID      int
	name       string
	taskCount  int
	tasks      []planGraphTask
	tokenLimit int
	// background marks a plan that outlives the run that launched it, so the
	// stale-run guard must not drop its progress.
	background bool
}

// planGraphTask is one node of the admitted graph, in execution order.
type planGraphTask struct {
	id        string
	dependsOn []string
	phase     string
}

// planTaskStartMsg opens a card for a dispatched task. cardKey is a temporary
// id (the child session does not exist yet) reconciled on completion.
type planTaskStartMsg struct {
	runID   int
	taskID  string
	summary string
	cardKey string
	// model is what this task will run on, empty when it inherits the session's.
	// Known at DISPATCH, which is why it rides the start message rather than the
	// done one: a task's model is worth seeing while it is running, not only in
	// the report afterwards.
	model string
	// background marks a plan that outlives the run that launched it, so the
	// stale-run guard must not drop its progress.
	background bool
}

// planPreflightMsg reports work happening BEFORE a plan exists — listing the
// provider's models, probing them, asking the router. Empty status clears it.
//
// NOT A PLAN ROW. There is no plan yet; admission may still refuse one. A row
// would put a task on screen that never runs.
type planPreflightMsg struct {
	runID  int
	status string
}

// planTaskDoneMsg closes a task's card.
//
// dispatched distinguishes a task that ran from one that never started
// (dependency-skipped, budget-skipped, cancelled before dispatch). A task that
// never started has no card, and closing the last dispatched task's card for it
// would mark the wrong task — the specialist-card collision defect in a new
// costume.
type planTaskDoneMsg struct {
	runID      int
	taskID     string
	cardKey    string
	dispatched bool
	sessionID  string
	status     specialistStatus
	outcome    string
	reason     string
	// output is what the task produced, bounded at the bridge. Until this
	// existed the TUI knew a task had finished and what it cost, but never what
	// it actually returned — so a finished agent row could report everything
	// except the thing the user ran it for.
	output string
	// tokens is what the task actually spent. The card omits the segment when
	// it is zero rather than reporting a total nobody measured.
	tokens int
	// attempts is how many times the task ran — more than one when the stall
	// watchdog fired and the executor retried it. Carried so the detail can say
	// why an apparently single run took twice as long as its siblings.
	attempts int
	// model is what the task ACTUALLY ran on, which is not always what it was
	// dispatched with: a model the provider refuses is retried on the session's,
	// and this arrives empty in that case because empty means "the session's".
	model string
	// fellBackFrom names the assigned model that could not run. Carried to the
	// surface a PERSON reads, not only to the report the model reads — otherwise
	// the card goes on claiming a model that never executed the task.
	fellBackFrom string
	// background marks a plan that outlives the run that launched it, so the
	// stale-run guard must not drop its progress.
	background bool
}

// planTaskProgressMsg is one tool call made by ONE task's child, already
// resolved to that task's card. The routing happens at the recorder, which is
// the only place that knows which card belongs to which task.
type planTaskProgressMsg struct {
	runID    int
	taskID   string
	cardKey  string
	toolName string
	detail   string
	// background marks a plan that outlives the run that launched it, so the
	// stale-run guard must not drop its progress.
	background bool
}

// planCompletedMsg carries the plan's terminal record.
type planCompletedMsg struct {
	runID           int
	name            string
	status          string
	succeeded       int
	failed          int
	skipped         int
	cancelled       int
	tokensUsed      int
	tokenLimit      int
	maxSpeedup      float64
	durabilityError string
	// background marks a plan that outlives the run that launched it, so the
	// stale-run guard must not drop its progress.
	background bool
}

// planNoticeLine renders the one-line plan notices shown in the transcript.
// Kept here beside the messages so the wording and the data stay together.
func planAdmittedLine(name string, taskCount int) string {
	label := "tasks"
	if taskCount == 1 {
		label = "task"
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Sprintf("plan: %d %s", taskCount, label)
	}
	return fmt.Sprintf("plan %q: %d %s", name, taskCount, label)
}

func planCompletedLine(msg planCompletedMsg) string {
	var b strings.Builder
	fmt.Fprintf(&b, "plan %s: %d succeeded", msg.status, msg.succeeded)
	if msg.failed > 0 {
		fmt.Fprintf(&b, ", %d failed", msg.failed)
	}
	if msg.skipped > 0 {
		fmt.Fprintf(&b, ", %d skipped", msg.skipped)
	}
	if msg.cancelled > 0 {
		fmt.Fprintf(&b, ", %d cancelled", msg.cancelled)
	}
	if msg.tokenLimit > 0 {
		fmt.Fprintf(&b, " · %d/%d tokens", msg.tokensUsed, msg.tokenLimit)
	}
	if msg.maxSpeedup > 0 {
		fmt.Fprintf(&b, " · max_speedup %.2fx", msg.maxSpeedup)
	}
	if strings.TrimSpace(msg.durabilityError) != "" {
		fmt.Fprintf(&b, " · warning: progress was not fully saved; this plan is not safely resumable (%s)", msg.durabilityError)
	}
	return b.String()
}

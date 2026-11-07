package tui

import "strings"

// Per-plan control: stop, pause and resume the PLAN without stopping the TURN.
//
// Before this, abandoning a twenty-task plan meant Ctrl-C, which cancels the
// whole run and takes the conversation with it. The two are different
// intentions and now have different acts.
//
// WHY A SLASH COMMAND rather than the reference's bare p/x/r keys. Bare letters
// go to the composer here, and almost every ctrl chord is already bound —
// ctrl+g alone had to dodge ctrl+o (detailed transcript) and ctrl+p (the
// update_plan panel). More importantly, only commandPrompt is queued while a
// run is pending; a slash command is dispatched immediately, which is the whole
// requirement for "stop the plan that is running right now".
//
// NOT BUILT, with reasons, both from the same gap-report item:
//
//   - RESTART. Re-running a plan means re-issuing the tool call with the same
//     arguments, and nothing stores them — the panel holds a rendering of the
//     plan, not the plan. That is the persistence seam named plans (§5.7) is
//     for, and faking it from the panel's copy would produce a plan that only
//     resembles the one that ran.
//   - FILTER. A 50-task plan needs SCROLLING, which is the row cap, not a
//     predicate. Filtering a list the user cannot see all of solves the second
//     problem first.
const (
	planControlStop   = "stop"
	planControlPause  = "pause"
	planControlResume = "resume"
)

// orchestrateControlText handles `/plans`, `/plans stop|pause|resume`.
//
// Bare `/plans` keeps its old behaviour exactly — the graph — so the command
// that existed before this does not change under anyone.
func (m model) orchestrateControlText(args string) string {
	verb := strings.ToLower(strings.TrimSpace(args))
	switch verb {
	case "":
		return m.orchestratePlansText() + m.planControlHint()
	case planControlStop:
		if !m.planProgress.StopPlan() {
			return planControlNotice("warning", "No plan is running, so there is nothing to stop.")
		}
		// The tasks already finished KEEP their results: the executor records the
		// remainder as cancelled rather than failed, and a stopped plan reports
		// as cancelled or partial, never as a broken one.
		return planControlNotice("info",
			"Stopping the plan. The task in flight is cancelled, the rest are recorded as cancelled, "+
				"and finished tasks keep their results. The turn itself is untouched.")
	case planControlPause:
		if !m.planProgress.SetPlanPaused(true) {
			return planControlNotice("warning", "No plan is running, so there is nothing to pause.")
		}
		// SAY WHERE IT TAKES EFFECT. A child already talking to a provider
		// cannot be suspended, so a pause that claimed to be immediate would be
		// claiming tokens had stopped being spent when they had not.
		return planControlNotice("info",
			"Pausing at the next task boundary. The task in flight runs to completion — a child mid-request "+
				"cannot be suspended. Resume with /plans resume, or abandon it with /plans stop.")
	case planControlResume:
		if !m.planProgress.SetPlanPaused(false) {
			return planControlNotice("warning", "No plan is running, so there is nothing to resume.")
		}
		return planControlNotice("info", "Resuming the plan.")
	default:
		return planControlNotice("warning",
			"Unknown: /plans "+verb+"\nUse /plans on its own for the task graph, "+
				"/plans stop | pause | resume for the running plan, "+
				"or /plans save <name> | list | show <name> | run <name> | restart for saved ones.")
	}
}

// planControlHint appends the verbs to the graph, and ONLY while a plan is
// actually running: advertising "stop" against a finished plan is an offer the
// next line refuses.
func (m model) planControlHint() string {
	if !m.planProgress.PlanRunningNow() {
		return ""
	}
	if m.planProgress.PlanPaused() {
		return "\n\npaused at a task boundary · /plans resume to continue · /plans stop to abandon"
	}
	return "\n\n/plans pause to hold at the next task · /plans stop to abandon the plan (the turn keeps going)"
}

func planControlNotice(status, body string) string {
	return "Plans\nstatus: " + status + "\n" + body
}

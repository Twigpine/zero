package specialist

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Twigpine/zero/internal/sessions"
)

// Resume: what a plan had already done when the process went away.
//
// This is the READ half of durability. The write half put the five lifecycle
// events in the session log from both surfaces; this reduces them back into
// plan state. ARCHITECTURE.md's rule is the shape of the whole thing — plan
// state is a DETERMINISTIC REDUCTION over the five events, and there is no
// second store to disagree with.
//
// WHAT THE EVENTS DO AND DO NOT CARRY. The payloads are deliberately small: ids,
// counts, durations, terminal status — not prompts. So the log says which tasks
// finished; it cannot say what they were asked to do. That is why resuming is
// built on a SAVED plan: the plan supplies the work, the log supplies the
// progress, and neither has to grow into the other. Putting prompts in the log
// would have multiplied a fifty-task plan's on-disk size to make the log a
// second copy of something already stored better.
//
// A DISPATCH WITHOUT A TERMINAL EVENT IS THE POINT. Dispatch is written before
// the child runs, so a task that was in flight when the process died is
// distinguishable from one that never started — and it is treated as UNFINISHED,
// never as done. Assuming otherwise would silently drop the one task most likely
// to have been interrupted mid-work.

// PlanProgress is the state of one plan, reduced from its events.
type PlanProgress struct {
	// Name is the plan's name from plan_admitted.
	Name string
	// Order is the execution order recorded at admission.
	Order []string
	// Succeeded holds the ids that reached task_completed.
	Succeeded []string
	// Unfinished holds ids that were dispatched and never reached a terminal
	// event — the process went away mid-task.
	Unfinished []string
	// Failed holds ids that reached task_failed, whatever the outcome. A failed
	// task is worth RE-RUNNING: unlike a success it produced no work to keep.
	Failed []string
	// Outputs holds the BOUNDED output each succeeded task recorded, so a
	// resumed dependent can be briefed on what its completed dependency found —
	// see RemainingPlan. Empty for a task that produced nothing, or one recorded
	// before outputs were stored.
	Outputs map[string]string
	// Identities holds the fingerprint each succeeded task recorded when it ran,
	// so RemainingPlan can tell a completed task that is UNCHANGED from one whose
	// prompt, model, tools or dependencies were edited since. Absent for a task
	// recorded before resume became identity-aware — which RemainingPlan treats
	// as "unknown", matching by id alone exactly as it did before.
	Identities map[string]string
	// Complete reports that a plan_completed event was seen, so the plan ended
	// on purpose rather than being cut off.
	Complete bool
	// Status is the terminal status from plan_completed, empty if there was none.
	Status string
}

// ReducePlanEvents folds a session's events into the state of its LAST plan.
//
// The last one, deliberately: a session can run several plans, and "resume"
// means the one that was interrupted, which is the most recent. plan_admitted
// RESETS the accumulated state rather than merging into it, so a second plan
// never inherits the first plan's completed tasks — which would mark work as
// done that this plan never did.
func ReducePlanEvents(events []sessions.Event) (PlanProgress, bool) {
	var progress PlanProgress
	seen := false

	// Sequence order, not file order: the reduction has to be deterministic and
	// events are only meaningful in the order they happened.
	ordered := append([]sessions.Event(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Sequence < ordered[j].Sequence })

	dispatched := map[string]bool{}
	terminal := map[string]bool{}
	for _, event := range ordered {
		switch event.Type {
		case sessions.EventPlanAdmitted:
			var payload struct {
				Name  string   `json:"name"`
				Order []string `json:"order"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil {
				// Malformed is not a silent skip: an admission we cannot read
				// means we do not know this plan's shape, so the previous plan's
				// state must not be reported as if it were this one's.
				return PlanProgress{}, false
			}
			seen = true
			progress = PlanProgress{Name: payload.Name, Order: payload.Order}
			dispatched = map[string]bool{}
			terminal = map[string]bool{}
		case sessions.EventTaskDispatched:
			if id := planEventTaskID(event); id != "" {
				dispatched[id] = true
			}
		case sessions.EventTaskCompleted:
			if id := planEventTaskID(event); id != "" {
				terminal[id] = true
				progress.Succeeded = append(progress.Succeeded, id)
				if out := planEventOutput(event); out != "" {
					if progress.Outputs == nil {
						progress.Outputs = map[string]string{}
					}
					progress.Outputs[id] = out
				}
				if identity := planEventIdentity(event); identity != "" {
					if progress.Identities == nil {
						progress.Identities = map[string]string{}
					}
					progress.Identities[id] = identity
				}
			}
		case sessions.EventTaskFailed:
			if id := planEventTaskID(event); id != "" {
				terminal[id] = true
				progress.Failed = append(progress.Failed, id)
			}
		case sessions.EventPlanCompleted:
			var payload struct {
				Status string `json:"status"`
			}
			_ = json.Unmarshal(event.Payload, &payload)
			progress.Complete = true
			progress.Status = payload.Status
		}
	}
	if !seen {
		return PlanProgress{}, false
	}
	// Dispatched with no terminal event: in flight when the process went away.
	for _, id := range progress.Order {
		if dispatched[id] && !terminal[id] {
			progress.Unfinished = append(progress.Unfinished, id)
		}
	}
	return progress, true
}

// planEventOutput reads the bounded task output stored on a task_completed
// event, empty when absent (an older event, or a task that produced nothing).
func planEventOutput(event sessions.Event) string {
	var payload struct {
		Output string `json:"output"`
	}
	_ = json.Unmarshal(event.Payload, &payload)
	return payload.Output
}

// planEventIdentity reads the task fingerprint stored on a task_completed event,
// empty when absent — an event recorded before resume became identity-aware.
func planEventIdentity(event sessions.Event) string {
	var payload struct {
		Identity string `json:"identity"`
	}
	_ = json.Unmarshal(event.Payload, &payload)
	return payload.Identity
}

func planEventTaskID(event sessions.Event) string {
	var payload struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil {
		return ""
	}
	return payload.ID
}

// RemainingPlan narrows a plan to the work that still needs to run.
//
// A task is DONE — removed, and every reference to it removed with it — only
// when it SUCCEEDED and its fingerprint is UNCHANGED since (planDoneSet). A task
// whose prompt, model, tools or dependencies were edited since it ran has a
// different fingerprint, so it is NOT done: it stays in the plan and runs again,
// and so does everything that depends on it, because their input changed. That
// is the difference between resuming and replaying stale work.
//
// Leaving a done task's edge behind would produce a plan whose dependency names
// nothing — ParsePlan refuses that, correctly — so a done dependency is dropped
// and its finding folded into the dependent below. A dependency that is NOT done
// (it re-runs) keeps its edge, so the dependent still waits on the fresh result.
//
// The result goes back through ParsePlan, so a narrowed plan is validated like
// any other: it cannot acquire a tool, a task count or a depth the original did
// not have, and a narrowing that produced a cycle would be caught rather than
// executed.
func RemainingPlan(plan Plan, progress PlanProgress, limits Limits) (Plan, error) {
	done := planDoneSet(plan, progress)

	args := plan.Args()
	rawTasks, _ := args["tasks"].([]any)
	kept := make([]any, 0, len(rawTasks))
	for _, raw := range rawTasks {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := entry["id"].(string)
		if done[id] {
			continue
		}
		if deps, ok := entry["depends_on"].([]any); ok {
			remaining := make([]any, 0, len(deps))
			var completed []string
			for _, dep := range deps {
				name, _ := dep.(string)
				if done[name] {
					// A DEPENDENCY THAT IS DONE is not dropped silently: its finding
					// is what this task was meant to build on, and after a resume the
					// live results are gone. Its output is folded into this task's
					// prompt below so the work survives the interruption instead of
					// being invisible to the task that needed it. A dependency that
					// is NOT done stays in the edge list — it re-runs, and this task
					// must wait for its fresh result, not a stale briefing.
					completed = append(completed, name)
					continue
				}
				remaining = append(remaining, dep)
			}
			if len(remaining) == 0 {
				delete(entry, "depends_on")
			} else {
				entry["depends_on"] = remaining
			}
			if brief := resumeDependencyBrief(completed, progress.Outputs); brief != "" {
				entry["prompt"] = brief + planString(entry, "prompt")
			}
		}
		kept = append(kept, entry)
	}
	if len(kept) == 0 {
		return Plan{}, fmt.Errorf("every task in plan %q already succeeded; there is nothing left to run", plan.Name())
	}
	args["tasks"] = kept
	return ParsePlan(args, limits)
}

// planDoneSet reports which tasks need not run again: those that SUCCEEDED with a
// fingerprint UNCHANGED since they ran, AND whose dependencies are all likewise
// done. The dependency clause is what makes an edit cascade — a task that is
// itself unchanged but depends on an edited one is NOT done, because the edited
// dependency will produce a different result to build on.
//
// Computed over plan.Order(), which is topological: a task is visited only after
// its dependencies, so their done-ness is settled before this one is decided. A
// single forward pass is therefore enough to close "done" under the graph.
func planDoneSet(plan Plan, progress PlanProgress) map[string]bool {
	succeeded := map[string]bool{}
	for _, id := range progress.Succeeded {
		succeeded[id] = true
	}
	byID := map[string]Task{}
	for _, task := range plan.Tasks() {
		byID[task.ID] = task
	}

	done := map[string]bool{}
	for _, id := range plan.Order() {
		task, ok := byID[id]
		if !ok {
			continue
		}
		if !succeeded[id] || !taskUnchanged(task, progress) {
			continue // never finished, or edited since — must run
		}
		allDepsDone := true
		for _, dep := range task.DependsOn {
			if !done[dep] {
				allDepsDone = false
				break
			}
		}
		if allDepsDone {
			done[id] = true
		}
	}
	return done
}

// ResumeChangedTasks returns the ids of tasks that SUCCEEDED in the prior run but
// will run again on resume because they — or a task they depend on — were edited
// since. It is empty for an ordinary resume; a resume notice uses it to explain
// why a task the user believes finished is back in the plan.
//
// Returned in execution order, so the explanation reads the way the plan runs.
func ResumeChangedTasks(plan Plan, progress PlanProgress) []string {
	done := planDoneSet(plan, progress)
	succeeded := map[string]bool{}
	for _, id := range progress.Succeeded {
		succeeded[id] = true
	}
	var changed []string
	for _, id := range plan.Order() {
		if succeeded[id] && !done[id] {
			changed = append(changed, id)
		}
	}
	return changed
}

// taskUnchanged reports whether a task's current fingerprint matches the one it
// recorded when it ran.
//
// A task with NO recorded identity — one completed before resume became
// identity-aware — is treated as UNCHANGED, matched by id alone exactly as
// resume behaved before this existed. Treating an absent identity as a mismatch
// would make the first resume after upgrading re-run every completed task, which
// is the opposite of what a resume is for.
func taskUnchanged(task Task, progress PlanProgress) bool {
	recorded, ok := progress.Identities[task.ID]
	if !ok {
		return true
	}
	return recorded == taskIdentity(task)
}

// resumeDependencyBrief prefixes a resumed task with what its ALREADY-COMPLETED
// dependencies found, so a plan cut short mid-run does not lose the findings the
// remaining tasks were meant to build on.
//
// Deterministic order (the depends_on order the plan declared), so a resumed
// plan produces the same prompt twice. A completed dependency with no stored
// output — an older event, or a task that produced nothing — contributes a
// heading noting it completed, never a silent gap that reads as "it found
// nothing".
func resumeDependencyBrief(completed []string, outputs map[string]string) string {
	if len(completed) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## What the tasks you depend on found in an earlier run\n\n")
	for _, id := range completed {
		fmt.Fprintf(&b, "### Result of task %q (completed before this run)\n", id)
		if out := strings.TrimSpace(outputs[id]); out != "" {
			b.WriteString(out)
			b.WriteString("\n")
		} else {
			b.WriteString("(completed, but its output was not recorded for resume)\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("Use this instead of rediscovering it. Verify anything you rely on — a result above is a previous run's conclusion, not established fact.\n\n## Your task\n\n")
	return b.String()
}

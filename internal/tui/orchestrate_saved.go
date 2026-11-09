package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/pathjail"
	"github.com/Twigpine/zero/internal/specialist"
)

// Saved plans, from the TUI: save the plan that just ran, list what is stored,
// read one back, and run one again.
//
// RUNNING ONE GOES THROUGH THE MODEL, not straight into the tool. `/plans run x`
// dispatches an ordinary prompt, the model calls orchestrate with `saved: "x"`,
// and the plan is re-admitted by the same ParsePlan every other plan goes
// through. A command that reached into tool execution directly would be a
// SECOND way to run a plan, with its own copy of the posture gate, the depth
// check, the grant intersection and the recorder wiring — and this feature's
// entire defect history is second call paths that carry less than the first.

// handlePlansCommand dispatches `/plans [verb] [args]`.
//
// Split from orchestrateControlText because `run` has to start a turn, which
// needs a tea.Cmd; the rest only produce text.
func (m model) handlePlansCommand(args string) (tea.Model, tea.Cmd) {
	verb, rest := splitPlansArgs(args)
	switch verb {
	case "save":
		return m.appendPlansNotice(m.savePlanText(rest)), nil
	case "list":
		return m.appendPlansNotice(m.savedPlansText()), nil
	case "show":
		return m.appendPlansNotice(m.showSavedPlanText(rest)), nil
	case "run":
		return m.runSavedPlan(rest, false)
	case "restart":
		return m.restartLastPlan()
	case "resume":
		// TWO MEANINGS OF ONE WORD, split by arity, and they are the same
		// intention at two scales: continue what was interrupted.
		//
		//   /plans resume         → un-pause the plan running right now
		//   /plans resume <name>  → pick up a saved plan where its last run
		//                           stopped
		//
		// Bare resume keeps un-pausing a LIVE plan; with no plan running it now
		// resumes the last one from where it stopped — the pause control shipped
		// before this still fires whenever a plan is actually in flight.
		if strings.TrimSpace(rest) == "" {
			// A LIVE plan un-pauses; a plan that was CANCELLED resumes from where
			// it stopped. Both are "continue what was interrupted", and which one
			// applies is decided by whether a plan is actually running now — not
			// by the user having to remember two spellings.
			if m.planProgress.PlanRunningNow() {
				return m.appendPlansNotice(m.orchestrateControlText(args)), nil
			}
			return m.resumeLastPlan()
		}
		return m.runSavedPlan(rest, true)
	default:
		return m.appendPlansNotice(m.orchestrateControlText(args)), nil
	}
}

func (m model) appendPlansNotice(text string) model {
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
	return m
}

func splitPlansArgs(args string) (verb, rest string) {
	fields := strings.Fields(strings.TrimSpace(args))
	if len(fields) == 0 {
		return "", ""
	}
	return strings.ToLower(fields[0]), strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), fields[0]))
}

// savePlanText writes the plan that ran this session under the given name.
//
// It saves to the PROJECT directory, and that is the useful default: a plan is a
// piece of team knowledge about a repo — "the pre-release sweep" — not a
// personal preference. It is also the scope a user can see and review in a diff.
func (m model) savePlanText(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return planControlNotice("warning", "Name it: /plans save <name>")
	}
	args, planName, ok := m.planProgress.LastPlan()
	if !ok {
		return planControlNotice("warning",
			"No plan has run this session, so there is nothing to save. Run one first — a saved plan is a plan that worked.")
	}
	root, dir := m.planWriteTarget()
	if dir == "" {
		return planControlNotice("warning", "There is nowhere to save plans in this run.")
	}
	// This converts args back into a Plan because SavePlan takes a Plan: its
	// signature is what makes "only a validated plan can be written" true by
	// construction rather than by convention.
	//
	// THE ERROR BRANCH IS UNREACHABLE FROM HERE and is not pretending otherwise.
	// These args came from a Plan that ParsePlan already admitted, and
	// savedPlanLimits is deliberately wider than any run's, so nothing can
	// reject them. It is handled because ignoring a returned error is how a
	// reachable version of this goes wrong later — not because it guards
	// something today. The mutation sweep is what forced this to be stated: no
	// test can kill this branch, and a comment claiming it protects the file on
	// disk would have been the third wrong-mechanism comment in this feature.
	//
	// The branch that DOES fire is on the way back in — showSavedPlanText and
	// the tool's own load — where a hand-edited file is re-admitted and can be
	// refused.
	plan, err := specialist.ParsePlan(args, m.savedPlanLimits())
	if err != nil {
		return planControlNotice("warning", "That plan no longer validates, so it was not saved: "+err.Error())
	}
	path, err := specialist.SavePlan(root, dir, name, plan)
	if err != nil {
		return planControlNotice("warning", "Could not save the plan: "+err.Error())
	}
	label := planName
	if label == "" {
		label = "the last plan"
	}
	return planControlNotice("info", fmt.Sprintf(
		"Saved %s as %q (%d tasks)\n%s\n\nRun it again with /plans run %s.", label, name, plan.TaskCount(), path, name))
}

// savedPlanLimits are the limits used to re-admit a plan for SAVING and for
// SHOWING — deliberately permissive on the tool grant, because saving is not
// running. The grant that matters is the one applied when the plan is actually
// executed, by the tool, against the run that runs it.
//
// MaxTasks is left unset for the same reason: refusing to save a plan that
// already ran, because the tier moved since, would be refusing to record
// history.
func (m model) savedPlanLimits() specialist.Limits {
	// GRANTABLE, not read-only. These limits parse a plan that is being saved,
	// shown, restarted or resumed — plans that already ran, or are about to run
	// through the ordinary path with its own approval gate. Granting only the
	// read-only names here meant a plan that ParsePlan accepts at run time
	// (write tools may be named per task) failed to parse on every /plans verb,
	// so the entire durability surface silently excluded exactly the plans whose
	// work is most worth keeping.
	//
	// This widens no authority: the parent grant is intersected again when the
	// plan actually runs, and a write-capable plan still needs its approval and
	// its isolated worktree.
	return specialist.Limits{ParentTools: specialist.PlanGrantableToolNames()}
}

func (m model) savedPlansText() string {
	plans, problems := specialist.LoadPlans(m.planPaths)
	if len(plans) == 0 && len(problems) == 0 {
		return planControlNotice("info",
			"No saved plans.\nRun a plan, then keep it with /plans save <name>.")
	}
	// The BUNDLED plans do not count as "yours". A listing showing only the
	// shipped example while saying nothing about saving would read as though
	// the user already had plans of their own.
	saved := 0
	for _, plan := range plans {
		if plan.Scope != specialist.PlanScopeBuiltin {
			saved++
		}
	}
	var b strings.Builder
	b.WriteString("Plans\nstatus: info\n")
	if len(plans) == 0 {
		b.WriteString("No readable saved plans.")
	}
	for _, plan := range plans {
		fmt.Fprintf(&b, "  %-20s %2d tasks  %-8s", plan.Name, plan.TaskCount, plan.Scope)
		if plan.Description != "" {
			b.WriteString("  · " + plan.Description)
		}
		b.WriteString("\n")
	}
	// Unreadable files are NAMED. "No saved plans" while three sit on disk
	// unparseable is a lie by omission, and the user is the only one who can fix
	// the file.
	if len(problems) > 0 {
		b.WriteString("\ncould not be read:\n")
		for _, problem := range problems {
			b.WriteString("  " + problem + "\n")
		}
	}
	if saved == 0 {
		b.WriteString("\nNothing saved yet — run a plan, then keep it with /plans save <name>.")
	}
	b.WriteString("\n/plans show <name> to read one · /plans run <name> to run it")
	return strings.TrimRight(b.String(), "\n")
}

func (m model) showSavedPlanText(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return planControlNotice("warning", "Name it: /plans show <name>")
	}
	stored, err := specialist.FindSavedPlan(m.planPaths, name)
	if err != nil {
		return planControlNotice("warning", err.Error())
	}
	// Shown through ParsePlan, so what is displayed is what would RUN — including
	// the execution order, which is the part a stored task list does not show.
	plan, err := specialist.ParsePlan(stored.Args, m.savedPlanLimits())
	if err != nil {
		return planControlNotice("warning", fmt.Sprintf("%s does not validate: %v", stored.Path, err))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Plans\nstatus: info\n%s · %d tasks · %s\n", name, plan.TaskCount(), stored.Path)
	if plan.Description() != "" {
		b.WriteString(plan.Description() + "\n")
	}
	byID := map[string]specialist.Task{}
	for _, task := range plan.Tasks() {
		byID[task.ID] = task
	}
	for _, id := range plan.Order() {
		task := byID[id]
		fmt.Fprintf(&b, "\n  %s", id)
		if len(task.DependsOn) > 0 {
			deps := append([]string(nil), task.DependsOn...)
			sort.Strings(deps)
			b.WriteString("  after " + strings.Join(deps, ", "))
		}
		if summary := planTaskSummaryLine(task.Prompt); summary != "" {
			b.WriteString("\n      " + summary)
		}
	}
	return b.String()
}

// planTaskSummaryLine is the first line of a prompt, bounded. The full prompt
// stays in the file — a display surface must not become the data path.
func planTaskSummaryLine(prompt string) string {
	// THE SAME SANITIZER THE LIVE PATH USES (planTaskSummary →
	// sanitizeCardText). A saved plan's prompts are model-authored text read
	// back from disk — the same trust level as a live task's — and this line
	// only cut at the first newline, so control characters (ESC included, the
	// byte that starts an ANSI sequence) reached the display. Untrusted input
	// is untrusted on every path it enters by.
	return truncateRunes(sanitizeCardText(prompt), planTaskSummaryWidth)
}

// runSavedPlan asks the MODEL to run it. See the note at the top of this file:
// a command that called the tool directly would be a second execution path.
//
// resume narrows the plan to the work the session log says has not succeeded.
func (m model) runSavedPlan(args string, resume bool) (tea.Model, tea.Cmd) {
	verb := "run"
	if resume {
		verb = "resume"
	}
	name, values := splitPlanTarget(args)
	if name == "" {
		return m.appendPlansNotice(planControlNotice("warning", "Name it: /plans "+verb+" <name>")), nil
	}
	// Resolved HERE so an unknown name is a plain refusal rather than a turn
	// spent discovering the plan does not exist.
	stored, err := specialist.FindSavedPlan(m.planPaths, name)
	if err != nil {
		return m.appendPlansNotice(planControlNotice("warning", err.Error())), nil
	}
	// A PARAMETERISED PLAN IS REFUSED HERE, for the same reason an unknown name
	// is: expandPlanParams would refuse it too, but only after a turn and a model
	// call have been spent reaching admission. The bundled research plan is five
	// child agents, so the difference between refusing here and refusing there is
	// five sub-agent runs against a placeholder.
	paramsArg, err := savedPlanParamsArg(stored, values, verb)
	if err != nil {
		return m.appendPlansNotice(planControlNotice("warning", err.Error())), nil
	}

	target := name
	instruction := "Run the saved plan %s by calling the orchestrate tool with the `saved` argument set to that name. Do not restate its tasks."
	if resume {
		remaining, notice, ok := m.resumeSavedPlan(stored)
		if !ok {
			return m.appendPlansNotice(notice), nil
		}
		target = remaining
		instruction = "Resume the plan by calling the orchestrate tool with the `saved` argument set to %s. " +
			"That is the saved plan narrowed to the tasks that had not finished. Do not restate its tasks."
		m = m.appendPlansNotice(notice)
	}
	encoded, err := json.Marshal(target)
	if err != nil {
		return m.appendPlansNotice(planControlNotice("warning", "Could not reference that plan: "+err.Error())), nil
	}
	// CONCATENATED, never interpolated. paramsArg carries the user's own words,
	// and a subject containing a percent sign would be read as a verb by Sprintf
	// and rewritten into the instruction as "%!s(MISSING)".
	text := fmt.Sprintf(instruction, string(encoded))
	if paramsArg != "" {
		text += " " + paramsArg
	}
	return m.dispatchCommand(parsedCommand{
		kind: commandPrompt,
		text: text,
	})
}

// splitPlanTarget separates a saved plan's name from the text that follows it.
//
// NOT splitPlansArgs: that lowercases its first field so a verb matches whatever
// case it was typed in, and a plan name is compared exactly — lowercasing one
// would make /plans run Sweep report that no plan named "sweep" exists.
func splitPlanTarget(args string) (name, rest string) {
	trimmed := strings.TrimSpace(args)
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "", ""
	}
	return fields[0], strings.TrimSpace(strings.TrimPrefix(trimmed, fields[0]))
}

// savedPlanParamsArg turns the words after a plan's name into the `params`
// object the orchestrate tool takes, or refuses.
//
// FAILS CLOSED IN BOTH DIRECTIONS, matching expandPlanParams: a plan that
// declares a parameter will not run without one, and words supplied to a plan
// that declares none are refused rather than dropped — a user who typed them
// meant them, and silently ignoring them runs a different plan than the one they
// asked for while reporting success.
func savedPlanParamsArg(stored specialist.SavedPlan, values, verb string) (string, error) {
	declared := specialist.PlanParams(stored.Args)
	values = strings.TrimSpace(values)
	switch {
	case len(declared) == 0 && values == "":
		return "", nil
	case len(declared) == 0:
		return "", fmt.Errorf("%q takes no parameters, so %q would be ignored. Run it as /plans %s %s",
			stored.Name, values, verb, stored.Name)
	case values == "":
		return "", fmt.Errorf("%q takes %s. Run it as /plans %s %s <%s>",
			stored.Name, describePlanParams(declared), verb, stored.Name, strings.Join(declared, "> <"))
	}
	// ONE PARAMETER IS RESOLVED HERE, not by the model. The mapping is total —
	// everything after the name is the value — so deciding it in the UI makes the
	// substituted plan a function of what was typed rather than of what a model
	// inferred from it.
	if len(declared) == 1 {
		encoded, err := json.Marshal(map[string]string{declared[0]: values})
		if err != nil {
			return "", fmt.Errorf("could not read that parameter: %w", err)
		}
		return "Set the `params` argument to exactly " + string(encoded) + ".", nil
	}
	// SEVERAL PARAMETERS HAVE NO TOTAL MAPPING from one line of prose, so the
	// model splits it — and is told the exact key set, so it fills those and
	// invents none.
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("could not read those parameters: %w", err)
	}
	return "Set the `params` argument to an object whose keys are exactly " +
		strings.Join(quotePlanParams(declared), ", ") +
		", taking the values from this text: " + string(encoded) + ".", nil
}

func describePlanParams(declared []string) string {
	if len(declared) == 1 {
		return "a parameter: " + declared[0]
	}
	return "parameters: " + strings.Join(declared, ", ")
}

func quotePlanParams(declared []string) []string {
	out := make([]string, 0, len(declared))
	for _, name := range declared {
		out = append(out, `"`+name+`"`)
	}
	return out
}

// restartLastPlan runs the plan that last ran, from the beginning.
//
// It stages the plan the BRIDGE holds, not the panel's rendering of it — the
// panel has ids, statuses and depths, which would restart something that merely
// resembled what ran. Staging it as a saved plan rather than replaying the
// original tool call is what makes restart inspectable: /plans show last_run
// reads exactly what is about to happen, and it travels the one execution path
// every other plan travels.
//
// The staged plan is inspectable under the name the notice reports. stagePlan
// never replaces a plan a user already owns under the preferred name, and the
// staged copy is removed once the plan is admitted.
// resumeLastPlan runs the plan that last ran, from WHERE IT STOPPED — the
// completed tasks are skipped and their findings are folded into the tasks that
// remain (RemainingPlan). It is bare "/plans resume" when no plan is live to
// un-pause: the one-command "continue what I cancelled" the pause/restart pair
// did not cover.
//
// SAME-SESSION, because the progress comes from this session's event log. A new
// session has no record of what already ran, so there is nothing to resume from
// and the message says so rather than silently restarting.
func (m model) resumeLastPlan() (tea.Model, tea.Cmd) {
	args, planName, ok := m.planProgress.LastPlan()
	if !ok {
		return m.appendPlansNotice(planControlNotice("warning",
			"No plan has run this session, so there is nothing to resume. "+
				"/plans list shows what is saved, and /plans run <name> starts one.")), nil
	}
	if m.planProgress.PlanRunningNow() {
		return m.appendPlansNotice(planControlNotice("warning",
			"A plan is still running. Pause it with /plans pause, or stop it with /plans stop.")), nil
	}
	if m.sessionStore == nil || m.activeSession.SessionID == "" {
		return m.appendPlansNotice(planControlNotice("warning",
			"This session has no event log, so there is no record of what already ran.")), nil
	}
	events, err := m.sessionStore.ReadEvents(m.activeSession.SessionID)
	if err != nil {
		return m.appendPlansNotice(planControlNotice("warning", "Could not read this session's events: "+err.Error())), nil
	}
	progress, found := specialist.ReducePlanEvents(events)
	if !found {
		return m.appendPlansNotice(planControlNotice("warning",
			"No plan has run in this session, so there is nothing to resume.")), nil
	}
	plan, err := specialist.ParsePlan(args, m.savedPlanLimits())
	if err != nil {
		return m.appendPlansNotice(planControlNotice("warning",
			"That plan no longer validates, so it was not resumed: "+err.Error())), nil
	}
	if progress.Name != plan.Name() {
		return m.appendPlansNotice(planControlNotice("warning", fmt.Sprintf(
			"The last plan recorded in this session was %q, not %q, so its progress cannot be applied here.",
			progress.Name, plan.Name()))), nil
	}
	remaining, err := specialist.RemainingPlan(plan, progress, m.savedPlanLimits())
	if err != nil {
		// The only error here is "everything already succeeded" — which is not a
		// failure, it is the plan being done. Say so plainly.
		return m.appendPlansNotice(planControlNotice("info", err.Error())), nil
	}
	root, dir := m.planWriteTarget()
	if dir == "" {
		return m.appendPlansNotice(planControlNotice("warning", "There is nowhere to stage the plan in this run.")), nil
	}
	stagedName, err := m.stagePlan(root, dir, resumePlanName, remaining)
	if err != nil {
		return m.appendPlansNotice(planControlNotice("warning", "Could not stage the remaining plan: "+err.Error())), nil
	}
	label := planName
	if label == "" {
		label = "the last plan"
	}
	m = m.appendPlansNotice(planControlNotice("info",
		resumeNotice(label, plan, remaining, progress, stagedName)))
	encoded, err := json.Marshal(stagedName)
	if err != nil {
		return m.appendPlansNotice(planControlNotice("warning", "Could not reference that plan: "+err.Error())), nil
	}
	return m.dispatchCommand(parsedCommand{
		kind: commandPrompt,
		text: "Run the saved plan " + string(encoded) + " by calling the orchestrate tool with the `saved` argument set to that name. Do not restate its tasks.",
	})
}

// resumeNotice describes a staged resume: how much is left, how much is already
// done, and — when an edit brought a completed task back — which tasks re-run.
//
// The changed clause is the recovery diagnostic. Without it a user who edited a
// saved plan between runs sees a task they believe finished reappear in the
// remainder with no explanation; naming it, and saying it changed, is what makes
// identity-aware resume legible rather than surprising. "Already done" counts
// only the tasks that stay done, so an edited task is not both done and left.
func resumeNotice(label string, plan, remaining specialist.Plan, progress specialist.PlanProgress, stagedName string) string {
	changed := specialist.ResumeChangedTasks(plan, progress)
	doneCount := len(progress.Succeeded) - len(changed)
	if doneCount < 0 {
		doneCount = 0
	}
	msg := fmt.Sprintf("Resuming %s from where it stopped: %d task(s) left, %d already done.",
		label, remaining.TaskCount(), doneCount)
	if len(changed) > 0 {
		msg += fmt.Sprintf(" Re-running %s because they or an input changed since the last run.",
			strings.Join(changed, ", "))
	}
	msg += fmt.Sprintf(" Staged as %q — /plans show %s to read it first.", stagedName, stagedName)
	return msg
}

// restartLastPlan runs the plan that last ran, from the beginning.
func (m model) restartLastPlan() (tea.Model, tea.Cmd) {
	args, planName, ok := m.planProgress.LastPlan()
	if !ok {
		return m.appendPlansNotice(planControlNotice("warning",
			"No plan has run this session, so there is nothing to restart. "+
				"/plans list shows what is saved, and /plans run <name> starts one.")), nil
	}
	if m.planProgress.PlanRunningNow() {
		// Restarting under a running plan would leave two plans reporting into
		// one panel, and the panel is keyed by task id — the second would
		// overwrite the first's rows. Stop it first, deliberately, rather than
		// having the UI silently pick one.
		return m.appendPlansNotice(planControlNotice("warning",
			"A plan is still running. Stop it with /plans stop first, then restart.")), nil
	}
	plan, err := specialist.ParsePlan(args, m.savedPlanLimits())
	if err != nil {
		return m.appendPlansNotice(planControlNotice("warning",
			"That plan no longer validates, so it was not restarted: "+err.Error())), nil
	}
	root, dir := m.planWriteTarget()
	if dir == "" {
		return m.appendPlansNotice(planControlNotice("warning",
			"There is nowhere to stage the plan in this run.")), nil
	}
	stagedName, err := m.stagePlan(root, dir, restartPlanName, plan)
	if err != nil {
		return m.appendPlansNotice(planControlNotice("warning", "Could not stage the plan: "+err.Error())), nil
	}
	label := planName
	if label == "" {
		label = "the last plan"
	}
	m = m.appendPlansNotice(planControlNotice("info", fmt.Sprintf(
		"Restarting %s from the beginning (%d tasks). Staged as %q — /plans show %s to read it first.",
		label, plan.TaskCount(), stagedName, stagedName)))
	encoded, err := json.Marshal(stagedName)
	if err != nil {
		return m.appendPlansNotice(planControlNotice("warning", "Could not reference that plan: "+err.Error())), nil
	}
	return m.dispatchCommand(parsedCommand{
		kind: commandPrompt,
		text: "Run the saved plan " + string(encoded) + " by calling the orchestrate tool with the `saved` argument set to that name. Do not restate its tasks.",
	})
}

// restartPlanName is where a restart prefers to stage the last plan;
// resumePlanName is where a resume-from-stop prefers to stage the remainder.
// They are preferences, not reservations: stagePlan falls back to a free
// variant rather than replace a plan that already owns the name.
const restartPlanName = "last_run"
const resumePlanName = "last_run_resume"

// maxStagingCandidates bounds the free-name search: base, base_2 … base_N.
const maxStagingCandidates = 100

// planStaging owns the plans restart and resume generate in the user's plan
// storage. Generated plans share that namespace because the model runs them
// through the ordinary `saved` lookup, so ownership is carried here instead of
// by the name: a staged plan is published only under a name nothing else
// holds, and is removed once its plan has been admitted, if it still holds
// exactly the bytes that were staged.
type planStaging struct {
	mu     sync.Mutex
	staged []stagedPlan
}

type stagedPlan struct {
	root, dir string
	name      string // the saved-plan name it was published under
	planName  string // the plan's own name, which admission reports
	body      []byte
}

func newPlanStaging() *planStaging { return &planStaging{} }

// stagePlan publishes plan under base, or under the first free base_N when
// base is taken, and records it for removal after admission. A name is taken
// when any scope resolves it (so staging never shadows another plan) or a file
// of that name exists in dir; publication is exclusive, so a name claimed
// between the check and the write is skipped, never replaced.
func (m model) stagePlan(root, dir, base string, plan specialist.Plan) (string, error) {
	taken := map[string]bool{}
	loaded, _ := specialist.LoadPlans(m.planPaths)
	for _, existing := range loaded {
		taken[existing.Name] = true
	}
	for attempt := 1; attempt <= maxStagingCandidates; attempt++ {
		name := stagingCandidate(base, attempt)
		if taken[name] || planFileExists(root, dir, name) {
			continue
		}
		path, err := specialist.SavePlanExclusive(root, dir, name, plan)
		if err != nil {
			if planFileExists(root, dir, name) {
				continue // claimed concurrently; try the next variant
			}
			return "", err
		}
		m.planStaging.track(root, dir, name, plan.Name(), path)
		return name, nil
	}
	return "", fmt.Errorf("every staging name from %q to %q is already taken; remove stale staged plans and try again",
		stagingCandidate(base, 1), stagingCandidate(base, maxStagingCandidates))
}

// stagingCandidate returns base for the first attempt and base_N after it,
// trimming base so the result stays within the saved-plan name limit.
func stagingCandidate(base string, attempt int) string {
	const maxPlanName = 64
	suffix := ""
	if attempt > 1 {
		suffix = "_" + strconv.Itoa(attempt)
	}
	if len(base)+len(suffix) > maxPlanName {
		base = base[:maxPlanName-len(suffix)]
	}
	return base + suffix
}

func planFileExists(root, dir, name string) bool {
	handle, relative, err := pathjail.OpenExisting(root, dir)
	if err != nil {
		return false
	}
	defer handle.Close()
	_, err = handle.Lstat(filepath.Join(relative, name+".json"))
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

func (staging *planStaging) track(root, dir, name, planName, path string) {
	if staging == nil {
		return
	}
	handle, relative, err := pathjail.OpenExisting(root, dir)
	if err != nil {
		return
	}
	defer handle.Close()
	body, err := handle.ReadFile(filepath.Join(relative, filepath.Base(path)))
	if err != nil {
		return
	}
	staging.mu.Lock()
	defer staging.mu.Unlock()
	staging.staged = append(staging.staged, stagedPlan{root: root, dir: dir, name: name, planName: planName, body: body})
}

// releaseAdmitted removes every staged plan whose plan was just admitted. The
// tool read the file before admission, so it is no longer needed. A file whose
// bytes changed since staging was edited by someone else and is left alone.
func (staging *planStaging) releaseAdmitted(planName string) {
	if staging == nil {
		return
	}
	staging.mu.Lock()
	var release []stagedPlan
	kept := staging.staged[:0]
	for _, staged := range staging.staged {
		if staged.planName == planName {
			release = append(release, staged)
		} else {
			kept = append(kept, staged)
		}
	}
	staging.staged = kept
	staging.mu.Unlock()
	for _, staged := range release {
		staged.remove()
	}
}

func (staged stagedPlan) remove() {
	handle, relative, err := pathjail.OpenExisting(staged.root, staged.dir)
	if err != nil {
		return
	}
	defer handle.Close()
	path := filepath.Join(relative, staged.name+".json")
	current, err := handle.ReadFile(path)
	if err != nil || !bytes.Equal(current, staged.body) {
		return
	}
	_ = handle.Remove(path)
}

// resumeSavedPlan narrows a stored plan by the CURRENT session's plan events and
// saves the remainder under its own name.
//
// It writes a new saved plan rather than teaching the tool a second "skip these
// ids" argument. The remainder is a plan — it validates, it runs, it can be
// inspected with /plans show before anyone spends a token on it — and running it
// travels the one path every other plan travels. A skip list would have been a
// parallel notion of what a plan is, resolved somewhere other than ParsePlan.
func (m model) resumeSavedPlan(stored specialist.SavedPlan) (name string, notice string, ok bool) {
	if m.sessionStore == nil || m.activeSession.SessionID == "" {
		return "", planControlNotice("warning",
			"This session has no event log, so there is no record of what already ran."), false
	}
	events, err := m.sessionStore.ReadEvents(m.activeSession.SessionID)
	if err != nil {
		return "", planControlNotice("warning", "Could not read this session's events: "+err.Error()), false
	}
	progress, found := specialist.ReducePlanEvents(events)
	if !found {
		return "", planControlNotice("warning",
			"No plan has run in this session, so there is nothing to resume. Use /plans run "+stored.Name+" to run it from the start."), false
	}
	plan, err := specialist.ParsePlan(stored.Args, m.savedPlanLimits())
	if err != nil {
		return "", planControlNotice("warning", fmt.Sprintf("%s does not validate: %v", stored.Path, err)), false
	}
	// The reduction keeps only the LAST admitted plan's progress, so it may
	// belong to a different plan than the one being resumed. Narrowing across
	// that boundary silently drops work: ids like "tests" or "lint" collide
	// between plans routinely, and every task of this plan whose id sits in the
	// other plan's succeeded set would vanish from the remainder and never run.
	//
	// Compared against plan.Name(), not stored.Name: plan_admitted records the
	// plan's own name, which is independent of the name it was saved under.
	if progress.Name != plan.Name() {
		return "", planControlNotice("warning", fmt.Sprintf(
			"The last plan to run in this session was %q, not %q, so there is no record of what %q already did. Use /plans run %s to run it from the start.",
			progress.Name, plan.Name(), plan.Name(), stored.Name)), false
	}
	remaining, err := specialist.RemainingPlan(plan, progress, m.savedPlanLimits())
	if err != nil {
		return "", planControlNotice("info", err.Error()), false
	}

	root, dir := m.planWriteTarget()
	// <name>_resume when that name is free, otherwise the next free variant:
	// stagePlan never replaces a plan it did not stage itself.
	resumeName, err := m.stagePlan(root, dir, stored.Name+"_resume", remaining)
	if err != nil {
		return "", planControlNotice("warning", "Could not stage the remaining plan: "+err.Error()), false
	}
	// A terminal event also closes a stopped or budget-limited run. Report its
	// recorded status rather than infer that the whole graph ran to the end.
	why := "the plan was interrupted before it finished"
	if progress.Complete {
		why = "the previous run ended"
		if progress.Status != "" {
			why += fmt.Sprintf(" with status %q", progress.Status)
		}
	}

	notice = fmt.Sprintf(
		"Resuming %q: %d of %d tasks already succeeded, %d left — %s.",
		stored.Name, len(progress.Succeeded), len(progress.Order), remaining.TaskCount(), why)
	// A completed task that reappears in the remainder was EDITED since it ran
	// (or depends on one that was). Naming it is what keeps an identity-aware
	// resume legible: the file changed between runs, so the fingerprint no longer
	// matches and the task is not resumed as done.
	if changed := specialist.ResumeChangedTasks(plan, progress); len(changed) > 0 {
		notice += fmt.Sprintf("\n%s changed since the last run and will re-run (with anything that depends on them).",
			strings.Join(changed, ", "))
	}
	notice += fmt.Sprintf("\nSaved the remainder as %q — /plans show %s to read it first.", resumeName, resumeName)
	return resumeName, planControlNotice("info", notice), true
}

func (m model) planWriteTarget() (root, dir string) {
	if m.planPaths.ProjectDir != "" {
		return m.planPaths.ProjectRoot, m.planPaths.ProjectDir
	}
	return m.planPaths.UserRoot, m.planPaths.UserDir
}

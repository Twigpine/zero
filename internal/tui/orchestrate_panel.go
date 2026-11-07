package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// completedHideAfter keeps a finished orchestration visible briefly unless the
// user expanded it. The old pinned-plan panel that previously owned this timing
// was removed on main, but orchestration retains the lifecycle independently.
const completedHideAfter = 30 * time.Second

// orchestrateTaskStatus is a plan task's state in the panel.
type orchestrateTaskStatus int

const (
	orchestratePending orchestrateTaskStatus = iota
	orchestrateRunning
	orchestrateDone
	orchestrateFailed
	orchestrateSkipped
	orchestrateCancelled
)

func (s orchestrateTaskStatus) label() string {
	switch s {
	case orchestrateRunning:
		return "running"
	case orchestrateDone:
		return "done"
	case orchestrateFailed:
		return "failed"
	case orchestrateSkipped:
		return "skipped"
	case orchestrateCancelled:
		return "cancelled"
	default:
		return "pending"
	}
}

// orchestrateTask is one row of the panel.
type orchestrateTask struct {
	id        string
	dependsOn []string
	phase     string
	status    orchestrateTaskStatus
	summary   string
	startedAt time.Time
	endedAt   time.Time
	// depth is the task's rank in the dependency graph: 0 for a task with no
	// dependencies, otherwise one more than its deepest dependency. This is what
	// makes a diamond LOOK like a diamond — tasks at the same depth ran from the
	// same fan-out and are drawn on the same rung.
	depth int
	// attempts is how many times the task ran. A stalled task is retried, and
	// the retry happens INSIDE the executor — one dispatch, one card — so
	// without this the panel shows a task that silently took twice as long.
	attempts int
	// model is what the task runs on, empty when it inherits the session's. Shown
	// only when set: a line saying "on <the model you are already using>" against
	// every task buries the one that differs.
	model string
	// fellBackFrom names the model this task was assigned and could not run on.
	// Empty for the overwhelming majority; set only after a provider refused the
	// assigned model and the task was re-run on the session's.
	fellBackFrom string
	// cardKey links this task to its live agent state in the specialist
	// tracker: the temporary key while it runs, the child's real session id
	// once it finishes. The detail pane reads tool counts, the current tool and
	// spend through it rather than duplicating that state here.
	cardKey string
}

// elapsed is the task's duration: live while running, frozen once ended.
func (t orchestrateTask) elapsed(now time.Time) time.Duration {
	if t.startedAt.IsZero() {
		return 0
	}
	if t.endedAt.IsZero() {
		return now.Sub(t.startedAt)
	}
	return t.endedAt.Sub(t.startedAt)
}

// orchestratePanelState is the live view of one orchestrate plan.
//
// Distinct from planPanelState, which belongs to the update_plan tool (the
// model's own TODO list). Two different things called "plan" is a real
// collision risk, so this type, its command and its state are named
// ORCHESTRATE throughout, and only the user-facing command says "plans".
type orchestratePanelState struct {
	name        string
	tasks       []orchestrateTask
	byID        map[string]int
	status      string
	tokensUsed  int
	tokenLimit  int
	maxSpeedup  float64
	startedAt   time.Time
	completedAt time.Time
	// frozenAt stops the header clock when the run ends without a terminal plan
	// event — an interrupt or a crash. Mirrors planPanelState.frozenAt.
	frozenAt time.Time
	expanded bool
	// sidebarCollapsed hides the task list and detail in the right column,
	// toggled by clicking the sidebar's PLAN header. Separate from `expanded`,
	// which is the inline panel above the composer: they are two surfaces and
	// collapsing one must not silently collapse the other.
	sidebarCollapsed bool
}

func (s *orchestratePanelState) clear() { *s = orchestratePanelState{} }

func (s orchestratePanelState) isEmpty() bool { return len(s.tasks) == 0 }

func (s orchestratePanelState) isComplete() bool { return s.status != "" }

// admit installs the shape. Called once per plan, before any task runs.
func (s *orchestratePanelState) admit(msg planAdmittedMsg, now time.Time) {
	s.clear()
	s.name = msg.name
	s.tokenLimit = msg.tokenLimit
	s.startedAt = now
	s.byID = make(map[string]int, len(msg.tasks))
	s.tasks = make([]orchestrateTask, 0, len(msg.tasks))
	for _, task := range msg.tasks {
		s.byID[task.id] = len(s.tasks)
		s.tasks = append(s.tasks, orchestrateTask{
			id:        task.id,
			dependsOn: task.dependsOn,
			phase:     task.phase,
			status:    orchestratePending,
		})
	}
	s.computeDepths()
}

// computeDepths ranks tasks by dependency depth. The message carries them in
// the validated topological order, so every dependency is already ranked when a
// task is reached — the same property criticalPath relies on.
func (s *orchestratePanelState) computeDepths() {
	for index := range s.tasks {
		depth := 0
		for _, dep := range s.tasks[index].dependsOn {
			position, ok := s.byID[dep]
			if !ok || position >= index {
				continue
			}
			if next := s.tasks[position].depth + 1; next > depth {
				depth = next
			}
		}
		s.tasks[index].depth = depth
	}
}

func (s *orchestratePanelState) markStarted(taskID, summary, cardKey, model string, now time.Time) {
	index, ok := s.byID[taskID]
	if !ok {
		return
	}
	s.tasks[index].status = orchestrateRunning
	s.tasks[index].summary = summary
	s.tasks[index].startedAt = now
	if strings.TrimSpace(model) != "" {
		s.tasks[index].model = model
	}
	if cardKey != "" {
		s.tasks[index].cardKey = cardKey
	}
}

// cancelRunning marks every still-running task cancelled. Called when the USER
// cancels the run: the children are killed by the run context, so a task left
// orchestrateRunning would spin in the sidebar forever over a process that no
// longer exists. Cancelled, not failed — the user stopping a run is not a
// defect (the same distinction sidebarOrchestrateStyle already draws).
func (s *orchestratePanelState) cancelRunning(now time.Time) {
	for index := range s.tasks {
		if s.tasks[index].status == orchestrateRunning {
			s.tasks[index].status = orchestrateCancelled
			s.tasks[index].endedAt = now
		}
	}
}

// linkCard repoints a task at its child's real session id, so the detail pane
// keeps finding it after the tracker reconciles the temporary key.
func (s *orchestratePanelState) linkCard(taskID, cardKey string) {
	if index, ok := s.byID[taskID]; ok && cardKey != "" {
		s.tasks[index].cardKey = cardKey
	}
}

// markDoneOn is markDone knowing WHICH MODEL ACTUALLY RAN.
//
// ranOn is the model the task finished on and is authoritative: empty means it
// inherited the session's, which is exactly what a fallback produces. Correcting
// it here is the difference between a row that names the model that worked and
// one that names the model that was refused.
func (s *orchestratePanelState) markDoneOn(taskID, outcome, ranOn, fellBackFrom string, tokens, attempts int, now time.Time) {
	index, ok := s.byID[taskID]
	if !ok {
		return
	}
	// ACCUMULATE AS TASKS FINISH. tokensUsed was only ever assigned from the
	// plan_completed event, which arrives when the whole plan ends — so the
	// footer read "budget 0/200000" for the entire run while the cards above it
	// showed tens of thousands of tokens spent. A budget line that reports zero
	// while spending is the one number a user is watching it for.
	s.tokensUsed += tokens
	task := &s.tasks[index]
	task.status = orchestrateStatusFromOutcome(outcome)
	task.attempts = attempts
	if strings.TrimSpace(fellBackFrom) != "" {
		// Assigned one model, ran on another. Both facts are shown: the row must
		// stop claiming the refused model, and must still name it — an unusable
		// model stays in the provider's list and the next plan picks it again.
		task.model = strings.TrimSpace(ranOn)
		task.fellBackFrom = strings.TrimSpace(fellBackFrom)
	}
	task.endedAt = now
	if task.startedAt.IsZero() {
		// Never dispatched: it has no duration, and pretending otherwise would
		// put time against work that did not happen.
		task.startedAt = now
	}
}

// orchestrateStatusFromOutcome maps the executor's terminal outcomes onto panel
// statuses. The string values are specialist's TaskOutcome constants; matching
// on them here rather than importing keeps the panel free of the executor.
func orchestrateStatusFromOutcome(outcome string) orchestrateTaskStatus {
	switch outcome {
	case "succeeded":
		return orchestrateDone
	case "failed":
		return orchestrateFailed
	case "cancelled":
		return orchestrateCancelled
	case "dependency_failed", "budget_exhausted":
		return orchestrateSkipped
	default:
		return orchestrateFailed
	}
}

func (s *orchestratePanelState) complete(msg planCompletedMsg, now time.Time) {
	s.status = msg.status
	// The executor's total is AUTHORITATIVE and replaces what the panel
	// accumulated: it counts every task, including any whose message the panel
	// dropped as stale.
	if msg.tokensUsed > 0 {
		s.tokensUsed = msg.tokensUsed
	}
	if msg.tokenLimit > 0 {
		s.tokenLimit = msg.tokenLimit
	}
	s.maxSpeedup = msg.maxSpeedup
	s.completedAt = now
}

// visible mirrors planPanelState.visible: nothing to show when empty, and a
// finished plan stops pinning itself after a grace period unless expanded.
//
// THIS IS THE MOUNT-POINT ANSWER. With no plan admitted the state is empty and
// this returns false, so the panel contributes nothing to the view — the
// posture-off, no-plan TUI is unchanged because the panel never renders, not
// because it renders something empty.
func (s orchestratePanelState) visible(now time.Time) bool {
	if s.isEmpty() {
		return false
	}
	if s.isComplete() && !s.expanded && !s.completedAt.IsZero() && now.Sub(s.completedAt) > completedHideAfter {
		return false
	}
	return true
}

func (s orchestratePanelState) counts() (done, failed, skipped, cancelled, running int) {
	for _, task := range s.tasks {
		switch task.status {
		case orchestrateDone:
			done++
		case orchestrateFailed:
			failed++
		case orchestrateSkipped:
			skipped++
		case orchestrateCancelled:
			cancelled++
		case orchestrateRunning:
			running++
		}
	}
	return
}

const (
	// orchestrateMaxRows bounds what the pinned panel draws. A plan may hold up
	// to defaultPlanMaxTasks tasks and the panel must never push the composer
	// off screen; beyond this the panel says how many it is not showing rather
	// than silently dropping them.
	orchestrateMaxRows = 12
	// orchestrateSummaryWidth bounds a task's one-line summary.
	orchestrateSummaryWidth = 48
	// orchestrateMinWidth is the narrowest sensible render.
	orchestrateMinWidth = 24
	// orchestrateMaxIndentDepth bounds the dependency indent.
	orchestrateMaxIndentDepth = 5
)

// orchestrateTaskLinger is how long a finished task stays on screen before it
// drops out. Long enough to see it land, short enough that the panel tracks
// what is actually happening rather than accumulating history.
//
// Matches the AGENTS sidebar's treatment of finished agents, which retires them
// the same way and for the same reason.
const orchestrateTaskLinger = 5 * time.Second

// liveTasks returns the tasks the panel should draw: everything not yet
// finished, plus anything that finished within the linger window.
//
// The header's counts still cover EVERY task — a faded task is hidden, not
// forgotten — and /plans still lists the whole plan. This is the panel choosing
// to show live work rather than accumulate a transcript.
//
// TRADE-OFF, stated because it cost something real: the dependency shape goes
// with them. A diamond stops looking like a diamond once its first task fades.
// /plans is where the shape stays readable for the whole run.
func (s orchestratePanelState) liveTasks(now time.Time) []orchestrateTask {
	live := make([]orchestrateTask, 0, len(s.tasks))
	for _, task := range s.tasks {
		if task.endedAt.IsZero() || now.Sub(task.endedAt) < orchestrateTaskLinger {
			live = append(live, task)
		}
	}
	return live
}

// sidebarOwnsOrchestrate reports whether the right-hand column is currently the
// running plan's surface — in which case the inline panel is a second copy of it
// two rows above the composer, and does not draw.
//
// The right column already carries the progress bar, the task list, the live
// detail and every task's agent row. The inline panel stayed because it predates
// all of that; keeping both meant the plan announced itself three times on one
// screen (the admission row, the footer summary, the sidebar) and stole footer
// height from the conversation to do it.
//
// It is a FALLBACK, not dead code. The sidebar needs a wide enough terminal, is
// suppressed under every full-screen overlay, and hands its PLAN section to
// update_plan whenever that has steps — so there are real states where the
// inline panel is the only place a running plan appears, and it still draws in
// all of them.
func (m model) sidebarOwnsOrchestrate() bool {
	if m.orchestrate.isEmpty() || !m.sidebarActive() {
		return false
	}
	// A section collapsed by a click on its header is not a surface: it shows a
	// count and a hint to reopen. The plan goes back to the inline panel until
	// it is opened again, rather than existing nowhere.
	return !m.orchestrate.sidebarCollapsed
}

// renderOrchestratePanel draws the plan: one row per task, indented by
// dependency depth so the shape is legible.
func (m model) renderOrchestratePanel(width int) string {
	state := m.orchestrate
	now := m.orchestrateNow()
	if !state.visible(now) {
		return ""
	}
	if m.sidebarOwnsOrchestrate() {
		return ""
	}
	if width < orchestrateMinWidth {
		width = orchestrateMinWidth
	}

	var b strings.Builder
	b.WriteString(zeroTheme.ink.Render(orchestrateHeaderLine(state, now)))

	// COLLAPSED BY DEFAULT. A plan's tasks are detail; the header is the state.
	// A six-task chain otherwise takes seven lines of the footer for the whole
	// run, pushing the conversation up for information that is one keypress
	// away. Ctrl+O expands.
	if !state.expanded {
		return b.String()
	}

	// Hidden by the ROW CAP is worth saying; hidden by the linger is not. A
	// truncated list reads as a complete one, but a faded task is already
	// accounted for in the header's done count.
	//
	// The window FOLLOWS the running task rather than always starting at the
	// first — see orchestrate_window.go. With the large plan-size tier a plan can
	// hold fifty tasks, and a list pinned to tasks 1-12 while task 40 runs shows
	// everything except the thing in progress.
	rows, above, below := m.orchestrateVisibleRows(orchestrateMaxRows)
	for _, task := range rows {
		b.WriteString("\n")
		b.WriteString(m.renderOrchestrateTaskLine(task, now, width))
	}
	if note := orchestrateHiddenNote(above, below); note != "" {
		b.WriteString("\n")
		b.WriteString(zeroTheme.faint.Render("  … " + note))
	}
	if footer := orchestrateFooterLine(state); footer != "" {
		b.WriteString("\n")
		b.WriteString(zeroTheme.faint.Render(footer))
	}
	return b.String()
}

func orchestrateHeaderLine(state orchestratePanelState, now time.Time) string {
	done, failed, skipped, cancelled, running := state.counts()
	var b strings.Builder
	// The affordance says which way the keypress goes, so the collapsed state
	// does not look like the whole panel.
	if state.expanded {
		b.WriteString("▾ ")
	} else {
		b.WriteString("▸ ")
	}
	b.WriteString("PLAN")
	if name := strings.TrimSpace(state.name); name != "" {
		fmt.Fprintf(&b, " %s", truncateRunes(name, 24))
	}
	fmt.Fprintf(&b, "  %d/%d done", done, len(state.tasks))
	if running > 0 {
		fmt.Fprintf(&b, " · %d running", running)
	}
	if failed > 0 {
		fmt.Fprintf(&b, " · %d failed", failed)
	}
	if skipped > 0 {
		fmt.Fprintf(&b, " · %d skipped", skipped)
	}
	if cancelled > 0 {
		fmt.Fprintf(&b, " · %d cancelled", cancelled)
	}
	if !state.startedAt.IsZero() {
		fmt.Fprintf(&b, " · %s", formatElapsedSeconds(now.Sub(state.startedAt)))
	}
	if !state.expanded {
		b.WriteString("  ")
		b.WriteString("click to open · ctrl+g to expand")
	}
	return b.String()
}

func orchestrateFooterLine(state orchestratePanelState) string {
	var parts []string
	switch {
	case state.tokenLimit > 0:
		parts = append(parts, fmt.Sprintf("budget %d/%d tokens", state.tokensUsed, state.tokenLimit))
	case state.tokensUsed > 0:
		// No bound was asked for, so there is no denominator to show. The spend
		// is still reported — an unbounded plan must not also be an unmeasured
		// one.
		parts = append(parts, fmt.Sprintf("%d tokens", state.tokensUsed))
	}
	if state.isComplete() {
		parts = append(parts, "status "+state.status)
		if state.maxSpeedup > 0 {
			parts = append(parts, fmt.Sprintf("max_speedup %.2fx", state.maxSpeedup))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "  " + strings.Join(parts, " · ")
}

func (m model) renderOrchestrateTaskLine(task orchestrateTask, now time.Time, width int) string {
	// Indent by dependency depth: tasks that fan out from the same parent share
	// a rung, so a diamond reads as one node, two beside each other, one node.
	//
	// CAPPED. A strict chain adds a rung per link, so a twenty-task chain would
	// indent forty columns and run off a narrow terminal — the shape stops being
	// legible long before that. Past the cap every task sits at the same depth,
	// which is honest for a chain: they are all one-after-another anyway.
	indent := strings.Repeat("  ", minInt(task.depth, orchestrateMaxIndentDepth)+1)

	glyph := orchestrateGlyph(task.status)
	if task.status == orchestrateRunning {
		glyph = zeroTheme.accent.Render(m.spinnerGlyph())
	}

	head := fmt.Sprintf("%s%s %s", indent, glyph, truncateRunes(task.id, 24))
	meta := task.status.label()
	if elapsed := task.elapsed(now); elapsed > 0 {
		meta += " " + formatElapsedSeconds(elapsed)
	}

	// The summary is what is left after the fixed parts, and it is a SUMMARY:
	// the task's full result stays in the tool output. A display formatter on
	// the data path is how a 583-rune work product became 200 mangled runes.
	//
	// MEASURED IN COLUMNS, NOT RUNES. head carries the styled glyph, so its
	// escape sequences counted as visible characters — around a dozen columns
	// of phantom width per row. remaining came out short, the `> 8` guard
	// suppressed summaries that fit comfortably, and the wider the terminal the
	// more often a finished task rendered with nothing to say. lipgloss.Width
	// ignores the escapes and counts wide runes properly.
	remaining := width - lipgloss.Width(head) - lipgloss.Width(meta) - 3
	line := head + " " + zeroTheme.faint.Render(meta)
	if remaining > 8 && strings.TrimSpace(task.summary) != "" {
		limit := remaining
		if limit > orchestrateSummaryWidth {
			limit = orchestrateSummaryWidth
		}
		line += " " + zeroTheme.faint.Render(truncateRunes(task.summary, limit))
	}
	return line
}

func orchestrateGlyph(status orchestrateTaskStatus) string {
	switch status {
	case orchestrateDone:
		return zeroTheme.green.Render("✓")
	case orchestrateFailed:
		return zeroTheme.red.Render("✗")
	case orchestrateSkipped, orchestrateCancelled:
		// Neutral: a skipped or stopped task is not a defect.
		return zeroTheme.faint.Render("⊘")
	default:
		return zeroTheme.faint.Render("·")
	}
}

// orchestrateNow freezes the plan clock while no run is in flight, mirroring
// planNow — a task left mid-flight when the agent yields must stop ticking up
// against a turn that is no longer running.
func (m model) orchestrateNow() time.Time {
	// The plan's own completion stops its clock, whatever the run is doing: a
	// finished plan inside a turn that continues must not keep counting.
	if !m.orchestrate.completedAt.IsZero() {
		return m.orchestrate.completedAt
	}
	// Otherwise the run ending stops it — a plan left mid-flight by an
	// interrupt would tick forever against a turn that is no longer running.
	if m.activeRunID == 0 && !m.orchestrate.frozenAt.IsZero() {
		return m.orchestrate.frozenAt
	}
	return m.now()
}

// orchestratePlansText answers /plans. It reports the plan whether or not the
// pinned panel is currently showing it — a finished plan stops pinning itself
// after a grace period, and asking for it explicitly should still work.
func (m model) orchestratePlansText() string {
	state := m.orchestrate
	if state.isEmpty() {
		return "No plan has run this session. Under the zeromaxing posture, ask for one and " +
			"the orchestrate tool will run it; tasks appear here as they go."
	}

	now := m.orchestrateNow()
	var b strings.Builder
	b.WriteString(orchestrateHeaderLine(state, now))
	for _, task := range state.tasks {
		b.WriteString("\n")
		b.WriteString(orchestratePlainTaskLine(task, now))
	}
	if footer := orchestrateFooterLine(state); footer != "" {
		b.WriteString("\n")
		b.WriteString(footer)
	}
	return b.String()
}

// orchestratePlainTaskLine is the /plans row: no spinner and no colour, since
// this is a static transcript entry rather than a live panel. Every task is
// listed — the command is where a plan larger than the panel's row budget can
// be read in full.
func orchestratePlainTaskLine(task orchestrateTask, now time.Time) string {
	indent := strings.Repeat("  ", task.depth+1)
	line := fmt.Sprintf("%s%s %s [%s]", indent, orchestratePlainGlyph(task.status), task.id, task.status.label())
	if elapsed := task.elapsed(now); elapsed > 0 {
		line += " " + formatElapsedSeconds(elapsed)
	}
	if len(task.dependsOn) > 0 {
		line += " ← " + strings.Join(task.dependsOn, ", ")
	}
	return line
}

func orchestratePlainGlyph(status orchestrateTaskStatus) string {
	switch status {
	case orchestrateDone:
		return "✓"
	case orchestrateFailed:
		return "✗"
	case orchestrateSkipped, orchestrateCancelled:
		return "⊘"
	case orchestrateRunning:
		return "▸"
	default:
		return "·"
	}
}

// maxSidebarOrchestrateLines caps the sidebar's plan list. The column is 26-40
// wide and shares its height with AGENTS, FILES and ACTIVITY, so a twenty-task
// plan must not push those off — the panel and the detail view are where a
// whole plan is read.
const maxSidebarOrchestrateLines = 6

// sidebarOrchestrateLines renders the running plan for the right-hand column:
// one line per task, coloured by status, in the same shape the update_plan
// steps use so the section reads consistently whichever plan is in it.
//
// Live tasks first — the ones that have not finished — so a long plan shows
// what is happening rather than what already happened. What it drops is stated.
func (m model) sidebarOrchestrateLines(width int) []string {
	state := m.orchestrate
	if state.isEmpty() || state.sidebarCollapsed {
		return nil
	}
	room := maxInt(4, width-3)

	rows, above, below := m.orchestrateVisibleRows(maxSidebarOrchestrateLines)
	lines := make([]string, 0, len(rows)+1)
	for _, task := range rows {
		icon, body := sidebarOrchestrateStyle(task, room)
		lines = append(lines, " "+icon+" "+body)
	}
	if note := orchestrateHiddenNote(above, below); note != "" {
		lines = append(lines, " "+zeroTheme.faint.Render("  "+note))
	}
	return lines
}

// dependents lists the tasks that depend on the given one — the reverse of the
// declared edges, which the plan only stores forward.
func (s orchestratePanelState) dependents(taskID string) []string {
	var out []string
	for _, task := range s.tasks {
		for _, dep := range task.dependsOn {
			if dep == taskID {
				out = append(out, task.id)
				break
			}
		}
	}
	return out
}

// sidebarProgressBar draws the plan's progress across the column: done, failed
// and skipped each keep their own colour inside one bar, so a glance says both
// how far along it is AND whether it is going well.
//
// Proportional to the COLUMN, not to a fixed width — a 26-cell sidebar and a
// 40-cell one both get a bar that fills their space.
func sidebarProgressBar(state orchestratePanelState, width int) string {
	return sidebarProgressBarWith(state, width, progressBarSkin{
		settled: "█",
		running: "▓",
		pending: func(_, n, _ int) string {
			return zeroTheme.faint.Render(strings.Repeat("░", n))
		},
	})
}

// progressBarSkin is the glyph-and-track dress a progress bar renders in. The
// plain skin above is the historical bar byte-for-byte; the zeromaxing skin
// (posturePlanProgressBar) swaps the language, never the layout or the counts.
type progressBarSkin struct {
	settled string
	running string
	// pending renders the unfilled track: (startCell, cellCount, totalCells).
	pending func(startCell, n, cells int) string
	// paint, when non-nil, renders a filled segment instead of the semantic
	// block colours — (kind, startCell, cellCount, totalCells) with kind one of
	// "done", "failed", "skipped", "running". nil keeps the historical styles,
	// which is what makes the plain bar byte-identical.
	paint func(kind string, startCell, n, cells int) string
}

func sidebarProgressBarWith(state orchestratePanelState, width int, skin progressBarSkin) string {
	total := len(state.tasks)
	if total == 0 || width < 12 {
		return ""
	}
	done, failed, skipped, cancelled, running := state.counts()

	cells := maxInt(4, width-8)
	fill := func(count int) int {
		if count <= 0 {
			return 0
		}
		// At least one cell for anything non-zero: a failure that rounds to
		// zero cells is a failure the bar does not show.
		return maxInt(1, count*cells/total)
	}
	// SOLID MEANS SETTLED. done, failed, skipped and cancelled are terminal — the
	// task is not coming back, and the bar can spend a full block on it. Running
	// is not progress, it is work underway, and drawing it with the same solid
	// block made a plan with four of nine dispatched and NOTHING finished look
	// 44% complete beside its own "0/9". The half-shade ranks correctly between
	// the settled blocks and the pending track, and cannot be misread as either.
	segments := []struct {
		count int
		kind  string
		mark  string
		style lipgloss.Style
	}{
		{done, "done", skin.settled, zeroTheme.green},
		{failed, "failed", skin.settled, zeroTheme.red},
		{skipped + cancelled, "skipped", skin.settled, zeroTheme.muted},
		{running, "running", skin.running, zeroTheme.accent},
	}

	var b strings.Builder
	used := 0
	for _, segment := range segments {
		n := fill(segment.count)
		if used+n > cells {
			n = cells - used
		}
		if n <= 0 {
			continue
		}
		if skin.paint != nil {
			b.WriteString(skin.paint(segment.kind, used, n, cells))
		} else {
			b.WriteString(segment.style.Render(strings.Repeat(segment.mark, n)))
		}
		used += n
	}
	if used < cells {
		b.WriteString(skin.pending(used, cells-used, cells))
	}
	return " " + b.String() + zeroTheme.faint.Render(fmt.Sprintf(" %d/%d", done, total))
}

// sidebarOrchestrateStyle picks the glyph and text colour for one task. The
// palette matches the update_plan steps above it: green done, red failed, accent
// in flight, faint everything else — and cancelled/skipped are deliberately NOT
// red, since neither is a defect.
func sidebarOrchestrateStyle(task orchestrateTask, room int) (string, string) {
	label := task.id
	if summary := strings.TrimSpace(task.summary); summary != "" && len(label)+3 < room {
		label += " " + summary
	}
	switch task.status {
	case orchestrateDone:
		return zeroTheme.green.Render("✓"), zeroTheme.muted.Render(truncateStep(label, room))
	case orchestrateRunning:
		return zeroTheme.accent.Render("•"), zeroTheme.ink.Render(truncateStep(label, room))
	case orchestrateFailed:
		return zeroTheme.red.Render("✗"), zeroTheme.muted.Render(truncateStep(label, room))
	case orchestrateSkipped, orchestrateCancelled:
		return zeroTheme.faint.Render("⊘"), zeroTheme.faint.Render(truncateStep(label, room))
	default:
		return zeroTheme.faint.Render("○"), zeroTheme.faint.Render(truncateStep(label, room))
	}
}

package tui

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The /loop command repeats a prompt or slash command in one of two modes:
//
//   - Fixed interval — `/loop 5m <prompt>` re-fires on a wall-clock cadence.
//   - Self-paced     — bare `/loop <prompt>` runs a maintenance playbook: the model
//     does one increment of work each iteration and ends its reply with a control
//     line (`LOOP: DONE` / `LOOP: CONTINUE [interval]`) that stops the loop or picks
//     the next wake. With no control line it falls back to an adaptive cadence.
//
// Loops are foreground and session-scoped: a tick only fires while the session is
// idle between turns (it never interrupts a streaming turn), the loop lives with the
// session that created it, and switching sessions (/new, /resume) stops it. Safety
// rails cap iterations, age, consecutive failures, and no-progress repetition.
// Everything here is pure/loop-local; the turn-lifecycle wiring (ticker, launch,
// continuation) lives in model.go.

type loopMode int

const (
	loopModeFixed     loopMode = iota // re-fire every fixed interval
	loopModeSelfPaced                 // model chooses the next wake each iteration
)

// Safety bounds. A loop is foreground + session-scoped, but these keep a runaway
// or forgotten loop from burning tokens forever.
const (
	loopMinInterval   = 30 * time.Second
	loopMaxInterval   = 24 * time.Hour
	loopMaxIterations = 500 // hard ceiling; the loop auto-stops after this many

	// loopMaxAge auto-expires a loop this long after it was created, so a forgotten
	// loop stops on its own even if it never reaches the iteration cap.
	loopMaxAge = 24 * time.Hour

	loopSelfPaceMin     = 1 * time.Minute
	loopSelfPaceMax     = 1 * time.Hour
	loopSelfPaceDefault = 15 * time.Minute

	// loopDoomThreshold ends a loop after this many consecutive identical final
	// answers — a self-paced or interval loop that gets stuck repeating the same
	// no-progress result should stop rather than churn.
	loopDoomThreshold = 5

	// loopMaxFailures ends a loop after this many consecutive failed iterations, so
	// a loop whose iterations keep erroring stops instead of churning to the
	// iteration cap (a failed turn has no final answer, so the doom guard can't see
	// the lack of progress).
	loopMaxFailures = 3

	// loopPollInterval is how often the controller checks whether a loop is due.
	// A loop only fires while idle, so this is a cheap between-turn poll.
	loopPollInterval = 1 * time.Second
)

// loopState is one active loop owned by the current session.
type loopState struct {
	id        string
	mode      loopMode
	prompt    string // the prompt or "/command" re-run each iteration
	interval  time.Duration
	iteration int
	maxIter   int // 0 => loopMaxIterations
	createdAt time.Time
	nextRunAt time.Time
	// lastResult / repeatRun drive doom-loop detection: consecutive identical
	// final answers past loopDoomThreshold end the loop.
	lastResult string
	repeatRun  int
	// failRun counts consecutive failed iterations; past loopMaxFailures the loop
	// stops (a failed turn has no final answer for the doom guard to compare).
	failRun int
	paused  bool
}

func (l *loopState) iterationCap() int {
	if l.maxIter > 0 {
		return l.maxIter
	}
	return loopMaxIterations
}

// due reports whether the loop should fire at now (active, not paused, past its
// next-run mark).
func (l *loopState) due(now time.Time) bool {
	return !l.paused && !l.nextRunAt.IsZero() && !now.Before(l.nextRunAt)
}

// cadenceText renders the loop's cadence for status lines.
func (l *loopState) cadenceText() string {
	if l.mode == loopModeSelfPaced {
		return "self-paced"
	}
	return "every " + formatLoopDuration(l.interval)
}

type loopAction int

const (
	loopActionUsage   loopAction = iota // show usage / list when empty
	loopActionStart                     // start a new loop
	loopActionStop                      // stop one by id
	loopActionStopAll                   // stop every loop
	loopActionList                      // list active loops
)

// loopCommand is the parsed form of a `/loop ...` invocation (the text after the
// command name).
type loopCommand struct {
	action   loopAction
	targetID string
	mode     loopMode
	prompt   string
	interval time.Duration
	// note carries a non-fatal advisory (e.g. an interval that was clamped) to
	// surface back to the user.
	note string
}

var loopIntervalRe = regexp.MustCompile(`^(\d+)(s|m|h|d)$`)

// loopControlRe matches a self-paced loop's control line at the start of a line,
// tolerant of a leading markdown bullet/quote marker and any trailing note after
// the keyword: "LOOP: DONE", "- loop: continue", "LOOP: CONTINUE 10m — still tidying".
// The leading ^ anchor keeps a mid-sentence mention ("I'll emit LOOP: DONE when…")
// from matching; parseLoopControl normalizes CRLF first so `$` can reach line end.
var loopControlRe = regexp.MustCompile(`(?im)^[^\S\r\n]*(?:[-*>•][^\S\r\n]*)?loop:[^\S\r\n]*(done|continue)\b(?:[^\S\r\n]+(\d+[smhd])\b)?.*$`)

// parseLoopCommand interprets the arguments to `/loop`. Forms:
//
//	(empty)                 -> usage/list
//	list | status          -> list active loops
//	stop | stop all | stop <id>
//	<interval> <prompt>     -> fixed-interval loop (e.g. "5m /babysit")
//	<prompt> every <interval>
//	<prompt>                -> self-paced loop (no interval)
func parseLoopCommand(args string) loopCommand {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return loopCommand{action: loopActionUsage}
	}

	first := strings.ToLower(strings.Fields(trimmed)[0])
	switch first {
	case "list", "status", "ls":
		return loopCommand{action: loopActionList}
	case "stop", "cancel", "kill":
		rest := strings.TrimSpace(trimmed[len(strings.Fields(trimmed)[0]):])
		if rest == "" || strings.EqualFold(rest, "all") {
			if strings.EqualFold(rest, "all") {
				return loopCommand{action: loopActionStopAll}
			}
			// bare "stop" stops all when ambiguous; the caller may special-case a
			// single active loop.
			return loopCommand{action: loopActionStop, targetID: ""}
		}
		return loopCommand{action: loopActionStop, targetID: strings.TrimSpace(rest)}
	}

	// Leading interval token: "5m <prompt>".
	if fields := strings.Fields(trimmed); len(fields) >= 2 {
		if d, ok := parseLoopInterval(fields[0]); ok {
			prompt := strings.TrimSpace(trimmed[len(fields[0]):])
			cmd := loopCommand{action: loopActionStart, mode: loopModeFixed, prompt: prompt}
			cmd.interval, cmd.note = clampLoopInterval(d)
			return cmd
		}
	}

	// Trailing "every <interval>".
	if prompt, d, ok := splitTrailingEvery(trimmed); ok {
		cmd := loopCommand{action: loopActionStart, mode: loopModeFixed, prompt: prompt}
		cmd.interval, cmd.note = clampLoopInterval(d)
		return cmd
	}

	// No interval anywhere -> self-paced.
	return loopCommand{action: loopActionStart, mode: loopModeSelfPaced, prompt: trimmed}
}

// splitTrailingEvery matches "<prompt> every <interval>" (the interval must be a
// real duration token so "check every PR" is not mis-parsed as a cadence).
func splitTrailingEvery(input string) (string, time.Duration, bool) {
	fields := strings.Fields(input)
	if len(fields) < 3 {
		return "", 0, false
	}
	last := fields[len(fields)-1]
	prev := fields[len(fields)-2]
	if !strings.EqualFold(prev, "every") {
		return "", 0, false
	}
	d, ok := parseLoopInterval(last)
	if !ok {
		return "", 0, false
	}
	prompt := strings.TrimSpace(strings.Join(fields[:len(fields)-2], " "))
	if prompt == "" {
		return "", 0, false
	}
	return prompt, d, true
}

// parseLoopInterval parses a bare "<N><unit>" token (s/m/h/d) into a duration.
func parseLoopInterval(token string) (time.Duration, bool) {
	m := loopIntervalRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(token)))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	unit := map[string]time.Duration{
		"s": time.Second,
		"m": time.Minute,
		"h": time.Hour,
		"d": 24 * time.Hour,
	}[m[2]]
	return time.Duration(n) * unit, true
}

// clampLoopInterval bounds an interval to [loopMinInterval, loopMaxInterval] and
// returns an advisory note when it was adjusted.
func clampLoopInterval(d time.Duration) (time.Duration, string) {
	switch {
	case d < loopMinInterval:
		return loopMinInterval, fmt.Sprintf("interval raised to the %s minimum", formatLoopDuration(loopMinInterval))
	case d > loopMaxInterval:
		return loopMaxInterval, fmt.Sprintf("interval lowered to the %s maximum", formatLoopDuration(loopMaxInterval))
	default:
		return d, ""
	}
}

// loopTickMsg is the between-turn poll that fires a due loop while the session is
// idle. seq invalidates a tick left over from before the loop set changed.
type loopTickMsg struct{ seq int }

// handleLoopCommand dispatches a parsed `/loop ...` invocation.
func (m model) handleLoopCommand(args string) (model, tea.Cmd) {
	cmd := parseLoopCommand(args)
	switch cmd.action {
	case loopActionList:
		return m.appendLoopSystem(m.loopListText()), nil
	case loopActionUsage:
		if len(m.loops) > 0 {
			return m.appendLoopSystem(m.loopListText()), nil
		}
		return m.appendLoopSystem(loopUsageText()), nil
	case loopActionStop:
		return m.stopLoop(cmd.targetID)
	case loopActionStopAll:
		return m.stopAllLoops()
	case loopActionStart:
		return m.startLoop(cmd)
	}
	return m, nil
}

// startLoop registers a new loop and (re)starts the idle poll ticker. The first
// iteration fires on the next idle tick (nextRunAt = now).
func (m model) startLoop(cmd loopCommand) (model, tea.Cmd) {
	if reason, ok := m.validateLoopTarget(cmd.prompt); !ok {
		return m.appendLoopSystem(reason), nil
	}
	// A /command loop must be fixed-interval. Self-paced mode drives a free-form
	// goal through the playbook + LOOP: control line (see firePrompt), which a
	// custom command's expanded template never carries — so a bare self-paced
	// /command would silently degrade to adaptive polling with no way to report
	// done. Require an explicit interval for command loops instead.
	if cmd.mode == loopModeSelfPaced && strings.HasPrefix(strings.TrimSpace(cmd.prompt), "/") {
		return m.appendLoopSystem("A /command loop needs an interval — e.g. /loop 5m " + strings.Fields(cmd.prompt)[0] + ". Self-paced mode (no interval) is for a free-form goal."), nil
	}
	m.loopCounter++
	id := "L" + strconv.Itoa(m.loopCounter)
	loop := &loopState{
		id:        id,
		mode:      cmd.mode,
		prompt:    cmd.prompt,
		interval:  cmd.interval,
		createdAt: m.now(),
		nextRunAt: m.now(), // fire the first iteration on the next idle tick
		paused:    m.planModeBlocksContinuations(),
	}
	m.loops = append(m.loops, loop)
	note := ""
	if cmd.note != "" {
		note = " (" + cmd.note + ")"
	}
	m = m.appendLoopSystem(fmt.Sprintf(
		"Loop %s started — %s%s. Runs while this session is idle; stop with /loop stop %s.",
		id, loop.cadenceText(), note, id))
	return m.ensureLoopTick()
}

// stopLoop stops one loop by id, or the single active loop when the id is omitted.
func (m model) stopLoop(id string) (model, tea.Cmd) {
	if len(m.loops) == 0 {
		return m.appendLoopSystem("No active loops."), nil
	}
	if id == "" {
		if len(m.loops) == 1 {
			return m.removeLoop(m.loops[0].id, "Loop "+m.loops[0].id+" stopped.").ensureLoopTick()
		}
		return m.appendLoopSystem("Multiple loops active — use /loop stop <id> or /loop stop all.\n" + m.loopListText()), nil
	}
	if m.findLoop(id) == nil {
		return m.appendLoopSystem("No loop " + id + ". " + m.loopListText()), nil
	}
	// removeLoop stops the poll ticker; re-arm it so any OTHER loops that remain
	// keep firing (ensureLoopTick no-ops when this was the last loop).
	return m.removeLoop(id, "Loop "+id+" stopped.").ensureLoopTick()
}

func (m model) stopAllLoops() (model, tea.Cmd) {
	if len(m.loops) == 0 {
		return m.appendLoopSystem("No active loops."), nil
	}
	n := len(m.loops)
	m.loops = nil
	m.loopSeq++ // invalidate the pending poll tick
	m.loopTicking = false
	return m.appendLoopSystem(fmt.Sprintf("Stopped all %d loop(s).", n)), nil
}

// fireDueLoopIfIdle fires the earliest due loop when the session is idle. Called
// from the poll tick; a no-op while a turn, modal, queued user message, or plan
// mode is pending (the loop simply waits for the next idle tick).
func (m model) fireDueLoopIfIdle() (model, tea.Cmd) {
	if m.loopBusy() || len(m.loops) == 0 || m.planModeBlocksContinuations() {
		return m, nil
	}
	now := m.now()
	var due *loopState
	for _, l := range m.loops {
		if l.due(now) && (due == nil || l.nextRunAt.Before(due.nextRunAt)) {
			due = l
		}
	}
	if due == nil {
		return m, nil
	}
	m.activeLoopID = due.id
	due.nextRunAt = time.Time{} // mark running
	return m.fireLoopPrompt(due.firePrompt())
}

// firePrompt is the text actually sent for this iteration. A self-paced plain
// prompt is wrapped in the maintenance playbook so the model knows to do one
// increment of work and end with a LOOP: control line; a /command or a
// fixed-interval prompt is sent verbatim.
func (l *loopState) firePrompt() string {
	if l.mode == loopModeSelfPaced && !strings.HasPrefix(strings.TrimSpace(l.prompt), "/") {
		return loopSelfPacePlaybook(l)
	}
	return l.prompt
}

// fireLoopPrompt runs a loop's prompt as a turn — a custom /command if it resolves
// to one, otherwise a plain prompt.
func (m model) fireLoopPrompt(prompt string) (model, tea.Cmd) {
	if strings.HasPrefix(strings.TrimSpace(prompt), "/") {
		if next, teaCmd, ok := m.handleUserCommand(prompt); ok {
			return next, teaCmd
		}
	}
	return m.launchPrompt(prompt)
}

// advanceLoop updates a loop after one of its iterations completes: it folds the
// result into the failure/no-progress guards, honors a self-paced DONE/next-wake
// control line, enforces the iteration and age caps, then schedules the next wake
// (or stops the loop).
func (m model) advanceLoop(id, finalAnswer string, runErr error) model {
	l := m.findLoop(id)
	if l == nil {
		return m // stopped mid-run
	}
	l.iteration++
	if runErr != nil {
		l.failRun++
	} else {
		l.failRun = 0
	}
	res := strings.TrimSpace(finalAnswer)
	if res != "" && res == l.lastResult {
		l.repeatRun++
	} else {
		l.repeatRun = 0
	}
	l.lastResult = res

	// Only self-paced iterations run the playbook, so only they can carry a control
	// line; a failed turn has no trustworthy final answer to read one from.
	done, wake := false, time.Duration(0)
	if l.mode == loopModeSelfPaced && runErr == nil {
		done, wake = parseLoopControl(finalAnswer)
	}

	switch {
	case done:
		return m.removeLoop(id, fmt.Sprintf("Loop %s finished — the task reported done after %d iteration(s).", id, l.iteration))
	case l.failRun >= loopMaxFailures:
		return m.removeLoop(id, fmt.Sprintf("Loop %s stopped after %d consecutive failed iterations.", id, l.failRun))
	case l.iteration >= l.iterationCap():
		return m.removeLoop(id, fmt.Sprintf("Loop %s stopped at its %d-iteration cap.", id, l.iteration))
	case !l.createdAt.IsZero() && m.now().Sub(l.createdAt) >= loopMaxAge:
		return m.removeLoop(id, fmt.Sprintf("Loop %s expired after %s.", id, formatLoopDuration(loopMaxAge)))
	case l.repeatRun >= loopDoomThreshold:
		return m.removeLoop(id, fmt.Sprintf("Loop %s stopped — %d identical results in a row (no progress).", id, l.repeatRun))
	}

	if l.mode == loopModeSelfPaced {
		// A model-chosen next wake (from the control line) wins; otherwise fall back to
		// an adaptive cadence that checks back often while work is fresh and settles to
		// a slow heartbeat as iterations accrue.
		delay := wake
		if delay <= 0 {
			delay = adaptiveSelfPaceDelay(l.iteration)
		}
		l.nextRunAt = m.now().Add(clampSelfPaceDelay(delay))
	} else {
		l.nextRunAt = m.now().Add(l.interval)
	}
	return m
}

// parseLoopControl reads a self-paced loop's control signal from the model's final
// answer. The last control line wins (the model may reference the protocol before
// emitting it). Returns done=true to stop the loop, or a next-wake hint (0 = none).
func parseLoopControl(answer string) (done bool, wake time.Duration) {
	// Normalize CRLF/CR so the line-end anchor can reach `$` (Go's (?m)$ sits right
	// before `\n`, and the horizontal-whitespace class can't consume a stray `\r`).
	norm := strings.ReplaceAll(answer, "\r\n", "\n")
	norm = strings.ReplaceAll(norm, "\r", "\n")
	matches := loopControlRe.FindAllStringSubmatch(norm, -1)
	if len(matches) == 0 {
		return false, 0
	}
	last := matches[len(matches)-1]
	if strings.EqualFold(last[1], "done") {
		return true, 0
	}
	if last[2] != "" {
		if d, ok := parseLoopInterval(last[2]); ok {
			return false, d
		}
	}
	return false, 0
}

// loopSelfPacePlaybook wraps a self-paced loop's goal in the maintenance-loop
// instructions the model follows each iteration: do one real increment of work,
// then emit a single control line so the harness knows whether to stop or when to
// wake next — the self-paced maintenance playbook the feature ships with.
func loopSelfPacePlaybook(l *loopState) string {
	var b strings.Builder
	b.WriteString("You are running in a self-paced maintenance loop (iteration ")
	b.WriteString(strconv.Itoa(l.iteration + 1))
	b.WriteString("). Do one concrete increment of work toward the goal below — actual work, not just a plan — then stop and report.\n\n")
	b.WriteString("Goal: ")
	b.WriteString(strings.TrimSpace(l.prompt))
	b.WriteString("\n\nEnd your reply with a control line — on its own line, starting with \"LOOP:\":\n")
	b.WriteString("  LOOP: DONE          — the goal is fully met; the loop stops.\n")
	b.WriteString("  LOOP: CONTINUE      — more to do; the loop wakes again automatically.\n")
	b.WriteString("  LOOP: CONTINUE 10m  — more to do; wake after the delay you name (e.g. 5m, 1h).\n")
	b.WriteString("A short note after the keyword is fine. Report DONE only when the goal is genuinely complete — the loop trusts that signal to stop.")
	return b.String()
}

// adaptiveSelfPaceDelay picks a self-paced loop's next-wake by iteration: frequent
// early (work is actively progressing), slowing to a heartbeat as it matures.
func adaptiveSelfPaceDelay(iteration int) time.Duration {
	switch {
	case iteration < 2:
		return 2 * time.Minute
	case iteration < 5:
		return 5 * time.Minute
	case iteration < 10:
		return 15 * time.Minute
	default:
		return 30 * time.Minute
	}
}

func (m model) findLoop(id string) *loopState {
	for _, l := range m.loops {
		if l.id == id {
			return l
		}
	}
	return nil
}

func (m model) removeLoop(id, note string) model {
	kept := make([]*loopState, 0, len(m.loops))
	for _, l := range m.loops {
		if l.id != id {
			kept = append(kept, l)
		}
	}
	m.loops = kept
	m.loopSeq++ // invalidate the pending poll tick; a fresh one is scheduled if loops remain
	m.loopTicking = false
	if note != "" {
		m = m.appendLoopSystem(note)
	}
	return m
}

// ensureLoopTick starts the idle poll ticker if it is not already running.
func (m model) ensureLoopTick() (model, tea.Cmd) {
	if m.loopTicking || len(m.loops) == 0 {
		return m, nil
	}
	m.loopTicking = true
	return m, m.scheduleLoopTick()
}

func (m model) scheduleLoopTick() tea.Cmd {
	seq := m.loopSeq
	return tea.Tick(loopPollInterval, func(time.Time) tea.Msg {
		return loopTickMsg{seq: seq}
	})
}

// loopBusy reports whether the session is too busy for a loop to fire — a turn,
// queued user message, compaction, or ANY blocking modal (permission/askUser/spec
// review, provider or MCP wizard, MCP manager, or an open picker) is in flight.
// The modal check is load-bearing: a loop must not launch a run behind an open
// /resume picker, or that run would complete into whatever session the user then
// switches to. Mirrors launchQueuedMessageIfReady so loops and queued prompts never
// collide.
func (m model) loopBusy() bool {
	return m.pending || m.exiting || m.compactInFlight || m.hasQueuedMessage() || !m.noBlockingModal()
}

func (m model) appendLoopSystem(text string) model {
	m.transcript = reduceTranscript(m.transcript, transcriptAction{kind: actionAppendSystem, text: text})
	return m
}

// loopActive reports whether any loop is running (for the footer + leave warning).
func (m model) loopActive() bool { return len(m.loops) > 0 }

// loopFooterSummary renders the persistent "N loops · next 3:05pm" footer segment,
// or "" when no loops are active.
func (m model) loopFooterSummary() string {
	if len(m.loops) == 0 {
		return ""
	}
	var next time.Time
	for _, l := range m.loops {
		if l.nextRunAt.IsZero() || l.paused {
			continue
		}
		if next.IsZero() || l.nextRunAt.Before(next) {
			next = l.nextRunAt
		}
	}
	label := fmt.Sprintf("%d loop", len(m.loops))
	if len(m.loops) != 1 {
		label += "s"
	}
	if next.IsZero() {
		return label
	}
	return label + " · next " + next.Format("3:04pm")
}

func (m model) loopListText() string {
	if len(m.loops) == 0 {
		return "No active loops."
	}
	var b strings.Builder
	b.WriteString("Active loops:\n")
	now := m.now()
	for _, l := range m.loops {
		when := "running"
		if !l.nextRunAt.IsZero() {
			if d := l.nextRunAt.Sub(now); d > 0 {
				when = "next in " + formatLoopDuration(d.Round(time.Second))
			} else {
				when = "due"
			}
		}
		fmt.Fprintf(&b, "  %s · %s · iter %d · %s · %s\n",
			l.id, l.cadenceText(), l.iteration, when, truncateLoopPrompt(l.prompt))
	}
	b.WriteString("Stop with /loop stop <id> or /loop stop all.")
	return b.String()
}

func loopUsageText() string {
	return "Repeat a prompt or command:\n" +
		"  /loop 5m /babysit-prs   — run a command every 5 minutes\n" +
		"  /loop watch CI every 2m — trailing interval form\n" +
		"  /loop keep tidying docs — self-paced (the model picks its own cadence and stops when done)\n" +
		"  /loop list              — show active loops\n" +
		"  /loop stop [id|all]     — stop a loop\n" +
		"Loops run while this session is idle and stop when you close it."
}

// validateLoopTarget rejects looping a built-in command or an unresolved custom
// command — only a prompt or an existing custom /command (.zero/commands/<name>.md)
// may loop. Resolving custom commands up front stops a typo'd `/loop 5m /nope` from
// being scheduled and then re-sent as literal text on every tick.
func (m model) validateLoopTarget(prompt string) (string, bool) {
	p := strings.TrimSpace(prompt)
	if p == "" {
		return "Nothing to loop — give a prompt or a /command.", false
	}
	if strings.HasPrefix(p, "/") {
		switch parseCommand(p).kind {
		case commandPrompt:
			// A bare "/" or similar that isn't a command — treat as a plain prompt.
		case commandUnknown:
			name, _ := splitUserCommand(p)
			if _, ok := m.lookupUserCommand(name); !ok {
				return "No command /" + name + " — a loop can only run a prompt or an existing custom /command.", false
			}
		default:
			return "A loop can only run a prompt or a custom /command, not a built-in like " + strings.Fields(p)[0] + ".", false
		}
	}
	return "", true
}

// clearLoopsForSessionSwitch drops every active loop and its ticker. Loops belong to
// the session that created them; carrying them across /new or /resume would fire the
// old session's prompt into an unrelated conversation. Returns the count cleared so
// the caller can note it. Pure state reset — no transcript writes.
// pauseLoopsForPlan marks every active loop paused so the idle ticker cannot
// fire implementation turns while plan mode is read-only. Returns how many
// loops were newly paused.
func (m model) pauseLoopsForPlan() (model, int) {
	n := 0
	for _, l := range m.loops {
		if l == nil || l.paused {
			continue
		}
		l.paused = true
		n++
	}
	return m, n
}

// resumeLoopsAfterPlan unpauses loops that were held while plan mode was
// active so the next idle tick can fire them again.
func (m model) resumeLoopsAfterPlan() model {
	for _, l := range m.loops {
		if l == nil {
			continue
		}
		l.paused = false
	}
	return m
}

func (m model) clearLoopsForSessionSwitch() (model, int) {
	n := len(m.loops)
	if n == 0 {
		return m, 0
	}
	m.loops = nil
	m.activeLoopID = ""
	m.loopTicking = false
	m.loopSeq++ // invalidate any pending poll tick
	return m, n
}

func truncateLoopPrompt(prompt string) string {
	const max = 48
	p := strings.TrimSpace(strings.ReplaceAll(prompt, "\n", " "))
	runes := []rune(p)
	if len(runes) <= max {
		return p
	}
	return strings.TrimSpace(string(runes[:max])) + "…"
}

// clampSelfPaceDelay bounds a model-chosen self-paced delay to the self-pace
// band, defaulting an unset/invalid value.
func clampSelfPaceDelay(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return loopSelfPaceDefault
	case d < loopSelfPaceMin:
		return loopSelfPaceMin
	case d > loopSelfPaceMax:
		return loopSelfPaceMax
	default:
		return d
	}
}

// formatLoopDuration renders a duration compactly (90s, 5m, 2h, 1d) for status
// text, preferring the largest whole unit.
func formatLoopDuration(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0 && d >= 24*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	case d%time.Hour == 0 && d >= time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d%time.Minute == 0 && d >= time.Minute:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	default:
		return strconv.Itoa(int(d/time.Second)) + "s"
	}
}

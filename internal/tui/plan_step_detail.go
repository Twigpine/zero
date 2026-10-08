// plan_step_detail.go makes the context-sidebar plan steps clickable: each step
// records the file mutations made while it was in_progress ("what was built"),
// and clicking a step drops a transcript card listing them. The work is captured
// from tool-result rows as they stream; the click maps a sidebar y-coordinate to
// a step, mirroring sidebarAgentSelectables' offset accounting.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/zeroruntime"
)

// planStepWork is one captured unit of implementation attributed to a plan step:
// a file mutation or command run while that step was in_progress.
type planStepWork struct {
	tool    string
	summary string
	detail  string // the tool's full output — the diff for edits, stdout/stderr for commands
}

// isPlanWorkTool reports whether a tool's result counts as implementation worth
// surfacing under a plan step — the file mutations and the commands run.
func isPlanWorkTool(name string) bool {
	switch name {
	case "write_file", "edit_file", "apply_patch", "bash", "exec_command":
		return true
	}
	return false
}

// isPlanCommandTool reports whether a captured work item is a command run (vs a
// file change), so the detail view can group them.
func isPlanCommandTool(name string) bool {
	return name == "bash" || name == "exec_command"
}

// planStepWorkSummary renders a concise one-line summary of a tool-result row
// for the step-detail card (the row's first line, else the tool name).
func planStepWorkSummary(row transcriptRow) string {
	text := strings.TrimSpace(strings.SplitN(row.text, "\n", 2)[0])
	if text == "" {
		text = row.tool
	}
	return text
}

// captureStepWork attributes a finished tool-result row to the plan step that
// was in_progress when it ran. No-op for non-mutation tools or when no step is
// active. Keyed by step content (stable enough across the model's full-replace
// plan updates for this read-only view).
func (m model) captureStepWork(row transcriptRow) model {
	if row.kind != rowToolResult || !isPlanWorkTool(row.tool) {
		return m
	}
	key := currentStepContent(m.plan.steps)
	if key == "" {
		return m
	}
	if m.stepWork == nil {
		m.stepWork = map[string][]planStepWork{}
	}
	m.stepWork[key] = append(m.stepWork[key], planStepWork{tool: row.tool, summary: planStepWorkSummary(row), detail: row.detail})
	return m
}

// captureStepNarration attributes a finalized assistant prose segment to the
// plan step that was in_progress when it streamed — the agent's own running
// account of the work, replayed as the step-detail card's explanation.
// Consecutive duplicates are collapsed so a re-emitted segment doesn't repeat.
func (m model) captureStepNarration(text string) model {
	text = strings.TrimSpace(text)
	if text == "" {
		return m
	}
	key := currentStepContent(m.plan.steps)
	if key == "" {
		return m
	}
	if m.stepNarration == nil {
		m.stepNarration = map[string][]string{}
	}
	prev := m.stepNarration[key]
	if n := len(prev); n > 0 && prev[n-1] == text {
		return m
	}
	m.stepNarration[key] = append(prev, text)
	return m
}

// planStepHit is a clickable plan-step row in the context sidebar.
type planStepHit struct {
	lineOffset int
	stepIndex  int
}

// sidebarPlanSelectables returns each plan step's clickable sidebar line. The
// PLAN section renders after AGENTS in renderContextSidebar: the AGENTS header +
// its body (the agent lines, or a 1-line placeholder), then a blank line and the
// PLAN header, then one line per step (sidebarPlanLines is one line per step).
// The offset accounting mirrors that layout exactly.
func (m model) sidebarPlanSelectables(width int) []planStepHit {
	if m.plan.isEmpty() {
		return nil
	}
	agentBody := len(m.sidebarAgentLines(width))
	if agentBody == 0 {
		agentBody = 1 // the "no agents spawned" placeholder occupies one line
	}
	base := 1 + agentBody + 2 // AGENTS header + body + (blank line + PLAN header)
	hits := make([]planStepHit, 0, len(m.plan.steps))
	for i := range m.plan.steps {
		hits = append(hits, planStepHit{lineOffset: base + i, stepIndex: i})
	}
	return hits
}

// planStepDetailRowID is the stable transcript id for the single plan-step
// detail card, so re-clicking toggles it instead of stacking duplicates.
const planStepDetailRowID = "plan/step-detail"

// dropTranscriptRowsByID returns the transcript with any rows carrying id removed.
func dropTranscriptRowsByID(rows []transcriptRow, id string) []transcriptRow {
	if id == "" {
		return rows
	}
	out := make([]transcriptRow, 0, len(rows))
	for _, r := range rows {
		if r.id != id {
			out = append(out, r)
		}
	}
	return out
}

// openPlanStepDetail toggles a transcript card explaining a plan step. The card
// adapts to the step's status: a finished step (completed/failed) reads as "what
// we did"; an unfinished step (pending or in_progress) reads as "what we will do
// / are doing". The card shows the local structured sections (outcome, the
// model's own note, the file changes + commands captured while it ran)
// immediately; the natural-language explanation is written FRESH BY THE MODEL on
// click. While that one-shot request is in flight the explanation reads "Writing
// explanation…" and the returned tea.Cmd produces the text, which replaces the
// card in place (by stable row id). Once written, the explanation is cached per
// step (content+status) so re-clicking is instant with no second model call.
// Clicking the open step hides it; clicking a different step replaces it, so at
// most one card shows at a time.
func (m model) openPlanStepDetail(stepIndex int) (model, tea.Cmd) {
	if stepIndex < 0 || stepIndex >= len(m.plan.steps) {
		return m, nil
	}
	wasOpen := m.planDetailOpen && m.planDetailStep == stepIndex
	m.transcript = dropTranscriptRowsByID(m.transcript, planStepDetailRowID)
	if wasOpen {
		m.planDetailOpen = false
		return m, nil
	}
	m.planDetailOpen = true
	m.planDetailStep = stepIndex

	step := m.plan.steps[stepIndex]
	key := planStepExplanationKey(step)

	// Cached (or no provider available): render the final card immediately, no
	// model call. With no provider, the local status-aware summary stands in.
	explanation, cached := m.stepExplanation[key]
	if cached {
		m.transcript = m.appendPlanStepCard(stepIndex, explanation, false)
		return m, nil
	}
	if m.provider == nil {
		m.transcript = m.appendPlanStepCard(stepIndex, "", false)
		return m, nil
	}

	// Not cached: drop the loading card now (immediate feedback) and dispatch the
	// one-shot write-up request; its result replaces this card in place.
	m.transcript = m.appendPlanStepCard(stepIndex, "", true)
	return m, m.requestPlanStepExplanation(stepIndex, step)
}

// appendPlanStepCard builds the plan-step detail card and appends it to the
// transcript (replacing any prior card by stable id is the caller's job). The
// explanation section shows: the loading line when loading is set; else the
// supplied model-written explanation; else (empty, not loading) a local
// status-aware fallback summary. The structured sections are always local.
func (m model) appendPlanStepCard(stepIndex int, explanation string, loading bool) []transcriptRow {
	step := m.plan.steps[stepIndex]
	work := m.stepWork[step.content]

	var changes, commands []planStepWork
	for _, w := range work {
		if isPlanCommandTool(w.tool) {
			commands = append(commands, w)
		} else {
			changes = append(changes, w)
		}
	}
	done := step.status == "completed" || step.status == "failed"

	// The lead section is titled with the step itself and opens with a
	// status-aware outcome line ("Done in 1m 20s." / "Not started yet…").
	sections := []commandSection{{
		Title: step.content,
		Lines: m.planStepOutcomeLines(step, len(changes), len(commands)),
	}}

	// The centerpiece: a fresh, plain-English explanation written by the model on
	// click — "what we did" for a finished step, "what we'll do" for a queued one.
	sections = append(sections, commandSection{
		Title: planStepExplanationTitle(step.status),
		Lines: m.planStepExplanationLines(step, explanation, loading),
	})

	// The model's own per-step note is a second, distilled statement of intent
	// (before) or record (after); surface it verbatim under a fitting label.
	if note := strings.TrimSpace(step.notes); note != "" {
		label := "Plan"
		if done {
			label = "Notes"
		}
		sections = append(sections, commandSection{Title: label, Lines: planWrapText(note, 76)})
	}

	if len(changes) > 0 {
		sections = append(sections, commandSection{
			Title: fmt.Sprintf("Files changed (%d)", len(changes)),
			Lines: planChangeLines(changes),
		})
	}
	if len(commands) > 0 {
		sections = append(sections, commandSection{
			Title: fmt.Sprintf("Commands run (%d)", len(commands)),
			Lines: planCommandLines(commands),
		})
	}

	card := renderPlanCard(commandOutput{
		Title:    fmt.Sprintf("Plan step %d of %d · %s", stepIndex+1, len(m.plan.steps), planStepStateLabel(step.status)),
		Status:   planStepDetailStatus(step.status),
		Sections: sections,
		Hints:    []string{planStepDetailHint(step.status)},
	})
	return appendTranscriptRow(m.transcript, transcriptRow{kind: rowSystem, tool: "plan", id: planStepDetailRowID, text: card})
}

// planStepExplanationKey is the cache key for a step's written explanation:
// content + status, so a step re-explains itself when it transitions (e.g.
// pending → completed reads forward then back), but a re-click in the same state
// is served from cache with no second model call.
func planStepExplanationKey(step planStep) string {
	return step.status + "\x00" + step.content
}

// planStepOutcomeLines opens the lead section with a status-aware framing of the
// step: a finished step gets an outcome + duration ("Done in 1m 20s.") and a
// tally of what was built; an in_progress step gets its running clock and the
// work so far; a pending step is framed forward-looking ("what this step will
// do"), since no work has been captured yet.
func (m model) planStepOutcomeLines(step planStep, nChanges, nCommands int) []string {
	switch step.status {
	case "completed":
		head := "Done."
		if d := formatPlanStepDuration(step.completedAt.Sub(step.startedAt)); d != "" {
			head = "Done in " + d + "."
		}
		return []string{head, planWorkTally("Built", nChanges, nCommands)}
	case "failed":
		head := "Failed."
		if d := formatPlanStepDuration(step.completedAt.Sub(step.startedAt)); d != "" {
			head = "Failed after " + d + "."
		}
		return []string{head, planWorkTally("Attempted", nChanges, nCommands)}
	case "in_progress":
		head := "In progress."
		if d := formatPlanStepDuration(m.planNow().Sub(step.startedAt)); d != "" {
			head = "In progress — running for " + d + "."
		}
		return []string{head, planWorkTally("So far", nChanges, nCommands)}
	default: // pending
		return []string{"Not started yet — here's what this step will do."}
	}
}

// planStepExplanationTitle is the header for the prose explanation section,
// matching the status framing the user asked for: "what we did" for a finished
// step, "what we'll do" for a queued one.
func planStepExplanationTitle(status string) string {
	switch status {
	case "completed", "failed":
		return "What we did"
	case "in_progress":
		return "What we're doing"
	default:
		return "What we'll do"
	}
}

// planStepExplanationLines is the prose explanation of a step. The model writes
// it fresh on click: while that one-shot request is in flight, loading is set
// and a "Writing explanation…" line shows; once it returns, explanation carries
// the written paragraph. When no model text is available (loading off, empty
// explanation — e.g. no provider, or the write-up failed) a local status-aware
// summary stands in, pointing at the structured sections below.
func (m model) planStepExplanationLines(step planStep, explanation string, loading bool) []string {
	if loading {
		return []string{"Writing explanation…"}
	}
	if e := strings.TrimSpace(explanation); e != "" {
		return planWrapText(e, 76)
	}
	switch step.status {
	case "completed":
		return planWrapText("This step finished. The files and commands it touched are listed below.", 76)
	case "failed":
		return planWrapText("This step failed. What it attempted is listed below.", 76)
	case "in_progress":
		return planWrapText("Work is underway. Changes and commands appear below as they happen.", 76)
	default: // pending
		return planWrapText("This step is queued and hasn't started. Once the agent reaches it, its file changes and commands will appear here.", 76)
	}
}

// planStepExplanationModel is the maximum time a one-shot step write-up may take
// before it's abandoned (the card then falls back to the local summary).
const planStepExplanationTimeout = 30 * time.Second

// requestPlanStepExplanation returns a tea.Cmd that asks the model, in ONE
// non-turn request, for a short plain-English write-up of the step — past tense
// for a finished step, future tense for an unsolved one. It reuses the TUI's own
// provider (no new client, no hardcoded provider) and emits a
// planStepExplanationMsg. The request is built from already-captured local data
// (the step text/status/notes plus a compact digest of the file edits, commands,
// and the agent's narration for that step), so it sends no code and asks for no
// jargon. It runs only on click — never pre-computed or in the background.
func (m model) requestPlanStepExplanation(stepIndex int, step planStep) tea.Cmd {
	provider := m.provider
	system, user := planStepExplanationPrompt(step, m.stepWork[step.content], m.stepNarration[step.content])
	effort := string(m.reasoningEffort)
	key := planStepExplanationKey(step)
	gen := m.planDetailGen // a new run bumps this; a result from an older gen is dropped
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), planStepExplanationTimeout)
		defer cancel()
		events, err := provider.StreamCompletion(ctx, zeroruntime.CompletionRequest{
			Messages:        zeroruntime.SeedMessages(system, user),
			ReasoningEffort: effort,
		})
		if err != nil {
			return planStepExplanationMsg{stepIndex: stepIndex, key: key, gen: gen, err: err}
		}
		collected := zeroruntime.CollectStream(ctx, events)
		if collected.Error != "" {
			return planStepExplanationMsg{stepIndex: stepIndex, key: key, gen: gen, err: fmt.Errorf("%s", collected.Error)}
		}
		return planStepExplanationMsg{stepIndex: stepIndex, key: key, gen: gen, text: strings.TrimSpace(collected.Text)}
	}
}

// planStepExplanationPrompt builds the system + user prompts for the one-shot
// step write-up. The user prompt carries the step's text, status, notes, and a
// compact digest of the captured work (file edits + commands) and the agent's
// own narration, and asks for a few plain, non-technical sentences framed in the
// tense that matches the step's state.
func planStepExplanationPrompt(step planStep, work []planStepWork, narration []string) (system, user string) {
	done := step.status == "completed" || step.status == "failed"
	tense := "future tense (what this step is going to do / is doing)"
	if done {
		tense = "past tense (what was accomplished in this step)"
	}
	system = "You explain one step of a coding agent's task plan to a non-technical person. " +
		"Write a SHORT, friendly, plain-English explanation — a few sentences at most. " +
		"No code, no file paths, no command lines, no jargon. Do not use bullet points or headings; " +
		"reply with the explanation prose only, nothing else. Frame it in " + tense + "."

	var b strings.Builder
	fmt.Fprintf(&b, "Step: %s\n", strings.TrimSpace(step.content))
	fmt.Fprintf(&b, "Status: %s\n", step.status)
	if note := strings.TrimSpace(step.notes); note != "" {
		fmt.Fprintf(&b, "The agent's own note for this step: %s\n", note)
	}

	var changes, commands []planStepWork
	for _, w := range work {
		if isPlanCommandTool(w.tool) {
			commands = append(commands, w)
		} else {
			changes = append(changes, w)
		}
	}
	if len(changes) > 0 {
		b.WriteString("Files changed during this step:\n")
		for _, w := range planStepDigestItems(changes, 8) {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}
	if len(commands) > 0 {
		b.WriteString("Commands run during this step:\n")
		for _, w := range planStepDigestItems(commands, 8) {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}
	if len(narration) > 0 {
		b.WriteString("The agent's running notes while doing this step:\n")
		digest := narration
		if len(digest) > 6 {
			digest = digest[len(digest)-6:]
		}
		for _, n := range digest {
			if n = strings.TrimSpace(n); n != "" {
				fmt.Fprintf(&b, "- %s\n", n)
			}
		}
	}
	if len(work) == 0 && len(narration) == 0 {
		b.WriteString("(No file changes, commands, or notes were captured for this step yet.)\n")
	}
	if done {
		b.WriteString("\nIn a few plain sentences, tell the user what we DID in this step.")
	} else {
		b.WriteString("\nIn a few plain sentences, tell the user what we ARE GOING TO DO (or are doing) in this step.")
	}
	return system, b.String()
}

// planStepDigestItems returns up to max one-line summaries of captured work for
// the prompt digest, trimmed to a sane length so a giant diff can't blow up the
// request. A trailing "(+N more)" marks truncation.
func planStepDigestItems(items []planStepWork, max int) []string {
	out := make([]string, 0, max+1)
	for i, w := range items {
		if i >= max {
			out = append(out, fmt.Sprintf("(+%d more)", len(items)-max))
			break
		}
		// Rune-safe truncation: byte slicing would split a multi-byte rune.
		s := truncateRunes(strings.TrimSpace(w.summary), 100)
		out = append(out, s)
	}
	return out
}

// planWorkTally summarizes how much work a step captured, or notes that none was
// recorded yet. verb frames it for the step's state ("Built", "So far", …).
func planWorkTally(verb string, nChanges, nCommands int) string {
	if nChanges == 0 && nCommands == 0 {
		return "No file changes or commands recorded yet."
	}
	var parts []string
	if nChanges > 0 {
		parts = append(parts, pluralCount(nChanges, "file change"))
	}
	if nCommands > 0 {
		parts = append(parts, pluralCount(nCommands, "command"))
	}
	return verb + " " + strings.Join(parts, " and ") + "."
}

// planChangeLines lists each file mutation as a bullet with a +added/−removed
// diffstat and a short excerpt of the changed lines.
func planChangeLines(items []planStepWork) []string {
	var out []string
	for _, w := range items {
		head := "• " + w.summary
		if add, del := planDiffStat(w.detail); add+del > 0 {
			head += fmt.Sprintf("  (+%d −%d)", add, del)
		}
		out = append(out, head)
		out = append(out, planDetailExcerpt(w.detail, 3)...)
	}
	return out
}

// planCommandLines lists each command run as a bullet with a short excerpt of
// its output.
func planCommandLines(items []planStepWork) []string {
	var out []string
	for _, w := range items {
		out = append(out, "• "+w.summary)
		out = append(out, planDetailExcerpt(w.detail, 3)...)
	}
	return out
}

// planDiffStat counts added/removed lines in a unified diff, ignoring the
// +++/--- file headers. Output that isn't a diff (e.g. a written file's body or
// command stdout) yields no counts.
func planDiffStat(detail string) (added, removed int) {
	for _, line := range strings.Split(detail, "\n") {
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return added, removed
}

// planDetailExcerpt returns up to max non-blank lines of a tool's output for the
// card, trimmed, with a trailing "… (N more lines)" when truncated. The card
// renderer collapses indentation, so this only needs to keep each line short.
func planDetailExcerpt(detail string, max int) []string {
	if strings.TrimSpace(detail) == "" {
		return nil
	}
	var kept []string
	for _, line := range strings.Split(detail, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		kept = append(kept, strings.TrimSpace(line))
	}
	more := 0
	if len(kept) > max {
		more = len(kept) - max
		kept = kept[:max]
	}
	if more > 0 {
		kept = append(kept, fmt.Sprintf("… (%d more line%s)", more, map[bool]string{true: "s", false: ""}[more != 1]))
	}
	return kept
}

// planStepStateLabel is the short human label after "Plan step N of M ·" in the
// card title.
func planStepStateLabel(status string) string {
	switch status {
	case "completed":
		return "done"
	case "failed":
		return "failed"
	case "in_progress":
		return "in progress"
	default:
		return "up next"
	}
}

// planStepDetailStatus maps a step's status to the card's status banner: a
// completed step reads ok, a failed step blocked, everything else neutral info.
func planStepDetailStatus(status string) commandStatus {
	switch status {
	case "completed":
		return commandStatusOK
	case "failed":
		return commandStatusBlocked
	default:
		return commandStatusInfo
	}
}

// planStepDetailHint is the trailing one-line hint, framing the card as a record
// ("what was done") or a preview ("what this step will do").
func planStepDetailHint(status string) string {
	switch status {
	case "completed", "failed":
		return "what was done in this step"
	case "in_progress":
		return "what this step is doing now"
	default:
		return "what this step will do"
	}
}

// formatPlanStepDuration renders a step's span in a friendlier form than the raw
// footer clock: seconds under a minute, "Nm Ns" above. A zero or negative span
// renders empty so callers can drop the clause.
func formatPlanStepDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()+0.5))
	}
	mins := int(d / time.Minute)
	secs := int((d % time.Minute) / time.Second)
	if secs == 0 {
		return fmt.Sprintf("%dm", mins)
	}
	return fmt.Sprintf("%dm %ds", mins, secs)
}

// planWrapText word-wraps s to width columns, returning one line per wrapped
// row, so a long note isn't truncated by the card's single-line fitter.
func planWrapText(s string, width int) []string {
	if width < 8 {
		width = 8
	}
	var out []string
	for _, para := range strings.Split(strings.TrimSpace(s), "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			continue
		}
		line := words[0]
		for _, w := range words[1:] {
			if len(line)+1+len(w) > width {
				out = append(out, line)
				line = w
				continue
			}
			line += " " + w
		}
		out = append(out, line)
	}
	return out
}

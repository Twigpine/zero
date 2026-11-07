package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The plan's detail lives in the SIDEBAR, in the space below ACTIVITY that was
// otherwise padded with blank lines to the token floor.
//
// It was an overlay over the chat, and that was wrong: an overlay composited
// into the transcript column collided with the conversation mid-render, so a
// twelve-task plan drew its phase list straight through the user's own prompt.
// The right column already owns "what is happening" — agents, files, activity —
// and had half a screen of dead space under it.
//
// Clicking a task row selects it; clicking the PLAN header collapses the whole
// section. Both are reachable by keyboard too, since the sidebar is not
// focusable and a mouse-only affordance is not an affordance.

// orchestrateTaskHit is one clickable plan row in the sidebar.
type orchestrateTaskHit struct {
	lineOffset int
	taskIndex  int
}

// sidebarOrchestrateSelectables returns the clickable plan rows with their line
// offsets, mirroring sidebarPlanSelectables' accounting exactly: AGENTS header +
// body, then a blank line and the PLAN header, then one line per rendered task.
//
// Derived from the SAME slice the renderer draws, so a row the renderer dropped
// (the live-task filter, the six-line cap) is never clickable — an offset table
// built from the full task list would map clicks onto rows that are not there.
func (m model) sidebarOrchestrateSelectables(width int) []orchestrateTaskHit {
	if m.orchestrate.isEmpty() {
		return nil
	}
	agentBody := len(m.sidebarAgentLines(width))
	if agentBody == 0 {
		agentBody = 1
	}
	// Everything the PLAN section draws ABOVE the first task row: update_plan's
	// checklist when it has one, then the running plan's naming line and its
	// progress bar. MEASURED off the renderers rather than re-derived from the
	// conditions that produce them — the bar already had its own hand-written
	// correction term here, and a second one for the naming line would be a
	// second chance to disagree with what was actually drawn.
	steps := len(m.updatePlanStepLines(width))
	prefix := len(m.sidebarOrchestrateBlock(width)) - len(m.sidebarOrchestrateLines(width))
	base := 1 + agentBody + 2 + steps + prefix

	rows := m.sidebarOrchestrateRows()
	hits := make([]orchestrateTaskHit, 0, len(rows))
	for offset, index := range rows {
		if lineOffset := base + offset; m.sidebarRowOnScreen(lineOffset) {
			hits = append(hits, orchestrateTaskHit{lineOffset: lineOffset, taskIndex: index})
		}
	}
	return hits
}

// sidebarOrchestrateRows returns the task INDICES the sidebar draws, in order.
// One source for the renderer and the hit table, so they cannot disagree about
// which row is which.
func (m model) sidebarOrchestrateRows() []int {
	state := m.orchestrate
	if state.isEmpty() || state.sidebarCollapsed {
		return nil
	}
	// THE SAME WINDOW the renderer draws. These two disagreeing means a click
	// lands on a different task than the one under the cursor, so they read one
	// function rather than each re-deriving the slice — which is exactly how
	// they had already drifted, one taking the head and one the tail.
	live, _, _ := m.orchestrateVisibleRows(maxSidebarOrchestrateLines)
	rows := make([]int, 0, len(live))
	for _, task := range live {
		if index, ok := state.byID[task.id]; ok {
			rows = append(rows, index)
		}
	}
	return rows
}

// orchestrateHeaderAtMouse reports a click on the sidebar's PLAN header, which
// collapses and expands the whole section.
func (m model) orchestrateHeaderAtMouse(msg tea.MouseMsg) bool {
	if !m.sidebarActive() || m.orchestrate.isEmpty() {
		return false
	}
	// Each hit-tester supplies its own suggestionsActive() guard
	// because sidebarActive() deliberately does not exclude the palette; without
	// it, clicking the PLAN header while the / palette is open toggles the
	// section behind the overlay.
	if m.setup.visible || m.providerWizard != nil || m.mcpAddWizard != nil ||
		m.mcpManager != nil || m.picker != nil || m.suggestionsActive() {
		return false
	}
	sidebarW := sidebarWidth(m.width)
	if sidebarW <= 0 {
		return false
	}
	x0 := m.chatColumnWidth() + 3
	x, y := mouseX(msg), mouseY(msg)
	if x < x0 || x >= x0+sidebarW {
		return false
	}
	agentBody := len(m.sidebarAgentLines(sidebarW))
	if agentBody == 0 {
		agentBody = 1
	}
	// AGENTS header + body + blank line, then the PLAN header.
	return y == 1+agentBody+1
}

// sidebarPlanDetailLines renders the selected task into the space left between
// ACTIVITY and the token floor. Returns nothing when there is no room — the
// detail is the first thing to give way, since the task list above it is what
// the section is for.
func (m model) sidebarPlanDetailLines(width, budget int) []string {
	state := m.orchestrate
	if state.isEmpty() || state.sidebarCollapsed || budget < 4 {
		return nil
	}
	if m.orchestrateSelected < 0 || m.orchestrateSelected >= len(state.tasks) {
		return nil
	}
	task := state.tasks[m.orchestrateSelected]
	now := m.orchestrateNow()
	room := maxInt(6, width-3)

	lines := []string{
		"",
		m.postureHeaderWithCount("TASK", task.id, orchestrateStatusStyle(task.status), width),
	}

	head := task.status.label()
	if elapsed := task.elapsed(now); elapsed > 0 {
		head += " · " + formatElapsedSeconds(elapsed)
	}
	// A retried task ran more than once for one row's worth of elapsed time.
	// Said only when it happened, so the ordinary case is unchanged.
	if task.attempts > 1 {
		head += fmt.Sprintf(" · %d attempts", task.attempts)
	}
	lines = append(lines, " "+zeroTheme.muted.Render(truncateStep(head, room)))

	// WHICH MODEL DID THIS. Shown only when the task named one, because a line
	// against every task saying "on <the model you are already using>" buries
	// the one that differs — and a mixed-model plan is the only reason to look.
	if strings.TrimSpace(task.model) != "" {
		lines = append(lines, " "+zeroTheme.accent.Render(truncateStep("on "+task.model, room)))
	}
	// A REFUSED MODEL IS WORTH A LINE. It stays in the provider's list, so the
	// next plan chooses it again; without this the fallback is invisible here and
	// the only record of it is in the report the model reads, not the person.
	if fell := strings.TrimSpace(task.fellBackFrom); fell != "" {
		lines = append(lines, " "+zeroTheme.muted.Render(truncateStep(fell+" would not run", room)))
	}

	info, hasCard := m.specialists.getBySessionID(task.cardKey)
	if hasCard {
		meta := fmt.Sprintf("%d tool calls", info.toolCount)
		if info.toolCount == 1 {
			meta = "1 tool call"
		}
		if info.tokenCount > 0 {
			meta += " · " + formatTokenCount(info.tokenCount) + " tok"
		}
		lines = append(lines, " "+zeroTheme.faint.Render(truncateStep(meta, room)))
	}

	if len(task.dependsOn) > 0 {
		lines = append(lines, " "+zeroTheme.faint.Render(truncateStep("after "+strings.Join(task.dependsOn, ", "), room)))
	}
	// The REVERSE edge: what this task is holding up. A failure matters in
	// proportion to what waits on it, and the forward edge alone does not say.
	if blocks := state.dependents(task.id); len(blocks) > 0 {
		lines = append(lines, " "+zeroTheme.faint.Render(truncateStep("blocks "+strings.Join(blocks, ", "), room)))
	}

	// What it is doing right now, or where it ended up. One or the other — the
	// column has no room for both, and a running task's activity is the more
	// useful of the two.
	if hasCard && strings.TrimSpace(info.currentTool) != "" && task.status == orchestrateRunning {
		activity := info.currentTool
		if detail := strings.TrimSpace(info.currentDetail); detail != "" {
			activity += " " + detail
		}
		lines = append(lines, " "+zeroTheme.accent.Render("↳ ")+zeroTheme.faint.Render(truncateStep(activity, room-2)))
	} else if outcome := m.orchestrateOutcomeLine(task, hasCard, info); outcome != "" {
		lines = append(lines, " "+zeroTheme.faint.Render(truncateStep(outcome, room)))
	}

	// The prompt last: it is the least urgent and the first thing worth losing
	// when the column is short.
	if summary := strings.TrimSpace(task.summary); summary != "" && len(lines) < budget {
		lines = append(lines, " "+zeroTheme.faint.Render(truncateStep(summary, room)))
	}

	if len(lines) > budget {
		lines = lines[:budget]
	}
	return lines
}

// orchestrateStatusStyle is the header colour for a task's state, matching the
// glyph palette the task list uses.
func orchestrateStatusStyle(status orchestrateTaskStatus) lipgloss.Style {
	switch status {
	case orchestrateDone:
		return zeroTheme.green
	case orchestrateFailed:
		return zeroTheme.red
	case orchestrateRunning:
		return zeroTheme.accent
	default:
		return zeroTheme.faint
	}
}

// orchestrateOutcomeLine states where a task ended up, in one line.
func (m model) orchestrateOutcomeLine(task orchestrateTask, hasCard bool, info specialistInfo) string {
	switch task.status {
	case orchestrateRunning:
		return "still running…"
	case orchestratePending:
		if len(task.dependsOn) == 0 {
			return "queued"
		}
		return "waiting on " + strings.Join(task.dependsOn, ", ")
	case orchestrateDone:
		return "completed"
	}
	// Failed, skipped or cancelled: the reason is what matters, and the card's
	// error text is more specific than the status word.
	if hasCard && strings.TrimSpace(info.errorMsg) != "" {
		return firstLineOf(info.errorMsg)
	}
	return task.status.label()
}

func firstLineOf(text string) string {
	if index := strings.IndexAny(text, "\r\n"); index >= 0 {
		return strings.TrimSpace(text[:index])
	}
	return strings.TrimSpace(text)
}

// cycleOrchestrateSelection moves the sidebar's task selection. The sidebar is
// not focusable, so this is the keyboard route to what clicking a row does —
// a mouse-only affordance is not an affordance.
func (m model) cycleOrchestrateSelection() model {
	if len(m.orchestrate.tasks) == 0 {
		return m
	}
	m.orchestrateSelected = (m.orchestrateSelected + 1) % len(m.orchestrate.tasks)
	return m
}

// orchestratePlanBar is the progress bar under the sidebar's PLAN header, drawn
// only for an orchestrate plan — update_plan's steps have their own section
// conventions and no notion of failure to colour.
//
// It occupies ONE line, and sidebarOrchestrateSelectables accounts for it, so
// the click offsets below stay correct.
func (m model) orchestratePlanBar(width int) string {
	if m.orchestrate.isEmpty() || m.orchestrate.sidebarCollapsed {
		return ""
	}
	return m.posturePlanProgressBar(m.orchestrate, width)
}

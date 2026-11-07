package tui

import (
	"fmt"
	"strings"
	"testing"
)

func longPlanModel(t *testing.T, n int) model {
	t.Helper()
	msg := planAdmittedMsg{runID: 1, name: "sweep"}
	for index := 0; index < n; index++ {
		msg.tasks = append(msg.tasks, planGraphTask{id: fmt.Sprintf("t%02d", index)})
	}
	msg.taskCount = len(msg.tasks)
	return admittedModel(t, msg)
}

// THE DEFECT THIS EXISTS FOR. The large plan-size tier allows fifty tasks; a
// list pinned to the first twelve shows everything except the thing in
// progress.
func TestTheWindowFollowsTheRunningTask(t *testing.T) {
	m := longPlanModel(t, 40)
	m.orchestrate.markStarted("t30", "working", "k30", "", m.now())

	rendered := m.renderOrchestratePanel(100)
	if !strings.Contains(rendered, "t30") {
		t.Fatalf("the running task is not on screen:\n%s", rendered)
	}
	// ...and the window really moved, rather than the cap merely being larger.
	if strings.Contains(rendered, "t00") {
		t.Fatalf("the window did not move; it is still showing the head:\n%s", rendered)
	}
	if !strings.Contains(rendered, "more above") || !strings.Contains(rendered, "more below") {
		t.Fatalf("a scrolled window must name BOTH directions:\n%s", rendered)
	}
}

// With nothing running, the SELECTED task is what the window keeps in view —
// that is what makes ctrl+g and clicking able to move a long list at all.
func TestTheWindowFollowsTheSelectionWhenNothingRuns(t *testing.T) {
	m := longPlanModel(t, 40)
	m.orchestrateSelected = 35

	rows, above, below := m.orchestrateVisibleRows(6)
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.id)
	}
	joined := strings.Join(ids, ",")
	if !strings.Contains(joined, "t35") {
		t.Fatalf("the selected task is outside the window: %s", joined)
	}
	if above == 0 {
		t.Fatalf("a window near the end must report tasks above it: %s", joined)
	}
	_ = below
}

// RUNNING WINS OVER SELECTED. A user who clicked a task to read its detail is
// still watching the plan move; pinning the list to their last click would hide
// the work. The detail pane shows the selection regardless.
func TestARunningTaskOutranksTheSelectionForTheWindow(t *testing.T) {
	m := longPlanModel(t, 40)
	m.orchestrateSelected = 1
	m.orchestrate.markStarted("t30", "working", "k30", "", m.now())

	rows, _, _ := m.orchestrateVisibleRows(6)
	var sawRunning bool
	for _, row := range rows {
		if row.id == "t30" {
			sawRunning = true
		}
	}
	if !sawRunning {
		t.Fatal("the selection pinned the window and hid the running task")
	}
}

// A plan that fits needs no window and must report nothing hidden — the note is
// noise on a six-task plan.
func TestAShortPlanIsNotWindowed(t *testing.T) {
	m := longPlanModel(t, 4)
	rows, above, below := m.orchestrateVisibleRows(12)
	if len(rows) != 4 || above != 0 || below != 0 {
		t.Fatalf("rows=%d above=%d below=%d; a plan that fits must not be windowed", len(rows), above, below)
	}
	if note := orchestrateHiddenNote(above, below); note != "" {
		t.Fatalf("a plan that fits must say nothing about hidden rows: %q", note)
	}
}

// THE RENDERER AND THE HIT TABLE MUST AGREE. They are two doors onto one
// state — this is EQUALITY, not a subset: a click has to land on the task drawn
// under the cursor. They had already drifted, one taking the head and one the
// tail once everything faded, which is what a click on the wrong task looks
// like before anyone notices.
func TestTheSidebarListAndItsHitTableSeeTheSameTasks(t *testing.T) {
	m := longPlanModel(t, 40)
	m.orchestrate.markStarted("t30", "working", "k30", "", m.now())
	m.width = 160

	drawn, _, _ := m.orchestrateVisibleRows(maxSidebarOrchestrateLines)
	hitIndices := m.sidebarOrchestrateRows()
	if len(drawn) != len(hitIndices) {
		t.Fatalf("the renderer drew %d rows and the hit table has %d", len(drawn), len(hitIndices))
	}
	for position, index := range hitIndices {
		if index < 0 || index >= len(m.orchestrate.tasks) {
			t.Fatalf("hit row %d points outside the plan", position)
		}
		if got, want := m.orchestrate.tasks[index].id, drawn[position].id; got != want {
			t.Fatalf("row %d: the hit table says %q, the renderer drew %q", position, got, want)
		}
	}
}

// The bounds are the part that panics if it is wrong, so they are exercised
// directly across every shape rather than only through a rendered plan.
func TestTheWindowStaysInBounds(t *testing.T) {
	for _, count := range []int{0, 1, 5, 12, 13, 50} {
		for _, limit := range []int{0, 1, 6, 12} {
			for anchor := -2; anchor <= count+1; anchor++ {
				start, end, above, below := orchestrateWindow(count, limit, anchor)
				if start < 0 || end < start || end > count {
					t.Fatalf("count=%d limit=%d anchor=%d gave [%d:%d]", count, limit, anchor, start, end)
				}
				if above != start || below != count-end {
					t.Fatalf("count=%d limit=%d anchor=%d: counts %d/%d do not match [%d:%d]",
						count, limit, anchor, above, below, start, end)
				}
				if limit > 0 && count > 0 && end-start > limit {
					t.Fatalf("count=%d limit=%d anchor=%d drew %d rows", count, limit, anchor, end-start)
				}
				// An in-range anchor must actually be inside the window; that is
				// the entire point of the function.
				if limit > 0 && anchor >= 0 && anchor < count && (anchor < start || anchor >= end) {
					t.Fatalf("count=%d limit=%d anchor=%d fell outside [%d:%d]", count, limit, anchor, start, end)
				}
			}
		}
	}
}

// "+38 more" under a list whose first row is task 30 reads as though the plan
// has 68 tasks. Both directions are named, and singular reads as singular.
func TestTheHiddenNoteNamesBothDirections(t *testing.T) {
	if got := orchestrateHiddenNote(3, 4); got != "3 more above · 4 more below" {
		t.Fatalf("note = %q", got)
	}
	if got := orchestrateHiddenNote(1, 0); got != "1 more above" {
		t.Fatalf("note = %q", got)
	}
	if got := orchestrateHiddenNote(0, 1); got != "1 more below" {
		t.Fatalf("note = %q", got)
	}
}

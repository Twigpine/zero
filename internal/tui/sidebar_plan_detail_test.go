package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func sidebarDetailModel(t *testing.T) model {
	t.Helper()
	m := model{now: func() time.Time { return time.Unix(1000, 0) }}
	m.orchestrate.admit(diamondAdmitted(), m.now())
	now := m.now()
	m.orchestrate.markStarted("a", "survey the packages", "k1", "", now)
	m.orchestrate.markDone("a", "succeeded", 9000, 1, now)
	m.orchestrate.markStarted("b", "read the left branch", "k2", "", now)
	m.specialists.start("b", "read the left branch", "k2", now)
	m.specialists.incrementToolCount("k2")
	m.specialists.setCurrentTool("k2", "read_file", "internal/agent/loop.go")
	m.specialists.setTokens("k2", 21400)
	m.orchestrate.linkCard("b", "k2")
	m.orchestrateSelected = 1
	m.width, m.height = 140, 40
	m.altScreen = true
	// The sidebar only renders once there is real conversation
	// (sidebarAvailable), so an empty transcript would leave every interaction
	// below unreachable and the tests passing for the wrong reason.
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowUser, text: "hello"})
	if !m.sidebarActive() {
		t.Fatal("setup: the sidebar must be active for these interactions to be reachable")
	}
	return m
}

// THE DEAD SPACE IS THE POINT. The detail fills what the sidebar previously
// padded with blank lines down to the token floor.
func TestTheTaskDetailFillsTheSidebarsSpareSpace(t *testing.T) {
	m := sidebarDetailModel(t)
	rendered := stripANSILines(m.renderContextSidebar(34, 28))

	if !strings.Contains(rendered, "TASK") {
		t.Fatalf("the TASK section is missing:\n%s", rendered)
	}
	for _, want := range []string{"b", "running", "1 tool call", "21,400 tok", "read_file"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the detail does not show %q:\n%s", want, rendered)
		}
	}
}

// It gives way when there is no room: the task LIST is what the section is for,
// and a detail that pushed it off would be worse than no detail.
func TestTheDetailYieldsWhenTheColumnIsShort(t *testing.T) {
	m := sidebarDetailModel(t)
	if got := m.sidebarPlanDetailLines(34, 3); len(got) != 0 {
		t.Fatalf("with 3 rows to spare the detail must yield, got %d lines", len(got))
	}
	if got := m.sidebarPlanDetailLines(34, 8); len(got) == 0 {
		t.Fatal("with room to spare the detail must render")
	}
}

// THE OFFSET ARITHMETIC. The progress bar sits between the PLAN header and the
// first task, so every clickable row moves down one. A hit table that ignored
// it would select the task ABOVE the one clicked — silently.
func TestTheProgressBarIsAccountedForInClickOffsets(t *testing.T) {
	m := sidebarDetailModel(t)
	width := 34

	hits := m.sidebarOrchestrateSelectables(width)
	if len(hits) == 0 {
		t.Fatal("no clickable plan rows")
	}

	// Render the section and find where the first task actually lands.
	lines := stripANSILines(m.renderContextSidebar(width, 28))
	rows := strings.Split(lines, "\n")
	firstTaskRow := -1
	for index, row := range rows {
		if strings.Contains(row, "✓ a") {
			firstTaskRow = index
			break
		}
	}
	if firstTaskRow < 0 {
		t.Fatalf("the first task is not in the rendered sidebar:\n%s", lines)
	}
	if hits[0].lineOffset != firstTaskRow {
		t.Fatalf("the hit table puts the first task at row %d, it renders at %d — clicks would select the wrong task",
			hits[0].lineOffset, firstTaskRow)
	}
}

// FILES sits below the PLAN section, so its offsets must move with the bar too.
// Asserted against where the row actually RENDERS, not against a re-derivation
// of the arithmetic — the first version of this test recomputed the sum and
// would have passed with the bar unaccounted for on both sides.
func TestFileOffsetsAccountForTheProgressBar(t *testing.T) {
	m := sidebarDetailModel(t)
	// Touched files are derived from the transcript, so seed one the way a real
	// run would rather than poking a field.
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{
		kind:         rowToolResult,
		changedFiles: []string{"internal/tui/model.go"},
		detail:       "1 insertion(+), 1 deletion(-)",
	})
	if m.orchestratePlanBar(34) == "" {
		t.Fatal("setup: expected a progress bar")
	}

	hits := m.sidebarFileSelectables(34)
	if len(hits) == 0 {
		t.Fatal("setup: expected a clickable file row")
	}
	rows := strings.Split(stripANSILines(m.renderContextSidebar(34, 30)), "\n")
	rendered := -1
	for index, row := range rows {
		if strings.Contains(row, "model.go") {
			rendered = index
			break
		}
	}
	if rendered < 0 {
		t.Fatalf("the file row is not in the rendered sidebar:\n%s", strings.Join(rows, "\n"))
	}
	if hits[0].lineOffset != rendered {
		t.Fatalf("the file hit table says row %d, it renders at %d — a click would open the wrong thing",
			hits[0].lineOffset, rendered)
	}
}

// Main removed the permanent sidebar rail. Its old coordinates must not mutate
// compact run-details selection.
func TestFormerSidebarTaskRailDoesNotInterceptClicks(t *testing.T) {
	m := sidebarDetailModel(t)
	width := sidebarWidth(m.width)
	hits := m.sidebarOrchestrateSelectables(width)
	if len(hits) < 2 {
		t.Fatalf("expected several clickable rows, got %d", len(hits))
	}

	target := hits[0]
	before := m.orchestrateSelected
	x := m.chatColumnWidth() + 4
	updated, _ := m.Update(tea.MouseClickMsg{X: x, Y: target.lineOffset, Button: tea.MouseLeft})
	m = updated.(model)
	if m.orchestrateSelected != before {
		t.Fatalf("former sidebar coordinate changed selection from %d to %d", before, m.orchestrateSelected)
	}
}

func TestFormerSidebarPlanHeaderDoesNotInterceptClicks(t *testing.T) {
	m := sidebarDetailModel(t)
	sidebarW := sidebarWidth(m.width)
	agentBody := len(m.sidebarAgentLines(sidebarW))
	if agentBody == 0 {
		agentBody = 1
	}
	headerRow := 1 + agentBody + 1

	x := m.chatColumnWidth() + 4
	updated, _ := m.Update(tea.MouseClickMsg{X: x, Y: headerRow, Button: tea.MouseLeft})
	m = updated.(model)
	if m.orchestrate.sidebarCollapsed {
		t.Fatal("former sidebar header coordinate changed compact run-details state")
	}
}

// The keyboard reaches what the mouse does. The sidebar is not focusable, so a
// mouse-only affordance is not an affordance.
func TestCtrlGCyclesTheSidebarSelection(t *testing.T) {
	m := sidebarDetailModel(t)
	before := m.orchestrateSelected
	updated, _ := m.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
	m = updated.(model)
	if m.orchestrateSelected == before {
		t.Fatal("ctrl+g did not move the sidebar's task selection")
	}
	// ...and it wraps rather than sticking at the end.
	for range len(m.orchestrate.tasks) {
		updated, _ = m.Update(tea.KeyPressMsg{Code: 'g', Mod: tea.ModCtrl})
		m = updated.(model)
	}
	if m.orchestrateSelected < 0 || m.orchestrateSelected >= len(m.orchestrate.tasks) {
		t.Fatalf("selection escaped the task list: %d", m.orchestrateSelected)
	}
}

// The bar colours failure separately from progress, and never rounds a failure
// away to nothing.
func TestTheProgressBarShowsFailuresSeparately(t *testing.T) {
	m := sidebarDetailModel(t)
	m.orchestrate.markDone("b", "failed", 0, 1, m.now())

	bar := m.orchestratePlanBar(34)
	if bar == "" {
		t.Fatal("no progress bar rendered")
	}
	if !strings.Contains(bar, "1/4") {
		t.Fatalf("the bar must carry the count: %q", ansi.Strip(bar))
	}
	if strings.Count(ansi.Strip(bar), "█") < 2 {
		t.Fatalf("done and failed must both be drawn: %q", ansi.Strip(bar))
	}

	// THE ROUNDING CASE, which is the one that matters: more tasks than cells,
	// so a single failure divides to zero. A failure the bar rounds away is a
	// failure it does not show.
	big := model{now: m.now}
	msg := planAdmittedMsg{runID: 1, name: "big"}
	for index := 0; index < 30; index++ {
		msg.tasks = append(msg.tasks, planGraphTask{id: string(rune('a' + index%26))})
	}
	msg.taskCount = len(msg.tasks)
	big.orchestrate.admit(msg, big.now())
	big.orchestrate.tasks[0].status = orchestrateFailed

	plain := ansi.Strip(sidebarProgressBar(big.orchestrate, 34))
	if !strings.Contains(plain, "█") {
		t.Fatalf("one failure in thirty rounded away to nothing: %q", plain)
	}
}

// A task's reverse edge is shown: a failure matters in proportion to what waits
// on it, and the forward edge alone does not say.
func TestTheDetailShowsWhatATaskBlocks(t *testing.T) {
	m := sidebarDetailModel(t)
	m.orchestrateSelected = 0 // "a", which b and c depend on
	rendered := strings.Join(m.sidebarPlanDetailLines(34, 12), "\n")
	if !strings.Contains(ansi.Strip(rendered), "blocks b, c") {
		t.Fatalf("the detail does not show what the task blocks:\n%s", rendered)
	}
}

func TestFormerSidebarTaskRailDoesNotCreateHover(t *testing.T) {
	m := sidebarDetailModel(t)
	width := sidebarWidth(m.width)
	hits := m.sidebarOrchestrateSelectables(width)
	if len(hits) == 0 {
		t.Fatal("no clickable plan rows")
	}
	target := hits[0]

	m = m.updateHoverTarget(tea.MouseMotionMsg{X: m.chatColumnWidth() + 4, Y: target.lineOffset})
	if m.hover.kind != hoverNone {
		t.Fatalf("former sidebar rail created hover kind %v", m.hover.kind)
	}
}

// The hover is held by TASK ID, not by row index. A task that faded out of the
// list since the hover was set must stop highlighting rather than light up
// whatever row slid into its slot.
func TestAFadedTasksHoverStopsResolving(t *testing.T) {
	m := sidebarDetailModel(t)
	width := sidebarWidth(m.width)

	m.hover = hoverTarget{kind: hoverOrchestrateTask, taskID: "no-such-task"}
	if _, ok := m.hoveredSidebarLineOffset(width); ok {
		t.Fatal("a hover on a row that is gone still resolved to a line")
	}
}

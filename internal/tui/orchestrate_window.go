package tui

import "strconv"

// The plan list's WINDOW: which slice of a long plan the panel actually draws.
//
// Three surfaces truncated a long plan and all three took the FIRST N rows: the
// footer panel, the sidebar list, and the sidebar's hit table. That was
// survivable while the ceiling was twenty tasks and unacceptable now the large
// tier allows fifty — task 40 would be running and the user would be watching
// tasks 1 to 12, with "+38 more" underneath. A progress display that cannot show
// the thing in progress is not one.
//
// Rather than a scroll offset and another key chord, the window FOLLOWS: it
// keeps the anchor — the running task, or the one the user selected — in view.
// Selection already moves by click and by ctrl+g, so the list scrolls itself
// through the surfaces that exist rather than through new ones.
//
// ONE function, three callers. The three truncations had already drifted (two
// took the head, one took the tail when everything had faded), and a window that
// disagrees with the hit table means a click selects the wrong task —
// invariant 5 with a mouse attached.

// orchestrateWindow returns the visible slice of rows plus how many are hidden
// on each side, keeping index `anchor` inside the window.
//
// anchor is an index INTO rows. Out-of-range means "no anchor", which windows
// from the top exactly as before — a plan with nothing running and nothing
// selected should not scroll about.
func orchestrateWindow(count, limit, anchor int) (start, end, above, below int) {
	if count <= 0 {
		return 0, 0, 0, 0
	}
	// No room to draw anything. Everything is hidden BELOW rather than nowhere:
	// the counts must always account for every task, or a caller that adds them
	// up gets a different plan size depending on the terminal height.
	if limit <= 0 {
		return 0, 0, 0, count
	}
	if count <= limit {
		return 0, count, 0, 0
	}
	if anchor < 0 || anchor >= count {
		return 0, limit, 0, count - limit
	}
	// Centre on the anchor, then clamp. Centring rather than merely scrolling it
	// into view keeps the tasks either side of the running one visible, which is
	// what makes the shape readable while it moves.
	start = anchor - limit/2
	if start < 0 {
		start = 0
	}
	if start+limit > count {
		start = count - limit
	}
	return start, start + limit, start, count - (start + limit)
}

// orchestrateAnchor is the index in `rows` the window must keep visible: the
// running task if there is one, otherwise the selected task, otherwise none.
//
// RUNNING WINS over selected. A user who selected a task to read its detail is
// still watching the plan move; pinning the window to their last click would
// hide the work. The detail pane shows the selection either way, so nothing is
// lost by letting the list follow the run.
func orchestrateAnchor(rows []orchestrateTask, selectedID string) int {
	for index, task := range rows {
		if task.status == orchestrateRunning {
			return index
		}
	}
	if selectedID == "" {
		return -1
	}
	for index, task := range rows {
		if task.id == selectedID {
			return index
		}
	}
	return -1
}

// orchestrateSelectedID names the task the user has selected, or "" when the
// selection points nowhere. The panel state and the model's index live apart,
// so this is the one place that joins them.
func (m model) orchestrateSelectedID() string {
	if m.orchestrateSelected < 0 || m.orchestrateSelected >= len(m.orchestrate.tasks) {
		return ""
	}
	return m.orchestrate.tasks[m.orchestrateSelected].id
}

// orchestrateVisibleRows is THE windowing entry point every surface uses: the
// live tasks, windowed around the anchor, with the hidden counts.
func (m model) orchestrateVisibleRows(limit int) (rows []orchestrateTask, above, below int) {
	all := m.orchestrate.liveTasks(m.orchestrateNow())
	if len(all) == 0 {
		// Everything finished and faded. Show the TAIL rather than an empty
		// section — the end of a plan is what a user looks for once it is over.
		all = m.orchestrate.tasks
		if len(all) > limit {
			return all[len(all)-limit:], len(all) - limit, 0
		}
		return all, 0, 0
	}
	start, end, above, below := orchestrateWindow(len(all), limit, orchestrateAnchor(all, m.orchestrateSelectedID()))
	return all[start:end], above, below
}

// orchestrateHiddenNote renders the "more above / more below" line, or "" when
// nothing is hidden. Both directions are named: "+38 more" under a list whose
// first row is task 30 reads as though the plan has 68 tasks.
func orchestrateHiddenNote(above, below int) string {
	switch {
	case above > 0 && below > 0:
		return plural(above, "above") + " · " + plural(below, "below")
	case above > 0:
		return plural(above, "above")
	case below > 0:
		return plural(below, "below")
	default:
		return ""
	}
}

func plural(n int, where string) string {
	if n == 1 {
		return "1 more " + where
	}
	return strconv.Itoa(n) + " more " + where
}

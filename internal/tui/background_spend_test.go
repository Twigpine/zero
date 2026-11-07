package tui

import "testing"

// A BACKGROUND WORKER'S TOKENS AND TOOL COUNT SHOW, bridged from TaskOutput.
//
// THE MEASURED GAP. Four background workers rendered "0 tok · 0 tools" while the
// parent log recorded their real spend (W2: 1,355,600 tokens) — because a
// detached background child never streams via OnToolProgress, so the live
// bridge never saw it. TaskOutput is the one channel it has, and now carries the
// running totals: SET (whole), not added (per-turn).
func TestABackgroundWorkersSpendShowsFromThePoll(t *testing.T) {
	m := sidebarTestModel()
	m.activeRunID = 3
	u, _ := m.Update(specialistStartMsg{runID: 3, name: "worker", description: "W2: HTTP checker", childSessionID: "task_w2"})
	m = u.(model)

	// A poll mid-run: a running total.
	u, _ = m.Update(specialistTotalTokensMsg{runID: 3, toolCallID: "task_w2", totalTokens: 400000})
	m = u.(model)
	u, _ = m.Update(specialistToolCountMsg{runID: 3, toolCallID: "task_w2", tools: 12})
	m = u.(model)
	if info, _ := m.specialists.getBySessionID("task_w2"); info.tokenCount != 400000 || info.toolCount != 12 {
		t.Fatalf("first poll: tokens=%d tools=%d, want 400000 and 12", info.tokenCount, info.toolCount)
	}

	// A later poll: the total GREW. It must set, not double.
	u, _ = m.Update(specialistTotalTokensMsg{runID: 3, toolCallID: "task_w2", totalTokens: 1355600})
	m = u.(model)
	u, _ = m.Update(specialistToolCountMsg{runID: 3, toolCallID: "task_w2", tools: 40})
	m = u.(model)
	info, _ := m.specialists.getBySessionID("task_w2")
	if info.tokenCount != 1355600 {
		t.Fatalf("second poll set tokens to %d, want 1355600 (a running total, not a sum)", info.tokenCount)
	}
	if info.toolCount != 40 {
		t.Fatalf("tool count = %d, want 40", info.toolCount)
	}
}

// setToolCount never LOWERS the count — a late poll must not undo a higher live
// count from streaming.
func TestSetToolCountNeverGoesBackwards(t *testing.T) {
	m := sidebarTestModel()
	m.specialists.start("w", "d", "c1", m.now())
	m.specialists.setToolCount("c1", 20)
	m.specialists.setToolCount("c1", 5) // a stale/lower poll
	if info, _ := m.specialists.getBySessionID("c1"); info.toolCount != 20 {
		t.Fatalf("tool count regressed to %d, want 20", info.toolCount)
	}
}

// THE POLL-PARSE DECISION, pinned — the emit lives in the untestable agent loop.
func TestBackgroundPollUpdateParsesTheMeta(t *testing.T) {
	got, ok := backgroundPollUpdate("TaskOutput", map[string]string{
		"task_id": "task_w2", "status": "completed", "tokens": "1355600", "tools": "40",
	})
	if !ok || got.taskID != "task_w2" || got.tokens != 1355600 || got.tools != 40 || !got.done || got.status != specialistCompleted {
		t.Fatalf("terminal poll parsed to %+v (ok=%v)", got, ok)
	}
	// A running poll: spend present, not yet done.
	got, ok = backgroundPollUpdate("TaskOutput", map[string]string{"task_id": "t", "status": "running", "tokens": "500"})
	if !ok || got.done || got.tokens != 500 {
		t.Fatalf("running poll parsed to %+v", got)
	}
	// Not a TaskOutput, or no task id: no update.
	if _, ok := backgroundPollUpdate("read_file", map[string]string{"task_id": "t"}); ok {
		t.Fatal("a non-TaskOutput tool produced a poll update")
	}
	if _, ok := backgroundPollUpdate("TaskOutput", map[string]string{"status": "completed"}); ok {
		t.Fatal("a poll with no task id produced an update")
	}
}

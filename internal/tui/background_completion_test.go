package tui

import "testing"

// A BACKGROUND SUB-AGENT MUST EVENTUALLY COMPLETE — the regression the
// "don't complete on spawn" fix created.
//
// The row is registered keyed on the tool CALL id. A background Task returns a
// task_id (the child session id) and finishes later via a TaskOutput poll keyed
// on THAT id. Suppressing the spawn-time completion — correct in itself — left
// the row keyed on the call id, so the poll's completion, keyed on task_id,
// never found it. The agent ran forever.
//
// Driven through the real Update handler, because the defect is entirely in
// which key each message targets.
func TestABackgroundAgentCompletesWhenPolled(t *testing.T) {
	m := sidebarTestModel()
	m.activeRunID = 7

	// Spawn: the row is keyed on the tool call id.
	updated, _ := m.Update(specialistStartMsg{
		runID: 7, name: "worker", description: "W1", childSessionID: "call_abc", model: "glm-5.2",
	})
	m = updated.(model)

	// The background Task returns immediately. The DECISION to rebind is the
	// predicate below; a mutation removing the emission cannot be caught here
	// (the emission lives inside runAgentWithOptions, a full agent loop), so the
	// predicate is what is pinned, and the handler is driven with its output.
	from, to, ok := backgroundSpawnRebind("Task", "call_abc", map[string]string{"background": "true", "task_id": "task_xyz"})
	if !ok || from != "call_abc" || to != "task_xyz" {
		t.Fatalf("a background spawn did not ask to rebind: from=%q to=%q ok=%v", from, to, ok)
	}
	updated, _ = m.Update(specialistRebindMsg{runID: 7, fromKey: from, toKey: to})
	m = updated.(model)

	if _, ok := m.specialists.getBySessionID("task_xyz"); !ok {
		t.Fatal("the row did not rebind to the task id, so the poll can never find it")
	}

	// TaskOutput polls it terminal, keyed on the task id.
	updated, _ = m.Update(specialistCompleteMsg{
		runID: 7, toolCallID: "task_xyz", childSessionID: "task_xyz", status: specialistCompleted,
	})
	m = updated.(model)

	info, ok := m.specialists.getBySessionID("task_xyz")
	if !ok {
		t.Fatal("the row vanished after completion")
	}
	if info.status != specialistCompleted {
		t.Fatalf("a polled background agent is still %v, not completed — it runs forever", info.status)
	}
}

// A foreground Task is unchanged: it reconciles and completes inside the one
// completion message, and no rebind is emitted for it.
func TestAForegroundAgentStillCompletesWithoutARebind(t *testing.T) {
	m := sidebarTestModel()
	m.activeRunID = 7
	updated, _ := m.Update(specialistStartMsg{
		runID: 7, name: "explorer", description: "look", childSessionID: "call_1", model: "glm-5.2",
	})
	m = updated.(model)
	updated, _ = m.Update(specialistCompleteMsg{
		runID: 7, toolCallID: "call_1", childSessionID: "sess_1", status: specialistCompleted,
	})
	m = updated.(model)

	info, ok := m.specialists.getBySessionID("sess_1")
	if !ok {
		t.Fatal("the foreground row did not reconcile+complete")
	}
	if info.status != specialistCompleted {
		t.Fatalf("foreground agent status = %v", info.status)
	}
}

// THE REBIND DECISION, pinned. A foreground Task and a spawn whose ids already
// agree must NOT rebind; only a background spawn with a distinct task_id does.
func TestOnlyABackgroundSpawnWithADistinctTaskIDRebinds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tool       string
		toolCallID string
		meta       map[string]string
		wantOK     bool
	}{
		{"background, distinct task id", "Task", "call_1", map[string]string{"background": "true", "task_id": "task_1"}, true},
		{"foreground Task", "Task", "call_1", map[string]string{"session_id": "sess_1"}, false},
		{"background but task id equals the call id", "Task", "call_1", map[string]string{"background": "true", "task_id": "call_1"}, false},
		{"background with no task id", "Task", "call_1", map[string]string{"background": "true"}, false},
		{"another tool", "TaskOutput", "call_1", map[string]string{"background": "true", "task_id": "task_1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from, to, ok := backgroundSpawnRebind(tc.tool, tc.toolCallID, tc.meta)
			if ok != tc.wantOK {
				t.Fatalf("rebind ok = %v, want %v (from=%q to=%q)", ok, tc.wantOK, from, to)
			}
			if ok && (from != tc.toolCallID || to != tc.meta["task_id"]) {
				t.Fatalf("rebind from=%q to=%q, want %q -> %q", from, to, tc.toolCallID, tc.meta["task_id"])
			}
		})
	}
}

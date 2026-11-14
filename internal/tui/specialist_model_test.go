package tui

import "testing"

// A DELEGATED SUB-AGENT MUST NAME THE MODEL IT RUNS ON.
//
// The AGENTS sidebar already renders "on <model>" (sidebar.go), and it showed
// nothing for a Task sub-agent for its whole life — setModel was reached from
// the PLAN path alone, so a delegated agent was drawn as an anonymous "worker"
// while a plan task beside it named its model.
//
// Reproduced from the logs: every specialist_start the parent recorded carried
// model=(absent), while the child's own session metadata knew it was glm-5.2.
func TestADelegatedAgentNamesItsModelWhileItRuns(t *testing.T) {
	// THROUGH THE REAL HANDLER, not by calling setModel directly: an earlier
	// version did the latter and a mutation deleting the handler's setModel
	// call passed it cleanly, because nothing proved the message was consulted.
	m := sidebarTestModel()
	m.activeRunID = 7
	updated, _ := m.Update(specialistStartMsg{
		runID:          7,
		name:           "worker",
		description:    "W1: HTML link extractor",
		childSessionID: "call_1",
		model:          "glm-5.2",
	})
	live := updated.(model)

	info, ok := live.specialists.getBySessionID("call_1")
	if !ok {
		t.Fatal("the agent was not tracked")
	}
	if info.model != "glm-5.2" {
		t.Fatalf("a running sub-agent names model %q; the sidebar renders \"on <model>\" and has nothing to show", info.model)
	}
}

// startedAgent runs a specialist start through the real handler, seeded with
// the session's model, and returns the model.
func startedAgent(t *testing.T, seeded string) model {
	t.Helper()
	m := sidebarTestModel()
	m.activeRunID = 7
	updated, _ := m.Update(specialistStartMsg{
		runID:          7,
		name:           "code-review",
		description:    "audit",
		childSessionID: "call_1",
		model:          seeded,
	})
	return updated.(model)
}

// THE RESULT IS AUTHORITATIVE. A specialist whose manifest names its own model
// did not run on the session's, and the row seeded at start would keep naming
// the wrong one.
//
// Through the completion handler: calling setModel directly passed even with
// the handler's model update deleted.
func TestTheResultsModelCorrectsTheSeededOne(t *testing.T) {
	m := startedAgent(t, "glm-5.2")
	updated, _ := m.Update(specialistCompleteMsg{
		runID:          7,
		toolCallID:     "call_1",
		childSessionID: "child_1",
		status:         specialistCompleted,
		model:          "kimi-k2.6",
	})
	done := updated.(model)
	info, ok := done.specialists.getBySessionID("child_1")
	if !ok {
		t.Fatal("the completed agent was not tracked under its real session id")
	}
	if info.model != "kimi-k2.6" {
		t.Fatalf("the row still names %q after the executor reported kimi-k2.6", info.model)
	}
}

// An empty report must not blank a model already shown. Through the real
// handler: setModel clears the field for "", so only the handler's non-empty
// guard keeps the seeded model — and the old test never reached it.
func TestAnEmptyModelReportDoesNotBlankTheRow(t *testing.T) {
	m := startedAgent(t, "glm-5.2")
	updated, _ := m.Update(specialistCompleteMsg{
		runID:          7,
		toolCallID:     "call_1",
		childSessionID: "child_1",
		status:         specialistCompleted,
	})
	done := updated.(model)
	info, ok := done.specialists.getBySessionID("child_1")
	if !ok {
		t.Fatal("the completed agent was not tracked under its real session id")
	}
	if info.model != "glm-5.2" {
		t.Fatalf("an empty report blanked the row: %q", info.model)
	}
}

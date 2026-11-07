package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/specialist"
)

// THE ROW MUST NAME THE MODEL THAT RAN, not the one that was chosen.
//
// A task's model is set once at DISPATCH, from the model auto-assignment picked.
// When the provider refuses that model the executor re-runs the task on the
// session's — and the card went on displaying the refused one. The plan report
// the MODEL reads was corrected; the sidebar a PERSON reads was not, which is
// the wrong half to get right.
func TestTheSidebarNamesTheModelThatRanNotTheOneThatWasRefused(t *testing.T) {
	now := time.Now()
	state := &orchestratePanelState{}
	state.admit(planAdmittedMsg{name: "p", tasks: []planGraphTask{{id: "config-overrides"}}}, now)
	state.markStarted("config-overrides", "survey config precedence", "card_1",
		"grok-4.20-multi-agent-0309", now)

	// Dispatched on the assigned model, finished on the session's.
	state.markDoneOn("config-overrides", string(specialist.TaskSucceeded),
		"", "grok-4.20-multi-agent-0309", 1200, 2, now.Add(time.Second))

	task := state.tasks[state.byID["config-overrides"]]
	if task.model == "grok-4.20-multi-agent-0309" {
		t.Error("the row still claims the model the provider refused")
	}
	if task.fellBackFrom != "grok-4.20-multi-agent-0309" {
		t.Errorf("the refused model was not recorded for display: %q", task.fellBackFrom)
	}
}

// An ordinary task must be untouched: markDone is the overwhelming majority of
// calls and a fallback is rare, so nothing about the common row may change.
func TestAnOrdinaryTaskKeepsItsAssignedModelOnCompletion(t *testing.T) {
	now := time.Now()
	state := &orchestratePanelState{}
	state.admit(planAdmittedMsg{name: "p", tasks: []planGraphTask{{id: "t"}}}, now)
	state.markStarted("t", "work", "card_1", "grok-4.5", now)
	state.markDone("t", string(specialist.TaskSucceeded), 900, 1, now.Add(time.Second))

	task := state.tasks[state.byID["t"]]
	if task.model != "grok-4.5" {
		t.Errorf("a normal task lost its model on completion: %q", task.model)
	}
	if task.fellBackFrom != "" {
		t.Errorf("a normal task was marked as a fallback: %q", task.fellBackFrom)
	}
}

// The refused model is shown, because it stays in the provider's list and the
// next plan will choose it again unless someone excludes it.
func TestTheRefusedModelIsRenderedInTheTaskDetail(t *testing.T) {
	m := model{now: func() time.Time { return time.Unix(1000, 0) }}
	now := m.now()
	m.orchestrate.admit(planAdmittedMsg{name: "p", tasks: []planGraphTask{{id: "t"}}}, now)
	m.orchestrate.markStarted("t", "work", "k1", "grok-4.20-multi-agent-0309", now)
	m.orchestrate.markDoneOn("t", string(specialist.TaskSucceeded),
		"", "grok-4.20-multi-agent-0309", 10, 2, now)
	m.orchestrate.linkCard("t", "k1")
	m.orchestrateSelected = 0
	m.width, m.height = 140, 40
	m.altScreen = true

	rendered := strings.Join(m.sidebarPlanDetailLines(60, 40), "\n")
	if !strings.Contains(rendered, "grok-4.20-multi-agent-0309") {
		t.Errorf("the refused model is invisible to the user:\n%s", rendered)
	}
	if !strings.Contains(rendered, "would not run") {
		t.Errorf("the detail does not say the model was refused:\n%s", rendered)
	}
}

// THE BRIDGE MUST ACTUALLY CARRY IT. The three tests above drive markDoneOn
// directly, which proves the panel handles a fallback and proves nothing about
// whether anything ever tells it one happened — the "assert the helper, not the
// caller that consults it" shape. A mutation that stopped the bridge sending
// RetriedOnParentModel passed all three.
func TestTheBridgeCarriesWhatTheTaskActuallyRanOnToTheSurface(t *testing.T) {
	var sent []tea.Msg
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(msg tea.Msg) { sent = append(sent, msg) }, 7, nil, "")

	bridge.TaskDispatched(specialist.Task{
		ID: "config-overrides", Prompt: "survey", Model: "grok-4.20-multi-agent-0309",
	})
	bridge.TaskCompleted(specialist.TaskResult{
		ID:       "config-overrides",
		Outcome:  specialist.TaskSucceeded,
		Attempts: 2,
		// Ran on the session's model after the assigned one was refused.
		Model:                "",
		RetriedOnParentModel: "grok-4.20-multi-agent-0309",
	})

	var done *planTaskDoneMsg
	for _, msg := range sent {
		if typed, ok := msg.(planTaskDoneMsg); ok {
			done = &typed
		}
	}
	if done == nil {
		t.Fatalf("no done message reached the surface: %#v", sent)
	}
	if done.fellBackFrom != "grok-4.20-multi-agent-0309" {
		t.Errorf("the bridge dropped the refused model: %q", done.fellBackFrom)
	}
	if done.model != "" {
		t.Errorf("the bridge reported a model the task did not run on: %q", done.model)
	}
}

// AND THE HANDLER MUST PASS IT ON. Bridge→message is covered above and
// message→panel by markDoneOn, which leaves the seam BETWEEN them: the Update
// case that unpacks the message. A mutation dropping fellBackFrom there passed
// every other test in this file — the same shape, one link further along.
//
// Three links, three tests, because a chain is only as covered as its weakest
// joint and this defect class is exactly a joint nobody asserted.
func TestTheUpdateHandlerPassesTheRefusedModelIntoThePanel(t *testing.T) {
	m := model{now: func() time.Time { return time.Unix(1000, 0) }}
	m.activeRunID = 3
	m.orchestrate.admit(planAdmittedMsg{name: "p", tasks: []planGraphTask{{id: "t"}}}, m.now())
	m.orchestrate.markStarted("t", "work", "k1", "grok-4.20-multi-agent-0309", m.now())
	// The AGENTS tracker is set at START from the assigned model — same path a
	// real dispatch takes. Without that seed this test would pass even if the
	// done handler never corrected the card.
	m.specialists.start("t", "work", "k1", m.now())
	m.specialists.setModel("k1", "grok-4.20-multi-agent-0309")

	updated, _ := m.Update(planTaskDoneMsg{
		runID:        3,
		taskID:       "t",
		cardKey:      "k1",
		dispatched:   true,
		status:       specialistCompleted,
		outcome:      string(specialist.TaskSucceeded),
		attempts:     2,
		model:        "",
		fellBackFrom: "grok-4.20-multi-agent-0309",
	})
	next, ok := updated.(model)
	if !ok {
		t.Fatalf("Update returned %T", updated)
	}
	task := next.orchestrate.tasks[next.orchestrate.byID["t"]]
	if task.fellBackFrom != "grok-4.20-multi-agent-0309" {
		t.Errorf("the handler dropped the refused model: %q", task.fellBackFrom)
	}
	if task.model == "grok-4.20-multi-agent-0309" {
		t.Error("the panel still claims the model the provider refused")
	}
	// THE AGENTS SURFACE MUST AGREE. PLAN was corrected first; leaving AGENTS on
	// the refused model is the same silent-fallback defect one column over.
	info, ok := next.specialists.getBySessionID("k1")
	if !ok {
		t.Fatal("the agent card disappeared on completion")
	}
	if info.model == "grok-4.20-multi-agent-0309" {
		t.Error("the AGENTS row still claims the model the provider refused")
	}
	if info.model != "" {
		t.Errorf("fallback finished on the session's model, but the card names %q", info.model)
	}
}

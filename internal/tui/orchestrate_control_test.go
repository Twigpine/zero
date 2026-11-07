package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/specialist"
	"github.com/Twigpine/zero/internal/streamjson"
)

// runningBridge is a bridge with a plan in flight, holding a cancel whose
// effect the test can observe.
func runningBridge(t *testing.T) (*PlanProgressBridge, context.Context) {
	t.Helper()
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(tea.Msg) {}, 1, nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bridge.PlanRunning(cancel)
	return bridge, ctx
}

// STOPPING A PLAN MUST NOT STOP THE TURN. Ctrl-C cancels the run; this cancels
// only the context the plan runs under, which is a child of it.
func TestStoppingAPlanCancelsOnlyThePlansContext(t *testing.T) {
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(tea.Msg) {}, 1, nil, "")
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	planCtx, cancelPlan := context.WithCancel(runCtx)
	defer cancelPlan()
	bridge.PlanRunning(cancelPlan)

	if !bridge.StopPlan() {
		t.Fatal("StopPlan reported no running plan")
	}
	if planCtx.Err() == nil {
		t.Fatal("the plan's context was not cancelled")
	}
	if runCtx.Err() != nil {
		t.Fatal("stopping the plan cancelled the whole run; the turn must survive")
	}
}

// A control verb with no plan running must SAY so rather than appear to work.
func TestPlanControlWithNoPlanRunningRefusesWithAReason(t *testing.T) {
	m := model{planProgress: NewPlanProgressBridge()}
	for _, verb := range []string{"stop", "pause", "resume"} {
		text := m.orchestrateControlText(verb)
		if !strings.Contains(text, "status: warning") {
			t.Errorf("/plans %s with no plan running: %q", verb, text)
		}
		if !strings.Contains(text, "No plan is running") {
			t.Errorf("/plans %s must name the reason: %q", verb, text)
		}
	}
}

func TestSessionSwitchCommandsRefuseDuringBackgroundLaunchWindow(t *testing.T) {
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(tea.Msg) {}, 1, nil, "session-a")
	// The launcher sets this before admission or PlanRunning reaches the bridge.
	bridge.SetBackground(true)

	for _, command := range []parsedCommand{{kind: commandNew}, {kind: commandResume}} {
		m := model{planProgress: bridge}
		updated, _ := m.dispatchCommand(command)
		text := transcriptText(updated.(model).transcript)
		if !strings.Contains(strings.ToLower(text), "background plan") {
			t.Fatalf("%v switched sessions during the launch window: %q", command.kind, text)
		}
	}
}

// THE PAUSE ACTUALLY HOLDS THE EXECUTOR, and the release actually releases it.
// Asserting the boolean alone would pass against a flag nothing waits on.
func TestPauseHoldsTheExecutorAtATaskBoundary(t *testing.T) {
	bridge, ctx := runningBridge(t)
	if !bridge.SetPlanPaused(true) {
		t.Fatal("SetPlanPaused reported no running plan")
	}

	released := make(chan struct{})
	go func() {
		bridge.WaitWhilePaused(ctx)
		close(released)
	}()

	select {
	case <-released:
		t.Fatal("WaitWhilePaused returned while paused; nothing is holding the executor")
	case <-time.After(80 * time.Millisecond):
	}

	bridge.SetPlanPaused(false)
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("the executor was never released after resume")
	}
}

// STOPPING A PAUSED PLAN MUST NOT DEADLOCK — a plan cancelled on paper and
// parked forever in fact is the worst of both. Two separate things have to
// hold, and the mutation sweep showed they are not the same thing: the waiter
// has to be released (ctx does that), AND the reported pause state has to
// follow (clearing the flag does that). Asserting only the first passes against
// a bridge that goes on calling itself paused.
func TestStoppingAPausedPlanReleasesTheExecutor(t *testing.T) {
	bridge, ctx := runningBridge(t)
	bridge.SetPlanPaused(true)

	released := make(chan struct{})
	go func() {
		bridge.WaitWhilePaused(ctx)
		close(released)
	}()
	time.Sleep(30 * time.Millisecond)

	bridge.StopPlan()
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("a stopped plan is still parked in the pause; stop must release the waiter")
	}

	// AND THE REPORTED STATE MUST FOLLOW. The cancel alone frees the waiter —
	// WaitWhilePaused selects on ctx — so releasing it proves nothing about the
	// pause FLAG. Left set, the surface goes on offering "/plans resume" for a
	// plan that is being abandoned.
	if bridge.PlanPaused() {
		t.Fatal("a stopped plan still reports itself paused, so the surface would offer to resume it")
	}
	m := model{planProgress: bridge}
	if strings.Contains(m.planControlHint(), "resume") {
		t.Fatalf("the hint offers resume on a stopped plan: %q", m.planControlHint())
	}
}

// A waiter that arrives AFTER the resume must not block. This is why resume is
// a closed channel rather than a signal nobody is there to receive.
func TestAWaiterArrivingAfterTheResumeDoesNotBlock(t *testing.T) {
	bridge, ctx := runningBridge(t)
	bridge.SetPlanPaused(true)
	bridge.SetPlanPaused(false)

	done := make(chan struct{})
	go func() { bridge.WaitWhilePaused(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a waiter that arrived after the resume is stuck")
	}
}

// The plan's context alone must release the waiter too: WaitWhilePaused takes
// ctx precisely so a cancellation from anywhere ends the wait.
func TestACancelledContextReleasesThePause(t *testing.T) {
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(tea.Msg) {}, 1, nil, "")
	ctx, cancel := context.WithCancel(context.Background())
	bridge.PlanRunning(cancel)
	bridge.SetPlanPaused(true)

	done := make(chan struct{})
	go func() { bridge.WaitWhilePaused(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a cancelled context did not release the pause")
	}
}

// THE HANDLE IS DROPPED WHEN THE PLAN ENDS. A stale cancel would let a later
// "/plans stop" cancel a context that has since been reused — the PostureGate
// lifetime mistake in another costume.
func TestTheCancelHandleIsDroppedWhenThePlanEnds(t *testing.T) {
	bridge, _ := runningBridge(t)
	if !bridge.PlanRunningNow() {
		t.Fatal("the bridge does not consider the plan running")
	}
	bridge.PlanCompleted(samplePlan(t), specialist.PlanReport{Status: specialist.PlanCompleted, Succeeded: 1})
	if bridge.PlanRunningNow() {
		t.Fatal("the cancel handle survived the plan")
	}
	if bridge.StopPlan() {
		t.Fatal("StopPlan acted on a finished plan")
	}
}

// A NEW PLAN MUST NOT START PAUSED. The user paused the last one; carrying that
// into the next plan would suspend work nobody asked to suspend.
func TestANewPlanDoesNotInheritTheLastPlansPause(t *testing.T) {
	bridge, _ := runningBridge(t)
	bridge.SetPlanPaused(true)

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge.PlanRunning(cancel)
	if bridge.PlanPaused() {
		t.Fatal("the new plan inherited the previous plan's pause")
	}
}

// The hint is offered only while a plan is running: advertising "stop" against a
// finished plan is an offer the next line refuses.
func TestTheControlHintIsOfferedOnlyWhileAPlanRuns(t *testing.T) {
	idle := model{planProgress: NewPlanProgressBridge()}
	if strings.Contains(idle.planControlHint(), "stop") {
		t.Fatal("the hint offers stop with no plan running")
	}

	bridge, _ := runningBridge(t)
	running := model{planProgress: bridge}
	if !strings.Contains(running.planControlHint(), "/plans stop") {
		t.Fatalf("a running plan must advertise the verbs: %q", running.planControlHint())
	}
	bridge.SetPlanPaused(true)
	if !strings.Contains(running.planControlHint(), "/plans resume") {
		t.Fatalf("a paused plan must advertise resume: %q", running.planControlHint())
	}
}

// An unrecognised verb is refused by NAME, and bare /plans keeps its old
// behaviour exactly — the command that existed before this must not change.
func TestPlansKeepsItsGraphAndRefusesUnknownVerbs(t *testing.T) {
	m := model{planProgress: NewPlanProgressBridge()}
	if got := m.orchestrateControlText(""); got != m.orchestratePlansText() {
		t.Fatalf("bare /plans changed:\n%q\nvs\n%q", got, m.orchestratePlansText())
	}
	unknown := m.orchestrateControlText("halt")
	if !strings.Contains(unknown, "halt") || !strings.Contains(unknown, "status: warning") {
		t.Fatalf("an unknown verb must be refused by name: %q", unknown)
	}
}

// PROBLEM 1: card keys collided. `dispatched` reset on every Attach, so a
// background plan still dispatching when the next run attached would restart the
// counter and hand the new run's tasks the same keys — one card overwriting
// another, which is the specialist-card collision defect in a new costume.
func TestCardKeysDoNotRestartWhenANewRunAttaches(t *testing.T) {
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(tea.Msg) {}, 1, nil, "")

	seen := map[string]bool{}
	collect := func(runID int) {
		var got []tea.Msg
		bridge.Attach(func(msg tea.Msg) { got = append(got, msg) }, runID, nil, "")
		for i := 0; i < 3; i++ {
			bridge.TaskDispatched(specialist.Task{ID: "t", Prompt: "x"})
		}
		for _, msg := range got {
			if start, ok := msg.(planTaskStartMsg); ok {
				if seen[start.cardKey] {
					t.Fatalf("card key %q was handed out twice", start.cardKey)
				}
				seen[start.cardKey] = true
			}
		}
	}
	collect(2)
	collect(3)
	if len(seen) != 6 {
		t.Fatalf("got %d distinct card keys for 6 dispatches", len(seen))
	}
}

// PROBLEM 2: a background plan's progress was dropped. The stale-run guard
// discards anything whose runID is not the active one, which is right for a
// finished run's leftovers and wrong for a plan that is still working — the
// panel would simply freeze with no error and no card.
func TestABackgroundPlansProgressSurvivesALaterRun(t *testing.T) {
	m := model{now: func() time.Time { return time.Unix(1000, 0) }, activeRunID: 9,
		planProgress: NewPlanProgressBridge()}

	// A plan admitted under an EARLIER run, marked background.
	admitted := planAdmittedMsg{runID: 4, name: "bg", taskCount: 1,
		tasks: []planGraphTask{{id: "a"}}, background: true}
	updated, _ := m.Update(admitted)
	after := updated.(model)
	if after.orchestrate.isEmpty() {
		t.Fatal("a background plan's admission was dropped by the stale-run guard; the panel would never show it")
	}

	// ...and a FOREGROUND message from a stale run is still dropped, which is
	// what makes the guard worth keeping at all.
	stale := planAdmittedMsg{runID: 4, name: "old", taskCount: 1, tasks: []planGraphTask{{id: "z"}}}
	fresh := model{now: func() time.Time { return time.Unix(1000, 0) }, activeRunID: 9,
		planProgress: NewPlanProgressBridge()}
	staleUpdated, _ := fresh.Update(stale)
	if !staleUpdated.(model).orchestrate.isEmpty() {
		t.Fatal("a stale foreground plan was accepted; the guard no longer guards")
	}
}

// The completion the MODEL is told about: drained once, never twice, and it
// names the plan — by the time it arrives the conversation has moved on.
func TestABackgroundCompletionIsDeliveredOnceAndNamesThePlan(t *testing.T) {
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(tea.Msg) {}, 1, nil, "")
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge.PlanRunning(cancel)
	bridge.SetBackground(true)

	plan := samplePlan(t)
	bridge.PlanCompleted(plan, specialist.PlanReport{Status: specialist.PlanPartial, Succeeded: 1, Failed: 1})

	first := bridge.DrainCompletedPlans()
	if !strings.Contains(first, plan.Name()) {
		t.Fatalf("the completion must name the plan: %q", first)
	}
	if !strings.Contains(first, "partial") {
		t.Fatalf("the completion must carry the result: %q", first)
	}
	if second := bridge.DrainCompletedPlans(); second != "" {
		t.Fatalf("a completion was delivered twice: %q", second)
	}
}

// A FOREGROUND plan queues nothing: it already returned its result as the tool
// output, and telling the model again would be reporting the same work twice.
func TestAForegroundPlanQueuesNoCompletion(t *testing.T) {
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(tea.Msg) {}, 1, nil, "")
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge.PlanRunning(cancel)

	bridge.PlanCompleted(samplePlan(t), specialist.PlanReport{Status: specialist.PlanCompleted, Succeeded: 2})
	if got := bridge.DrainCompletedPlans(); got != "" {
		t.Fatalf("a foreground plan queued a completion: %q", got)
	}
}

// The background flag CLEARS when the plan ends, so the next foreground plan is
// not silently treated as one that outlives its run.
func TestTheBackgroundFlagClearsWhenThePlanEnds(t *testing.T) {
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(tea.Msg) {}, 1, nil, "")
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge.PlanRunning(cancel)
	bridge.SetBackground(true)
	if !bridge.PlanIsBackground() {
		t.Fatal("SetBackground did not take")
	}
	bridge.PlanCompleted(samplePlan(t), specialist.PlanReport{Status: specialist.PlanCompleted})
	if bridge.PlanIsBackground() {
		t.Fatal("the background flag survived the plan")
	}
}

// THE TERMINAL MESSAGE MUST CARRY THE MARK TOO, and it is the easiest one to
// lose: by the time PlanCompleted builds it, the flag has already been cleared,
// so it has to be captured BEFORE the clear and carried down. Unmarked, the
// panel of a background plan freezes one row from the end — every task done, the
// plan never closing.
func TestTheTerminalMessageOfABackgroundPlanIsMarked(t *testing.T) {
	var got []tea.Msg
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(msg tea.Msg) { got = append(got, msg) }, 4, nil, "")
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge.PlanRunning(cancel)
	bridge.SetBackground(true)

	bridge.PlanCompleted(samplePlan(t), specialist.PlanReport{Status: specialist.PlanCompleted, Succeeded: 1})

	var done planCompletedMsg
	found := false
	for _, msg := range got {
		if typed, ok := msg.(planCompletedMsg); ok {
			done, found = typed, true
		}
	}
	if !found {
		t.Fatal("no planCompletedMsg was posted")
	}
	if !done.background {
		t.Fatal("the terminal message is not marked background; a later run's guard would drop it and the panel would never close")
	}

	// ...and it really survives the guard, which is the behaviour that matters.
	m := model{now: func() time.Time { return time.Unix(1000, 0) }, activeRunID: 99,
		planProgress: NewPlanProgressBridge()}
	m.orchestrate.admit(planAdmittedMsg{runID: 4, name: "bg", taskCount: 1,
		tasks: []planGraphTask{{id: "a"}}, background: true}, m.now())
	updated, _ := m.Update(done)
	if updated.(model).orchestrate.frozenAt.IsZero() && updated.(model).orchestrate.isEmpty() {
		t.Fatal("the terminal message was dropped by the stale-run guard")
	}
}

// A FOREGROUND plan's terminal message is NOT marked, so the guard still drops a
// finished run's leftovers — which is the whole reason the guard exists.
func TestTheTerminalMessageOfAForegroundPlanIsNotMarked(t *testing.T) {
	var got []tea.Msg
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(msg tea.Msg) { got = append(got, msg) }, 4, nil, "")
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge.PlanRunning(cancel)

	bridge.PlanCompleted(samplePlan(t), specialist.PlanReport{Status: specialist.PlanCompleted, Succeeded: 1})
	for _, msg := range got {
		if typed, ok := msg.(planCompletedMsg); ok && typed.background {
			t.Fatal("a foreground plan's terminal message was marked background")
		}
	}
}

// PER-TASK PROGRESS, which is the whole point: two tasks in flight, and each
// child's tool calls land on ITS OWN card.
//
// The display used to attribute an unrecognised event to "whichever task was
// dispatched last" — sound while one task ran at a time, a lie the moment two
// could. This asserts the identity travels with the event.
func TestEachTasksProgressLandsOnItsOwnCard(t *testing.T) {
	var got []tea.Msg
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(msg tea.Msg) { got = append(got, msg) }, 3, nil, "")

	bridge.TaskDispatched(specialist.Task{ID: "alpha", Prompt: "one"})
	bridge.TaskDispatched(specialist.Task{ID: "beta", Prompt: "two"})

	// Interleaved, which is what concurrency actually produces.
	bridge.TaskProgress("beta", streamjson.Event{Type: streamjson.EventToolCall, Name: "grep"})
	bridge.TaskProgress("alpha", streamjson.Event{Type: streamjson.EventToolCall, Name: "read_file"})
	bridge.TaskProgress("beta", streamjson.Event{Type: streamjson.EventToolCall, Name: "glob"})

	cardOf := map[string]string{}
	for _, msg := range got {
		if start, ok := msg.(planTaskStartMsg); ok {
			cardOf[start.taskID] = start.cardKey
		}
	}
	if cardOf["alpha"] == "" || cardOf["beta"] == "" || cardOf["alpha"] == cardOf["beta"] {
		t.Fatalf("the two tasks must have distinct cards: %v", cardOf)
	}

	byCard := map[string][]string{}
	for _, msg := range got {
		if progress, ok := msg.(planTaskProgressMsg); ok {
			byCard[progress.cardKey] = append(byCard[progress.cardKey], progress.toolName)
		}
	}
	if want := []string{"read_file"}; len(byCard[cardOf["alpha"]]) != 1 || byCard[cardOf["alpha"]][0] != want[0] {
		t.Fatalf("alpha's card got %v, want %v", byCard[cardOf["alpha"]], want)
	}
	if got := byCard[cardOf["beta"]]; len(got) != 2 || got[0] != "grep" || got[1] != "glob" {
		t.Fatalf("beta's card got %v, want [grep glob]", got)
	}
}

// A task the bridge never dispatched has no card, and inventing one would put a
// row on screen for work the panel never admitted.
func TestProgressForAnUnknownTaskIsDropped(t *testing.T) {
	var got []tea.Msg
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(msg tea.Msg) { got = append(got, msg) }, 3, nil, "")
	bridge.TaskProgress("ghost", streamjson.Event{Type: streamjson.EventToolCall, Name: "grep"})
	for _, msg := range got {
		if _, ok := msg.(planTaskProgressMsg); ok {
			t.Fatal("progress for an undispatched task produced a message")
		}
	}
}

// Only TOOL CALLS are forwarded. A message per streamed token would put the
// event loop under a load the card cannot even display.
func TestOnlyToolCallsReachTheCard(t *testing.T) {
	var got []tea.Msg
	bridge := NewPlanProgressBridge()
	bridge.Attach(func(msg tea.Msg) { got = append(got, msg) }, 3, nil, "")
	bridge.TaskDispatched(specialist.Task{ID: "a", Prompt: "x"})
	for _, kind := range []string{"assistant", "reasoning", "usage", ""} {
		bridge.TaskProgress("a", streamjson.Event{Type: streamjson.EventType(kind), Name: "noise"})
	}
	for _, msg := range got {
		if _, ok := msg.(planTaskProgressMsg); ok {
			t.Fatal("a non-tool-call event reached the card")
		}
	}
	bridge.TaskProgress("a", streamjson.Event{Type: streamjson.EventToolCall, Name: "grep"})
	found := false
	for _, msg := range got {
		if _, ok := msg.(planTaskProgressMsg); ok {
			found = true
		}
	}
	if !found {
		t.Fatal("a tool call did not reach the card")
	}
}

// The MODEL routes by the card the recorder resolved, with no guessing left.
func TestTheModelRoutesTaskProgressByCard(t *testing.T) {
	m := model{now: func() time.Time { return time.Unix(1000, 0) }, activeRunID: 5}
	m.specialists.start("alpha", "one", "card_a", m.now())
	m.specialists.start("beta", "two", "card_b", m.now())

	updated, _ := m.Update(planTaskProgressMsg{runID: 5, taskID: "beta", cardKey: "card_b",
		toolName: "grep", detail: "pattern"})
	after := updated.(model)

	alpha, _ := after.specialists.getBySessionID("card_a")
	beta, _ := after.specialists.getBySessionID("card_b")
	if beta.toolCount != 1 || beta.currentTool != "grep" {
		t.Fatalf("beta's card = %d calls / %q; the event must land there", beta.toolCount, beta.currentTool)
	}
	if alpha.toolCount != 0 || alpha.currentTool != "" {
		t.Fatalf("alpha's card was touched: %d calls / %q", alpha.toolCount, alpha.currentTool)
	}
}

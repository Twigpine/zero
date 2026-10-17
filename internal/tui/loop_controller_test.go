package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/usercommands"
)

func loopTestModel(t *testing.T, now time.Time) model {
	t.Helper()
	m := newModel(context.Background(), Options{})
	m.now = func() time.Time { return now }
	return m
}

func startFixedLoop(m model, prompt string, interval time.Duration) model {
	m, _ = m.startLoop(loopCommand{action: loopActionStart, mode: loopModeFixed, prompt: prompt, interval: interval})
	return m
}

func TestStartLoopRegistersAndSchedules(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m = startFixedLoop(m, "check the build", 5*time.Minute)

	if len(m.loops) != 1 {
		t.Fatalf("want 1 loop, got %d", len(m.loops))
	}
	l := m.loops[0]
	if l.mode != loopModeFixed || l.interval != 5*time.Minute || l.prompt != "check the build" {
		t.Fatalf("unexpected loop %+v", l)
	}
	// First iteration is due immediately (nextRunAt == now).
	if !l.due(now) {
		t.Errorf("first iteration should be due at creation")
	}
	if !m.loopTicking {
		t.Errorf("starting a loop should start the poll ticker")
	}
}

func TestAdvanceLoopSchedulesNextWake(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m = startFixedLoop(m, "poll", 5*time.Minute)
	id := m.loops[0].id

	m = m.advanceLoop(id, "did some work", nil)
	l := m.loops[0]
	if l.iteration != 1 {
		t.Fatalf("iteration = %d, want 1", l.iteration)
	}
	if !l.nextRunAt.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("nextRunAt = %v, want +5m", l.nextRunAt)
	}
}

func TestSelfPacedUsesModelDelay(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m, _ = m.startLoop(loopCommand{action: loopActionStart, mode: loopModeSelfPaced, prompt: "keep tidying"})
	id := m.loops[0].id
	// The model carries its chosen next wake in the reply's control line.
	m = m.advanceLoop(id, "made progress\nLOOP: CONTINUE 10m", nil)
	if len(m.loops) == 0 {
		t.Fatal("a CONTINUE control line should keep the loop running")
	}
	if !m.loops[0].nextRunAt.Equal(now.Add(10 * time.Minute)) {
		t.Fatalf("self-paced nextRunAt = %v, want +10m", m.loops[0].nextRunAt)
	}
}

func TestLoopDoneStops(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m, _ = m.startLoop(loopCommand{action: loopActionStart, mode: loopModeSelfPaced, prompt: "finish the task"})
	id := m.loops[0].id
	m = m.advanceLoop(id, "all done\nLOOP: DONE", nil)
	if len(m.loops) != 0 {
		t.Fatalf("loop should stop when the task reports done, %d remain", len(m.loops))
	}
}

func TestFixedLoopIgnoresControlLine(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m = startFixedLoop(m, "watch CI", time.Minute)
	id := m.loops[0].id
	// A fixed-interval loop never runs the playbook, so a stray control line in the
	// output is not a completion signal — it keeps its wall-clock cadence.
	m = m.advanceLoop(id, "LOOP: DONE", nil)
	if len(m.loops) != 1 {
		t.Fatal("a fixed-interval loop should ignore a control line and keep running")
	}
	if !m.loops[0].nextRunAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("fixed nextRunAt = %v, want +1m", m.loops[0].nextRunAt)
	}
}

func TestLoopExpiresByAge(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m = startFixedLoop(m, "forgotten", time.Minute)
	id := m.loops[0].id
	m.loops[0].createdAt = now.Add(-loopMaxAge - time.Hour) // created past the age cap
	m = m.advanceLoop(id, "still going", nil)
	if len(m.loops) != 0 {
		t.Fatal("a loop past its age cap should auto-expire")
	}
}

func TestLoopDoomGuardStops(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m = startFixedLoop(m, "spin", time.Minute)
	id := m.loops[0].id
	for i := 0; i < loopDoomThreshold+1 && len(m.loops) > 0; i++ {
		m = m.advanceLoop(id, "identical answer", nil)
	}
	if len(m.loops) != 0 {
		t.Fatalf("doom guard should stop a loop repeating identical results")
	}
}

func TestLoopIterationCapStops(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m = startFixedLoop(m, "bounded", time.Minute)
	m.loops[0].maxIter = 3
	id := m.loops[0].id
	for i := 0; i < 3 && len(m.loops) > 0; i++ {
		m = m.advanceLoop(id, "answer "+string(rune('a'+i)), nil) // distinct answers so doom doesn't fire
	}
	if len(m.loops) != 0 {
		t.Fatalf("loop should stop at its iteration cap")
	}
}

func TestStopLoopByID(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m = startFixedLoop(m, "a", time.Minute)
	m = startFixedLoop(m, "b", time.Minute)
	id := m.loops[0].id
	m, cmd := m.stopLoop(id)
	if len(m.loops) != 1 || m.findLoop(id) != nil {
		t.Fatalf("stopLoop(%s) should remove exactly that loop", id)
	}
	// Stopping one of several loops must keep the poll ticker alive for the rest:
	// removeLoop kills the ticker, so stopLoop has to re-arm it (jatmn).
	if cmd == nil || !m.loopTicking {
		t.Fatal("stopping one of several loops must leave the remaining loop scheduled (ticker re-armed)")
	}
	// Bare stop with a single active loop stops that sole loop (parseLoopCommand
	// leaves the target empty and stopLoop special-cases the one-loop case).
	soleID := m.loops[0].id
	m, _ = m.stopLoop("")
	if len(m.loops) != 0 || m.findLoop(soleID) != nil {
		t.Fatalf("bare stop should remove the sole active loop")
	}
	m = startFixedLoop(m, "c", time.Minute)
	m = startFixedLoop(m, "d", time.Minute)
	m, _ = m.stopAllLoops()
	if len(m.loops) != 0 {
		t.Fatalf("stopAllLoops should clear all loops")
	}
}

func TestFireDueLoopSkipsWhenBusy(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m = startFixedLoop(m, "x", time.Minute)
	m.pending = true // a turn is in flight
	got, cmd := m.fireDueLoopIfIdle()
	if got.activeLoopID != "" || cmd != nil {
		t.Fatalf("a due loop must not fire while a turn is pending")
	}
}

func TestFireDueLoopSkipsBehindModal(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m = startFixedLoop(m, "poll", time.Minute) // due immediately
	m.picker = &commandPicker{}                // e.g. an open /resume session picker
	got, cmd := m.fireDueLoopIfIdle()
	if got.activeLoopID != "" || cmd != nil {
		t.Fatal("a due loop must not launch a run behind an open picker/modal — it would complete into whatever session the user switches to")
	}
	if got.loops[0].nextRunAt.IsZero() {
		t.Fatal("a loop skipped because a modal is open must stay scheduled, not have its next run cleared")
	}
}

func TestAdaptiveSelfPaceDelayWidensOverTime(t *testing.T) {
	if adaptiveSelfPaceDelay(0) >= adaptiveSelfPaceDelay(5) {
		t.Error("early iterations should check back sooner than later ones")
	}
	if adaptiveSelfPaceDelay(20) != 30*time.Minute {
		t.Errorf("a mature loop should settle to a 30m heartbeat, got %v", adaptiveSelfPaceDelay(20))
	}
}

func TestLoopFooterSummary(t *testing.T) {
	now := time.Date(2026, 7, 5, 15, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	if m.loopFooterSummary() != "" {
		t.Error("no loops -> empty footer summary")
	}
	m = startFixedLoop(m, "a", 5*time.Minute)
	m.loops[0].nextRunAt = now.Add(5 * time.Minute)
	got := m.loopFooterSummary()
	wantWake := "next " + now.Add(5*time.Minute).Format("3:04pm")
	if !strings.HasPrefix(got, "1 loop") || !strings.Contains(got, wantWake) {
		t.Fatalf("footer summary = %q, want it to start with a loop count and contain %q", got, wantWake)
	}
}

func TestLoopCommandThroughSubmit(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m.input.SetValue("/loop 5m tidy the imports")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if len(next.loops) != 1 {
		t.Fatalf("submitting /loop should create a loop, got %d", len(next.loops))
	}
	if next.loops[0].mode != loopModeFixed || next.loops[0].interval != 5*time.Minute {
		t.Fatalf("unexpected loop from submit: %+v", next.loops[0])
	}
	if !next.loopTicking {
		t.Fatalf("submitting /loop should start the poll ticker")
	}

	// /loop stop all through the same path clears it.
	next.input.SetValue("/loop stop all")
	updated2, _ := next.Update(testKey(tea.KeyEnter))
	final := updated2.(model)
	if len(final.loops) != 0 {
		t.Fatalf("/loop stop all should clear loops, got %d", len(final.loops))
	}
}

func TestStaleLoopTickIgnored(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m = startFixedLoop(m, "x", time.Minute)
	m.loopSeq = 7
	// A tick from before the seq changed must be dropped and stop the ticker.
	updated, cmd := m.Update(loopTickMsg{seq: 3})
	next := updated.(model)
	if next.loopTicking {
		t.Error("a stale loop tick should stop the ticker")
	}
	if cmd != nil {
		t.Error("a stale loop tick should not reschedule")
	}
}

func TestLoopStopsAfterConsecutiveFailures(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m = startFixedLoop(m, "flaky", time.Minute)
	id := m.loops[0].id
	boom := errors.New("boom")
	for i := 0; i < loopMaxFailures && len(m.loops) > 0; i++ {
		m = m.advanceLoop(id, "", boom)
	}
	if len(m.loops) != 0 {
		t.Fatalf("loop should stop after %d consecutive failed iterations", loopMaxFailures)
	}
}

func TestLoopFailureCounterResetsOnSuccess(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m = startFixedLoop(m, "recovers", time.Minute)
	id := m.loops[0].id
	m = m.advanceLoop(id, "", errors.New("x"))
	m = m.advanceLoop(id, "", errors.New("x"))
	m = m.advanceLoop(id, "back to healthy", nil) // a success resets the streak
	if len(m.loops) != 1 {
		t.Fatalf("a recovered failure streak should not stop the loop")
	}
	if m.loops[0].failRun != 0 {
		t.Fatalf("a successful iteration should reset failRun, got %d", m.loops[0].failRun)
	}
}

func TestCancelRunClearsActiveLoopAndRearms(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m = startFixedLoop(m, "x", 5*time.Minute)
	m.activeLoopID = m.loops[0].id
	m.loops[0].nextRunAt = time.Time{} // as if the iteration is running
	m.pending = true
	m.activeRunID = 7
	m.runCancel = func() {}

	m.cancelRun()

	if m.activeLoopID != "" {
		t.Fatal("cancel should clear the active-loop tag so the next turn isn't misattributed")
	}
	if m.loops[0].nextRunAt.IsZero() {
		t.Fatal("cancel should re-arm the interrupted loop, not leave it running forever")
	}
	if !m.loops[0].nextRunAt.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("re-armed nextRunAt = %v, want +5m", m.loops[0].nextRunAt)
	}
}

func TestValidateLoopTargetRejectsBuiltin(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	if _, ok := m.validateLoopTarget("/model list"); ok {
		t.Error("looping a built-in command should be rejected")
	}
	if _, ok := m.validateLoopTarget("keep improving docs"); !ok {
		t.Error("a plain prompt should be a valid loop target")
	}
	if _, ok := m.validateLoopTarget(""); ok {
		t.Error("an empty target should be rejected")
	}
	// A custom command that does not exist is rejected up front, so it is never
	// scheduled and then re-sent as literal "/…" text on every tick.
	if _, ok := m.validateLoopTarget("/babysit-prs"); ok {
		t.Error("an unresolved custom command should be rejected")
	}
	// A registered custom command is accepted.
	m.userCommands = []usercommands.Command{{Name: "babysit-prs", Template: "check the PRs"}}
	if _, ok := m.validateLoopTarget("/babysit-prs"); !ok {
		t.Error("an existing custom command should be a valid loop target")
	}
}

func TestSelfPacedCommandLoopRejected(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m.userCommands = []usercommands.Command{{Name: "babysit-prs", Template: "check the PRs"}}
	// A /command with no interval parses as self-paced, but self-paced mode drives
	// a free-form goal via the playbook + LOOP: protocol, which a command's
	// expanded template never carries — so a bare self-paced /command is rejected.
	m, _ = m.startLoop(loopCommand{action: loopActionStart, mode: loopModeSelfPaced, prompt: "/babysit-prs"})
	if len(m.loops) != 0 {
		t.Fatal("a bare self-paced /command loop should be rejected (a command loop needs an interval)")
	}
	// The same command with an interval (fixed) is accepted.
	m, _ = m.startLoop(loopCommand{action: loopActionStart, mode: loopModeFixed, prompt: "/babysit-prs", interval: 5 * time.Minute})
	if len(m.loops) != 1 {
		t.Fatal("a fixed-interval /command loop should be accepted")
	}
}

func TestClearLoopsForSessionSwitch(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m = startFixedLoop(m, "a", time.Minute)
	m = startFixedLoop(m, "b", time.Minute)
	m.activeLoopID = m.loops[0].id
	m, n := m.clearLoopsForSessionSwitch()
	if n != 2 || len(m.loops) != 0 || m.activeLoopID != "" || m.loopTicking {
		t.Fatalf("session switch should clear all loop state; got n=%d loops=%d active=%q ticking=%v",
			n, len(m.loops), m.activeLoopID, m.loopTicking)
	}
}

func TestStartNewSessionStopsLoops(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m = startFixedLoop(m, "x", time.Minute)
	m = m.startNewSession()
	if len(m.loops) != 0 || m.loopTicking {
		t.Fatal("/new should stop loops tied to the previous session")
	}
}

func TestLoopClearRequiresConfirm(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m = startFixedLoop(m, "x", time.Minute)

	// First /clear with a loop active arms the confirm and does not clear yet.
	m.input.SetValue("/clear")
	u1, _ := m.Update(testKey(tea.KeyEnter))
	n1 := u1.(model)
	if n1.loopLeavePrompt != commandClear {
		t.Fatal("first /clear with active loops should arm the confirm, not clear")
	}
	if len(n1.loops) != 1 {
		t.Fatal("the loop should still be active after the warning")
	}

	// A different command disarms the confirm.
	n1.input.SetValue("/loop list")
	u2, _ := n1.Update(testKey(tea.KeyEnter))
	n2 := u2.(model)
	if n2.loopLeavePrompt != commandEmpty {
		t.Fatal("an intervening command should disarm the leave confirm")
	}

	// Re-arm, then a second consecutive /clear confirms (disarms + clears).
	n2.input.SetValue("/clear")
	u3, _ := n2.Update(testKey(tea.KeyEnter))
	n3 := u3.(model)
	n3.input.SetValue("/clear")
	u4, _ := n3.Update(testKey(tea.KeyEnter))
	n4 := u4.(model)
	if n4.loopLeavePrompt != commandEmpty {
		t.Fatal("a second consecutive /clear should confirm and disarm")
	}
	if len(n4.loops) != 1 {
		t.Fatal("/clear keeps loops running (it wipes the screen, not the session)")
	}
	// The confirmed clear must actually wipe the transcript, not silently no-op:
	// only the clear path appends the "Transcript cleared" note, and the pre-clear
	// loop-start row must be gone.
	clearedNote, staleRow := false, false
	for _, r := range n4.transcript {
		if strings.Contains(r.text, "Transcript cleared") {
			clearedNote = true
		}
		if strings.Contains(r.text, "Loop L1 started") {
			staleRow = true
		}
	}
	if !clearedNote || staleRow {
		t.Fatalf("the second /clear must wipe the transcript (clearedNote=%v staleRow=%v)", clearedNote, staleRow)
	}
}

func TestLeaveConfirmDisarmedByQueuedPrompt(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	m := loopTestModel(t, now)
	m = startFixedLoop(m, "x", time.Minute)

	// Arm the /clear confirm.
	m.input.SetValue("/clear")
	u1, _ := m.Update(testKey(tea.KeyEnter))
	n1 := u1.(model)
	if n1.loopLeavePrompt != commandClear {
		t.Fatal("first /clear should arm the confirm")
	}

	// A prompt submitted while a run streams is queued via an early return in
	// handleSubmit; it must still disarm the confirm (the disarm runs before those
	// returns). Otherwise a later /clear would be treated as the second press.
	n1.pending = true
	n1.input.SetValue("a follow-up question")
	u2, _ := n1.Update(testKey(tea.KeyEnter))
	n2 := u2.(model)
	if n2.loopLeavePrompt != commandEmpty {
		t.Fatal("a queued prompt should disarm the leave confirm; the early return must not skip the disarm")
	}
}

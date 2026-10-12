package tui

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/config"
	"github.com/Gitlawb/zero/internal/notify"
	"github.com/Gitlawb/zero/internal/sessions"
)

func newBTWTestModel(t *testing.T) model {
	t.Helper()
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	parent, err := store.Create(sessions.CreateInput{
		SessionID: "main-session",
		Title:     "Main task",
		Cwd:       "/repo",
		ModelID:   "test-model",
		Provider:  "test-provider",
	})
	if err != nil {
		t.Fatalf("create parent session: %v", err)
	}
	for _, payload := range []map[string]any{
		{"role": "user", "content": "implement the main task"},
		{"role": "assistant", "content": "working on it"},
	} {
		if _, err := store.AppendEvent(parent.SessionID, sessions.AppendEventInput{Type: sessions.EventMessage, Payload: payload}); err != nil {
			t.Fatalf("append parent event: %v", err)
		}
	}
	loaded, err := store.Get(parent.SessionID)
	if err != nil || loaded == nil {
		t.Fatalf("load parent session: session=%#v err=%v", loaded, err)
	}
	events, err := store.ReadEvents(parent.SessionID)
	if err != nil {
		t.Fatalf("read parent events: %v", err)
	}
	m := newModel(context.Background(), Options{
		Cwd:          "/repo",
		ModelName:    "test-model",
		ProviderName: "test-provider",
		SessionStore: store,
	})
	m.activeSession = *loaded
	m.sessionEvents = events
	m.transcript = appendTranscriptRowsDedup(initialTranscript(), transcriptRowsFromSessionEvents(events))
	return m
}

func TestBTWCommandParsesInlineQuestion(t *testing.T) {
	got := parseCommand("/btw double-check the approach")
	if got.kind != commandBTW || got.text != "double-check the approach" {
		t.Fatalf("parseCommand(/btw ...) = %#v", got)
	}
}

func TestBTWCreatesIsolatedForkAndReturnsWithoutMerge(t *testing.T) {
	m := newBTWTestModel(t)
	parentID := m.activeSession.SessionID
	parentEventCount := len(m.sessionEvents)

	side, cmd := m.handleBTWCommand("")
	if cmd != nil {
		t.Fatal("bare /btw should not start an agent run")
	}
	if !side.btw.active || side.btw.parent == nil {
		t.Fatal("expected active BTW state with saved parent")
	}
	if side.activeSession.SessionKind != sessions.SessionKindSide || side.activeSession.ParentSessionID != parentID {
		t.Fatalf("side metadata = %#v", side.activeSession)
	}
	if side.activeSession.Tag != btwSessionTag {
		t.Fatalf("side tag = %q, want %q", side.activeSession.Tag, btwSessionTag)
	}
	if len(side.sessionEvents) <= parentEventCount {
		t.Fatalf("side should contain copied context plus fork marker: got %d events", len(side.sessionEvents))
	}
	if !strings.Contains(side.sessionPrompt("question"), btwContextBoundary) {
		t.Fatal("side prompt is missing the inherited-context boundary")
	}

	updated, err := side.appendSessionEvent(sessions.EventMessage, map[string]any{
		"role": "assistant", "content": "side-only answer",
	})
	if err != nil {
		t.Fatalf("append side event: %v", err)
	}
	returned, _ := updated.leaveBTW()
	if returned.activeSession.SessionID != parentID {
		t.Fatalf("returned session = %q, want %q", returned.activeSession.SessionID, parentID)
	}
	if len(returned.sessionEvents) != parentEventCount {
		t.Fatalf("side events merged into parent: got %d events, want %d", len(returned.sessionEvents), parentEventCount)
	}
	parentEvents, err := returned.sessionStore.ReadEvents(parentID)
	if err != nil {
		t.Fatalf("read parent after return: %v", err)
	}
	for _, event := range parentEvents {
		if strings.Contains(string(event.Payload), "side-only answer") {
			t.Fatal("side-only event was persisted into the parent session")
		}
	}
}

func TestBTWCanOpenWhileParentRunContinues(t *testing.T) {
	m := newBTWTestModel(t)
	m.pending = true
	m.runID = 7
	m.activeRunID = 7

	side, _ := m.handleBTWCommand("")
	if side.pending || side.activeRunID != 0 {
		t.Fatalf("side inherited parent run state: pending=%v activeRunID=%d", side.pending, side.activeRunID)
	}
	if side.btw.parent == nil || !side.btw.parent.pending || side.btw.parent.activeRunID != 7 {
		t.Fatalf("parent run was not preserved: %#v", side.btw.parent)
	}

	routed, _, ok := side.routeBTWParentMessage(agentTextMsg{runID: 7, delta: "main progress"})
	if !ok {
		t.Fatal("parent run message was not routed")
	}
	if strings.Contains(routed.streamingTextString(), "main progress") {
		t.Fatal("parent streaming output leaked into the side transcript")
	}
	if routed.btw.parent == nil || !strings.Contains(routed.btw.parent.streamingTextString(), "main progress") {
		t.Fatal("parent streaming output was not retained on the hidden parent")
	}
}

func TestBTWHiddenParentDoesNotLaunchGoalContinuation(t *testing.T) {
	m := newBTWTestModel(t)
	goalSession, _, err := m.sessionStore.CreateGoal(m.activeSession.SessionID, "Stay visible to the user", 0)
	if err != nil {
		t.Fatal(err)
	}
	m.activeSession = goalSession
	m.provider = &fakeProvider{}
	m.pending = true
	m.runID = 7
	m.activeRunID = 7

	side, _ := m.handleBTWCommand("")
	routed, _, ok := side.routeBTWParentMessage(agentResponseMsg{
		runID:     7,
		goalAware: true,
		rows:      []transcriptRow{{kind: rowAssistant, text: "main turn finished", final: true}},
	})
	if !ok || routed.btw.parent == nil {
		t.Fatal("parent completion was not routed while BTW was active")
	}
	parent := routed.btw.parent
	if parent.pending {
		t.Fatal("hidden parent launched an automatic goal continuation")
	}
	if parent.activeSession.Goal == nil || parent.activeSession.Goal.ContinuationCount != 0 {
		t.Fatalf("hidden parent consumed a continuation: %#v", parent.activeSession.Goal)
	}

	returned, cmd := routed.leaveBTW()
	if cmd == nil || !returned.pending {
		t.Fatalf("returning from BTW did not resume the deferred goal: pending=%v cmd=%v", returned.pending, cmd)
	}
	if returned.goalContinuationsSuspended {
		t.Fatal("goal continuations remained suspended after leaving BTW")
	}
	if returned.activeSession.Goal == nil || returned.activeSession.Goal.ContinuationCount != 1 {
		t.Fatalf("returning from BTW reserved an unexpected continuation: %#v", returned.activeSession.Goal)
	}
	again, duplicateCmd := returned.launchGoalContinuationIfReady()
	if duplicateCmd != nil || again.activeSession.Goal.ContinuationCount != 1 {
		t.Fatalf("returning from BTW launched more than one continuation: goal=%#v cmd=%v", again.activeSession.Goal, duplicateCmd)
	}
}

func TestBTWInlineQuestionStartsSideRun(t *testing.T) {
	m := newBTWTestModel(t)
	m.provider = &fakeProvider{}

	side, cmd := m.handleBTWCommand("double-check this assumption")
	if cmd == nil || !side.pending {
		t.Fatalf("inline /btw question did not start a run: pending=%v cmd=%v", side.pending, cmd)
	}
	if side.lastPrompt != "double-check this assumption" {
		t.Fatalf("side last prompt = %q", side.lastPrompt)
	}
	if side.btw.parent == nil || side.btw.parent.pending {
		t.Fatal("idle parent should remain idle while the side run starts")
	}
	parentEvents, err := side.sessionStore.ReadEvents(side.btw.parent.activeSession.SessionID)
	if err != nil {
		t.Fatalf("read parent events: %v", err)
	}
	for _, event := range parentEvents {
		if strings.Contains(string(event.Payload), "double-check this assumption") {
			t.Fatal("inline side question was written into the parent session")
		}
	}
}

func TestBTWRejectsBeforeMainSessionStarts(t *testing.T) {
	m := newModel(context.Background(), Options{SessionStore: sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})})
	got, cmd := m.handleBTWCommand("")
	if cmd != nil {
		t.Fatal("pre-session /btw should not start work")
	}
	if got.btw.active || !transcriptContains(got.transcript, "Start the main session") {
		t.Fatalf("unexpected pre-session /btw result: active=%v transcript=%#v", got.btw.active, got.transcript)
	}
}

func TestBTWBlocksCommandsThatWouldReplaceItsSession(t *testing.T) {
	m := newBTWTestModel(t)
	side, _ := m.handleBTWCommand("")
	updated, _ := side.dispatchCommand(parseCommand("/new"))
	got := updated.(model)
	if !got.btw.active || got.activeSession.SessionKind != sessions.SessionKindSide {
		t.Fatalf("/new replaced the active BTW session: active=%v metadata=%#v", got.btw.active, got.activeSession)
	}
	if !transcriptContains(got.transcript, "unavailable in a BTW conversation") {
		t.Fatalf("missing blocked-command guidance: %#v", got.transcript)
	}
}

func TestBTWBlocksPersistentConfigurationCommands(t *testing.T) {
	for _, input := range []string{
		"/model other",
		"/provider add",
		"/turns 100",
		"/profile fast",
		"/theme dark",
		"/config recaps off",
		"/stt-model",
		"/mcp",
		"/rewind",
		"/compact",
		// Notify mutations reconfigure the parent's notifier (shared pointer)
		// and write the global preference; they must stay blocked in a BTW
		// side conversation (maintainer review, PR #1001).
		"/notify",
		"/notify off",
		"/notify bell always",
	} {
		t.Run(input, func(t *testing.T) {
			m := newBTWTestModel(t)
			side, _ := m.handleBTWCommand("")
			updated, _ := side.dispatchCommand(parseCommand(input))
			got := updated.(model)
			if !got.btw.active || got.activeSession.SessionKind != sessions.SessionKindSide {
				t.Fatalf("%s escaped the active BTW session: active=%v metadata=%#v", input, got.btw.active, got.activeSession)
			}
			if !transcriptContains(got.transcript, "unavailable in a BTW conversation") {
				t.Fatalf("%s missing blocked-command guidance: %#v", input, got.transcript)
			}
		})
	}
}

func TestBTWAllowsReadOnlyConfigurationCommands(t *testing.T) {
	for _, input := range []string{
		"/model list",
		"/provider status",
		"/turns",
		"/profile status",
		"/theme list",
		"/config",
		"/notify list",
	} {
		t.Run(input, func(t *testing.T) {
			m := newBTWTestModel(t)
			side, _ := m.handleBTWCommand("")
			updated, _ := side.dispatchCommand(parseCommand(input))
			got := updated.(model)
			if transcriptContains(got.transcript, "unavailable in a BTW conversation") {
				t.Fatalf("%s was blocked even though it is read-only", input)
			}
		})
	}
}

// Maintainer regression (PR #1001): running notify mutations inside a BTW
// side conversation must not touch the PARENT's live notifier or the stored
// preference. The side surface shallow-copies the parent model and shares its
// notifier pointer, so an unblocked mutation would reconfigure the parent and
// write the global file while only the side's display fields change; after
// returning, the parent could report one policy in /notify list while emitting
// another. Checks both the parent's actual notification output and the stored
// preference, per the review.
func TestBTWNotifyMutationsLeaveParentAndStoredPreferenceIntact(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"notify":{"mode":"bell","focusMode":"always"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := newBTWTestModel(t)
	parent.userConfigPath = cfgPath
	parent.notifyMode = "bell"
	parent.notifyFocusMode = "always"
	var buf bytes.Buffer
	parent.notifier = notify.New(&buf, notify.Config{Mode: notify.ModeBell, FocusMode: notify.FocusAlways})
	parent.notifier.SetFocused(true)

	// Enter the side conversation and attempt a mutation.
	side, _ := parent.handleBTWCommand("")
	updated, _ := side.dispatchCommand(parseCommand("/notify off"))
	got := updated.(model)
	if got.picker != nil {
		t.Fatal("/notify inside BTW must not open the picker")
	}
	if !transcriptContains(got.transcript, "unavailable in a BTW conversation") {
		t.Fatal("expected the BTW blocked-command guidance")
	}

	// Return to the parent: the live policy still bells (actual output), and
	// the stored preference is unchanged.
	returned, _ := got.handleBTWCommand("")
	parent = returned
	parent.notifier.Notify(notify.Completion, "x")
	if buf.String() != "\x07" {
		t.Fatalf("parent notifier was reconfigured by the side conversation: got %q, want the original bell", buf.String())
	}
	stored, err := config.UserNotify(cfgPath)
	if err != nil {
		t.Fatalf("read stored notify: %v", err)
	}
	if stored.Mode != "bell" || stored.FocusMode != "always" {
		t.Fatalf("stored preference changed from inside BTW: %+v", stored)
	}
	if parent.notifyMode != "bell" || parent.notifyFocusMode != "always" {
		t.Fatalf("parent live policy changed from inside BTW: %q/%q", parent.notifyMode, parent.notifyFocusMode)
	}
}

func TestBTWExitBlockedWhileParentRunActive(t *testing.T) {
	m := newBTWTestModel(t)
	m.pending = true
	m.activeRunID = 7
	side, _ := m.handleBTWCommand("")

	updated, cmd := side.dispatchCommand(parseCommand("/exit"))
	got := updated.(model)
	if cmd != nil || got.exiting || !got.btw.active {
		t.Fatalf("/exit escaped BTW while parent was active: cmd=%v exiting=%v active=%v", cmd, got.exiting, got.btw.active)
	}
	if got.btw.parent == nil || !got.btw.parent.pending {
		t.Fatalf("hidden parent run was not preserved: %#v", got.btw.parent)
	}
	if !transcriptContains(got.transcript, "main session is still running") {
		t.Fatalf("missing active-parent exit guidance: %#v", got.transcript)
	}
}

func TestBTWReturnRestartsParentSpinner(t *testing.T) {
	for _, reducedMotion := range []bool{false, true} {
		t.Run(map[bool]string{false: "animated", true: "reduced-motion"}[reducedMotion], func(t *testing.T) {
			m := newBTWTestModel(t)
			m.reducedMotion = reducedMotion
			m.pending = true
			m.spinnerTicking = true
			side, _ := m.handleBTWCommand("")
			side.spinnerTicking = false

			returned, cmd := side.leaveBTW()
			if !returned.pending || !returned.spinnerTicking {
				t.Fatalf("parent spinner was not restarted: pending=%v ticking=%v", returned.pending, returned.spinnerTicking)
			}
			if cmd == nil {
				t.Fatal("returning to an active parent did not schedule a spinner tick")
			}
		})
	}
}

func TestBTWCancelledRunFlushesBeforeReturnAndRunIDsStayUnique(t *testing.T) {
	m := newBTWTestModel(t)
	m.provider = &fakeProvider{}

	side1, _ := m.handleBTWCommand("first side question")
	firstRunID := side1.activeRunID
	side1.cancelRun()
	blocked, _ := side1.leaveBTW()
	if !blocked.btw.active || !transcriptContains(blocked.transcript, "still saving its session events") {
		t.Fatal("BTW returned before the cancelled side run finished flushing")
	}

	flushedModel, _ := side1.updateModel(agentResponseMsg{runID: firstRunID})
	flushed := flushedModel.(model)
	if len(flushed.flushRunIDs) != 0 {
		t.Fatalf("cancelled side run did not drain: %#v", flushed.flushRunIDs)
	}
	parent, _ := flushed.leaveBTW()
	side2, _ := parent.handleBTWCommand("second side question")
	if side2.activeRunID == firstRunID {
		t.Fatalf("BTW run ID was reused across re-entry: %d", firstRunID)
	}

	updated, _ := side2.updateModel(agentResponseMsg{
		runID: firstRunID,
		rows:  []transcriptRow{{kind: rowAssistant, text: "answer to the FIRST question"}},
	})
	got := updated.(model)
	if !got.pending || transcriptContains(got.transcript, "answer to the FIRST question") {
		t.Fatal("a stale side response hijacked the next BTW run")
	}
}

func TestBTWRejectsExplicitResumeOfSideSession(t *testing.T) {
	m := newBTWTestModel(t)
	side, _ := m.handleBTWCommand("")

	if _, err := m.resolveResumeSession(side.activeSession.SessionID); err == nil || !strings.Contains(err.Error(), "not resumable") {
		t.Fatalf("explicit side-session resume error = %v, want non-resumable rejection", err)
	}
}

func TestBTWCannotStartDuringDeferredExit(t *testing.T) {
	m := newBTWTestModel(t)
	m.exiting = true

	got, cmd := m.handleBTWCommand("should not run")
	if cmd != nil || got.btw.active {
		t.Fatalf("BTW started during deferred exit: active=%v cmd=%v", got.btw.active, cmd)
	}
	if !transcriptContains(got.transcript, "cannot start now") {
		t.Fatalf("missing deferred-exit guidance: %#v", got.transcript)
	}
}

func TestBTWStatusAndHelpStayVisible(t *testing.T) {
	m := newBTWTestModel(t)
	side, _ := m.handleBTWCommand("")

	if status := plainRender(t, side.statusLine(100)); !strings.Contains(status, "BTW") {
		t.Fatalf("status line has no persistent BTW indicator: %q", status)
	}
	groups := side.buildKeybindingGroups()
	if got := groups[0].bindings[3].desc; !strings.Contains(got, "return to the main session") {
		t.Fatalf("BTW Ctrl+C help = %q", got)
	}
}

func TestBTWResizeUpdatesHiddenParent(t *testing.T) {
	m := newBTWTestModel(t)
	m.width = 80
	m.height = 24
	side, _ := m.handleBTWCommand("")

	updated, _ := side.updateModel(tea.WindowSizeMsg{Width: 120, Height: 50})
	got := updated.(model)
	if got.btw.parent == nil || got.btw.parent.width != 120 || got.btw.parent.height != 50 {
		t.Fatalf("hidden parent kept stale geometry: %#v", got.btw.parent)
	}
	returned, _ := got.leaveBTW()
	if returned.width != 120 || returned.height != 50 {
		t.Fatalf("restored parent geometry = %dx%d, want 120x50", returned.width, returned.height)
	}
}

func TestBTWUsesIndependentUsageTracker(t *testing.T) {
	m := newBTWTestModel(t)
	parentTracker := m.usageTracker
	side, _ := m.handleBTWCommand("")
	if side.usageTracker == nil || side.usageTracker == parentTracker {
		t.Fatal("BTW conversation shared the main session usage tracker")
	}
	returned, _ := side.leaveBTW()
	if returned.usageTracker != parentTracker {
		t.Fatal("returning from BTW did not restore the main session usage tracker")
	}
}

func TestBTWFailedForkKeepsMainSessionLoops(t *testing.T) {
	badRoot := filepath.Join(t.TempDir(), "sessions")
	if err := os.WriteFile(badRoot, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := newModel(context.Background(), Options{SessionStore: sessions.NewStore(sessions.StoreOptions{RootDir: badRoot})})
	m.activeSession = sessions.Metadata{SessionID: "main-session"}
	m.loops = []*loopState{{id: "loop-1"}}

	got, _ := m.handleBTWCommand("")
	if len(got.loops) != 1 || got.loops[0].id != "loop-1" {
		t.Fatalf("failed BTW fork cleared main-session loops: %#v", got.loops)
	}
	if !transcriptContains(got.transcript, "Could not open BTW conversation") {
		t.Fatalf("missing fork failure notice: %#v", got.transcript)
	}
}

func TestBTWCommandReturnsToParentSession(t *testing.T) {
	m := newBTWTestModel(t)
	parentID := m.activeSession.SessionID
	side, _ := m.handleBTWCommand("")

	updated, cmd := side.dispatchCommand(parseCommand("/btw"))
	returned := updated.(model)
	if cmd != nil {
		t.Fatal("returning from an idle BTW conversation should not start work")
	}
	if returned.btw.active || returned.btw.parent != nil {
		t.Fatalf("BTW state remained active after /btw: %#v", returned.btw)
	}
	if returned.activeSession.SessionID != parentID {
		t.Fatalf("returned session = %q, want parent %q", returned.activeSession.SessionID, parentID)
	}
}

func TestBTWCtrlCDuringRunDoesNotClearDraft(t *testing.T) {
	m := newBTWTestModel(t)
	side, _ := m.handleBTWCommand("")
	side.pending = true
	side.input.SetValue("keep this draft")

	updated, cmd := side.handleCtrlC()
	got := updated.(model)
	if cmd != nil {
		t.Fatal("Ctrl+C should not start a command while a BTW response is running")
	}
	if !got.btw.active {
		t.Fatal("Ctrl+C returned from BTW while its response was still running")
	}
	if got.composerValue() != "keep this draft" {
		t.Fatalf("Ctrl+C cleared the in-flight BTW draft: %q", got.composerValue())
	}
	if !transcriptContains(got.transcript, "BTW response is still running") {
		t.Fatalf("missing in-flight return guidance: %#v", got.transcript)
	}
}

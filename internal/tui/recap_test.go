package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Twigpine/zero/internal/sessions"
	"github.com/Twigpine/zero/internal/zeroruntime"
)

func TestHandleConfigCommandReportsPersistError(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "afile")
	if err := os.WriteFile(parent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := model{userConfigPath: filepath.Join(parent, "config.json")}
	if _, text := m.handleConfigCommand("recaps off"); !strings.Contains(text, "Failed to save") {
		t.Errorf("a failed persist should report failure, got %q", text)
	}
}

func TestMaybeScheduleIdleRecapDefersProviderWork(t *testing.T) {
	provider := &fakeProvider{}
	withProvider := func() model {
		return model{recapsEnabled: true, provider: provider}
	}

	if _, cmd := (model{provider: provider}).maybeScheduleIdleRecap(1, "did X"); cmd != nil {
		t.Error("recaps disabled: want no timer")
	}
	if _, cmd := (model{recapsEnabled: true}).maybeScheduleIdleRecap(1, "did X"); cmd != nil {
		t.Error("no provider: want no timer")
	}
	if _, cmd := withProvider().maybeScheduleIdleRecap(1, " "); cmd != nil {
		t.Error("empty answer: want no timer")
	}

	m, cmd := withProvider().maybeScheduleIdleRecap(7, "Built the website")
	if cmd == nil || m.recapSeq != 1 {
		t.Fatalf("successful turn should arm one idle timer: seq=%d cmd=%v", m.recapSeq, cmd != nil)
	}
	if len(provider.requests) != 0 {
		t.Fatal("scheduling an idle recap must not call the provider")
	}
}

func TestHandleRecapIdleUsesGoalPlanAndRecentConversation(t *testing.T) {
	provider := &fakeProvider{events: []zeroruntime.StreamEvent{
		{Type: zeroruntime.StreamEventText, Content: "The goal is to ship safer edits. Next, run the cross-platform checks."},
		{Type: zeroruntime.StreamEventDone},
	}}
	m := newModel(context.Background(), Options{Provider: provider})
	m.recapsEnabled = true
	m.runID = 4
	m.recapSeq = 9
	m.activeSession.Goal = &sessions.Goal{Objective: "Ship safer edits", Status: sessions.GoalStatusActive}
	m.plan.steps = []planStep{{content: "Run cross-platform checks", status: "in_progress"}}
	m.transcript = []transcriptRow{
		{kind: rowUser, text: "Make edit safety reliable"},
		{kind: rowAssistant, text: "The implementation is ready.", final: true},
	}

	next, cmd := m.handleRecapIdle(recapIdleMsg{runID: 4, seq: 9})
	if cmd == nil || !next.recapRunning || next.recapCancel == nil {
		t.Fatal("an eligible idle session should start cancellable recap generation")
	}
	if len(provider.requests) != 0 {
		t.Fatal("provider work must remain inside the returned command")
	}

	msg, ok := cmd().(recapGeneratedMsg)
	if !ok {
		t.Fatalf("command returned %T, want recapGeneratedMsg", msg)
	}
	if len(provider.requests) != 1 || len(provider.requests[0].Messages) != 2 {
		t.Fatalf("recap request = %#v", provider.requests)
	}
	contextMessage := provider.requests[0].Messages[1].Content
	for _, want := range []string{"Overall goal: Ship safer edits", "Current task: Run cross-platform checks", "User: Make edit safety reliable", "Assistant: The implementation is ready."} {
		if !strings.Contains(contextMessage, want) {
			t.Errorf("recap context missing %q:\n%s", want, contextMessage)
		}
	}

	next, _ = next.handleRecapGenerated(msg)
	if !strings.Contains(next.idleRecap, "cross-platform checks") || next.recapRunning {
		t.Fatalf("generated idle recap not published correctly: recap=%q running=%v", next.idleRecap, next.recapRunning)
	}
}

func TestRecapIdleAndGeneratedMessagesAreDroppedWhenStaleOrBusy(t *testing.T) {
	provider := &fakeProvider{}
	base := newModel(context.Background(), Options{Provider: provider})
	base.recapsEnabled = true
	base.runID = 3
	base.recapSeq = 5
	base.transcript = []transcriptRow{{kind: rowAssistant, text: "done", final: true}}

	cases := []struct {
		name string
		edit func(*model)
		msg  recapIdleMsg
	}{
		{name: "stale activity", msg: recapIdleMsg{runID: 3, seq: 4}},
		{name: "newer run", msg: recapIdleMsg{runID: 2, seq: 5}},
		{name: "pending run", msg: recapIdleMsg{runID: 3, seq: 5}, edit: func(m *model) { m.pending = true }},
		{name: "draft", msg: recapIdleMsg{runID: 3, seq: 5}, edit: func(m *model) { m.input.SetValue("still typing") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base
			if tc.edit != nil {
				tc.edit(&m)
			}
			if _, cmd := m.handleRecapIdle(tc.msg); cmd != nil {
				t.Fatal("ineligible idle message started provider work")
			}
		})
	}

	base.idleRecap = ""
	base, _ = base.handleRecapGenerated(recapGeneratedMsg{runID: 3, seq: 4, recap: "stale"})
	if base.idleRecap != "" {
		t.Fatal("stale generated recap must be dropped")
	}
}

func TestUserActivityCancelsAndClearsIdleRecap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := newModel(context.Background(), Options{})
	m.recapSeq = 2
	m.recapRunning = true
	m.recapCancel = cancel
	m.idleRecap = "Continue with the release checks."

	updated, _ := m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	next := updated.(model)
	if next.recapSeq != 3 || next.recapRunning || next.recapCancel != nil || next.idleRecap != "" {
		t.Fatalf("activity did not clear recap state: seq=%d running=%v cancel=%v recap=%q", next.recapSeq, next.recapRunning, next.recapCancel != nil, next.idleRecap)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("activity did not cancel in-flight recap context")
	}
}

func TestUserActivityRearmsPendingIdleRecap(t *testing.T) {
	provider := &fakeProvider{}
	m := newModel(context.Background(), Options{Provider: provider})
	m.recapsEnabled = true
	m.runID = 7
	m.transcript = []transcriptRow{{kind: rowAssistant, text: "done", final: true}}
	m, initial := m.maybeScheduleIdleRecap(7, "done")
	if initial == nil {
		t.Fatal("precondition: successful turn did not arm recap")
	}

	oldTimerDone := make(chan tea.Msg, 1)
	go func() { oldTimerDone <- initial() }()
	updated, cmd := m.Update(tea.MouseMotionMsg{})
	next := updated.(model)
	if cmd == nil {
		t.Fatal("activity canceled the pending recap without rearming the idle timer")
	}
	if next.recapSeq <= m.recapSeq {
		t.Fatalf("activity did not invalidate the old timer: before=%d after=%d", m.recapSeq, next.recapSeq)
	}
	select {
	case msg := <-oldTimerDone:
		if msg != nil {
			t.Fatalf("canceled timer returned %#v, want nil", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("activity left the old four-minute timer running")
	}
}

func TestIdleRecapRendersAboveComposer(t *testing.T) {
	m := newModel(context.Background(), Options{})
	m.width = 100
	m.height = 30
	m.idleRecap = "Continue by running the release checks."
	got := plainRender(t, m.footerView(100))
	if !strings.Contains(got, "※ Continue by running the release checks.") {
		t.Fatalf("footer missing idle recap:\n%s", got)
	}
}

func TestHandleConfigCommandTogglesRecaps(t *testing.T) {
	m := model{recapsEnabled: true, idleRecap: "old recap"}
	m, text := m.handleConfigCommand("recaps off")
	if m.recapsEnabled || m.idleRecap != "" || !strings.Contains(text, "recaps: off") {
		t.Errorf("recaps off: enabled=%v recap=%q text=%q", m.recapsEnabled, m.idleRecap, text)
	}
	m, text = m.handleConfigCommand("recaps on")
	if !m.recapsEnabled || !strings.Contains(text, "recaps: on") {
		t.Errorf("recaps on: enabled=%v text=%q", m.recapsEnabled, text)
	}
	if _, text := m.handleConfigCommand("bogus"); !strings.Contains(text, "Unknown setting") {
		t.Errorf("unknown arg should be rejected, got %q", text)
	}
}

func TestHandleConfigCommandCancelsInFlightRecap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := model{
		recapsEnabled: true,
		recapRunning:  true,
		recapCancel:   cancel,
		idleRecap:     "old recap",
	}

	m, _ = m.handleConfigCommand("recaps off")
	if m.recapRunning || m.recapCancel != nil || m.idleRecap != "" {
		t.Fatalf(
			"disabling recaps did not clear in-flight state: running=%v cancel=%v recap=%q",
			m.recapRunning,
			m.recapCancel != nil,
			m.idleRecap,
		)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("disabling recaps did not cancel in-flight generation context")
	}
}

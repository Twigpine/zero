package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/tools"
)

type fakeExecSessionTool struct {
	sessions []tools.ExecSessionSnapshot
	stopped  []int
	stopAll  bool
	writes   [][]byte

	resizeCalls int
	resizeCols  int
	resizeRows  int
}

func (tool *fakeExecSessionTool) Name() string { return tools.ExecCommandToolName }

func (tool *fakeExecSessionTool) Description() string { return "fake exec sessions" }

func (tool *fakeExecSessionTool) Parameters() tools.Schema {
	return tools.Schema{Type: "object", AdditionalProperties: false}
}

func (tool *fakeExecSessionTool) Safety() tools.Safety {
	return tools.Safety{SideEffect: tools.SideEffectShell, Permission: tools.PermissionPrompt}
}

func (tool *fakeExecSessionTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{Status: tools.StatusOK}
}

func (tool *fakeExecSessionTool) ExecSessions() []tools.ExecSessionSnapshot {
	return append([]tools.ExecSessionSnapshot(nil), tool.sessions...)
}

func (tool *fakeExecSessionTool) ExecSession(id int) (tools.ExecSessionSnapshot, bool) {
	for _, session := range tool.sessions {
		if session.ID == id {
			return session, true
		}
	}
	return tools.ExecSessionSnapshot{}, false
}

func (tool *fakeExecSessionTool) StopExecSession(id int) bool {
	for _, session := range tool.sessions {
		if session.ID == id {
			tool.stopped = append(tool.stopped, id)
			return true
		}
	}
	return false
}

func (tool *fakeExecSessionTool) StopAllExecSessions() []int {
	tool.stopAll = true
	ids := make([]int, 0, len(tool.sessions))
	for _, session := range tool.sessions {
		ids = append(ids, session.ID)
	}
	return ids
}

func (tool *fakeExecSessionTool) WriteExecSessionInput(id int, data []byte) error {
	for _, session := range tool.sessions {
		if session.ID != id {
			continue
		}
		if !session.TTY {
			return execution.ErrProcessStdinDisabled
		}
		tool.writes = append(tool.writes, append([]byte(nil), data...))
		return nil
	}
	return execution.ErrProcessNotFound
}

func (tool *fakeExecSessionTool) ResizeExecSession(id int, cols, rows int) error {
	for _, session := range tool.sessions {
		if session.ID != id {
			continue
		}
		if !session.TTY {
			return execution.ErrProcessStdinDisabled
		}
		tool.resizeCalls++
		tool.resizeCols, tool.resizeRows = cols, rows
		return nil
	}
	return execution.ErrProcessNotFound
}

func modelWithFakeExecSessions(tool *fakeExecSessionTool, now time.Time) model {
	registry := tools.NewRegistry()
	registry.Register(tool)
	return model{
		registry: registry,
		now:      func() time.Time { return now },
	}
}

func TestBackgroundTerminalsTextEmpty(t *testing.T) {
	m := modelWithFakeExecSessions(&fakeExecSessionTool{}, time.Unix(100, 0))

	text := m.backgroundTerminalsText()
	for _, want := range []string{"Background Terminals", "0 running", "No background terminals running."} {
		if !strings.Contains(text, want) {
			t.Fatalf("backgroundTerminalsText missing %q:\n%s", want, text)
		}
	}
}

func TestBackgroundTerminalsTextListsSessions(t *testing.T) {
	now := time.Unix(200, 0)
	m := modelWithFakeExecSessions(&fakeExecSessionTool{
		sessions: []tools.ExecSessionSnapshot{{
			ID:           1000,
			Command:      "python3 -m http.server 8000",
			RelativeCwd:  ".",
			StartedAt:    now.Add(-2 * time.Minute),
			Status:       "running",
			RecentOutput: "Serving HTTP on 0.0.0.0 port 8000",
		}},
	}, now)

	text := m.backgroundTerminalsText()
	for _, want := range []string{"Background Terminals", "1000", "running", "2m", "python3 -m http.server 8000", "/stop <session_id>"} {
		if !strings.Contains(text, want) {
			t.Fatalf("backgroundTerminalsText missing %q:\n%s", want, text)
		}
	}
	if summary := m.backgroundTerminalSummary(); summary != "1 background terminal running · /ps to view · /stop to close" {
		t.Fatalf("summary = %q", summary)
	}
}

func TestStopBackgroundTerminalsTextStopsAllAndOne(t *testing.T) {
	tool := &fakeExecSessionTool{
		sessions: []tools.ExecSessionSnapshot{
			{ID: 1000, StartedAt: time.Unix(100, 0), Status: "running"},
			{ID: 1001, StartedAt: time.Unix(100, 0), Status: "running"},
		},
	}
	m := modelWithFakeExecSessions(tool, time.Unix(200, 0))

	all := m.stopBackgroundTerminalsText("")
	if !tool.stopAll || !strings.Contains(all, "stopping 1000, 1001") {
		t.Fatalf("stop all did not render expected result: stopAll=%v text=%s", tool.stopAll, all)
	}

	one := m.stopBackgroundTerminalsText("1001")
	if len(tool.stopped) != 1 || tool.stopped[0] != 1001 || !strings.Contains(one, "stopping 1001") {
		t.Fatalf("stop one did not render expected result: stopped=%#v text=%s", tool.stopped, one)
	}
}

func TestStopBackgroundTerminalsTextRejectsInvalidSessionID(t *testing.T) {
	m := modelWithFakeExecSessions(&fakeExecSessionTool{}, time.Unix(100, 0))

	text := m.stopBackgroundTerminalsText("abc")
	if !strings.Contains(text, "Usage: /stop [session_id]") {
		t.Fatalf("expected usage, got:\n%s", text)
	}
}

func TestBackgroundTerminalsTextAdvertisesAttachForTTY(t *testing.T) {
	now := time.Unix(200, 0)
	m := modelWithFakeExecSessions(&fakeExecSessionTool{
		sessions: []tools.ExecSessionSnapshot{{
			ID: 1000, Command: "sudo apt install x", StartedAt: now, Status: "running", TTY: true,
		}},
	}, now)

	text := m.backgroundTerminalsText()
	if !strings.Contains(text, "/attach <session_id>") {
		t.Fatalf("/ps card should offer /attach for a tty session:\n%s", text)
	}
	if summary := m.backgroundTerminalSummary(); !strings.HasSuffix(summary, " · /attach to type into it") {
		t.Fatalf("summary = %q, want /attach hint", summary)
	}
}

func TestAttachCommandNeedsController(t *testing.T) {
	m := model{now: func() time.Time { return time.Unix(100, 0) }}
	next, _ := m.attachTerminalCommand("7")
	if !transcriptContains(next.transcript, "exec_command is not registered.") {
		t.Fatalf("transcript = %#v", next.transcript)
	}
}

func TestAttachCommandExplicitID(t *testing.T) {
	now := time.Unix(200, 0)
	tool := &fakeExecSessionTool{
		sessions: []tools.ExecSessionSnapshot{
			{ID: 7, TTY: true, Status: "running", Command: "cat", StartedAt: now},
			{ID: 8, TTY: false, Status: "running", Command: "sleep 30", StartedAt: now},
		},
	}
	m := modelWithFakeExecSessions(tool, now)

	next, cmd := m.attachTerminalCommand("7")
	if cmd == nil || next.terminalAttach == nil || next.terminalAttach.sessionID != 7 {
		t.Fatalf("expected attach to session 7, state=%#v cmd=%v", next.terminalAttach, cmd)
	}

	next, _ = m.attachTerminalCommand("8")
	if !transcriptContains(next.transcript, "Session 8 has no terminal") {
		t.Fatalf("non-tty session should be refused: %#v", next.transcript)
	}
	next, _ = m.attachTerminalCommand("9")
	if !transcriptContains(next.transcript, "No running terminal session 9.") {
		t.Fatalf("missing session should be reported: %#v", next.transcript)
	}
	next, _ = m.attachTerminalCommand("abc")
	if !transcriptContains(next.transcript, "Usage: /attach [session_id]") {
		t.Fatalf("invalid id should show usage: %#v", next.transcript)
	}
}

func TestAttachCommandBarePicksSingleTTY(t *testing.T) {
	now := time.Unix(200, 0)
	tool := &fakeExecSessionTool{
		sessions: []tools.ExecSessionSnapshot{
			{ID: 7, TTY: true, Status: "running", Command: "cat", StartedAt: now},
			{ID: 8, TTY: false, Status: "running", Command: "sleep 30", StartedAt: now},
		},
	}
	m := modelWithFakeExecSessions(tool, now)

	next, _ := m.attachTerminalCommand("")
	if next.terminalAttach == nil || next.terminalAttach.sessionID != 7 {
		t.Fatalf("bare /attach should pick the only tty session, state=%#v", next.terminalAttach)
	}
}

func TestAttachCommandBareNoneOrSeveral(t *testing.T) {
	now := time.Unix(200, 0)
	m := modelWithFakeExecSessions(&fakeExecSessionTool{
		sessions: []tools.ExecSessionSnapshot{
			{ID: 8, TTY: false, Status: "running", Command: "sleep 30", StartedAt: now},
		},
	}, now)
	next, _ := m.attachTerminalCommand("")
	if next.terminalAttach != nil || !strings.Contains(next.transientNotice.text, "No interactive terminal sessions running.") {
		t.Fatalf("bare /attach with no tty sessions: notice=%q state=%#v", next.transientNotice.text, next.terminalAttach)
	}

	m = modelWithFakeExecSessions(&fakeExecSessionTool{
		sessions: []tools.ExecSessionSnapshot{
			{ID: 7, TTY: true, Status: "running", Command: "cat", StartedAt: now},
			{ID: 9, TTY: true, Status: "running", Command: "top", StartedAt: now},
		},
	}, now)
	next, _ = m.attachTerminalCommand("")
	if next.terminalAttach != nil {
		t.Fatal("bare /attach with several tty sessions should list, not attach")
	}
	if !transcriptContains(next.transcript, "/attach <session_id>") || !transcriptContains(next.transcript, "cat") || !transcriptContains(next.transcript, "top") {
		t.Fatalf("expected a choice card listing both sessions: %#v", next.transcript)
	}
}

func TestQuitStopsBackgroundTerminals(t *testing.T) {
	tool := &fakeExecSessionTool{
		sessions: []tools.ExecSessionSnapshot{{ID: 1000, StartedAt: time.Unix(100, 0), Status: "running"}},
	}
	m := modelWithFakeExecSessions(tool, time.Unix(200, 0))

	_, _ = m.quit()
	if !tool.stopAll {
		t.Fatal("quit should stop all background terminal sessions")
	}
}

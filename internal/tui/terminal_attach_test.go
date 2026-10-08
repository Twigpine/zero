package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/tools"
)

func attachedModel(t *testing.T) (model, *fakeExecSessionTool) {
	t.Helper()
	tool := &fakeExecSessionTool{
		sessions: []tools.ExecSessionSnapshot{{
			ID:           7,
			TTY:          true,
			Status:       "running",
			Command:      "sudo apt install x",
			RecentOutput: "[sudo] password for me: ",
			StartedAt:    time.Unix(100, 0),
		}},
	}
	m := modelWithFakeExecSessions(tool, time.Unix(200, 0))
	m.width = 100
	m.height = 30
	return m, tool
}

func TestOpenTerminalAttachSetsState(t *testing.T) {
	m, _ := attachedModel(t)
	next, cmd := m.openTerminalAttach(7)
	if cmd == nil {
		t.Fatal("openTerminalAttach should schedule a refresh tick")
	}
	state := next.terminalAttach
	if state == nil || state.sessionID != 7 {
		t.Fatalf("state = %#v", state)
	}
	if state.command != "sudo apt install x" || state.output != "[sudo] password for me: " {
		t.Fatalf("state did not pick up the snapshot: %#v", state)
	}
	if !next.terminalAttachSeen[7] {
		t.Fatal("openTerminalAttach should mark the session as seen")
	}
}

func TestTerminalAttachForwardsKeys(t *testing.T) {
	m, tool := attachedModel(t)
	next, _ := m.openTerminalAttach(7)

	for _, msg := range []tea.Msg{testKeyText("s"), testKey(tea.KeyEnter)} {
		updated, _ := next.Update(msg)
		next = updated.(model)
	}
	if next.terminalAttach == nil {
		t.Fatal("typing should stay attached")
	}
	if len(tool.writes) != 2 || string(tool.writes[0]) != "s" || string(tool.writes[1]) != "\r" {
		t.Fatalf("writes = %#v, want [s \\r]", tool.writes)
	}
}

func TestTerminalAttachForwardsCtrlC(t *testing.T) {
	m, tool := attachedModel(t)
	next, _ := m.openTerminalAttach(7)

	updated, _ := next.Update(testKeyCtrl('c'))
	next = updated.(model)
	if len(tool.writes) != 1 || len(tool.writes[0]) != 1 || tool.writes[0][0] != 0x03 {
		t.Fatalf("writes = %#v, want [0x03]", tool.writes)
	}
	if next.terminalAttach == nil {
		t.Fatal("Ctrl+C detached the session — it should go to the process")
	}
	if next.exitConfirmActive {
		t.Fatal("Ctrl+C armed the exit confirmation while attached")
	}
}

func TestTerminalAttachForwardsPaste(t *testing.T) {
	m, tool := attachedModel(t)
	next, _ := m.openTerminalAttach(7)

	updated, _ := next.Update(testPaste("pw"))
	next = updated.(model)
	if next.terminalAttach == nil {
		t.Fatal("paste should stay attached")
	}
	if len(tool.writes) != 1 || string(tool.writes[0]) != "pw" {
		t.Fatalf("writes = %#v, want [pw]", tool.writes)
	}
}

func TestTerminalAttachPasteKeepsBracketedFraming(t *testing.T) {
	m, tool := attachedModel(t)
	tool.sessions[0].RecentOutput = "\x1b[?2004h[sudo] password for me: "
	next, _ := m.openTerminalAttach(7)

	updated, _ := next.Update(testPaste("pw"))
	next = updated.(model)
	want := "\x1b[200~pw\x1b[201~"
	if len(tool.writes) != 1 || string(tool.writes[0]) != want {
		t.Fatalf("writes = %q, want [%q]", tool.writes, want)
	}

	// A disable sequence in later output turns framing back off once the
	// refresh tick observes it.
	tool.sessions[0].RecentOutput += "\x1b[?2004l"
	updated, _ = next.Update(terminalAttachTickMsg{})
	next = updated.(model)
	_, _ = next.Update(testPaste("pw3"))
	if last := string(tool.writes[len(tool.writes)-1]); last != "pw3" {
		t.Fatalf("write after disable = %q, want raw pw3", last)
	}
}

func TestBracketedPasteMode(t *testing.T) {
	cases := []struct {
		name    string
		output  string
		current bool
		want    bool
	}{
		{name: "enable last", output: "\x1b[?2004h", want: true},
		{name: "disable last", output: "\x1b[?2004l", current: true, want: false},
		{name: "neither keeps current", output: "plain output", current: true, want: true},
		{name: "neither stays off", output: "plain output", want: false},
		{name: "enable after disable", output: "\x1b[?2004ltext\x1b[?2004h", want: true},
		{name: "disable after enable", output: "\x1b[?2004htext\x1b[?2004l", current: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bracketedPasteMode(tc.output, tc.current); got != tc.want {
				t.Fatalf("bracketedPasteMode(%q, %v) = %v, want %v", tc.output, tc.current, got, tc.want)
			}
		})
	}
}

func TestTerminalAttachEscDetaches(t *testing.T) {
	m, tool := attachedModel(t)
	next, _ := m.openTerminalAttach(7)

	updated, _ := next.Update(testKey(tea.KeyEsc))
	next = updated.(model)
	if next.terminalAttach != nil {
		t.Fatal("Esc should detach")
	}
	if len(tool.writes) != 0 {
		t.Fatalf("Esc forwarded bytes to the process: %#v", tool.writes)
	}
	if !strings.Contains(next.transientNotice.text, "Detached from session 7") {
		t.Fatalf("notice = %q", next.transientNotice.text)
	}
}

func TestTerminalAttachTickAutoClosesOnExit(t *testing.T) {
	tool := &fakeExecSessionTool{
		sessions: []tools.ExecSessionSnapshot{{
			ID: 7, TTY: true, Status: "running", Command: "sudo apt install x",
			StartedAt: time.Unix(100, 0),
		}},
	}
	m := modelWithFakeExecSessions(tool, time.Unix(200, 0))
	next, _ := m.openTerminalAttach(7)

	exitCode := 1
	tool.sessions[0].Status = "exited"
	tool.sessions[0].ExitCode = &exitCode
	updated, _ := next.Update(terminalAttachTickMsg{})
	next = updated.(model)
	if next.terminalAttach != nil {
		t.Fatal("overlay should close itself when the session exits")
	}
	if !strings.Contains(next.transientNotice.text, "Terminal session 7 finished (exit 1).") {
		t.Fatalf("notice = %q", next.transientNotice.text)
	}
}

func TestTerminalAttachTickAutoClosesOnMissing(t *testing.T) {
	tool := &fakeExecSessionTool{
		sessions: []tools.ExecSessionSnapshot{{
			ID: 7, TTY: true, Status: "running", Command: "cat", StartedAt: time.Unix(100, 0),
		}},
	}
	m := modelWithFakeExecSessions(tool, time.Unix(200, 0))
	next, _ := m.openTerminalAttach(7)

	tool.sessions = nil
	updated, _ := next.Update(terminalAttachTickMsg{})
	next = updated.(model)
	if next.terminalAttach != nil {
		t.Fatal("overlay should close itself when the session disappears")
	}
	if !strings.Contains(next.transientNotice.text, "Terminal session 7 ended.") {
		t.Fatalf("notice = %q", next.transientNotice.text)
	}
}

func TestTerminalAttachOverlay(t *testing.T) {
	m, _ := attachedModel(t)
	next, _ := m.openTerminalAttach(7)

	overlay := next.terminalAttachOverlay(80)
	for _, want := range []string{"Terminal", "sudo apt install x", "[running]", "[sudo] password for me:", "Esc detach", "/stop 7"} {
		if !strings.Contains(overlay, want) {
			t.Fatalf("overlay missing %q:\n%s", want, overlay)
		}
	}
}

func TestTerminalAttachOverlayFillsViewport(t *testing.T) {
	m, _ := attachedModel(t)
	m.width, m.height = 120, 40
	next, _ := m.openTerminalAttach(7)

	overlay := next.terminalAttachOverlay(120)
	lines := strings.Split(overlay, "\n")
	frame := next.scrollableTranscriptFrame(next.pinnedTitleBar(120), next.footerView(120))
	if len(lines) != frame.bodyRect.height {
		t.Fatalf("overlay lines = %d, want viewport body height %d", len(lines), frame.bodyRect.height)
	}
	for index, line := range lines {
		if width := lipgloss.Width(line); width != 120 {
			t.Fatalf("overlay line %d width = %d, want 120: %q", index, width, line)
		}
	}
	plain := ansi.Strip(overlay)
	if !strings.Contains(plain, "Terminal") || !strings.Contains(plain, "[running]") {
		t.Fatalf("overlay missing border title: %q", lines[0])
	}
	if !strings.HasPrefix(plain, "╭") || !strings.Contains(lines[len(lines)-1], "╰") {
		t.Fatal("overlay should be a bordered box")
	}
}

func TestFooterViewWhileAttachedDropsComposer(t *testing.T) {
	m, _ := attachedModel(t)
	next, _ := m.openTerminalAttach(7)

	footer := ansi.Strip(next.footerView(100))
	if strings.Contains(footer, "describe a task") {
		t.Fatalf("attached footer still shows the composer: %q", footer)
	}
	if strings.TrimSpace(footer) == "" {
		t.Fatal("attached footer should still render the status line")
	}
	m.input.SetValue("typed")
	if plain := ansi.Strip(m.footerView(100)); !strings.Contains(plain, "typed") {
		t.Fatalf("detached footer lost the composer: %q", plain)
	}
}

func TestTerminalAttachResizesPTYOnOpenAndResize(t *testing.T) {
	m, tool := attachedModel(t)

	// A resize while nothing is attached must not reach the controller.
	if updated, _ := m.Update(tea.WindowSizeMsg{Width: 110, Height: 35}); updated != nil {
		m = updated.(model)
	}
	if tool.resizeCalls != 0 {
		t.Fatalf("WindowSizeMsg while detached resized: %d calls", tool.resizeCalls)
	}

	next, _ := m.openTerminalAttach(7)
	wantCols := chatWidth(m.width) - 5
	wantRows := next.terminalAttachViewportRows(chatWidth(m.width))
	if tool.resizeCalls != 1 || tool.resizeCols != wantCols || tool.resizeRows != wantRows {
		t.Fatalf("open resize = %d calls %dx%d, want 1 call %dx%d", tool.resizeCalls, tool.resizeCols, tool.resizeRows, wantCols, wantRows)
	}

	updated, _ := next.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	next = updated.(model)
	wantCols, wantRows = 115, next.terminalAttachViewportRows(120)
	if tool.resizeCalls != 2 || tool.resizeCols != wantCols || tool.resizeRows != wantRows {
		t.Fatalf("resize = %d calls %dx%d, want 2 calls %dx%d", tool.resizeCalls, tool.resizeCols, tool.resizeRows, wantCols, wantRows)
	}

	// A no-change resize is not forwarded again.
	_, _ = next.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if tool.resizeCalls != 2 {
		t.Fatalf("unchanged size resized again: %d calls", tool.resizeCalls)
	}
}

func TestExecCallWantsTTY(t *testing.T) {
	cases := []struct {
		name string
		args string
		want bool
	}{
		{"tty true", `{"cmd":"x","tty":true}`, true},
		{"tty false", `{"cmd":"x","tty":false}`, false},
		{"tty absent", `{"cmd":"x"}`, false},
		{"garbage", `not json`, false},
		{"empty", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := execCallWantsTTY(tc.args); got != tc.want {
				t.Fatalf("execCallWantsTTY(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestInteractiveExecStartAutoAttaches(t *testing.T) {
	tool := &fakeExecSessionTool{}
	m := modelWithFakeExecSessions(tool, time.Unix(200, 0))
	m.activeRunID = 3
	m.pending = true

	updated, cmd := m.Update(interactiveExecStartMsg{runID: 3})
	next := updated.(model)
	if next.terminalAutoAttach == nil || cmd == nil {
		t.Fatal("interactiveExecStartMsg should arm the watcher and a tick")
	}

	tool.sessions = append(tool.sessions, tools.ExecSessionSnapshot{
		ID: 7, TTY: true, Status: "running", Command: "cat", StartedAt: time.Unix(100, 0),
	})
	updated, _ = next.Update(terminalAutoAttachTickMsg{runID: 3})
	next = updated.(model)
	if next.terminalAttach == nil || next.terminalAttach.sessionID != 7 {
		t.Fatalf("auto-attach did not open on the new session: %#v", next.terminalAttach)
	}
	if !next.terminalAttachSeen[7] {
		t.Fatal("session 7 should be marked seen")
	}
	if next.terminalAutoAttach != nil {
		t.Fatal("watcher should stop once it attached")
	}
}

func TestTerminalAutoAttachSkipsKnownSession(t *testing.T) {
	tool := &fakeExecSessionTool{
		sessions: []tools.ExecSessionSnapshot{
			{ID: 5, TTY: true, Status: "running", Command: "top", StartedAt: time.Unix(100, 0)},
		},
	}
	m := modelWithFakeExecSessions(tool, time.Unix(200, 0))
	m.activeRunID = 3
	m.pending = true

	// Session 5 predates the tool call — it is not this call's session.
	updated, _ := m.Update(interactiveExecStartMsg{runID: 3, known: map[int]bool{5: true}})
	next := updated.(model)
	updated, cmd := next.Update(terminalAutoAttachTickMsg{runID: 3})
	next = updated.(model)
	if next.terminalAttach != nil {
		t.Fatal("watcher must not attach to a session that was already running")
	}
	if cmd == nil || next.terminalAutoAttach == nil {
		t.Fatal("watcher should keep ticking while nothing new appears")
	}

	tool.sessions = append(tool.sessions, tools.ExecSessionSnapshot{
		ID: 6, TTY: true, Status: "running", Command: "cat", StartedAt: time.Unix(200, 0),
	})
	updated, _ = next.Update(terminalAutoAttachTickMsg{runID: 3})
	next = updated.(model)
	if next.terminalAttach == nil || next.terminalAttach.sessionID != 6 {
		t.Fatalf("watcher should attach to the new session 6: %#v", next.terminalAttach)
	}
}

func TestInteractiveExecStartBaselinePredatesSessionRegistration(t *testing.T) {
	// The baseline is captured when OnToolCall fires, before the tool's Run
	// registers the session — a snapshot taken at Update time would already
	// contain it and wrongly exclude it as pre-existing.
	tool := &fakeExecSessionTool{
		sessions: []tools.ExecSessionSnapshot{
			{ID: 5, TTY: true, Status: "running", Command: "top", StartedAt: time.Unix(100, 0)},
		},
	}
	m := modelWithFakeExecSessions(tool, time.Unix(200, 0))
	m.activeRunID = 3
	m.pending = true

	msg := interactiveExecStartMsg{runID: 3, known: map[int]bool{5: true}}
	tool.sessions = append(tool.sessions, tools.ExecSessionSnapshot{
		ID: 6, TTY: true, Status: "running", Command: "cat", StartedAt: time.Unix(200, 0),
	})

	updated, _ := m.Update(msg)
	next := updated.(model)
	updated, _ = next.Update(terminalAutoAttachTickMsg{runID: 3})
	next = updated.(model)
	if next.terminalAttach == nil || next.terminalAttach.sessionID != 6 {
		t.Fatalf("watcher should attach to the session registered after the baseline: %#v", next.terminalAttach)
	}
}

func TestTerminalAutoAttachDoesNotReattachSeen(t *testing.T) {
	tool := &fakeExecSessionTool{
		sessions: []tools.ExecSessionSnapshot{
			{ID: 7, TTY: true, Status: "running", Command: "cat", StartedAt: time.Unix(100, 0)},
		},
	}
	m := modelWithFakeExecSessions(tool, time.Unix(200, 0))
	m.activeRunID = 3
	m.pending = true
	m.terminalAttachSeen = map[int]bool{7: true} // user already attached and detached

	updated, _ := m.Update(interactiveExecStartMsg{runID: 3})
	next := updated.(model)
	updated, cmd := next.Update(terminalAutoAttachTickMsg{runID: 3})
	next = updated.(model)
	if next.terminalAttach != nil {
		t.Fatal("watcher must not re-open a session the user detached")
	}
	if cmd == nil {
		t.Fatal("watcher should keep ticking")
	}
}

func TestTerminalAutoAttachTickIgnoresStaleRun(t *testing.T) {
	tool := &fakeExecSessionTool{
		sessions: []tools.ExecSessionSnapshot{
			{ID: 7, TTY: true, Status: "running", Command: "cat", StartedAt: time.Unix(100, 0)},
		},
	}
	m := modelWithFakeExecSessions(tool, time.Unix(200, 0))
	m.activeRunID = 3
	m.pending = true
	m.terminalAutoAttach = &terminalAutoAttachState{
		runID: 3, known: map[int]bool{}, deadline: m.now().Add(terminalAutoAttachTimeout),
	}

	updated, cmd := m.Update(terminalAutoAttachTickMsg{runID: 9})
	next := updated.(model)
	if next.terminalAttach != nil {
		t.Fatal("a stale tick must not attach")
	}
	if next.terminalAutoAttach == nil || cmd != nil {
		t.Fatal("a stale tick should be dropped without touching the watcher")
	}

	updated, _ = next.Update(terminalAutoAttachTickMsg{runID: 3})
	next = updated.(model)
	if next.terminalAttach == nil || next.terminalAttach.sessionID != 7 {
		t.Fatalf("the live tick should still attach: %#v", next.terminalAttach)
	}
}

func TestBTWRoutesAutoAttachTickToHiddenParent(t *testing.T) {
	tool := &fakeExecSessionTool{}
	m := newBTWTestModel(t)
	registry := tools.NewRegistry()
	registry.Register(tool)
	m.registry = registry
	m.pending = true
	m.runID = 7
	m.activeRunID = 7

	side, _ := m.handleBTWCommand("")
	routed, _, ok := side.routeBTWParentMessage(interactiveExecStartMsg{runID: 7})
	if !ok {
		t.Fatal("interactiveExecStartMsg was not routed to the parent")
	}
	side = routed
	if side.btw.parent == nil || side.btw.parent.terminalAutoAttach == nil {
		t.Fatal("watcher was not armed on the hidden parent")
	}

	// A tick tagged for the side run must not reach the parent watcher.
	if routed, _, ok := side.routeBTWParentMessage(terminalAutoAttachTickMsg{runID: side.btw.sideRunIDBase}); ok || routed.btw.parent.terminalAttach != nil {
		t.Fatal("a side-run tick must not reach the parent watcher")
	}

	tool.sessions = append(tool.sessions, tools.ExecSessionSnapshot{
		ID: 7, TTY: true, Status: "running", Command: "cat", StartedAt: time.Unix(100, 0),
	})
	routed, _, ok = side.routeBTWParentMessage(terminalAutoAttachTickMsg{runID: 7})
	if !ok {
		t.Fatal("parent-run tick was not routed to the parent")
	}
	side = routed
	if side.btw.parent == nil || side.btw.parent.terminalAttach == nil || side.btw.parent.terminalAttach.sessionID != 7 {
		t.Fatalf("parent overlay did not open: %#v", side.btw.parent)
	}
	if side.terminalAttach != nil {
		t.Fatal("attach state leaked onto the side model")
	}
}

func TestTerminalAutoAttachDefersToOtherViews(t *testing.T) {
	newWatchingModel := func(t *testing.T) (model, *fakeExecSessionTool) {
		t.Helper()
		tool := &fakeExecSessionTool{
			sessions: []tools.ExecSessionSnapshot{
				{ID: 7, TTY: true, Status: "running", Command: "cat", StartedAt: time.Unix(100, 0)},
			},
		}
		m := modelWithFakeExecSessions(tool, time.Unix(200, 0))
		m.activeRunID = 3
		m.pending = true
		m.terminalAutoAttach = &terminalAutoAttachState{
			runID: 3, known: map[int]bool{}, deadline: m.now().Add(terminalAutoAttachTimeout),
		}
		return m, tool
	}

	m, _ := newWatchingModel(t)
	m.helpOverlay = true
	updated, cmd := m.Update(terminalAutoAttachTickMsg{runID: 3})
	next := updated.(model)
	if next.terminalAttach != nil {
		t.Fatal("overlay must not open over the help overlay")
	}
	if cmd == nil || next.terminalAutoAttach == nil {
		t.Fatal("watcher should keep ticking while help owns the viewport")
	}
	next.helpOverlay = false
	updated, _ = next.Update(terminalAutoAttachTickMsg{runID: 3})
	next = updated.(model)
	if next.terminalAttach == nil {
		t.Fatal("overlay should open once help closes")
	}

	m, _ = newWatchingModel(t)
	m.subchat.active = true
	updated, cmd = m.Update(terminalAutoAttachTickMsg{runID: 3})
	next = updated.(model)
	if next.terminalAttach != nil {
		t.Fatal("overlay must not open inside the subchat view")
	}
	if cmd == nil || next.terminalAutoAttach == nil {
		t.Fatal("watcher should keep ticking while subchat owns the viewport")
	}
}

func TestAttachCommandRefusesWhileSubchatActive(t *testing.T) {
	m, _ := attachedModel(t)
	m.subchat.active = true

	next, _ := m.attachTerminalCommand("7")
	if next.terminalAttach != nil {
		t.Fatal("/attach must not open the overlay inside the subchat view")
	}
	if !strings.Contains(next.transientNotice.text, "Leave the current view first") {
		t.Fatalf("expected the leave-view notice, got %q", next.transientNotice.text)
	}
	next.subchat.active = false
	next, _ = next.attachTerminalCommand("7")
	if next.terminalAttach == nil {
		t.Fatal("/attach should open once the subchat view is left")
	}
}

func TestTerminalAutoAttachIgnoresWrongRun(t *testing.T) {
	m, _ := attachedModel(t)
	m.activeRunID = 3
	m.pending = true

	updated, cmd := m.Update(interactiveExecStartMsg{runID: 9})
	next := updated.(model)
	if next.terminalAutoAttach != nil || cmd != nil {
		t.Fatal("a msg for another run must not arm the watcher")
	}
}

func TestTerminalAutoAttachWaitsForPermissionPrompt(t *testing.T) {
	tool := &fakeExecSessionTool{
		sessions: []tools.ExecSessionSnapshot{
			{ID: 7, TTY: true, Status: "running", Command: "cat", StartedAt: time.Unix(100, 0)},
		},
	}
	m := modelWithFakeExecSessions(tool, time.Unix(200, 0))
	m.activeRunID = 3
	m.pending = true
	m.pendingPermission = &pendingPermissionPrompt{
		request: agent.PermissionRequest{ToolName: "exec_command"},
		decide:  func(agent.PermissionDecision) {},
	}

	// The watcher is armed before the session registers? Session already here —
	// simplest equivalent: arm it directly and confirm the modal blocks opening.
	m.terminalAutoAttach = &terminalAutoAttachState{
		runID: 3, known: map[int]bool{}, deadline: m.now().Add(terminalAutoAttachTimeout),
	}
	updated, cmd := m.Update(terminalAutoAttachTickMsg{runID: 3})
	next := updated.(model)
	if next.terminalAttach != nil {
		t.Fatal("overlay must not open over a permission prompt")
	}
	if cmd == nil {
		t.Fatal("watcher should keep ticking while the modal is up")
	}

	next.pendingPermission = nil
	updated, _ = next.Update(terminalAutoAttachTickMsg{runID: 3})
	next = updated.(model)
	if next.terminalAttach == nil || next.terminalAttach.sessionID != 7 {
		t.Fatalf("overlay should open once the modal clears: %#v", next.terminalAttach)
	}
}

// The user's keystrokes go to the PTY and nowhere else: not the transcript,
// not the composer, not a notice.
func TestTerminalAttachNeverEchoesInput(t *testing.T) {
	m, tool := attachedModel(t)
	next, _ := m.openTerminalAttach(7)

	for _, msg := range []tea.Msg{testKeyText("s"), testKeyText("3"), testKeyText("c"), testKeyText("r"), testKeyText("e"), testKeyText("t"), testPaste("pw")} {
		updated, _ := next.Update(msg)
		next = updated.(model)
	}
	if len(tool.writes) == 0 {
		t.Fatal("nothing was forwarded")
	}
	for _, row := range next.transcript {
		if strings.Contains(row.text, "s3cret") || strings.Contains(row.text, "pw") {
			t.Fatalf("typed bytes leaked into transcript row: %q", row.text)
		}
	}
	if strings.Contains(next.transientNotice.text, "s3cret") || strings.Contains(next.transientNotice.text, "pw") {
		t.Fatalf("typed bytes leaked into the notice: %q", next.transientNotice.text)
	}
	if strings.Contains(next.composerValue(), "s3cret") {
		t.Fatal("typed bytes leaked into the composer")
	}
}

func TestTerminalAttachSwallowsMouse(t *testing.T) {
	m, _ := attachedModel(t)
	next, _ := m.openTerminalAttach(7)
	updated, _ := next.Update(testMouseClick(tea.MouseLeft, 5, 5))
	next = updated.(model)
	if next.terminalAttach == nil {
		t.Fatal("mouse click detached the session")
	}
}

func TestPtyInputBytes(t *testing.T) {
	cases := []struct {
		name string
		msg  tea.KeyMsg
		want string
		ok   bool
	}{
		{"printable", testKeyText("a"), "a", true},
		{"space", testKey(tea.KeySpace), " ", true},
		{"enter", testKey(tea.KeyEnter), "\r", true},
		{"tab", testKey(tea.KeyTab), "\t", true},
		{"backspace", testKey(tea.KeyBackspace), "\x7f", true},
		{"delete", testKey(tea.KeyDelete), "\x1b[3~", true},
		{"up", testKey(tea.KeyUp), "\x1b[A", true},
		{"down", testKey(tea.KeyDown), "\x1b[B", true},
		{"right", testKey(tea.KeyRight), "\x1b[C", true},
		{"left", testKey(tea.KeyLeft), "\x1b[D", true},
		{"home", testKey(tea.KeyHome), "\x1b[H", true},
		{"end", testKey(tea.KeyEnd), "\x1b[F", true},
		{"pgup", testKey(tea.KeyPgUp), "\x1b[5~", true},
		{"pgdn", testKey(tea.KeyPgDown), "\x1b[6~", true},
		{"ctrl+c", testKeyCtrl('c'), "\x03", true},
		{"ctrl+d", testKeyCtrl('d'), "\x04", true},
		{"ctrl+a", testKeyCtrl('a'), "\x01", true},
		{"alt+x", testKeyAltText("x"), "\x1bx", true},
		{"esc unmapped", testKey(tea.KeyEsc), "", false},
		{"ctrl+1 unmapped", testKeyCtrl('1'), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ptyInputBytes(tc.msg)
			if ok != tc.ok || string(got) != tc.want {
				t.Fatalf("ptyInputBytes = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestRenderTerminalTail(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		width int
		rows  int
		want  []string
	}{
		{
			name:  "tail and padding",
			raw:   "one\ntwo\nthree",
			width: 10, rows: 4,
			want: []string{"one", "two", "three", ""},
		},
		{
			name:  "keeps last rows",
			raw:   "a\nb\nc\nd",
			width: 10, rows: 2,
			want: []string{"c", "d"},
		},
		{
			name:  "carriage return overwrites",
			raw:   "progress 10%\rprogress 90%",
			width: 20, rows: 1,
			want: []string{"progress 90%"},
		},
		{
			name:  "shorter overwrite keeps remainder",
			raw:   "downloading...\r100%",
			width: 20, rows: 1,
			want: []string{"100%loading..."},
		},
		{
			name:  "crlf normalized",
			raw:   "line1\r\nline2",
			width: 10, rows: 2,
			want: []string{"line1", "line2"},
		},
		{
			name:  "ansi stripped",
			raw:   "\x1b[31mred\x1b[0m plain",
			width: 20, rows: 1,
			want: []string{"red plain"},
		},
		{
			name:  "control runes dropped, tab expanded",
			raw:   "a\x07b\tc",
			width: 20, rows: 1,
			want: []string{"ab    c"},
		},
		{
			name:  "width truncation",
			raw:   "abcdefghijklmnop",
			width: 5, rows: 1,
			want: []string{"abcd…"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderTerminalTail(tc.raw, tc.width, tc.rows)
			if len(got) != len(tc.want) {
				t.Fatalf("renderTerminalTail = %#v, want %#v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("renderTerminalTail[%d] = %q, want %q (all: %#v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

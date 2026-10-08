package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
)

// TestSidebarActivityLines: the ACTIVITY feed is a bounded, newest-first list of
// recent completed work (stripped of the "tool result:" prefix), with a live
// "generating…" pulse when the run is active and quiet.
func TestSidebarActivityLines(t *testing.T) {
	m := model{now: time.Now}
	m.transcript = []transcriptRow{
		{kind: rowToolCall, tool: "bash", id: "c1", arg: "mkdir -p boutique-site"},
		{kind: rowToolResult, tool: "bash", id: "c1", status: tools.StatusOK, text: "tool result: bash ok Command completed with no output."},
		{kind: rowToolResult, tool: "write_file", id: "c2", status: tools.StatusOK, text: "tool result: write_file ok Created styles.css (1045 lines)."},
		{kind: rowAssistant, text: "Now the JS…"}, // ignored: not a work result
	}
	joined := plainRender(t, strings.Join(m.sidebarActivityLines(40, 10), "\n"))

	wfIdx := strings.Index(joined, "Created styles.css (1045 lines).")
	bashIdx := strings.Index(joined, "mkdir -p boutique-site")
	if wfIdx < 0 || bashIdx < 0 {
		t.Fatalf("activity should list the write_file summary and the bash command:\n%s", joined)
	}
	if wfIdx > bashIdx {
		t.Errorf("activity should be newest-first (write_file before bash):\n%s", joined)
	}
	if strings.Contains(joined, "tool result:") {
		t.Errorf("activity must strip the 'tool result:' prefix:\n%s", joined)
	}
	if got := m.sidebarActivityLines(40, 0); got != nil {
		t.Errorf("zero budget: want nil, got %v", got)
	}

	// Active + quiet run -> a live "generating…" pulse.
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	live := model{now: func() time.Time { return base.Add(30 * time.Second) }}
	live.activeRunID = 7
	live.turnStartedAt = base
	live.lastStreamActivity = base.Add(2 * time.Second) // 28s quiet
	if got := plainRender(t, strings.Join(live.sidebarActivityLines(40, 10), "\n")); !strings.Contains(got, "generating") {
		t.Errorf("active+quiet run should show a generating pulse:\n%s", got)
	}
}

func TestSidebarActivityNamesCurrentTool(t *testing.T) {
	m := sidebarTestModel()
	m.pending = true
	m.activeRunID = 7
	m.transcript = append(m.transcript, transcriptRow{kind: rowToolCall, id: "call-1", runID: 7, tool: "grep", detail: "internal/tui"})

	got := plainRender(t, strings.Join(m.sidebarActivityLines(40, 10), "\n"))
	if !strings.Contains(got, "searching") {
		t.Fatalf("sidebar activity should name the active tool, got:\n%s", got)
	}
}

func swarmSidebarTestModel(t *testing.T, sessionIDs map[string]string) model {
	t.Helper()
	m := sidebarTestModel()
	m.swarmSessionMap = sessionIDs
	m.transcript = append(m.transcript,
		transcriptRow{kind: rowToolCall, tool: "swarm_spawn", detail: "build the homepage"},
		transcriptRow{kind: rowToolResult, tool: "swarm_spawn", detail: "Spawned subagent as task subagent-1 on team default."},
		transcriptRow{kind: rowToolCall, tool: "swarm_spawn", detail: "build the stylesheet"},
		transcriptRow{kind: rowToolResult, tool: "swarm_spawn", detail: "Spawned subagent as task subagent-2 on team default."},
	)
	return m
}

func TestSwarmMemberRowCarriesSessionID(t *testing.T) {
	m := swarmSidebarTestModel(t, map[string]string{"subagent-1": "sess-1"})
	agents := m.swarmSpawnedAgents()
	if len(agents) != 2 {
		t.Fatalf("expected 2 members, got %d", len(agents))
	}
	if agents[0].sessionID != "sess-1" {
		t.Fatalf("member 1 should carry its session id, got %q", agents[0].sessionID)
	}
	if agents[1].sessionID != "" {
		t.Fatalf("member 2 has no mapped session yet, got %q", agents[1].sessionID)
	}
}

func TestSidebarAgentSelectablesMapToScreenRows(t *testing.T) {
	m := swarmSidebarTestModel(t, map[string]string{"subagent-1": "sess-1", "subagent-2": "sess-2"})
	sel := m.sidebarAgentSelectables(sidebarWidth(m.width))
	if len(sel) != 2 {
		t.Fatalf("expected 2 selectable member rows, got %d: %+v", len(sel), sel)
	}
	// AGENTS header occupies sidebar index 0, so the two members are at 1 and 2.
	if sel[0].lineOffset != 1 || sel[1].lineOffset != 2 {
		t.Fatalf("selectable offsets = %d,%d, want 1,2", sel[0].lineOffset, sel[1].lineOffset)
	}
	if sel[0].sessionID != "sess-1" || sel[1].sessionID != "sess-2" {
		t.Fatalf("selectable session ids = %q,%q", sel[0].sessionID, sel[1].sessionID)
	}
}

func TestSidebarMemberClickDoesNotInterceptFullWidthChat(t *testing.T) {
	// A real member session proves that a stale rail coordinate cannot open it.
	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{Title: "member: build the homepage", ModelID: "gpt-4.1", Provider: "openai"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := store.AppendEvent(session.SessionID, sessions.AppendEventInput{
		Type:    sessions.EventMessage,
		Payload: map[string]any{"role": "assistant", "content": "member work output"},
	}); err != nil {
		t.Fatalf("append event: %v", err)
	}

	m := swarmSidebarTestModel(t, map[string]string{"subagent-1": session.SessionID})
	m.sessionStore = store
	x := m.width - sidebarWidth(m.width) + 2
	next, _, handled := m.handleTranscriptSelectionMouse(testMouseClick(tea.MouseLeft, x, 1))
	if handled {
		t.Fatal("an invisible rail coordinate must not be handled")
	}
	if next.subchat.active {
		t.Fatalf("an invisible rail coordinate must not enter member session %q", session.SessionID)
	}
}

func TestSwarmSessionsMsgPopulatesMap(t *testing.T) {
	m := newModel(context.Background(), Options{})
	updated, _ := m.Update(swarmSessionsMsg{sessions: map[string]string{
		"subagent-1": "sess-1", "": "skip", "subagent-2": "",
	}})
	next := updated.(model)
	if next.swarmSessionMap["subagent-1"] != "sess-1" {
		t.Fatalf("expected sess-1, got %q", next.swarmSessionMap["subagent-1"])
	}
	if _, ok := next.swarmSessionMap[""]; ok {
		t.Fatal("an empty task id must be skipped")
	}
	if _, ok := next.swarmSessionMap["subagent-2"]; ok {
		t.Fatal("an empty session id must be skipped")
	}
}

func sidebarTestModel() model {
	m := newModel(context.Background(), Options{ProviderName: "test-provider", ModelName: "test-model"})
	m.width = 100
	m.height = 30
	m.altScreen = true
	m.headerPrinted = true
	// Real conversation content so the home-screen gate doesn't suppress the
	// sidebar (it stays single-column until the transcript has non-welcome rows).
	m.transcript = append(m.transcript, transcriptRow{kind: rowToolCall, tool: "read_file", detail: "main.go"})
	// A plan gives the sidebar content so it isn't auto-hidden as empty (the panel
	// only claims a column when there are agents or an active plan). Tests that
	// exercise specific agent/plan states set their own and override this.
	m.plan.steps = []planStep{{content: "wire it up", status: "in_progress"}}
	return m
}

func TestSidebarWidthClampsAndSuppresses(t *testing.T) {
	if got := sidebarWidth(40); got != 0 {
		t.Fatalf("sidebarWidth(40) = %d, want 0 (too narrow for a second column)", got)
	}
	if got := sidebarWidth(100); got < sidebarMinWidth || got > sidebarMaxWidth {
		t.Fatalf("sidebarWidth(100) = %d, want within [%d,%d]", got, sidebarMinWidth, sidebarMaxWidth)
	}
	if got := sidebarWidth(400); got != sidebarMaxWidth {
		t.Fatalf("sidebarWidth(400) = %d, want clamped to %d", got, sidebarMaxWidth)
	}
}

func TestSidebarActiveGating(t *testing.T) {
	m := sidebarTestModel()
	if !m.sidebarActive() {
		t.Fatalf("expected sidebar active for wide alt-screen model")
	}

	// Home/welcome screen (no real conversation yet): single column.
	home := m
	home.transcript = nil
	if home.sidebarActive() {
		t.Fatalf("sidebar should be inactive on the empty home screen")
	}

	// Too narrow: single column only.
	narrow := m
	narrow.width = 50
	if narrow.sidebarActive() {
		t.Fatalf("sidebar should be inactive on a narrow terminal")
	}

	// Inline (non-alt-screen) mode keeps the legacy single-column layout.
	inline := m
	inline.altScreen = false
	if inline.sidebarActive() {
		t.Fatalf("sidebar should be inactive in inline mode")
	}

	// Subchat drill-in owns the full width.
	sub := m
	sub.subchat.active = true
	if sub.sidebarActive() {
		t.Fatalf("sidebar should be inactive during subchat drill-in")
	}
}

func TestSidebarToggleHidesAndShows(t *testing.T) {
	m := sidebarTestModel()
	if !m.sidebarActive() || !m.sidebarAvailable() {
		t.Fatal("sidebar should be active and available for the test model")
	}

	// Ctrl+B hide preference suppresses the sidebar even though it's available.
	m.sidebarHidden = true
	if m.sidebarActive() {
		t.Fatal("sidebar should be inactive when hidden by the user")
	}
	if !m.sidebarAvailable() {
		t.Fatal("sidebarAvailable must ignore the hide preference (so Ctrl+B can re-show)")
	}
	// Hidden → the chat reflows to full width.
	if got, want := m.chatColumnWidth(), chatWidth(m.width); got != want {
		t.Fatalf("hidden sidebar: chat width = %d, want full %d", got, want)
	}

	// Toggling back restores the two-column layout.
	m.sidebarHidden = false
	if !m.sidebarActive() {
		t.Fatal("sidebar should be active again after un-hiding")
	}
}

func TestChatColumnWidthUsesFullConversationWidth(t *testing.T) {
	m := sidebarTestModel()
	if got, want := m.chatColumnWidth(), chatWidth(m.width); got != want {
		t.Fatalf("chat width = %d, want full width %d", got, want)
	}

	// When the sidebar is inactive, chat width is the full chat width.
	narrow := m
	narrow.width = 50
	if got := narrow.chatColumnWidth(); got != chatWidth(narrow.width) {
		t.Fatalf("narrow chatColumnWidth = %d, want full chatWidth %d", got, chatWidth(narrow.width))
	}
}

func TestRenderContextSidebarDimensions(t *testing.T) {
	m := sidebarTestModel()
	width := sidebarWidth(m.width)
	const height = 20
	lines := m.renderContextSidebar(width, height)
	if len(lines) != height {
		t.Fatalf("sidebar produced %d lines, want exactly %d", len(lines), height)
	}
	for i, line := range lines {
		if w := lipgloss.Width(line); w != width {
			t.Fatalf("sidebar line %d width = %d, want exactly %d", i, w, width)
		}
	}
	// Section headers and the token floor should be present.
	plain := stripSidebar(lines)
	if !strings.Contains(plain, "AGENTS") {
		t.Fatalf("sidebar missing AGENTS header:\n%s", plain)
	}
	if !strings.Contains(plain, "PLAN") {
		t.Fatalf("sidebar missing PLAN header:\n%s", plain)
	}
	if !strings.Contains(plain, "tokens") {
		t.Fatalf("sidebar missing token floor:\n%s", plain)
	}
}

// TestSidebarAutoHidesWhenEmpty: with no agents and no active plan the panel
// auto-hides and the chat reclaims the full width; adding a plan or an agent
// brings it back.
func TestSidebarAutoHidesWhenEmpty(t *testing.T) {
	m := sidebarTestModel() // has a plan -> sidebar active
	if !m.sidebarActive() {
		t.Fatal("expected sidebar active when the model has a plan")
	}

	// Clear the only content (the plan) -> empty -> auto-hidden.
	m.plan.steps = nil
	if m.sidebarHasContent() {
		t.Fatal("model should have no sidebar content after clearing the plan")
	}
	if m.sidebarActive() {
		t.Error("sidebar should auto-hide with no agents and no active plan")
	}
	if got, want := m.chatColumnWidth(), chatWidth(m.width); got != want {
		t.Errorf("empty sidebar: chat width = %d, want full %d", got, want)
	}

	// A spawned agent brings the panel back.
	m.specialists.start("explorer", "look around", "sess-x", time.Now())
	if !m.sidebarHasContent() || !m.sidebarActive() {
		t.Error("sidebar should return once an agent spawns")
	}
}

func TestSidebarShowsSpawnedAgents(t *testing.T) {
	m := sidebarTestModel()
	now := time.Now()
	// One running subagent with live tool activity, one completed.
	m.specialists.start("explorer", "map the codebase", "sess-1", now)
	m.specialists.setCurrentTool("sess-1", "grep", "auth")
	m.specialists.incrementToolCount("sess-1")
	m.specialists.start("reviewer", "review diff", "sess-2", now)
	m.specialists.complete("sess-2", specialistCompleted, 0, "", now)

	width := sidebarWidth(m.width)
	plain := stripSidebar(m.sidebarAgentLines(width))
	if !strings.Contains(plain, "explorer") {
		t.Fatalf("running subagent name missing:\n%s", plain)
	}
	if !strings.Contains(plain, "reviewer") {
		t.Fatalf("completed subagent name missing:\n%s", plain)
	}
	// The running subagent surfaces its live working detail (current tool).
	if !strings.Contains(plain, "grep") {
		t.Fatalf("running subagent working detail missing:\n%s", plain)
	}
	// Header shows the total agent count.
	hdr := stripSidebar([]string{m.sidebarAgentHeader(width)})
	if !strings.Contains(hdr, "AGENTS") || !strings.Contains(hdr, "2") {
		t.Fatalf("agent header should show AGENTS 2, got: %s", hdr)
	}
}

func TestSidebarShowsSwarmSpawnedAgents(t *testing.T) {
	m := sidebarTestModel()
	// Each swarm member is a CALL row (detail = the task briefing, as argHint
	// would produce it) paired with its following RESULT row (yielding the id).
	// The sidebar names the agent by the task, not the opaque "subagent-N".
	m.transcript = append(m.transcript,
		transcriptRow{kind: rowToolCall, tool: "swarm_spawn", detail: "audit the auth flow"},
		transcriptRow{kind: rowToolResult, tool: "swarm_spawn", detail: "Spawned subagent as task subagent-1 on team default."},
		transcriptRow{kind: rowToolCall, tool: "swarm_spawn", detail: "write integration tests"},
		transcriptRow{kind: rowToolResult, tool: "swarm_spawn", detail: "Spawned subagent as task subagent-2 on team default."},
		// Duplicate id (a re-report): deduped, keeps the first member's name.
		transcriptRow{kind: rowToolCall, tool: "swarm_spawn", detail: "audit the auth flow"},
		transcriptRow{kind: rowToolResult, tool: "swarm_spawn", detail: "Spawned subagent as task subagent-1 on team default."},
	)
	agents := m.swarmSpawnedAgents()
	if len(agents) != 2 {
		t.Fatalf("expected 2 unique swarm members, got %v", agents)
	}
	if agents[0].id != "subagent-1" || agents[0].name != "audit auth" {
		t.Fatalf("member 0 = %+v, want id subagent-1 named 'audit auth' (short task name)", agents[0])
	}
	if agents[1].id != "subagent-2" || agents[1].name != "write integration" {
		t.Fatalf("member 1 = %+v, want id subagent-2 named 'write integration' (short task name)", agents[1])
	}
	width := sidebarWidth(m.width)
	plain := stripSidebar(m.sidebarAgentLines(width))
	// The sidebar shows the short task-derived names, not the raw ids or full task.
	if !strings.Contains(plain, "audit auth") || !strings.Contains(plain, "write integration") {
		t.Fatalf("swarm member short task names missing from sidebar:\n%s", plain)
	}
	hdr := stripSidebar([]string{m.sidebarAgentHeader(width)})
	if !strings.Contains(hdr, "AGENTS") || !strings.Contains(hdr, "2") {
		t.Fatalf("header should show AGENTS 2, got: %s", hdr)
	}
}

// TestSwarmSpawnedAgentFallsBackToID covers a result row with no preceding call
// row (e.g. a resumed transcript that dropped the call): the member is still
// A finished member stays visible (and clickable) while the run is still in
// flight, so the user can inspect it; only once the turn ends does it drop.
func TestSwarmAgentDropsOnOwnCompletionEvenMidRun(t *testing.T) {
	base := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	m := sidebarTestModel()
	m.now = func() time.Time { return base }
	m.pending = true  // run STILL going — a member must still drop on its OWN completion
	m.activeRunID = 7 // exercise the run-scoped filter with a non-zero id
	m.transcript = append(m.transcript,
		transcriptRow{kind: rowToolCall, tool: "swarm_spawn", detail: "build homepage", runID: 7},
		transcriptRow{kind: rowToolResult, tool: "swarm_spawn", detail: "Spawned subagent as task subagent-1 on team default.", runID: 7},
		transcriptRow{kind: rowToolResult, tool: "swarm_collect", detail: "Results: 1 task(s)\n- subagent-1 [done] build homepage", runID: 7},
	)
	// Just finished, still within the linger window: shows briefly with a fading ✓.
	m.swarmDoneAt = map[string]time.Time{"subagent-1": base.Add(-sidebarAgentLinger / 2)}
	agents := m.swarmSpawnedAgents()
	if len(agents) != 1 {
		t.Fatalf("within the linger window a finished member should still show (fading), got %d: %+v", len(agents), agents)
	}
	if !agents[0].finishing {
		t.Fatalf("a finished member should render done (✓), got %+v", agents[0])
	}

	// Past the linger window: it drops — even though the overall run is still in
	// flight (previously a finished member lingered until the whole turn ended).
	m.swarmDoneAt = map[string]time.Time{"subagent-1": base.Add(-2 * sidebarAgentLinger)}
	if got := len(m.swarmSpawnedAgents()); got != 0 {
		t.Fatalf("a member past its linger window must drop on its own completion mid-run, got %d", got)
	}
}

// Members AND statuses from a previous run must not bleed into a later run, even
// when task ids repeat — both the spawn rows and the swarm_status/collect rows
// are scoped to the active run.
func TestSwarmAgentsScopedToActiveRun(t *testing.T) {
	m := sidebarTestModel()
	m.pending = true
	m.activeRunID = 2
	m.transcript = append(m.transcript,
		// Old run (runID 1): the SAME task id, and a stale "done" status for it.
		transcriptRow{kind: rowToolCall, tool: "swarm_spawn", detail: "old task", runID: 1},
		transcriptRow{kind: rowToolResult, tool: "swarm_spawn", detail: "Spawned subagent as task subagent-1 on team default.", runID: 1},
		transcriptRow{kind: rowToolResult, tool: "swarm_status", detail: "- subagent-1 [done] old task", runID: 1},
		// Current run (runID 2): the same id is reused and is still running.
		transcriptRow{kind: rowToolCall, tool: "swarm_spawn", detail: "new task", runID: 2},
		transcriptRow{kind: rowToolResult, tool: "swarm_spawn", detail: "Spawned subagent as task subagent-1 on team default.", runID: 2},
	)
	agents := m.swarmSpawnedAgents()
	if len(agents) != 1 || agents[0].id != "subagent-1" {
		t.Fatalf("only the current run's member should show, got %+v", agents)
	}
	// The stale prior-run "done" status must NOT mark the current member finished.
	if agents[0].finishing || agents[0].state == "done" {
		t.Fatalf("stale prior-run status must not affect the current member: %+v", agents[0])
	}
}

// shown, named by its id.
func TestSwarmSpawnedAgentFallsBackToID(t *testing.T) {
	m := sidebarTestModel()
	m.transcript = append(m.transcript,
		transcriptRow{kind: rowToolResult, tool: "swarm_spawn", detail: "Spawned subagent as task subagent-9 on team default."},
	)
	agents := m.swarmSpawnedAgents()
	if len(agents) != 1 || agents[0].id != "subagent-9" || agents[0].name != "subagent-9" {
		t.Fatalf("expected one member named by id, got %+v", agents)
	}
}

func TestShortTaskName(t *testing.T) {
	cases := map[string]string{
		"Explore the repository structure and summarize": "Explore repository",
		"Review the current git branch":                  "Review current",
		"Check for any TODOs, FIXMEs":                    "Check TODOs",
		"Provide a high-level overview":                  "Provide high-level",
		"single":                                         "single",
		"":                                               "",
	}
	for task, want := range cases {
		if got := shortTaskName(task); got != want {
			t.Errorf("shortTaskName(%q) = %q, want %q", task, got, want)
		}
	}
}

func TestSwarmAgentsLingerThenDisappearWhenDone(t *testing.T) {
	base := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	m := sidebarTestModel()
	m.now = func() time.Time { return base }
	m.transcript = append(m.transcript,
		transcriptRow{kind: rowToolCall, tool: "swarm_spawn", detail: "explore repo"},
		transcriptRow{kind: rowToolResult, tool: "swarm_spawn", detail: "Spawned teammate as task teammate-1 on team default."},
		transcriptRow{kind: rowToolCall, tool: "swarm_spawn", detail: "review branch"},
		transcriptRow{kind: rowToolResult, tool: "swarm_spawn", detail: "Spawned teammate as task teammate-2 on team default."},
	)
	if got := len(m.swarmSpawnedAgents()); got != 2 {
		t.Fatalf("expected 2 live members before any status report, got %d", got)
	}
	// teammate-1 reported done, teammate-2 still running.
	m.transcript = append(m.transcript, transcriptRow{
		kind: rowToolResult, tool: "swarm_status",
		detail: "Swarm status (team default): 2 task(s)\n– teammate-1 [done] (cyan) explore repo\n– teammate-2 [running] (blue) review branch",
	})
	// Freshly done (not yet stamped by the tick): teammate-1 LINGERS (finishing).
	agents := m.swarmSpawnedAgents()
	if len(agents) != 2 {
		t.Fatalf("a freshly-done member should linger, got %d: %+v", len(agents), agents)
	}
	var done *swarmAgent
	for i := range agents {
		if agents[i].id == "teammate-1" {
			done = &agents[i]
		}
	}
	if done == nil || !done.finishing {
		t.Fatalf("teammate-1 should be finishing (lingering), got %+v", agents)
	}

	// The spinner tick stamps the done time; once the linger window elapses the
	// member is removed.
	m.stampSwarmDone()
	if _, ok := m.swarmDoneAt["teammate-1"]; !ok {
		t.Fatal("stampSwarmDone should record the finished member")
	}
	m.swarmDoneAt["teammate-1"] = base.Add(-2 * sidebarAgentLinger) // past the window
	agents = m.swarmSpawnedAgents()
	if len(agents) != 1 || agents[0].id != "teammate-2" {
		t.Fatalf("after the linger the done member should be gone; got %+v", agents)
	}
}

// Regression: a swarm_collect that runs WHILE members are still working must not
// clear the AGENTS panel. Previously any swarm_collect result wiped the roster,
// so the sidebar showed "no agents spawned" mid-run even with 4 subagents live.
func TestSwarmAgentsStayVisibleWhileCollectRunsMidFlight(t *testing.T) {
	m := sidebarTestModel()
	m.transcript = append(m.transcript,
		transcriptRow{kind: rowToolCall, tool: "swarm_spawn", detail: "build the homepage"},
		transcriptRow{kind: rowToolResult, tool: "swarm_spawn", detail: "Spawned subagent as task subagent-1 on team default."},
		transcriptRow{kind: rowToolCall, tool: "swarm_spawn", detail: "build the stylesheet"},
		transcriptRow{kind: rowToolResult, tool: "swarm_spawn", detail: "Spawned subagent as task subagent-2 on team default."},
		// A collect mid-flight, both members still running.
		transcriptRow{kind: rowToolResult, tool: "swarm_collect",
			detail: "Results for team default: 2 task(s)\n- subagent-1 [running] build the homepage\n- subagent-2 [running] build the stylesheet"},
	)

	agents := m.swarmSpawnedAgents()
	if len(agents) != 2 {
		t.Fatalf("running members must survive a mid-flight swarm_collect, got %d: %+v", len(agents), agents)
	}
	for _, a := range agents {
		if a.finishing {
			t.Fatalf("a running member must not be marked finishing: %+v", a)
		}
		if a.state != "running" {
			t.Fatalf("swarm_collect should set member state to running, got %q for %s", a.state, a.id)
		}
	}

	plain := stripSidebar(m.sidebarAgentLines(sidebarWidth(m.width)))
	if !strings.Contains(plain, "homepage") || !strings.Contains(plain, "stylesheet") {
		t.Fatalf("sidebar should list the running members and their tasks:\n%s", plain)
	}
}

func TestSidebarHidesNotFoundSpecialistMisroutes(t *testing.T) {
	m := sidebarTestModel()
	now := time.Now()
	// A real running specialist + a failed tool-misroute (a swarm tool name called
	// as a specialist → "specialist not found"), which should be filtered out.
	m.specialists.start("worker", "build frontend", "sess-real", now)
	m.specialists.start("swarm_send", "coordinate", "sess-bogus", now)
	m.specialists.complete("sess-bogus", specialistError, 0, `specialist "swarm_send" not found`, now)

	got := m.sidebarSpecialists()
	if len(got) != 1 || got[0].name != "worker" {
		t.Fatalf("not-found misroute should be filtered; want only worker, got %+v", got)
	}
	plain := stripSidebar(m.sidebarAgentLines(sidebarWidth(m.width)))
	if strings.Contains(plain, "swarm_send") {
		t.Fatalf("bogus swarm_send specialist should not appear:\n%s", plain)
	}
	if !strings.Contains(plain, "worker") {
		t.Fatalf("real worker specialist should still appear:\n%s", plain)
	}
}

func TestSidebarPlanReflectsState(t *testing.T) {
	m := sidebarTestModel()
	m.plan.steps = []planStep{
		{content: "read code", status: "completed"},
		{content: "refactor auth", status: "in_progress"},
		{content: "run tests", status: "pending"},
	}
	header := plainRender(t, m.sidebarPlanHeader(40))
	if !strings.Contains(header, "PLAN") || !strings.Contains(header, "1/3") {
		t.Fatalf("plan header = %q, want PLAN with 1/3 count", header)
	}
	lines := m.sidebarPlanLines(40)
	if len(lines) != 3 {
		t.Fatalf("plan lines = %d, want 3", len(lines))
	}
	joined := stripSidebar(lines)
	if !strings.Contains(joined, "✓") || !strings.Contains(joined, "•") || !strings.Contains(joined, "○") {
		t.Fatalf("plan lines missing status glyphs:\n%s", joined)
	}
}

func TestJoinColumnsAligns(t *testing.T) {
	chat := []string{"hello", "world", "third row that is longer"}
	sidebar := []string{"A", "B"}
	const chatW, sidebarW = 12, 6
	rows := joinColumns(chat, sidebar, chatW, sidebarW)
	if len(rows) != 3 {
		t.Fatalf("joined %d rows, want max(3,2)=3", len(rows))
	}
	want := chatW + 3 + sidebarW // " │ " padded divider
	for i, row := range rows {
		if w := lipgloss.Width(row); w != want {
			t.Fatalf("row %d width = %d, want %d", i, w, want)
		}
	}
}

func TestTranscriptViewUsesFullWidth(t *testing.T) {
	m := sidebarTestModel()
	out := m.transcriptView()
	lines := strings.Split(out, "\n")
	if len(lines) != m.height {
		t.Fatalf("transcript view = %d lines, want terminal height %d", len(lines), m.height)
	}
	for i, line := range lines {
		if w := lipgloss.Width(line); w > m.width {
			t.Fatalf("row %d width = %d, exceeds full width %d", i, w, m.width)
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(ansiPattern.ReplaceAllString(line, ""), "╭") && lipgloss.Width(line) == m.width {
			return
		}
	}
	t.Fatal("full-width composer frame not found")
}

func TestFullWidthFooterKeepsContextAndComposerVisible(t *testing.T) {
	m := sidebarTestModel()
	m.width, m.height = 120, 34
	m.unpricedTokens = 10000
	out := plainRender(t, m.transcriptView())
	lines := strings.Split(out, "\n")

	tokenRow := -1
	composerTop := -1
	for index, line := range lines {
		if strings.Contains(line, "10K tok") {
			tokenRow = index
		}
		if strings.HasPrefix(line, "╭") {
			composerTop = index
		}
	}
	if tokenRow < 0 {
		t.Fatalf("footer should retain context information:\n%s", out)
	}
	if composerTop < 0 {
		t.Fatalf("chat composer missing:\n%s", out)
	}
	composerRunes := []rune(lines[composerTop])
	if len(composerRunes) != m.width || composerRunes[m.width-1] != '╮' {
		t.Fatalf("composer should use the full conversation width, got %q", lines[composerTop])
	}
}

// stripSidebar joins sidebar lines and strips ANSI for content assertions.
func stripSidebar(lines []string) string {
	return ansiPattern.ReplaceAllString(strings.Join(lines, "\n"), "")
}

// The `/` command palette must not reduce the conversation width.
func TestCommandPaletteKeepsConversationWidth(t *testing.T) {
	m := sidebarTestModel()
	m.altScreen = true
	m.height = 40
	m.headerPrinted = true
	m.transcript = append(m.transcript, transcriptRow{kind: rowToolCall, tool: "read_file", detail: "main.go"})
	m.suggestions = []commandSuggestion{{Name: "/model", Desc: "Pick a model."}, {Name: "/plan", Desc: "Show planning mode status."}}
	if !m.suggestionsActive() {
		t.Fatal("precondition: suggestions should be active")
	}
	// The palette is bounded within the full-width transcript.
	chatW := m.chatColumnWidth()
	for _, line := range strings.Split(plainRender(t, m.suggestionOverlay(chatW)), "\n") {
		if w := lipgloss.Width(line); w > chatW {
			t.Errorf("palette line is %d wide, wider than the chat column %d — it would overlap the sidebar", w, chatW)
		}
	}
}

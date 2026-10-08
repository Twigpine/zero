package tui

import (
	"strings"
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/Gitlawb/zero/internal/tools"
	"github.com/charmbracelet/colorprofile"
)

// TestWorkingStatusDoesNotDuplicatePlan keeps the activity cue focused on the
// current model/tool phase. Full plan state belongs to its transcript update.
func TestWorkingStatusDoesNotDuplicatePlan(t *testing.T) {
	m := model{now: time.Now}
	m.plan.steps = []planStep{{content: "Add product catalog", status: "in_progress"}}
	got := plainRender(t, m.workingStatusLine())
	if strings.Contains(got, "plan") || strings.Contains(got, "Add product catalog") {
		t.Fatalf("working line duplicated plan state: %q", got)
	}
}

func TestWorkingActivityNamesCurrentPhase(t *testing.T) {
	tests := []struct {
		name  string
		model model
		want  string
	}{
		{name: "default", want: "thinking"},
		{name: "assistant text", model: model{streamingText: []byte("drafting")}, want: "writing"},
		{name: "permission", model: model{pendingPermission: &pendingPermissionPrompt{}}, want: "waiting for approval"},
		{name: "question", model: model{pendingAskUser: &pendingAskUserPrompt{}}, want: "waiting for your answer"},
		{name: "streamed tool", model: model{streamCallName: "grep"}, want: "searching"},
		{
			name: "outstanding tool",
			model: model{
				pending:     true,
				activeRunID: 7,
				transcript:  []transcriptRow{{kind: rowToolCall, id: "call-1", runID: 7, tool: "read_file"}},
			},
			want: "reading",
		},
		{
			name: "completed tool",
			model: model{
				pending:     true,
				activeRunID: 7,
				transcript: []transcriptRow{
					{kind: rowToolCall, id: "call-1", runID: 7, tool: "read_file"},
					{kind: rowToolResult, id: "call-1", runID: 7, tool: "read_file", status: tools.StatusOK},
				},
			},
			want: "thinking",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.model.workingActivity(); got != test.want {
				t.Fatalf("workingActivity() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWorkingStatusAnimationAdvancesAcrossActiveRun(t *testing.T) {
	previousProfile := lipgloss.Writer.Profile
	lipgloss.Writer.Profile = colorprofile.TrueColor
	t.Cleanup(func() { lipgloss.Writer.Profile = previousProfile })

	m := newModel(t.Context(), Options{})
	m.width = 100
	m.pending = true
	m.reducedMotion = false

	m.spinnerPhase = 0
	first := m.workingStatusLine()
	m.spinnerPhase = 3
	second := m.workingStatusLine()
	if first == second {
		t.Fatalf("working status animation did not advance across phases:\nfirst:  %q\nsecond: %q", first, second)
	}
	firstPlain, secondPlain := []rune(plainRender(t, first)), []rune(plainRender(t, second))
	if len(firstPlain) < 3 || len(secondPlain) < 3 || string(firstPlain[2:]) != string(secondPlain[2:]) || !strings.Contains(string(firstPlain), "Working") {
		t.Fatalf("status animation must preserve its text while the grid moves:\nfirst:  %q\nsecond: %q", string(firstPlain), string(secondPlain))
	}
	if got := plainRender(t, m.workingStatusIndicator()); len([]rune(got)) != 2 {
		t.Fatalf("working status indicator = %q, want a compact two-cell 3x3 grid", got)
	}
	m.spinnerPhase = 0
	firstLabel := m.workingStatusLabel()
	m.spinnerPhase = 3
	secondLabel := m.workingStatusLabel()
	if firstLabel == secondLabel {
		t.Fatalf("working label shimmer did not advance across phases:\nfirst:  %q\nsecond: %q", firstLabel, secondLabel)
	}
	if got := plainRender(t, firstLabel); got != "Working" {
		t.Fatalf("working label shimmer changed its text: %q", got)
	}

	if got := workingDriveLevels(0)[3]; got != workingDriveBright {
		t.Fatalf("chevron lead cell level = %v, want bright", got)
	}
	if got := workingDriveLevelAt(12, 4*workingDriveStep); got != workingDriveBright {
		t.Fatalf("first Working letter level = %v, want bright after the grid", got)
	}

	m.reducedMotion = true
	m.spinnerPhase = 0
	first = m.workingStatusLine()
	m.spinnerPhase = 2
	second = m.workingStatusLine()
	if first != second {
		t.Fatalf("reduced-motion working status should stay stable:\nfirst:  %q\nsecond: %q", first, second)
	}
	if got := m.workingStatusIndicator(); got != "" {
		t.Fatalf("reduced-motion status indicator = %q, want empty", got)
	}
	if got := plainRender(t, m.workingStatusLabel()); got != "Working" {
		t.Fatalf("reduced-motion working label = %q, want Working", got)
	}
}

func TestModelUsesSmoothActiveAnimationCadence(t *testing.T) {
	m := newModel(t.Context(), Options{})
	if got := m.spinner.Spinner.FPS; got != activeAnimationFrameInterval {
		t.Fatalf("active spinner FPS = %v, want %v", got, activeAnimationFrameInterval)
	}
}

// TestHiddenPlumbingToolsSkippedFromTranscript: tool-search and specialist
// plumbing render nothing on success. update_plan hides only its call; the
// completed result is the user-facing plan checklist in the transcript.
func TestHiddenPlumbingToolsSkippedFromTranscript(t *testing.T) {
	rows := []transcriptRow{
		{kind: rowToolCall, tool: "update_plan", id: "c1", runID: 1},
		{kind: rowToolResult, tool: "update_plan", id: "c1", runID: 1, text: "10 steps · 2 done"},
		{kind: rowToolCall, tool: "tool_search", id: "c2", runID: 1},
		{kind: rowToolResult, tool: "tool_search", id: "c2", runID: 1, text: "select:swarm_spawn,…"},
		{kind: rowToolCall, tool: "TaskOutput", id: "c3", runID: 1},
		{kind: rowToolResult, tool: "TaskOutput", id: "c3", runID: 1, text: "task result"},
		{kind: rowToolResult, tool: "bash", id: "c4", runID: 1, text: "ok"},
	}
	rc := buildRowContext(rows)
	for _, i := range []int{0, 2, 3, 4, 5} {
		if !rc.skip(rows[i]) {
			t.Errorf("plumbing row %d (%s/%v) should be skipped", i, rows[i].tool, rows[i].kind)
		}
	}
	if rc.skip(rows[1]) {
		t.Error("completed update_plan result should render as a transcript checklist")
	}
	if rc.skip(rows[6]) {
		t.Error("a normal tool result (bash) must not be skipped")
	}

	// A FAILED plumbing result must still render — its error has to surface.
	for _, failed := range []transcriptRow{
		{kind: rowToolResult, tool: "update_plan", id: "c9", runID: 1, status: tools.StatusError, text: "tool result: update_plan error boom"},
		{kind: rowToolResult, tool: "TaskOutput", id: "c10", runID: 1, status: tools.StatusError, text: "tool result: TaskOutput error boom"},
	} {
		if buildRowContext([]transcriptRow{failed}).skip(failed) {
			t.Errorf("a failed plumbing result (%s) must NOT be skipped", failed.tool)
		}
	}

	if !isHiddenPlumbingTool("tool_search") || !isHiddenPlumbingTool("TaskOutput") {
		t.Error("tool_search and TaskOutput must be hidden plumbing")
	}
	if isHiddenPlumbingTool("update_plan") {
		t.Error("update_plan result must remain visible as its dedicated checklist")
	}
	if isHiddenPlumbingTool("write_file") || isHiddenPlumbingTool("web_search") {
		t.Error("real work tools must NOT be hidden")
	}
}

// TestQuietGenerationHint: after a silent stretch during an active run, the
// working line shows a "still generating…" cue with an advancing timer; while
// output is flowing (recent activity) or when idle, it stays empty.
func TestQuietGenerationHint(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	m := model{now: func() time.Time { return base.Add(30 * time.Second) }}
	m.activeRunID = 7
	m.turnStartedAt = base

	// Recently active -> no hint.
	m.lastStreamActivity = base.Add(28 * time.Second)
	if got := m.quietGenerationHint(); got != "" {
		t.Errorf("recent activity: want no hint, got %q", got)
	}

	// Quiet for >= the threshold -> a "still generating…" cue appears, and shows
	// up on the rendered working line.
	m.lastStreamActivity = base.Add(5 * time.Second) // 25s quiet at now
	if got := m.quietGenerationHint(); !strings.Contains(got, "still generating") {
		t.Errorf("quiet stretch: want a still-generating hint, got %q", got)
	}
	if line := plainRender(t, m.workingStatusLine()); !strings.Contains(line, "still generating") {
		t.Errorf("working line should carry the quiet hint, got %q", line)
	}

	// No active run -> never a hint.
	idle := m
	idle.activeRunID = 0
	if got := idle.quietGenerationHint(); got != "" {
		t.Errorf("idle: want no hint, got %q", got)
	}
}

func TestBeginRunResetsQuietGenerationClock(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	m := model{
		now:                func() time.Time { return now },
		lastStreamActivity: now.Add(-time.Minute),
	}

	m = m.beginRun(nil)

	if got := m.quietGenerationHint(); got != "" {
		t.Fatalf("new run inherited a stale quiet-generation warning: %q", got)
	}
}

// TestQuietGenerationHintEscalatesPastHalfIdleTimeout: a heartbeating-but-
// silent stream (chatgpt/gpt-5.x, ollama reasoning models — see
// providerio.ErrStreamStalled) and a genuinely healthy-but-slow one look
// identical under the plain "still generating… Xs" cue — the ticking number
// is the only signal, whether real content is still coming or nothing ever
// will. Past half the provider's idle timeout the cue must say so explicitly
// and name when Zero's own content-stall watchdog will act, rather than
// leaving the user to guess whether this is a hang.
func TestQuietGenerationHintEscalatesPastHalfIdleTimeout(t *testing.T) {
	// 30s idle timeout: half (15s) sits comfortably above quietWorkingHint (8s),
	// leaving a clean window to observe the plain cue before it escalates.
	t.Setenv("ZERO_STREAM_IDLE_TIMEOUT", "30s")
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	m := model{now: func() time.Time { return base }}
	m.activeRunID = 7
	m.turnStartedAt = base

	// Quiet past quietWorkingHint but under half the (30s) idle timeout -> the
	// plain cue, same as TestQuietGenerationHint's normal case.
	m.lastStreamActivity = base.Add(-10 * time.Second)
	got := m.quietGenerationHint()
	if !strings.Contains(got, "still generating") {
		t.Fatalf("under half the idle timeout: want the plain cue, got %q", got)
	}
	if strings.Contains(got, "auto-recover") {
		t.Fatalf("under half the idle timeout: should not have escalated yet, got %q", got)
	}

	// Quiet for >= half the idle timeout -> escalates to name the watchdog and
	// its ceiling (providerio.ContentStallTimeout(30s) = 30s*1.2 = 36s here).
	m.lastStreamActivity = base.Add(-15 * time.Second)
	got = m.quietGenerationHint()
	if !strings.Contains(got, "unusually quiet") || !strings.Contains(got, "auto-recover") {
		t.Fatalf("past half the idle timeout: want an escalated cue naming the watchdog, got %q", got)
	}
	if !strings.Contains(got, "36s") {
		t.Fatalf("escalated cue should name the content-stall ceiling (ContentStallTimeout(30s) = 36s), got %q", got)
	}
}

// TestQuietHintShownWithRunDetails verifies a silent generation remains visible
// in the working line, including while run details are available on demand.
func TestQuietHintShownWithRunDetails(t *testing.T) {
	base := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	m := sidebarTestModel() // alt-screen + a plan -> sidebar active
	m.now = func() time.Time { return base.Add(30 * time.Second) }
	m.activeRunID = 7
	m.turnStartedAt = base
	m.lastStreamActivity = base.Add(2 * time.Second) // 28s quiet
	if line := plainRender(t, m.workingStatusLine()); !strings.Contains(line, "still generating") {
		t.Errorf("working line should carry the quiet hint:\n%s", line)
	}
	if act := plainRender(t, strings.Join(m.sidebarActivityLines(sidebarWidth(m.width), 10), "\n")); !strings.Contains(act, "generating") {
		t.Errorf("run details should retain the generating pulse:\n%s", act)
	}
}

func TestFormatWorkingElapsed(t *testing.T) {
	cases := map[time.Duration]string{
		0:                 "0s",
		8 * time.Second:   "8s",
		59 * time.Second:  "59s",
		64 * time.Second:  "1m04s",
		125 * time.Second: "2m05s",
		-3 * time.Second:  "0s",
	}
	for d, want := range cases {
		if got := formatWorkingElapsed(d); got != want {
			t.Errorf("formatWorkingElapsed(%s) = %q, want %q", d, got, want)
		}
	}
}

// The key fix: the live working line (spinner + verb + elapsed) is shown even
// AFTER partial text has streamed, so an upstream stall never looks frozen.
func TestInterimBlockShowsWorkingLineWithStreamedText(t *testing.T) {
	m := newModel(t.Context(), Options{ModelName: "gpt-4.1"})
	m.width = 100
	base := time.Date(2026, 6, 14, 10, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return base.Add(12 * time.Second) }
	m.turnStartedAt = base
	m.streamingText = []byte("partial answer so far")

	got := plainRender(t, m.interimBlock(96))
	if !strings.Contains(got, "partial answer so far") {
		t.Fatalf("interim block should keep the streamed text:\n%s", got)
	}
	if !strings.Contains(got, "12s") {
		t.Fatalf("interim block should show live elapsed (12s) below the text:\n%s", got)
	}
	if !strings.Contains(got, "Working") {
		t.Fatalf("interim block should show the liveness label:\n%s", got)
	}
}

// The working line carries a live token estimate ("↑ <n> tok") that climbs as
// the model streams, replacing the old static scroll figure.
func TestWorkingTokenIndicatorEstimatesFromStreamedRunes(t *testing.T) {
	m := newModel(t.Context(), Options{ModelName: "gpt-4.1"})
	// Always visible during a run, starting at zero so the working line never
	// drops the counter (the bug: it blinked out during the initial think).
	if got := m.workingTokenIndicator(); !strings.Contains(got, "↑") || !strings.Contains(got, "0 tok") {
		t.Fatalf("at turn start the counter should read like ↑ 0 tok, got %q", got)
	}
	m.turnStreamedRunes = 4000 // ~1000 tokens at ~4 chars/token
	got := m.workingTokenIndicator()
	for _, want := range []string{"↑", "tok", "1K"} {
		if !strings.Contains(got, want) {
			t.Fatalf("indicator = %q, want it to contain %q", got, want)
		}
	}
}

// The estimate must keep climbing across the per-segment buffer clears (a tool
// call wipes streamingText/Reasoning) — turnStreamedRunes accumulates over the
// whole turn, so the counter never snaps back to zero mid-turn.
func TestWorkingTokenIndicatorAccumulatesAcrossSegmentClears(t *testing.T) {
	m := newModel(t.Context(), Options{ModelName: "gpt-4.1"})
	m = m.beginRun(nil)
	rid := m.activeRunID

	updated, _ := m.Update(agentReasoningMsg{runID: rid, delta: strings.Repeat("a", 40)})
	m = updated.(model)
	afterReasoning := m.turnStreamedRunes
	if afterReasoning == 0 {
		t.Fatal("reasoning deltas should accumulate streamed runes")
	}

	// Simulate the segment boundary that clears the live buffers, then stream
	// answer text in the next segment.
	m.streamingReasoning = ""
	m.streamingText = nil
	updated, _ = m.Update(agentTextMsg{runID: rid, delta: strings.Repeat("b", 40)})
	m = updated.(model)

	if m.turnStreamedRunes <= afterReasoning {
		t.Fatalf("token estimate must keep climbing across the buffer clear: before=%d after=%d", afterReasoning, m.turnStreamedRunes)
	}

	// The climbing figure must actually reach the rendered working line.
	if line := plainRender(t, m.workingStatusLine()); !strings.Contains(line, "tok") {
		t.Fatalf("working status line should carry the live token counter, got %q", line)
	}

	// A fresh turn resets the accumulator to zero.
	m = m.beginRun(nil)
	if m.turnStreamedRunes != 0 {
		t.Fatalf("beginRun should reset the per-turn token estimate, got %d", m.turnStreamedRunes)
	}
}

func TestPreviewTail(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  string
	}{
		{"short", 20, "short"},
		{"exactlyten", 10, "exactlyten"},
		{"abcdefghijklmnop", 6, "…lmnop"}, // tail with leading ellipsis
		{"", 8, ""},
	}
	for _, c := range cases {
		if got := previewTail(c.in, c.width); got != c.want {
			t.Errorf("previewTail(%q,%d) = %q, want %q", c.in, c.width, got, c.want)
		}
	}
}

// The fix: during a think (no answer text yet) the streaming reasoning TAIL is
// shown beneath the working line, so the screen shows live, changing content.
func TestInterimBlockShowsReasoningPreviewWhileThinking(t *testing.T) {
	m := newModel(t.Context(), Options{ModelName: "gpt-4.1"})
	m.width = 100
	base := time.Date(2026, 6, 18, 23, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return base.Add(90 * time.Second) }
	m.turnStartedAt = base
	m.streamingReasoning = "analyzing the layout\nthe patch was corrupt so re-planning the css edits"
	m.streamingText = nil // thinking phase: no answer yet

	got := plainRender(t, m.interimBlock(96))
	if !strings.Contains(got, "re-planning the css edits") {
		t.Fatalf("expected the live reasoning tail in the preview:\n%s", got)
	}
	if !strings.Contains(got, "1m30s") {
		t.Fatalf("expected the working-line elapsed clock:\n%s", got)
	}
}

// When the reasoning block is EXPANDED, the full body already shows — the
// collapsed tail preview must NOT be duplicated.
func TestInterimBlockNoPreviewWhenReasoningExpanded(t *testing.T) {
	m := newModel(t.Context(), Options{ModelName: "gpt-4.1"})
	m.width = 100
	m.now = func() time.Time { return time.Date(2026, 6, 18, 23, 0, 0, 0, time.UTC) }
	m.streamingReasoningExpanded = true
	m.streamingReasoning = "only line of reasoning here"
	m.streamingText = nil
	got := plainRender(t, m.interimBlock(96))
	if strings.Count(got, "only line of reasoning here") != 1 {
		t.Fatalf("reasoning should appear exactly once when expanded (no preview dup):\n%s", got)
	}
}

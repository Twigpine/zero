package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/tools"
)

func sizedTestModel(width int) model {
	m := newModel(context.Background(), Options{})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: 30})
	// Ack the one-time header print so later settles aren't queued behind it.
	updated, _ = updated.(model).Update(flushedMsg{})
	return updated.(model)
}

func TestSettledRowsAdvanceFrontierAndLeaveLiveView(t *testing.T) {
	m := sizedTestModel(80)
	m.transcript = appendRow(m.transcript, rowUser, "hello there")
	m.transcript = appendRow(m.transcript, rowSystem, "noted")

	next, cmd := m.settleTranscript()
	if next.flushed != len(next.transcript) {
		t.Fatalf("expected frontier at %d, got %d", len(next.transcript), next.flushed)
	}
	if cmd == nil {
		t.Fatal("expected a scrollback print command for the settled rows")
	}
	view := viewString(next.View())
	if strings.Contains(view, "hello there") || strings.Contains(view, "noted") {
		t.Fatalf("flushed rows must not re-render in the live view, got %q", view)
	}
}

func TestAltScreenAdvancesFrontierAndCachesSettledRows(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	m = updated.(model)
	m.transcript = appendRow(m.transcript, rowUser, "hello there")
	m.transcript = appendRow(m.transcript, rowSystem, "noted")

	next, cmd := m.settleTranscript()
	if cmd != nil {
		t.Fatal("alt-screen mode should not print rows into native scrollback")
	}
	if next.flushed != len(next.transcript) {
		t.Fatalf("alt-screen mode should advance the frontier to %d, got %d", len(next.transcript), next.flushed)
	}
	view := viewString(next.View())
	if !strings.Contains(view, "hello there") || !strings.Contains(view, "noted") {
		t.Fatalf("cached settled rows should remain in the managed alt-screen view, got %q", view)
	}
}

func TestAltScreenSettledCacheRebuildsAfterRowToggle(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	m = updated.(model)
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{kind: rowReasoning, text: "private thought"})
	m, _ = m.settleTranscript()

	m = m.toggleTranscriptRow(len(m.transcript) - 1)
	m, _ = m.settleTranscript()
	if !m.transcript[len(m.transcript)-1].expanded {
		t.Fatal("expected reasoning row to be expanded")
	}
	if m.altScreenSettledWidth == 0 || len(m.altScreenSettledItems) == 0 {
		t.Fatal("expected invalidated settled cache to be rebuilt")
	}
}

func TestAltScreenSettledCacheRebuildsAfterDoctorStatusUpdate(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	m = updated.(model)
	m = m.setDoctorStatusRow("frame 0")
	m, _ = m.settleTranscript()
	if m.altScreenSettledWidth == 0 {
		t.Fatal("precondition: settled cache should be populated")
	}

	// An in-place update to an already-settled doctor row (e.g. a spinner tick)
	// must invalidate the settled cache, just like toggleTranscriptRow does for
	// expand/collapse, otherwise the alt-screen view keeps serving the stale
	// snapshot forever.
	m = m.setDoctorStatusRow("frame 1")
	if m.altScreenSettledWidth != 0 {
		t.Fatal("expected in-place doctor status update to invalidate the settled cache")
	}
	m, _ = m.settleTranscript()
	view := viewString(m.View())
	if !strings.Contains(view, "frame 1") {
		t.Fatalf("expected updated doctor status text in view, got %q", view)
	}
	if strings.Contains(view, "frame 0") {
		t.Fatalf("stale doctor status text must not remain in view, got %q", view)
	}
}

func TestAltScreenSettledCacheInvalidatesWithFileSelection(t *testing.T) {
	m := filesPanelTestModel()
	m.altScreen = true
	m.width = 80
	m.height = 30
	m, _ = m.settleTranscript()
	if m.altScreenSettledWidth == 0 {
		t.Fatal("precondition: settled cache should be populated")
	}

	m = m.selectFile("internal/tui/sidebar.go")
	if m.altScreenSettledWidth != 0 {
		t.Fatal("selecting a file touched by a settled row must invalidate the cache")
	}
	m, _ = m.settleTranscript()
	_ = viewString(m.View())

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(model)
	if m.selectedFile != "" {
		t.Fatalf("Esc should clear the selected file, got %q", m.selectedFile)
	}
	if m.altScreenSettledWidth == 0 || m.altScreenSettledFrontier != m.flushed {
		t.Fatal("clearing a selected file should rebuild the invalidated cache")
	}
	_ = viewString(m.View())
}

func TestAltScreenSettledCacheInvalidatesForOlderFileCard(t *testing.T) {
	const path = "internal/tui/sidebar.go"
	m := model{
		transcript: []transcriptRow{
			{kind: rowToolResult, changedFiles: []string{path}},
			{kind: rowToolResult, changedFiles: []string{path}},
		},
		flushed:               1,
		altScreenSettledWidth: 80,
	}

	m.setSelectedFile(path)
	if m.altScreenSettledWidth != 0 {
		t.Fatal("selecting a file with an older settled card must invalidate the cache")
	}

	m.altScreenSettledWidth = 80
	m.setSelectedFile("")
	if m.altScreenSettledWidth != 0 {
		t.Fatal("clearing a file with an older settled card must invalidate the cache")
	}
}

func TestAltScreenRepeatedStatusCollapseRewindsSettledFrontier(t *testing.T) {
	m := newModel(context.Background(), Options{AltScreen: true})
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	m = updated.(model)
	m.pending = true
	m.activeRunID = 7
	m.transcript = appendTranscriptRow(m.transcript,
		transcriptRow{kind: rowToolCall, id: "old", tool: "swarm_status", runID: 7},
	)
	m.transcript = appendTranscriptRow(m.transcript,
		transcriptRow{kind: rowToolResult, id: "old", tool: "swarm_status", status: tools.StatusOK, detail: "1 running", runID: 7},
	)
	m.transcript = appendTranscriptRow(m.transcript,
		transcriptRow{kind: rowToolCall, id: "new", tool: "swarm_status", runID: 7},
	)
	m, _ = m.settleTranscript()
	if m.flushed != len(m.transcript)-1 {
		t.Fatalf("precondition: frontier=%d, want %d", m.flushed, len(m.transcript)-1)
	}

	updated, _ = m.Update(agentRowMsg{
		runID: 7,
		row: transcriptRow{
			kind: rowToolResult, id: "new", tool: "swarm_status", status: tools.StatusError, detail: "1 running", runID: 7,
		},
	})
	m = updated.(model)
	if len(m.transcript) != 3 {
		t.Fatalf("repeated status should collapse to welcome plus the new call/result pair, got %d rows", len(m.transcript))
	}
	if m.flushed != len(m.transcript) {
		t.Fatalf("collapsed replacement should settle through %d rows, got frontier %d", len(m.transcript), m.flushed)
	}
	if m.altScreenSettledWidth == 0 || m.altScreenSettledFrontier != m.flushed {
		t.Fatal("collapsed replacement should rebuild the invalidated settled cache")
	}
	cachedView := viewString(m.View())
	rebuilt := m
	rebuilt.altScreenSettledWidth = 0
	rebuilt, _ = rebuilt.settleTranscript()
	if rebuiltView := viewString(rebuilt.View()); rebuiltView != cachedView {
		t.Fatal("collapsed replacement cache is stale compared with a forced rebuild")
	}
}

func TestRunningToolCallBlocksFrontierUntilResult(t *testing.T) {
	m := sizedTestModel(80)
	m.pending = true
	m.activeRunID = 3
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{
		kind: rowToolCall, id: "call_1", text: "tool call: read_file", tool: "read_file", runID: 3,
	})

	next, _ := m.settleTranscript()
	if next.flushed != 1 { // welcome row settles; the live call must block
		t.Fatalf("running tool call should block the frontier at 1, got %d", next.flushed)
	}

	next.transcript = appendTranscriptRow(next.transcript, transcriptRow{
		kind: rowToolResult, id: "call_1", text: "tool result: read_file ok", tool: "read_file",
		status: tools.StatusOK, detail: "data", runID: 3,
	})
	settled, cmd := next.settleTranscript()
	if settled.flushed != len(settled.transcript) {
		t.Fatalf("resolved call should settle through, frontier=%d rows=%d", settled.flushed, len(settled.transcript))
	}
	if cmd == nil {
		t.Fatal("expected the result card to flush to scrollback")
	}
}

func TestOrphanToolCallSettlesAfterRunEnds(t *testing.T) {
	m := sizedTestModel(80)
	m.pending = false // run is over; the unresolved call is an orphan
	m.transcript = appendTranscriptRow(m.transcript, transcriptRow{
		kind: rowToolCall, id: "call_9", text: "tool call: bash", tool: "bash", runID: 2,
	})
	next, _ := m.settleTranscript()
	if next.flushed != len(next.transcript) {
		t.Fatalf("orphan call should settle once its run is over, frontier=%d", next.flushed)
	}
}

func TestClearResetsFlushFrontier(t *testing.T) {
	m := sizedTestModel(80)
	m.transcript = appendRow(m.transcript, rowUser, "first")
	m, _ = m.settleTranscript()
	if m.flushed == 0 {
		t.Fatal("precondition: something flushed")
	}

	m.input.SetValue("/clear")
	updated, _ := m.Update(testKey(tea.KeyEnter))
	next := updated.(model)
	if len(next.transcript) != 2 || next.transcript[0].kind != rowWelcome {
		t.Fatalf("expected /clear to reset transcript to welcome + note, got %#v", next.transcript)
	}
	if !transcriptContains(next.transcript, "/new") {
		t.Fatalf("expected /clear to point users to /new, got %#v", next.transcript)
	}
	if next.flushed != len(next.transcript) {
		t.Fatalf("expected frontier reset to match cleared transcript (%d rows), got %d", len(next.transcript), next.flushed)
	}
}

func TestEscCancellationLeavesVisibleMarker(t *testing.T) {
	m := sizedTestModel(80)
	m.pending = true
	m.activeRunID = 5
	m.runCancel = func() {}
	m.streamingText = []byte("half an answer")

	updated, _ := m.Update(testKey(tea.KeyEsc))
	next := updated.(model)
	updated, _ = next.Update(testKey(tea.KeyEsc))
	next = updated.(model)
	if !transcriptContains(next.transcript, "Run cancelled.") {
		t.Fatalf("expected visible cancellation marker, got %#v", next.transcript)
	}
	if !transcriptContains(next.transcript, "half an answer") {
		t.Fatalf("expected the partial streamed answer to be preserved, got %#v", next.transcript)
	}
	if len(next.streamingText) != 0 {
		t.Fatal("streaming text should be cleared after cancel")
	}
}

func TestRepeatedProviderToolIDsKeepDistinctRows(t *testing.T) {
	rows := initialTranscript()
	// Same provider id in two different runs (Gemini-style) must NOT dedupe.
	rows = appendTranscriptRow(rows, transcriptRow{kind: rowToolCall, id: "gemini_tool_0", runID: 1, tool: "grep"})
	rows = appendTranscriptRow(rows, transcriptRow{kind: rowToolCall, id: "gemini_tool_0", runID: 2, tool: "grep"})
	if countTranscriptRows(rows, rowToolCall) != 2 {
		t.Fatalf("run-scoped dedup should keep both calls, got %#v", rows)
	}
	// And within one run, the ordinal suffix disambiguates repeats.
	if got := effectiveToolRowID("gemini_tool_0", 2); got != "gemini_tool_0#2" {
		t.Fatalf("effectiveToolRowID = %q", got)
	}
	if got := effectiveToolRowID("gemini_tool_0", 1); got != "gemini_tool_0" {
		t.Fatalf("first occurrence should keep the raw id, got %q", got)
	}
}

func TestComposerHistoryRecall(t *testing.T) {
	m := sizedTestModel(80)
	for _, prompt := range []string{"first input", "second input"} {
		m.input.SetValue(prompt)
		updated, _ := m.Update(testKey(tea.KeyEnter))
		m = updated.(model)
	}

	updated, _ := m.Update(testKey(tea.KeyUp))
	m = updated.(model)
	if got := m.input.Value(); got != "second input" {
		t.Fatalf("first ↑ should recall the newest input, got %q", got)
	}
	updated, _ = m.Update(testKey(tea.KeyUp))
	m = updated.(model)
	if got := m.input.Value(); got != "first input" {
		t.Fatalf("second ↑ should recall the older input, got %q", got)
	}
	updated, _ = m.Update(testKey(tea.KeyDown))
	m = updated.(model)
	if got := m.input.Value(); got != "second input" {
		t.Fatalf("↓ should walk back toward the newest input, got %q", got)
	}
}

func TestCutRunesNeverSplitsUTF8(t *testing.T) {
	text := "héllo wörld — ünïcode"
	for limit := 0; limit <= len(text); limit++ {
		cut := cutRunes(text, limit)
		if !strings.HasPrefix(text, cut) {
			t.Fatalf("cutRunes(%d) = %q is not a prefix", limit, cut)
		}
		for _, r := range cut {
			if r == '�' {
				t.Fatalf("cutRunes(%d) produced invalid UTF-8: %q", limit, cut)
			}
		}
	}
}

func TestWrapPlainTextPreservesIndentation(t *testing.T) {
	lines := wrapPlainText("plain\n    indented code line", 40)
	found := false
	for _, line := range lines {
		if strings.HasPrefix(line, "    indented") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected indentation preserved, got %#v", lines)
	}
}

func TestLooksLikeDiffIgnoresPlainSeparators(t *testing.T) {
	if looksLikeDiff("build output\n--- summary ---\nall good") {
		t.Fatal("a '---' separator line must not trigger the diff renderer")
	}
	if !looksLikeDiff("--- a/f.go\n+++ b/f.go\n+new line") {
		t.Fatal("real unified diff headers must trigger the diff renderer")
	}
	if !looksLikeDiff("context\n@@ -1,2 +1,3 @@\n+x") {
		t.Fatal("a hunk header must trigger the diff renderer")
	}
}

func TestTruncateStyledLineClosesOpenHyperlink(t *testing.T) {
	line := hyperlink("file:///tmp/a.go", "averyveryverylongclickablepathsegment")
	cut := truncateStyledLine(line, 10)
	if !strings.Contains(cut, "\x1b]8;;\x1b\\") {
		t.Fatalf("truncated line must close its hyperlink, got %q", cut)
	}
}

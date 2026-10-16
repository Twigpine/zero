package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
)

func testOpenFile(m model, path string) model {
	next, cmd := m.openFileView(path)
	if cmd != nil {
		msg := cmd()
		updated, _ := next.Update(msg)
		return updated.(model)
	}
	return next
}

func testSetMode(m model, mode int) model {
	next, cmd := m.setFileViewMode(mode)
	if cmd != nil {
		msg := cmd()
		updated, _ := next.Update(msg)
		return updated.(model)
	}
	return next
}

// TestFileViewOpenExitRestoresScroll: opening saves the chat scroll position,
// resets it for the file body, and Esc restores it; switching files while open
// keeps the ORIGINAL saved position (not the file view's own).
func TestFileViewOpenExitRestoresScroll(t *testing.T) {
	m := filesPanelTestModel()
	m.chatScrollOffset = 12

	m, _ = m.openFileView("web/app.js")
	if !m.fileView.active || m.fileView.mode != fileViewDiff {
		t.Fatalf("open should activate in diff mode: %+v", m.fileView)
	}
	if m.chatScrollOffset != 0 || m.fileView.parentScrollOffset != 12 {
		t.Fatalf("open should reset scroll and save the parent offset: offset=%d saved=%d", m.chatScrollOffset, m.fileView.parentScrollOffset)
	}

	m.chatScrollOffset = 5 // scrolled within the file body
	m, _ = m.openFileView("internal/tui/sidebar.go")
	if m.fileView.parentScrollOffset != 12 {
		t.Fatalf("switching files must keep the original parent offset, got %d", m.fileView.parentScrollOffset)
	}

	m = m.exitFileView()
	if m.fileView.active || m.chatScrollOffset != 12 {
		t.Fatalf("exit should restore the chat scroll: active=%v offset=%d", m.fileView.active, m.chatScrollOffset)
	}
}

// TestFileViewEscAndModeKeys: Esc exits the view via the model's key handler;
// d/f switch modes while the composer is empty and never while typing.
func TestFileViewEscAndModeKeys(t *testing.T) {
	m := filesPanelTestModel()
	m, _ = m.openFileView("web/app.js")

	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	m = updated.(model)
	if cmd != nil {
		updated, _ = m.Update(cmd())
		m = updated.(model)
	}
	if m.fileView.mode != fileViewFull {
		t.Fatal("f should switch to full mode")
	}
	updated, cmd = m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	m = updated.(model)
	if cmd != nil {
		updated, _ = m.Update(cmd())
		m = updated.(model)
	}
	if m.fileView.mode != fileViewDiff {
		t.Fatal("d should switch back to diff mode")
	}

	// With text in the composer, d/f type as normal characters.
	m.input.SetValue("say")
	updated, _ = m.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	m = updated.(model)
	if m.fileView.mode != fileViewDiff {
		t.Fatal("f while typing must not hijack the composer")
	}
	m.input.SetValue("")

	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(model)
	if m.fileView.active {
		t.Fatal("Esc should exit the file view")
	}
}

// TestFileViewDiffBody: diff mode stacks the file's edit cards chronologically
// with "edit N of M" labels; a file with no recorded edits shows the quiet
// placeholder.
func TestFileViewDiffBody(t *testing.T) {
	m := filesPanelTestModel()
	m, _ = m.openFileView("internal/tui/sidebar.go")
	body := plainRender(t, m.renderFileViewDiff(78))
	if !strings.Contains(body, "edit 1 of 2") || !strings.Contains(body, "edit 2 of 2") {
		t.Fatalf("expected chronological edit labels:\n%s", body)
	}
	if !strings.Contains(body, "added one") || !strings.Contains(body, "three") {
		t.Errorf("expected both diffs' content:\n%s", body)
	}

	m.fileView.path = "never/touched.go"
	if got := plainRender(t, m.renderFileViewDiff(78)); !strings.Contains(got, "No recorded edits") {
		t.Errorf("untouched file should show the placeholder, got:\n%s", got)
	}
}

// TestFileViewFullBody: full mode shows the on-disk content with line numbers
// and marks session-added lines with the gutter marker; a missing file degrades
// to a readable error line.
func TestFileViewFullBody(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("let a = 1\nlet untouched = 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m.transcript = append(m.transcript, transcriptRow{
		kind: rowToolResult, tool: "write_file", id: "w9", status: tools.StatusOK,
		detail:       "+let a = 1",
		changedFiles: []string{"app.js"},
	})
	m = testOpenFile(m, "app.js")
	m = testSetMode(m, fileViewFull)

	body := m.renderFileViewFull(78)
	plain := plainRender(t, body)
	if !strings.Contains(plain, "1 ") || !strings.Contains(plain, "let untouched = 0") {
		t.Fatalf("full view should show numbered file content:\n%s", plain)
	}
	lines := strings.Split(plain, "\n")
	if len(lines) != 2 {
		t.Fatalf("one rendered line per file line, got %d:\n%s", len(lines), plain)
	}
	if !strings.Contains(lines[0], "▎") {
		t.Errorf("session-added line should carry the gutter marker: %q", lines[0])
	}
	if strings.Contains(lines[1], "▎") {
		t.Errorf("untouched line must not carry the marker: %q", lines[1])
	}

	m.fileView.path = "gone.js"
	m, cmd := m.startFileViewLoadCmd(78)
	if cmd != nil {
		updated, _ := m.Update(cmd())
		m = updated.(model)
	}
	if got := plainRender(t, m.renderFileViewFull(78)); !strings.Contains(got, "Could not read file") {
		t.Errorf("missing file should degrade to an error line, got:\n%s", got)
	}
}

// TestFileViewSwapsTranscriptBody: while active, transcriptBodyItems returns
// the file body (a single block) instead of the chat rows, and the pinned
// title bar swaps to the one-line nav bar — the geometry every frame consumer
// relies on.
func TestFileViewSwapsTranscriptBody(t *testing.T) {
	m := filesPanelTestModel()
	m, _ = m.openFileView("internal/tui/sidebar.go")

	items := m.transcriptBodyItems(m.chatColumnWidth(), "", false)
	if len(items) != 1 {
		t.Fatalf("file view should swap the body to a single block item, got %d items", len(items))
	}
	nav := plainRender(t, m.pinnedTitleBar(m.chatColumnWidth()))
	if !strings.Contains(nav, "sidebar.go") || !strings.Contains(nav, "esc back") {
		t.Fatalf("nav bar should show the path and key hints: %q", nav)
	}
	if lines := len(viewLines(m.fileViewNavBar(m.chatColumnWidth()))); lines != 1 {
		t.Fatalf("nav bar must be exactly one line (title-bar geometry), got %d", lines)
	}

	// The whole view renders without panicking in both modes and shows the nav.
	if view := plainRender(t, m.transcriptView()); !strings.Contains(view, "esc back") {
		t.Fatal("transcript view should carry the file nav bar")
	}
}

// TestSidebarAgentClickIsIgnoredWithoutRail verifies invisible legacy hit
// coordinates cannot unexpectedly replace the full-width conversation view.
func TestSidebarAgentClickIsIgnoredWithoutRail(t *testing.T) {
	store := sessions.NewStore(sessions.StoreOptions{RootDir: t.TempDir()})
	if _, err := store.Create(sessions.CreateInput{SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.sessionStore = store
	m.swarmSessionMap = map[string]string{"subagent-1": "sess-1"}
	m.transcript = append(m.transcript,
		transcriptRow{kind: rowToolCall, tool: "swarm_spawn", detail: "build it", runID: 1},
		transcriptRow{kind: rowToolResult, tool: "swarm_spawn", detail: "Spawned subagent as task subagent-1 on team default.", runID: 1},
	)
	m.activeRunID = 1
	m, _ = m.openFileView("web/app.js")

	width := sidebarWidth(m.width)
	agents := m.sidebarAgentSelectables(width)
	if len(agents) == 0 {
		t.Fatal("expected a clickable agent row")
	}
	click := testMouseClick(tea.MouseLeft, m.chatColumnWidth()+3, agents[0].lineOffset)
	updated, _, handled := m.handleTranscriptSelectionMouse(click)
	if handled {
		t.Fatal("invisible rail coordinates must not handle clicks")
	}
	if !updated.fileView.active {
		t.Fatal("an ignored rail click must leave the active file view alone")
	}
	if updated.subchat.active {
		t.Fatal("an ignored rail click must not enter a subchat")
	}
}

// TestChangedFilesRehydration: a persisted tool-result payload's changedFiles
// restores onto the rehydrated transcript row, so the FILES panel survives
// /resume.
func TestChangedFilesRehydration(t *testing.T) {
	events := []sessions.Event{{
		Type:    sessions.EventToolResult,
		Payload: json.RawMessage(`{"toolCallId":"t1","name":"edit_file","status":"ok","output":"+x","changedFiles":["pkg/a.go","pkg/b.go"],"changeSummaries":[{"path":"node_modules/","kind":"created","aggregated":true}]}`),
	}}
	rows := transcriptRowsFromSessionEvents(events)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	got := rows[0].changedFiles
	if len(got) != 2 || got[0] != "pkg/a.go" || got[1] != "pkg/b.go" {
		t.Fatalf("changedFiles not rehydrated: %v", got)
	}
	if summaries := rows[0].changeSummaries; len(summaries) != 1 || summaries[0].Path != "node_modules/" || !summaries[0].Aggregated {
		t.Fatalf("changeSummaries not rehydrated: %#v", summaries)
	}
}

// TestResumedFileEditUsesPersistedDisplayPreview keeps the reviewable diff a
// user saw while the run was live. The provider-facing output is intentionally
// a short confirmation, so it cannot substitute for the card-only preview.
func TestResumedFileEditUsesPersistedDisplayPreview(t *testing.T) {
	preview := "--- a/calculator.go\n+++ b/calculator.go\n@@ -4,1 +4,1 @@\n-oldValue := 1\n+newValue := 2"
	events := []sessions.Event{{
		Type:    sessions.EventToolResult,
		Payload: json.RawMessage(`{"toolCallId":"edit-1","name":"edit_file","status":"ok","output":"Successfully edited calculator.go (replaced 1 occurrence).","displayPreview":` + strconv.Quote(preview) + `}`),
	}}

	rows := transcriptRowsFromSessionEvents(events)
	if len(rows) != 1 {
		t.Fatalf("expected one restored row, got %d", len(rows))
	}
	if rows[0].detail != preview {
		t.Fatalf("resume should restore the saved diff preview, got %q", rows[0].detail)
	}
	card := renderToolResultCard(rows[0], 80, rowContext{}, cardRenderOptions{})
	if plain := plainRender(t, card); !strings.Contains(plain, "newValue := 2") {
		t.Fatalf("resumed edit should render its diff preview, got %q", plain)
	}
}

// TestOpenFileViewSamePathIsNoOp: re-clicking the FILES row of the file already
// being viewed must not clobber the user's mode or scroll — the old
// unconditional openFileView bounced full mode back to diff and reset scroll.
func TestOpenFileViewSamePathIsNoOp(t *testing.T) {
	m := filesPanelTestModel()
	m = testOpenFile(m, "web/app.js")
	m = testSetMode(m, fileViewFull)
	m.chatScrollOffset = 7 // scrolled within the file body

	m, _ = m.openFileView("web/app.js")
	if m.fileView.mode != fileViewFull {
		t.Fatal("re-opening the same file must keep full mode")
	}
	if m.chatScrollOffset != 7 {
		t.Fatalf("re-opening the same file must keep the scroll, got %d", m.chatScrollOffset)
	}
	// A DIFFERENT file still switches (and resets to diff mode as documented).
	m, _ = m.openFileView("internal/tui/sidebar.go")
	if m.fileView.path != "internal/tui/sidebar.go" || m.fileView.mode != fileViewDiff {
		t.Fatalf("opening another file should switch views: %+v", m.fileView)
	}
}

// TestFileViewFullBodyTruncatesLongFile: the full view stops reading at
// fileViewMaxLines (bounded read — a giant file must not be loaded wholesale)
// and appends the truncation trailer.
func TestFileViewFullBodyTruncatesLongFile(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < fileViewMaxLines+50; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m.gitTouched = []gitSweepFile{{path: "big.txt"}}
	m = testOpenFile(m, "big.txt")

	plain := plainRender(t, m.renderFileViewFull(80))
	lines := strings.Split(plain, "\n")
	if len(lines) != fileViewMaxLines+1 { // capped content + the trailer line
		t.Fatalf("rendered %d lines, want %d content lines + 1 trailer", len(lines), fileViewMaxLines)
	}
	if !strings.Contains(lines[len(lines)-1], "truncated") {
		t.Fatalf("expected the truncation trailer, got %q", lines[len(lines)-1])
	}
}

// TestDetailedTranscriptClosesFileView: entering detailed transcript mode
// closes an active file drill-in so the full transcript body replaces the file
// content.
func TestDetailedTranscriptClosesFileView(t *testing.T) {
	m := filesPanelTestModel()
	m.altScreen = true
	m, _ = m.openFileView("web/app.js")

	if !m.fileView.active {
		t.Fatal("sanity check: openFileView should activate the file view")
	}

	updated, _ := m.Update(testKeyCtrl('o'))
	m = updated.(model)

	if m.fileView.active {
		t.Fatal("detailed transcript should close the file drill-in")
	}
	if !m.transcriptDetailed {
		t.Fatal("Ctrl+O should enter detailed mode")
	}

	items := m.transcriptBodyItems(m.chatColumnWidth(), "", true)
	if len(items) <= 1 {
		t.Fatalf("detailed transcript should show multiple body items, got %d", len(items))
	}
}

// TestDetailedTranscriptStaysClosedOnSecondToggle: toggling out and back into
// detailed mode does not re-open the closed file view.
func TestDetailedTranscriptStaysClosedOnSecondToggle(t *testing.T) {
	m := filesPanelTestModel()
	m.altScreen = true
	m, _ = m.openFileView("web/app.js")

	updated, _ := m.Update(testKeyCtrl('o'))
	m = updated.(model)

	updated, _ = m.Update(testKeyCtrl('o'))
	m = updated.(model)

	if m.fileView.active {
		t.Fatal("exiting detailed mode must not re-open the file drill-in")
	}
	if m.transcriptDetailed {
		t.Fatal("second Ctrl+O should exit detailed mode")
	}
}

// TestFileViewKeysDeferToBlockingModal: with a permission prompt up, Esc and
// the d/f shortcuts belong to the prompt — the drill-in must not swallow them
// (Esc exiting the view instead of reaching the prompt's deny handling).
func TestFileViewKeysDeferToBlockingModal(t *testing.T) {
	m := filesPanelTestModel()
	m, _ = m.openFileView("web/app.js")
	m.pendingPermission = &pendingPermissionPrompt{
		request: agent.PermissionRequest{ToolName: "write_file"},
		decide:  func(agent.PermissionDecision) {},
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	m = updated.(model)
	if m.fileView.mode != fileViewDiff {
		t.Fatal("f with a permission prompt up must not switch file-view modes")
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = updated.(model)
	if !m.fileView.active {
		t.Fatal("Esc with a permission prompt up must not exit the file view")
	}
}

// TestFileViewRepeatedViewNoDiskIOOrHighlighting proves that repeated calls
// to render the full file view do not perform disk I/O or Chroma syntax
// highlighting after the initial load, and that modifying the file on disk
// properly invalidates and triggers a reload.
func TestFileViewRepeatedViewNoDiskIOOrHighlighting(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "sample.go")
	content := "package main\n\nfunc main() {\n\tprintln(\"hello world\")\n}\n"
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir
	m = testOpenFile(m, "sample.go")
	m = testSetMode(m, fileViewFull)

	// First render: Misses cache during testSetMode cmd execution, performed 1 disk read and 1 highlight call
	firstRender := m.renderFileViewFull(80)
	if !strings.Contains(firstRender, "hello world") {
		t.Fatalf("first render missing content: %s", firstRender)
	}

	statsAfterFirst := fileViewCacheStatsForTest()
	if statsAfterFirst.DiskReads != 1 {
		t.Fatalf("expected 1 disk read on initial view, got %d", statsAfterFirst.DiskReads)
	}
	if statsAfterFirst.HighlightCalls != 1 {
		t.Fatalf("expected 1 highlight call on initial view, got %d", statsAfterFirst.HighlightCalls)
	}

	// Repeated renders (e.g. 10 frames during typing/scrolling/resize)
	for i := 0; i < 10; i++ {
		rendered := m.renderFileViewFull(80)
		if rendered != firstRender {
			t.Fatalf("subsequent render %d mismatch", i)
		}
	}

	statsAfterRepeated := fileViewCacheStatsForTest()
	if statsAfterRepeated.DiskReads != 1 {
		t.Fatalf("repeated View calls must not trigger disk reads, got %d", statsAfterRepeated.DiskReads)
	}
	if statsAfterRepeated.HighlightCalls != 1 {
		t.Fatalf("repeated View calls must not trigger Chroma highlighting, got %d", statsAfterRepeated.HighlightCalls)
	}
	if statsAfterRepeated.CacheHits != statsAfterFirst.CacheHits+10 {
		t.Fatalf("expected 10 additional cache hits, got %d (before: %d)", statsAfterRepeated.CacheHits, statsAfterFirst.CacheHits)
	}

	// Same byte length as `content`, so only mtime can invalidate the entry.
	newContent := "package main\n\nfunc main() {\n\tprintln(\"HELLO WORLD\")\n}\n"
	if len(newContent) != len(content) {
		t.Fatalf("test setup: newContent length %d must match original length %d", len(newContent), len(content))
	}
	if err := os.WriteFile(filePath, []byte(newContent), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filePath, future, future); err != nil {
		t.Fatal(err)
	}

	// Re-trigger load command after file update
	m, cmd := m.startFileViewLoadCmd(80)
	if cmd != nil {
		updated, _ := m.Update(cmd())
		m = updated.(model)
	}

	updatedRender := m.renderFileViewFull(80)
	if !strings.Contains(updatedRender, "HELLO WORLD") {
		t.Fatalf("expected updated content after disk mutation, got: %s", updatedRender)
	}

	statsAfterUpdate := fileViewCacheStatsForTest()
	if statsAfterUpdate.DiskReads != 2 {
		t.Fatalf("expected 2 disk reads after file change, got %d", statsAfterUpdate.DiskReads)
	}
	if statsAfterUpdate.HighlightCalls != 2 {
		t.Fatalf("expected 2 highlight calls after file change, got %d", statsAfterUpdate.HighlightCalls)
	}
}

// TestFileViewMaxBytesBudgetTruncation verifies that files exceeding the
// total byte budget (fileViewMaxBytes) are truncated and display the trailer.
func TestFileViewMaxBytesBudgetTruncation(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "giant_bytes.txt")

	// Generate a file with total size ~1.5 MB (> fileViewMaxBytes of 1 MiB)
	line := strings.Repeat("a", 500) + "\n"
	numLines := (fileViewMaxBytes / 500) + 100
	var sb strings.Builder
	for i := 0; i < numLines; i++ {
		sb.WriteString(line)
	}
	if err := os.WriteFile(filePath, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir
	m = testOpenFile(m, "giant_bytes.txt")
	m = testSetMode(m, fileViewFull)

	body := m.renderFileViewFull(80)
	plain := plainRender(t, body)

	if !strings.Contains(plain, "truncated") {
		t.Fatalf("expected truncation trailer for file exceeding byte budget, got:\n%s", plain)
	}

	renderedLines := strings.Split(plain, "\n")
	if len(renderedLines) >= numLines {
		t.Fatalf("rendered line count %d should be strictly less than total file lines %d", len(renderedLines), numLines)
	}
}

// TestFileViewMaxLineBytesBudgetTruncation verifies that single overlong lines
// exceeding fileViewMaxLineBytes are clamped without crashing or unbounded memory.
func TestFileViewMaxLineBytesBudgetTruncation(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "giant_line.js")

	// Generate a single overlong line of 50,000 bytes (> fileViewMaxLineBytes of 4096)
	giantLine := "let data = \"" + strings.Repeat("x", 50000) + "\";\nlet next = 1;\n"
	if err := os.WriteFile(filePath, []byte(giantLine), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir
	m = testOpenFile(m, "giant_line.js")
	m = testSetMode(m, fileViewFull)

	body := m.renderFileViewFull(80)
	plain := plainRender(t, body)

	if !strings.Contains(plain, "let next = 1") {
		t.Fatalf("expected next line to be readable after overlong line truncation, got:\n%s", plain)
	}
	if !strings.Contains(plain, "truncated") {
		t.Fatalf("expected truncation trailer for overlong line, got:\n%s", plain)
	}
}

// TestFileViewCacheEviction verifies that the LRU cache caps entry count to
// defaultFileViewCacheMaxEntries.
func TestFileViewCacheEviction(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	numFiles := defaultFileViewCacheMaxEntries + 10
	for i := 0; i < numFiles; i++ {
		fname := fmt.Sprintf("file_%d.txt", i)
		if err := os.WriteFile(filepath.Join(dir, fname), []byte(fmt.Sprintf("content %d\n", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	m := filesPanelTestModel()
	m.cwd = dir
	for i := 0; i < numFiles; i++ {
		fname := fmt.Sprintf("file_%d.txt", i)
		m = testOpenFile(m, fname)
		m = testSetMode(m, fileViewFull)
		_ = m.renderFileViewFull(80)
	}

	defaultFileViewCache.mu.Lock()
	cachedCount := len(defaultFileViewCache.items)
	defaultFileViewCache.mu.Unlock()

	if cachedCount > defaultFileViewCacheMaxEntries {
		t.Fatalf("cache size %d exceeded maxEntries %d", cachedCount, defaultFileViewCacheMaxEntries)
	}
}

// TestFileViewClearOnThemeChange verifies that switching themes clears the
// file view cache so updated palette styles are applied.
func TestFileViewClearOnThemeChange(t *testing.T) {
	defer applyTheme(themeDark, true)
	resetFileViewCacheForTest()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "code.go")
	if err := os.WriteFile(filePath, []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir
	m = testOpenFile(m, "code.go")
	m = testSetMode(m, fileViewFull)
	_ = m.renderFileViewFull(80)

	defaultFileViewCache.mu.Lock()
	entriesBefore := len(defaultFileViewCache.items)
	defaultFileViewCache.mu.Unlock()

	if entriesBefore == 0 {
		t.Fatal("expected cached entries before theme switch")
	}

	applyTheme(themeLight, false)

	defaultFileViewCache.mu.Lock()
	entriesAfter := len(defaultFileViewCache.items)
	defaultFileViewCache.mu.Unlock()

	if entriesAfter != 0 {
		t.Fatalf("expected cache to be cleared after theme change, got %d entries", entriesAfter)
	}
}

// TestReadFileViewBounded_GiantSingleLineStopsAtBudget verifies that reading a
// multi-megabyte physical line without newlines stops immediately at maxTotalBytes
// rather than reading through to EOF or loading the entire line into memory.
func TestReadFileViewBounded_GiantSingleLineStopsAtBudget(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "giant_single_line.txt")

	// 5 MiB single line without any newlines
	totalSize := 5 * 1024 * 1024
	giantContent := strings.Repeat("A", totalSize)
	if err := os.WriteFile(filePath, []byte(giantContent), 0o644); err != nil {
		t.Fatal(err)
	}

	maxTotalBytes := 1 << 20 // 1 MiB budget
	maxLineBytes := 4096     // 4 KiB line cap
	maxLines := 4000

	res := readFileViewBounded(filePath, maxLines, maxLineBytes, maxTotalBytes)
	if res.err != nil {
		t.Fatalf("unexpected read error: %v", res.err)
	}

	if !res.truncated {
		t.Fatal("expected truncated=true when reading 5 MiB single line with 1 MiB budget")
	}

	if len(res.lines) != 1 {
		t.Fatalf("expected exactly 1 truncated line, got %d", len(res.lines))
	}

	if len(res.lines[0]) > maxLineBytes {
		t.Fatalf("retained line length %d exceeded per-line cap %d", len(res.lines[0]), maxLineBytes)
	}
}

// TestReadFileViewBounded_ExactMaxBytesNotTruncated verifies that a file exactly
// equal in size to maxTotalBytes without extra unread bytes is read completely
// without an erroneous truncation flag or trailer.
func TestReadFileViewBounded_ExactMaxBytesNotTruncated(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "exact_budget.txt")

	maxTotalBytes := 1024 * 1024 // 1 MiB
	maxLineBytes := 4096
	maxLines := 4000

	// Construct exactly 1024 * 1024 bytes with 512 lines of 2048 bytes (2047 chars + '\n')
	lineLen := 2048
	numLines := maxTotalBytes / lineLen
	remainder := maxTotalBytes % lineLen

	var sb strings.Builder
	for i := 0; i < numLines; i++ {
		sb.WriteString(strings.Repeat("B", lineLen-1) + "\n")
	}
	if remainder > 0 {
		sb.WriteString(strings.Repeat("C", remainder))
	}

	content := []byte(sb.String())
	if len(content) != maxTotalBytes {
		t.Fatalf("generated content size %d != %d", len(content), maxTotalBytes)
	}

	if err := os.WriteFile(filePath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	res := readFileViewBounded(filePath, maxLines, maxLineBytes, maxTotalBytes)
	if res.err != nil {
		t.Fatalf("unexpected read error: %v", res.err)
	}

	if res.truncated {
		t.Fatal("expected truncated=false for file exactly matching maxTotalBytes with no trailing data")
	}

	totalReadLen := 0
	for _, l := range res.lines {
		totalReadLen += len(l)
	}
	if totalReadLen == 0 {
		t.Fatal("expected lines to be populated")
	}
}

// TestReadFileViewBounded_ExactMaxBytesUnterminatedLineTruncated verifies that a single
// unterminated physical line of exactly maxTotalBytes is correctly marked truncated=true
// when the line length exceeds maxLineBytes.
func TestReadFileViewBounded_ExactMaxBytesUnterminatedLineTruncated(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "exact_single_line_unterminated.txt")

	maxTotalBytes := 1024 * 1024 // 1 MiB
	maxLineBytes := 4096
	maxLines := 4000

	// 1 MiB continuous single line without any newlines
	content := []byte(strings.Repeat("X", maxTotalBytes))
	if err := os.WriteFile(filePath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	res := readFileViewBounded(filePath, maxLines, maxLineBytes, maxTotalBytes)
	if res.err != nil {
		t.Fatalf("unexpected read error: %v", res.err)
	}

	if !res.truncated {
		t.Fatal("expected truncated=true for 1 MiB single unterminated line clipped at maxLineBytes")
	}

	if len(res.lines) != 1 {
		t.Fatalf("expected exactly 1 line, got %d", len(res.lines))
	}
	if len(res.lines[0]) > maxLineBytes {
		t.Fatalf("retained line length %d exceeded maxLineBytes %d", len(res.lines[0]), maxLineBytes)
	}
}

func TestReadFileViewBounded_SourceByteBudgetCountsDelimiters(t *testing.T) {
	tests := []struct {
		name          string
		content       string
		maxLines      int
		maxLineBytes  int
		maxTotalBytes int
		wantLines     []string
		wantTrunc     bool
		wantOmitted   bool
		wantTrailer   string
	}{
		{
			name:          "empty file",
			content:       "",
			maxLines:      4000,
			maxLineBytes:  4096,
			maxTotalBytes: 1,
			wantLines:     nil,
		},
		{
			name:          "budget 1 two LF empty lines",
			content:       "\n\n",
			maxLines:      4000,
			maxLineBytes:  4096,
			maxTotalBytes: 1,
			wantLines:     []string{""},
			wantTrunc:     true,
			wantOmitted:   true,
			wantTrailer:   "more lines",
		},
		{
			name:          "single LF exact budget",
			content:       "\n",
			maxLines:      4000,
			maxLineBytes:  4096,
			maxTotalBytes: 1,
			wantLines:     []string{""},
		},
		{
			name:          "LF content line exact budget",
			content:       "ab\n",
			maxLines:      4000,
			maxLineBytes:  4096,
			maxTotalBytes: 3,
			wantLines:     []string{"ab"},
		},
		{
			name:          "LF second line one byte over",
			content:       "ab\ncd\n",
			maxLines:      4000,
			maxLineBytes:  4096,
			maxTotalBytes: 3,
			wantLines:     []string{"ab"},
			wantTrunc:     true,
			wantOmitted:   true,
			wantTrailer:   "more lines",
		},
		{
			name:          "CRLF two empty lines budget 2",
			content:       "\r\n\r\n",
			maxLines:      4000,
			maxLineBytes:  4096,
			maxTotalBytes: 2,
			wantLines:     []string{""},
			wantTrunc:     true,
			wantOmitted:   true,
			wantTrailer:   "more lines",
		},
		{
			name:          "CRLF exact budget",
			content:       "ab\r\n",
			maxLines:      4000,
			maxLineBytes:  4096,
			maxTotalBytes: 4,
			wantLines:     []string{"ab"},
		},
		{
			name:          "CRLF one byte over",
			content:       "ab\r\ncd",
			maxLines:      4000,
			maxLineBytes:  4096,
			maxTotalBytes: 3,
			wantLines:     []string{"ab"},
			wantTrunc:     true,
			wantOmitted:   true,
			wantTrailer:   "more lines",
		},
		{
			name:          "exact budget unterminated last line",
			content:       "abc",
			maxLines:      4000,
			maxLineBytes:  4096,
			maxTotalBytes: 3,
			wantLines:     []string{"abc"},
		},
		{
			name:          "unterminated last line one byte over",
			content:       "abcd",
			maxLines:      4000,
			maxLineBytes:  4096,
			maxTotalBytes: 3,
			wantLines:     []string{"abc"},
			wantTrunc:     true,
			wantOmitted:   true,
			wantTrailer:   "more lines",
		},
		{
			name:          "overlong physical line clipped then next line shown",
			content:       strings.Repeat("x", 20) + "\nnext\n",
			maxLines:      4000,
			maxLineBytes:  8,
			maxTotalBytes: 100,
			wantLines:     []string{strings.Repeat("x", 8), "next"},
			wantTrunc:     true,
			wantTrailer:   "line content truncated",
		},
		{
			name:          "overlong physical line newline counts against byte budget",
			content:       strings.Repeat("x", 10) + "\ny\n",
			maxLines:      4000,
			maxLineBytes:  4,
			maxTotalBytes: 11,
			wantLines:     []string{strings.Repeat("x", 4)},
			wantTrunc:     true,
			wantOmitted:   true,
			wantTrailer:   "more lines",
		},
		{
			name:          "line count cap with leftover",
			content:       "a\nb\nc\nd\n",
			maxLines:      2,
			maxLineBytes:  4096,
			maxTotalBytes: 100,
			wantLines:     []string{"a", "b"},
			wantTrunc:     true,
			wantOmitted:   true,
			wantTrailer:   "more lines",
		},
		{
			name:          "line count cap exact file",
			content:       "a\nb\n",
			maxLines:      2,
			maxLineBytes:  4096,
			maxTotalBytes: 100,
			wantLines:     []string{"a", "b"},
		},
		{
			name:          "byte budget binds before line count cap on empty LF lines",
			content:       "\n\n\n\n",
			maxLines:      10,
			maxLineBytes:  4096,
			maxTotalBytes: 2,
			wantLines:     []string{"", ""},
			wantTrunc:     true,
			wantOmitted:   true,
			wantTrailer:   "more lines",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input.txt")
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}

			res := readFileViewBounded(path, tc.maxLines, tc.maxLineBytes, tc.maxTotalBytes)
			if res.err != nil {
				t.Fatalf("unexpected read error: %v", res.err)
			}
			if res.truncated != tc.wantTrunc {
				t.Fatalf("truncated=%v, want %v (lines=%q)", res.truncated, tc.wantTrunc, res.lines)
			}
			if res.omittedLines != tc.wantOmitted {
				t.Fatalf("omittedLines=%v, want %v (lines=%q)", res.omittedLines, tc.wantOmitted, res.lines)
			}
			if len(res.lines) != len(tc.wantLines) {
				t.Fatalf("retained %d lines %q, want %d %q", len(res.lines), res.lines, len(tc.wantLines), tc.wantLines)
			}
			for i := range tc.wantLines {
				if res.lines[i] != tc.wantLines[i] {
					t.Fatalf("line %d = %q, want %q", i, res.lines[i], tc.wantLines[i])
				}
			}

			plain := plainRender(t, formatFileViewLines(res.lines, res.lines, nil, res.truncated, res.omittedLines, 80, zeroTheme))
			if tc.wantTrailer == "" {
				if strings.Contains(plain, "truncated") {
					t.Fatalf("expected no trailer, got %q", plain)
				}
			} else if !strings.Contains(plain, tc.wantTrailer) {
				t.Fatalf("expected trailer %q in %q", tc.wantTrailer, plain)
			}
		})
	}
}

// TestFileViewCache_RenderVariantsBoundedUnderResize verifies that varying width
// and changed-line fingerprints cannot grow an entry's renders map beyond
// fileViewMaxRenderVariants, even under concurrent access from multiple goroutines.
func TestFileViewCache_RenderVariantsBoundedUnderResize(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "resize_test.go")
	if err := os.WriteFile(filePath, []byte("package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir
	m = testOpenFile(m, "resize_test.go")
	m = testSetMode(m, fileViewFull)

	// Execute mixed-width getOrRender calls concurrently from multiple goroutines
	var wg sync.WaitGroup
	workers := 8
	callsPerWorker := 20

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < callsPerWorker; i++ {
				width := 40 + ((workerID*callsPerWorker + i) % 50)
				changed := map[string]bool{}
				if width%2 == 0 {
					changed[fmt.Sprintf("marker_%d_%d", workerID, width)] = true
				}
				_ = defaultFileViewCache.getOrRender(filePath, "resize_test.go", width, changed)
			}
		}(w)
	}
	wg.Wait()

	defaultFileViewCache.mu.Lock()
	elem, ok := defaultFileViewCache.items[filePath]
	defaultFileViewCache.mu.Unlock()

	if !ok || elem == nil {
		t.Fatal("expected cached entry for file")
	}

	entry := elem.Value.(*fileViewCachedEntry)
	entry.rendersMu.RLock()
	variantCount := len(entry.renders)
	keyCount := len(entry.renderKeys)
	entry.rendersMu.RUnlock()

	if variantCount > fileViewMaxRenderVariants {
		t.Fatalf("variant count %d exceeded maximum limit %d", variantCount, fileViewMaxRenderVariants)
	}
	if keyCount > fileViewMaxRenderVariants {
		t.Fatalf("renderKeys count %d exceeded maximum limit %d", keyCount, fileViewMaxRenderVariants)
	}
}

// TestFileViewAsyncCacheMissLifecycle exercises the cache-miss lifecycle through
// the actual View/Update boundary:
//  1. Initial full-mode activation returns a command while View() renders the
//     loading placeholder without performing disk I/O or Chroma work.
//  2. The command executes asynchronously and returns fileViewLoadedMsg.
//  3. Update() applies the message to the model.
//  4. View() renders the loaded, formatted content.
func TestFileViewAsyncCacheMissLifecycle(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "async_sample.go")
	content := "package main\n\nfunc AsyncWork() string {\n\treturn \"done\"\n}\n"
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir

	// Step 1: Open file with no edit rows (opens in full mode directly with async cmd)
	m, cmd := m.openFileView("async_sample.go")
	if cmd == nil {
		t.Fatal("expected async load command on cache miss")
	}

	// View before cmd completes must render loading placeholder with 0 disk I/O or highlighting
	initialView := m.renderFileViewFull(80)
	if !strings.Contains(initialView, "Loading…") {
		t.Fatalf("expected loading placeholder before command completes, got:\n%s", initialView)
	}
	statsBefore := fileViewCacheStatsForTest()
	if statsBefore.DiskReads != 0 || statsBefore.HighlightCalls != 0 {
		t.Fatalf("View() must not stat/read/highlight directly: %+v", statsBefore)
	}

	// Step 2: Execute command asynchronously
	msg := cmd()
	loadedMsg, ok := msg.(fileViewLoadedMsg)
	if !ok {
		t.Fatalf("expected fileViewLoadedMsg, got %T", msg)
	}
	if loadedMsg.err != nil {
		t.Fatalf("unexpected load error: %v", loadedMsg.err)
	}

	// Step 3: Update model with loaded message
	updated, _ := m.Update(loadedMsg)
	m = updated.(model)

	// Step 4: View now renders the loaded content
	loadedView := m.renderFileViewFull(80)
	if !strings.Contains(loadedView, "AsyncWork") {
		t.Fatalf("expected loaded content in view, got:\n%s", loadedView)
	}
	statsAfter := fileViewCacheStatsForTest()
	if statsAfter.DiskReads != 1 || statsAfter.HighlightCalls != 1 {
		t.Fatalf("expected exactly 1 disk read and 1 highlight call, got: %+v", statsAfter)
	}

	// Also verify switching from diff mode to full mode triggers the cmd
	appFile := filepath.Join(dir, "web", "app.js")
	if err := os.MkdirAll(filepath.Join(dir, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(appFile, []byte("let webApp = true;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mDiff, cmdDiff := m.openFileView("web/app.js")
	if cmdDiff != nil || mDiff.fileView.mode != fileViewDiff {
		t.Fatalf("file with edit cards must open in diff mode with nil cmd: mode=%d, cmd=%v", mDiff.fileView.mode, cmdDiff)
	}
	mFull, cmdFull := mDiff.setFileViewMode(fileViewFull)
	if cmdFull == nil || mFull.fileView.mode != fileViewFull {
		t.Fatal("switching to full mode must return load command")
	}
	updated, _ = mFull.Update(cmdFull())
	mFull = updated.(model)
	if !strings.Contains(mFull.renderFileViewFull(80), "webApp") {
		t.Fatalf("expected loaded webApp content, got: %s", mFull.renderFileViewFull(80))
	}
}

// TestFileViewAsyncDiscardSupersededResult verifies that if a user switches files
// or exits the view while an async load is in flight, the completed message from
// the old file is safely discarded and does not overwrite the active view.
func TestFileViewAsyncDiscardSupersededResult(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	fileA := filepath.Join(dir, "fileA.txt")
	fileB := filepath.Join(dir, "fileB.txt")
	if err := os.WriteFile(fileA, []byte("Content of File A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileB, []byte("Content of File B\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir

	// Start loading File A
	m, cmdA := m.openFileView("fileA.txt")
	if cmdA == nil {
		t.Fatal("expected cmd for fileA")
	}

	// User switches to File B before cmdA returns
	m, cmdB := m.openFileView("fileB.txt")
	if cmdB == nil {
		t.Fatal("expected cmd for fileB")
	}

	// Now cmdA completes and its message is dispatched
	msgA := cmdA()
	updated, _ := m.Update(msgA)
	m = updated.(model)

	// File A's result must be discarded because active file is File B
	viewWhileB := m.renderFileViewFull(80)
	if strings.Contains(viewWhileB, "Content of File A") {
		t.Fatalf("stale File A result must not paint over File B: %s", viewWhileB)
	}

	// Now cmdB completes and is dispatched
	msgB := cmdB()
	updated, _ = m.Update(msgB)
	m = updated.(model)

	viewFinal := m.renderFileViewFull(80)
	if !strings.Contains(viewFinal, "Content of File B") {
		t.Fatalf("expected File B content, got: %s", viewFinal)
	}
}

// TestFileViewAsyncDiscardOnModeSwitchOrExit verifies that if a view exits or
// switches back to diff mode, completed async loads are safely ignored.
func TestFileViewAsyncDiscardOnModeSwitchOrExit(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	fileA := filepath.Join(dir, "discard_mode.txt")
	if err := os.WriteFile(fileA, []byte("Some content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir

	m, cmdA := m.openFileView("discard_mode.txt")
	if cmdA == nil {
		t.Fatal("expected cmd")
	}

	// Exit file view before command returns
	m = m.exitFileView()
	if m.fileView.active {
		t.Fatal("view should be inactive")
	}

	// Now deliver the message
	msgA := cmdA()
	updated, _ := m.Update(msgA)
	m = updated.(model)

	if m.fileView.active {
		t.Fatal("discarded message must not re-activate file view")
	}
}

// TestFileViewAsyncDiscardOnThemeInvalidation verifies that if a theme switch occurs
// while an async load is in flight, the old theme's completion message is discarded
// and a fresh load command for the new theme generation is triggered.
func TestFileViewAsyncDiscardOnThemeInvalidation(t *testing.T) {
	defer applyTheme(themeDark, true)
	resetFileViewCacheForTest()

	dir := t.TempDir()
	fileA := filepath.Join(dir, "theme_test.go")
	if err := os.WriteFile(fileA, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir

	m, cmd := m.openFileView("theme_test.go")
	if cmd == nil {
		t.Fatal("expected cmd")
	}

	// Invalidate cache by switching theme before cmd completes
	applyTheme(themeLight, false)

	// Now old cmd completes with stale generation
	msg := cmd()
	updated, retryCmd := m.Update(msg)
	m = updated.(model)

	// The message must have been rejected due to generation mismatch
	if m.fileView.renderedContent != "" {
		t.Fatalf("expected empty renderedContent after invalidation, got %q", m.fileView.renderedContent)
	}
	if retryCmd == nil {
		t.Fatal("expected retry command for new generation after invalidation")
	}

	// Executing the retry command loads the file under the new generation
	retryMsg := retryCmd()
	updated, _ = m.Update(retryMsg)
	m = updated.(model)

	if !strings.Contains(m.renderFileViewFull(80), "package") {
		t.Fatalf("expected file content loaded after retry, got: %s", m.renderFileViewFull(80))
	}
	if m.fileView.loadedGen != defaultFileViewCache.generation() {
		t.Fatalf("loadedGen %d != cache generation %d", m.fileView.loadedGen, defaultFileViewCache.generation())
	}
}

// TestFileViewThemeSwitchWhileLoaded verifies that switching themes invalidates
// the loaded generation and allows immediate reloading for the new palette.
func TestFileViewThemeSwitchWhileLoaded(t *testing.T) {
	defer applyTheme(themeDark, true)
	resetFileViewCacheForTest()

	dir := t.TempDir()
	fileA := filepath.Join(dir, "switch_test.go")
	if err := os.WriteFile(fileA, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir

	m = testOpenFile(m, "switch_test.go")
	if !strings.Contains(m.renderFileViewFull(80), "package") {
		t.Fatal("file should be loaded initially")
	}

	// Switch theme: generation advances, cache is cleared
	applyTheme(themeLight, false)

	// renderFileViewFull must not return the stale dark-theme content
	staleCheck := m.renderFileViewFull(80)
	if strings.Contains(staleCheck, "package") {
		t.Fatalf("stale renderedContent must not be rendered after generation increment: %s", staleCheck)
	}

	// Starting a new load command re-populates for the new theme
	m, reloadCmd := m.startFileViewLoadCmd(80)
	if reloadCmd == nil {
		t.Fatal("expected reload command")
	}
	updated, _ := m.Update(reloadCmd())
	m = updated.(model)

	if !strings.Contains(m.renderFileViewFull(80), "package") {
		t.Fatalf("expected reloaded content for new theme, got: %s", m.renderFileViewFull(80))
	}
}

// TestFileViewLifecycle_OpenToLoad tests the full model update flow from open to async load completion.
func TestFileViewLifecycle_OpenToLoad(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "app.go")
	if err := os.WriteFile(filePath, []byte("package app\nfunc Run() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir

	// Open file via model action
	m, cmd := m.openFileView("app.go")
	if !m.fileView.active || m.fileView.mode != fileViewFull {
		t.Fatal("file view should be active in full mode for new file")
	}
	if cmd == nil {
		t.Fatal("expected async load command on open")
	}

	// View displays loading placeholder before completion
	if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), fileViewLoadingPlaceholder) {
		t.Fatalf("expected loading placeholder, got: %s", plainRender(t, m.renderFileViewFull(80)))
	}

	// Process load completion
	updated, _ := m.Update(cmd())
	m = updated.(model)

	rendered := plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(rendered, "package app") || !strings.Contains(rendered, "func Run()") {
		t.Fatalf("expected loaded content, got: %s", rendered)
	}
	if m.fileView.hasError {
		t.Fatal("expected no error")
	}
}

// TestFileViewLifecycle_RapidResizeCoalesced verifies that repeated resize events
// do not cause race conditions or synchronous render spikes, and the latest resize wins.
func TestFileViewLifecycle_RapidResizeCoalesced(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "resize.go")
	if err := os.WriteFile(filePath, []byte("package resize\nconst BigWidth = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir
	m = testOpenFile(m, "resize.go")

	var cmds []tea.Cmd
	for w := 40; w <= 120; w += 10 {
		var cmd tea.Cmd
		m, cmd = m.startFileViewLoadCmd(w)
		if cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	// Deliver the latest resize command completion
	lastCmd := cmds[len(cmds)-1]
	updated, _ := m.Update(lastCmd())
	m = updated.(model)

	if m.fileView.loadedWidth != 120 {
		t.Fatalf("expected loadedWidth 120, got %d", m.fileView.loadedWidth)
	}
	if !strings.Contains(plainRender(t, m.renderFileViewFull(120)), "package resize") {
		t.Fatalf("expected content for width 120, got: %s", plainRender(t, m.renderFileViewFull(120)))
	}
}

// TestFileViewLifecycle_ThemeSwitchReloadsActiveView tests that selecting a
// registered theme in production immediately triggers a reload for the active
// file view, and that the returned command structure actually delivers the
// reloaded snapshot under the new cache generation.
func TestFileViewLifecycle_ThemeSwitchReloadsActiveView(t *testing.T) {
	defer applyTheme(themeDark, true)
	resetFileViewCacheForTest()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "theme_active.go")
	if err := os.WriteFile(filePath, []byte("package theme\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir
	m = testOpenFile(m, "theme_active.go")

	generationBefore := defaultFileViewCache.generation()

	// "dune" is a registered palette (unlike the retired "light" preference), so
	// the dispatch clears the theme's palette and advances the cache generation.
	cmdAction := parsedCommand{kind: commandTheme, text: "dune"}
	updated, dispatchCmd := m.dispatchCommand(cmdAction)
	m = updated.(model)

	if dispatchCmd == nil {
		t.Fatal("expected a command on the active file view after a valid theme change")
	}
	if generationAfter := defaultFileViewCache.generation(); generationAfter <= generationBefore {
		t.Fatalf("valid theme switch must advance the cache generation: before=%d after=%d", generationBefore, generationAfter)
	}

	// dispatchCommand returns tea.Batch(noticeCmd, loadCmd); unwrap the batch so
	// the load completion is delivered to Update instead of being dropped.
	m = deliverCommandMessages(t, m, dispatchCmd)

	if m.fileView.loading {
		t.Fatal("active file view must leave the loading state after the reload completes")
	}
	if got := plainRender(t, m.renderFileViewFull(80)); !strings.Contains(got, "package theme") {
		t.Fatalf("expected reloaded theme content, got: %s", got)
	}
	if m.fileView.loadedGen != defaultFileViewCache.generation() {
		t.Fatalf("expected loadedGen %d, got %d", defaultFileViewCache.generation(), m.fileView.loadedGen)
	}
}

// TestFileViewLifecycle_DirectToolMutationTriggersRefresh tests that tool result
// rows from write_file or edit_file directly refresh an active file view snapshot.
func TestFileViewLifecycle_DirectToolMutationTriggersRefresh(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "live_edit.go")
	if err := os.WriteFile(filePath, []byte("version 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir
	m = testOpenFile(m, "live_edit.go")

	if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "version 1") {
		t.Fatal("initial load should have version 1")
	}

	// Directly modify file on disk
	if err := os.WriteFile(filePath, []byte("version 2 modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Dispatch tool result for write_file affecting live_edit.go
	toolRow := transcriptRow{
		kind:         rowToolResult,
		tool:         "write_file",
		changedFiles: []string{"live_edit.go"},
		detail:       "+version 2 modified",
	}
	updated, reloadCmd := m.Update(agentRowMsg{runID: m.activeRunID, row: toolRow})
	m = updated.(model)

	if reloadCmd == nil {
		t.Fatal("expected reload command on direct tool mutation for active file")
	}

	// Execute reload
	updated, _ = m.Update(reloadCmd())
	m = updated.(model)

	rendered := plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(rendered, "version 2 modified") {
		t.Fatalf("expected version 2 after tool mutation reload, got: %s", rendered)
	}
}

// TestFileViewLifecycle_DeletionOverrulesStaleCache tests that when a file is deleted,
// the reload failure evicts the former cache entry and immediately displays the error.
func TestFileViewLifecycle_DeletionOverrulesStaleCache(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "deleted.go")
	if err := os.WriteFile(filePath, []byte("package deleted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir
	m = testOpenFile(m, "deleted.go")

	if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "package deleted") {
		t.Fatal("initial load failed")
	}

	// Delete the file
	if err := os.Remove(filePath); err != nil {
		t.Fatal(err)
	}

	// Force reload
	m, reloadCmd := m.startFileViewLoadCmd(80)
	if reloadCmd == nil {
		t.Fatal("expected reload command")
	}

	updated, _ := m.Update(reloadCmd())
	m = updated.(model)

	if !m.fileView.hasError {
		t.Fatal("expected hasError to be true after deletion")
	}

	rendered := plainRender(t, m.renderFileViewFull(80))
	if strings.Contains(rendered, "package deleted") {
		t.Fatalf("stale cache must not be shown after deletion, got: %s", rendered)
	}
	if !strings.Contains(rendered, "Could not read file") {
		t.Fatalf("expected error message in view, got: %s", rendered)
	}
}

// TestFileViewLifecycle_LateCompletionAcrossReopenDiscarded tests that if a file view
// is exited and the same path is reopened, any late-arriving completion from the first
// session is discarded and cannot populate the new session.
func TestFileViewLifecycle_LateCompletionAcrossReopenDiscarded(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "reopen.go")
	if err := os.WriteFile(filePath, []byte("original content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir

	// Session 1: open and get command
	m, cmd1 := m.openFileView("reopen.go")
	if cmd1 == nil {
		t.Fatal("expected cmd1")
	}

	// Exit session 1
	m = m.exitFileView()
	if m.fileView.active {
		t.Fatal("view should be inactive")
	}

	// Modify file on disk before session 2
	if err := os.WriteFile(filePath, []byte("new session content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Session 2: reopen same path
	m, cmd2 := m.openFileView("reopen.go")
	if cmd2 == nil {
		t.Fatal("expected cmd2")
	}

	// Late completion from session 1 arrives
	msg1 := cmd1()
	updated, _ := m.Update(msg1)
	m = updated.(model)

	// Session 1 message must have been discarded: view is still waiting on session 2
	if m.fileView.renderedContent != "" {
		t.Fatalf("late completion from session 1 must be discarded, got: %s", m.fileView.renderedContent)
	}

	// Session 2 completion arrives
	msg2 := cmd2()
	updated, _ = m.Update(msg2)
	m = updated.(model)

	rendered := plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(rendered, "new session content") {
		t.Fatalf("expected new session content, got: %s", rendered)
	}
}

// TestFileViewLifecycle_ReverseOrderResizeCompletions verifies that when two resize events
// schedule requests A (earlier) and B (later), and B completes before A, the subsequent
// late arrival of A is discarded and does not overwrite B's width or rendered snapshot.
func TestFileViewLifecycle_ReverseOrderResizeCompletions(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "resize_order.go")
	if err := os.WriteFile(filePath, []byte("package resize_order\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir
	m = testOpenFile(m, "resize_order.go")

	// Schedule Request A for width 60
	m, cmdA := m.startFileViewLoadCmd(60)
	if cmdA == nil {
		t.Fatal("expected cmdA")
	}

	// Schedule Request B for width 100
	m, cmdB := m.startFileViewLoadCmd(100)
	if cmdB == nil {
		t.Fatal("expected cmdB")
	}

	// Message B completes first
	msgB := cmdB()
	updated, _ := m.Update(msgB)
	m = updated.(model)

	if m.fileView.loadedWidth != 100 {
		t.Fatalf("expected loadedWidth 100 after B completes, got %d", m.fileView.loadedWidth)
	}
	if !strings.Contains(plainRender(t, m.renderFileViewFull(100)), "package resize_order") {
		t.Fatalf("expected width 100 content rendered, got: %s", plainRender(t, m.renderFileViewFull(100)))
	}

	// Message A arrives late (reverse-order)
	msgA := cmdA()
	updated, _ = m.Update(msgA)
	m = updated.(model)

	// State MUST remain B (width 100), not overwritten by A (width 60)
	if m.fileView.loadedWidth != 100 {
		t.Fatalf("late completion A must NOT overwrite loadedWidth, got %d (want 100)", m.fileView.loadedWidth)
	}
	if !strings.Contains(plainRender(t, m.renderFileViewFull(100)), "package resize_order") {
		t.Fatalf("expected width 100 content still visible, got: %s", plainRender(t, m.renderFileViewFull(100)))
	}
}

// TestFileViewLifecycle_ReverseOrderToolMutationCompletions verifies that when two tool
// mutations trigger requests A (version 1) and B (version 2), and B completes before A,
// the subsequent arrival of A cannot revert the visible snapshot back to version 1.
func TestFileViewLifecycle_ReverseOrderToolMutationCompletions(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "tool_order.go")
	if err := os.WriteFile(filePath, []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir
	m = testOpenFile(m, "tool_order.go")

	// Mutation A modifies file to v1
	if err := os.WriteFile(filePath, []byte("version 1 state\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rowA := transcriptRow{
		kind:         rowToolResult,
		tool:         "write_file",
		changedFiles: []string{"tool_order.go"},
		detail:       "+version 1 state",
	}
	updated, cmdA := m.Update(agentRowMsg{runID: m.activeRunID, row: rowA})
	m = updated.(model)
	if cmdA == nil {
		t.Fatal("expected cmdA for mutation A")
	}

	// Capture A while v1 is still on disk so msgA contains the genuine v1 snapshot
	msgA := cmdA()

	// Mutation B immediately modifies file to v2 before A is applied
	if err := os.WriteFile(filePath, []byte("version 2 state\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rowB := transcriptRow{
		kind:         rowToolResult,
		tool:         "edit_file",
		changedFiles: []string{"tool_order.go"},
		detail:       "+version 2 state",
	}
	updated, cmdB := m.Update(agentRowMsg{runID: m.activeRunID, row: rowB})
	m = updated.(model)
	if cmdB == nil {
		t.Fatal("expected cmdB for mutation B")
	}

	// B completes first
	msgB := cmdB()
	updated, _ = m.Update(msgB)
	m = updated.(model)

	if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "version 2 state") {
		t.Fatalf("expected version 2 state after B completes, got: %s", plainRender(t, m.renderFileViewFull(80)))
	}

	// A arrives late (reverse-order delivery of stale snapshot)
	updated, _ = m.Update(msgA)
	m = updated.(model)

	// View MUST remain version 2, never reverted by A
	rendered := plainRender(t, m.renderFileViewFull(80))
	if strings.Contains(rendered, "version 1 state") {
		t.Fatalf("stale version 1 completion must NOT overwrite version 2, got: %s", rendered)
	}
	if !strings.Contains(rendered, "version 2 state") {
		t.Fatalf("expected version 2 state still visible, got: %s", rendered)
	}
}

func TestFileViewSanitizesControlSequencesInHighlightedSource(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	// Recognized Go source with OSC 52 clipboard hijacking sequence in a comment
	// and ANSI CSI styling codes inside a string literal.
	goContent := `package main

// \x1b]52;c;cGFzc3dvcmQ=\x07 malicious comment
func main() {
	const escape = "\x1b[31;1mred\x1b[0m"
}
`
	// Replace raw escape escapes with actual byte values 0x1b and 0x07
	goContent = strings.ReplaceAll(goContent, `\x1b`, "\x1b")
	goContent = strings.ReplaceAll(goContent, `\x07`, "\x07")

	filePath := filepath.Join(dir, "malicious.go")
	if err := os.WriteFile(filePath, []byte(goContent), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir
	m = testOpenFile(m, "malicious.go")

	rendered := m.renderFileViewFull(80)

	// Verify that raw hostile control sequences never reach the rendered output
	if strings.Contains(rendered, "\x1b]52;") {
		t.Fatalf("rendered output must not contain raw OSC 52 sequence, got: %q", rendered)
	}
	if strings.Contains(rendered, "\x07") {
		t.Fatalf("rendered output must not contain BEL character, got: %q", rendered)
	}
	if strings.Contains(rendered, "\x1b[31;1m") {
		t.Fatalf("rendered output must not contain raw source-supplied CSI sequence, got: %q", rendered)
	}

	// Verify that the sanitized printable representation is present and styled
	plain := plainRender(t, rendered)
	if !strings.Contains(plain, "^[") {
		t.Fatalf("expected sanitized escape prefix '^[' in plain view, got: %q", plain)
	}
	if !strings.Contains(plain, "malicious comment") {
		t.Fatalf("expected safe comment text in plain view, got: %q", plain)
	}
}

func TestFileViewLifecycle_ResizeRoundTripDoesNotReuseStaleCache(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	filePath := filepath.Join(dir, "roundtrip.go")
	if err := os.WriteFile(filePath, []byte("package roundtrip\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir
	m = testOpenFile(m, "roundtrip.go")

	m, cmd80 := m.startFileViewLoadCmd(80)
	if cmd80 == nil {
		t.Fatal("expected first width-80 load")
	}
	updated, _ := m.Update(cmd80())
	m = updated.(model)
	if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "package roundtrip") {
		t.Fatal("expected committed width-80 content")
	}

	m, cmd100 := m.startFileViewLoadCmd(100)
	if cmd100 == nil {
		t.Fatal("expected width-100 load")
	}
	m, cmd80b := m.startFileViewLoadCmd(80)
	if cmd80b == nil {
		t.Fatal("expected second width-80 load")
	}

	got := plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(got, fileViewLoadingPlaceholder) {
		t.Fatalf("80→100→80 must stay on loading until desiredSeq completes, got: %s", got)
	}
	if strings.Contains(got, "package roundtrip") {
		t.Fatalf("must not reuse cached width 80 from an earlier seq, got: %s", got)
	}

	updated, _ = m.Update(cmd80b())
	m = updated.(model)
	if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "package roundtrip") {
		t.Fatalf("expected content after matching seq completes, got: %s", plainRender(t, m.renderFileViewFull(80)))
	}
}

func TestFileViewLifecycle_EmptyFileIsCompletedSnapshot(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "empty.go"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m = testOpenFile(m, "empty.go")
	got := plainRender(t, m.renderFileViewFull(80))
	if strings.Contains(got, fileViewLoadingPlaceholder) {
		t.Fatalf("empty completed snapshot must not stay on loading, got %q", got)
	}
	if !m.fileView.snapshotReady {
		t.Fatal("snapshotReady must be set for an empty file")
	}
}

func TestFileViewLifecycle_ShellEscapeReloadsFullView(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	path := filepath.Join(dir, "shell.go")
	if err := os.WriteFile(path, []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m = testOpenFile(m, "shell.go")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	oldTime := info.ModTime()
	if err := os.WriteFile(path, []byte("package new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	updated, cmd := m.Update(bashResultMsg{output: "ok"})
	m = updated.(model)
	if cmd == nil {
		t.Fatal("bashResultMsg must schedule a file-view reload")
	}
	updated, _ = m.Update(cmd())
	m = updated.(model)
	got := plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(got, "package new") {
		t.Fatalf("expected reloaded content after shell escape, got %s", got)
	}
}

func TestFileViewLifecycle_SupersededResizeSkipsWork(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "coalesce.go"), []byte("package coalesce\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	var cmd0 tea.Cmd
	m, cmd0 = m.openFileView("coalesce.go")
	if cmd0 == nil {
		t.Fatal("expected initial load")
	}
	var cmd1, cmd2 tea.Cmd
	m, cmd1 = m.startFileViewLoadCmd(60)
	m, cmd2 = m.startFileViewLoadCmd(100)
	_ = cmd0()
	_ = cmd1()
	msg := cmd2()
	updated, _ := m.Update(msg)
	m = updated.(model)
	stats := fileViewCacheStatsForTest()
	if stats.DiskReads != 1 {
		t.Fatalf("superseded loads must not re-read, DiskReads=%d", stats.DiskReads)
	}
	if !strings.Contains(plainRender(t, m.renderFileViewFull(100)), "package coalesce") {
		t.Fatal("latest width must still load")
	}
}

func TestFilesPanelSecondActivationOpensFullViewThroughUpdate(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "web", "app.js"), []byte("let a = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m.runDetailsOpen = true
	m, cmd := m.selectFile("web/app.js")
	if cmd != nil {
		t.Fatal("first FILES activation must only select")
	}
	if m.fileView.active {
		t.Fatal("first FILES activation must not open the file view")
	}
	if m.selectedFile != "web/app.js" {
		t.Fatalf("selectedFile = %q", m.selectedFile)
	}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(model)
	if !m.fileView.active || m.fileView.path != "web/app.js" {
		t.Fatal("Enter on the selected FILES row must call openFileView")
	}
	if m.runDetailsOpen {
		t.Fatal("drilling in must close run details")
	}

	updated, cmd = m.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	m = updated.(model)
	if m.fileView.mode != fileViewFull {
		t.Fatal("f must switch to full mode")
	}
	if cmd == nil {
		t.Fatal("full mode must schedule an async load")
	}
	if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), fileViewLoadingPlaceholder) {
		t.Fatal("expected loading before async result")
	}
	updated, _ = m.Update(cmd())
	m = updated.(model)
	if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "let a = 1") {
		t.Fatalf("expected loaded file content, got %s", plainRender(t, m.renderFileViewFull(80)))
	}
}

func TestFileViewLifecycle_SupersedeStopsInFlightWork(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "stall.go"), []byte("package stall\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	fileViewInsideLoad = func() {
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-release
	}
	defer func() { fileViewInsideLoad = nil }()

	m := filesPanelTestModel()
	m.cwd = dir
	m, cmdA := m.openFileView("stall.go")
	if cmdA == nil {
		t.Fatal("expected initial load")
	}
	doneA := make(chan tea.Msg, 1)
	go func() { doneA <- cmdA() }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("load A did not enter worker")
	}
	m, cmdB := m.startFileViewLoadCmd(100)
	if cmdB == nil {
		t.Fatal("expected superseding load")
	}
	close(release)
	msgA := <-doneA
	loaded, ok := msgA.(fileViewLoadedMsg)
	if !ok {
		t.Fatalf("msgA type %T", msgA)
	}
	if !errors.Is(loaded.err, errFileViewSuperseded) {
		t.Fatalf("in-flight A must stop, err=%v", loaded.err)
	}
	msgB := cmdB()
	updated, _ := m.Update(msgB)
	m = updated.(model)
	if !strings.Contains(plainRender(t, m.renderFileViewFull(100)), "package stall") {
		t.Fatal("latest request must complete")
	}
}

func TestFileViewLifecycle_SupersededHighlightMustNotClobberCache(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	path := filepath.Join(dir, "clobber.go")
	if err := os.WriteFile(path, []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m = testOpenFile(m, "clobber.go")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	oldTime := info.ModTime()
	if err := os.WriteFile(path, []byte("package new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var n int32
	fileViewBeforeCacheCommit = func() {
		if atomic.AddInt32(&n, 1) == 1 {
			close(entered)
			<-release
		}
	}
	defer func() { fileViewBeforeCacheCommit = nil }()

	m, cmdA := m.startFileViewRefreshCmd(80)
	if cmdA == nil {
		t.Fatal("expected refresh A")
	}
	doneA := make(chan tea.Msg, 1)
	go func() { doneA <- cmdA() }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("A did not reach cache commit")
	}
	m, cmdB := m.startFileViewRefreshCmd(80)
	if cmdB == nil {
		t.Fatal("expected refresh B")
	}
	updated, _ := m.Update(cmdB())
	m = updated.(model)
	if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "package new") {
		t.Fatal("B must be accepted before releasing A")
	}
	close(release)
	msgA := <-doneA
	loaded, ok := msgA.(fileViewLoadedMsg)
	if !ok {
		t.Fatalf("msgA type %T", msgA)
	}
	if !errors.Is(loaded.err, errFileViewSuperseded) {
		t.Fatalf("A must not commit, err=%v", loaded.err)
	}
	got := plainRender(t, m.renderFileViewFull(80))
	if strings.Contains(got, "package old") {
		t.Fatalf("stale highlight must not clobber cache, got %s", got)
	}
	if !strings.Contains(got, "package new") {
		t.Fatalf("expected package new, got %s", got)
	}
}

func TestFileViewExitRevokesWorkerToken(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	name := "leave.go"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("package p\nfunc B() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m, cmd := m.openFileView(name)
	if cmd == nil {
		t.Fatal("load cmd")
	}
	token := m.fileView.liveSeq
	if token == nil {
		t.Fatal("liveSeq")
	}
	seq := token.Load()
	m = m.exitFileView()
	if token.Load() == seq {
		t.Fatal("exitFileView must revoke the token the worker still holds")
	}
}

func TestFileViewCacheHitRespectsLiveSeq(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	name := "hit.go"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("package p\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m, cmd := m.openFileView(name)
	if cmd == nil {
		t.Fatal("cache miss must yield load cmd")
	}
	updated, _ := m.Update(cmd())
	m = updated.(model)
	if m.fileView.liveSeq == nil {
		t.Fatal("liveSeq")
	}
	seq := m.fileView.liveSeq.Load()
	live := m.fileView.liveSeq
	fileViewBeforeCacheCommit = func() { live.Store(seq + 1) }
	defer func() { fileViewBeforeCacheCommit = nil }()
	target := filepath.Join(dir, name)
	gen := defaultFileViewCache.generation()
	_, err := defaultFileViewCache.loadAndRender(target, name, m.fileView.loadedWidth+17, nil, m.fileView.loadedFingerprint, gen, zeroTheme, 0, live, seq)
	if !errors.Is(err, errFileViewSuperseded) {
		t.Fatalf("cache-hit put after supersede: err=%v", err)
	}
}

func TestFileViewPlanToolRefreshesWhenGitSweepNil(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	name := "mut.go"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m.activeRunID = 7
	m.gitFileBaseline = nil
	m.gitSweepUnavailable = true
	m = testOpenFile(m, name)
	if err := os.WriteFile(filepath.Join(dir, name), []byte("package new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	updated, cmd := m.Update(agentRowMsg{
		runID: 7,
		row: transcriptRow{
			kind:         rowToolResult,
			tool:         "bash",
			status:       tools.StatusOK,
			changedFiles: []string{name},
		},
	})
	m = updated.(model)
	if cmd == nil {
		t.Fatal("bash/plan mutation must refresh the file view even when maybeGitSweep is nil")
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c == nil {
				continue
			}
			updated, _ = m.Update(c())
			m = updated.(model)
		}
	} else {
		updated, _ = m.Update(msg)
		m = updated.(model)
	}
	got := plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(got, "package new") {
		t.Fatalf("stale view after git-nil mutation: %s", got)
	}
}

func TestFileViewTransitionMatrix_MutationClosedDiffFull(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	name := "sample.go"
	filePath := filepath.Join(dir, name)

	// Phase 1: Closed mutation with equal size & equal mtime
	if err := os.WriteFile(filePath, []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m.activeRunID = 1

	// Seed cache
	m = testOpenFile(m, name)
	if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "package old") {
		t.Fatal("initial seed must show package old")
	}
	m = m.exitFileView()

	// Mutate on disk with same length ("package new\n" has 12 bytes == "package old\n")
	fi, _ := os.Stat(filePath)
	oldTime := fi.ModTime()
	if err := os.WriteFile(filePath, []byte("package new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(filePath, oldTime, oldTime)

	// Deliver mutation row while closed
	updated, _ := m.Update(agentRowMsg{
		runID: 1,
		row: transcriptRow{
			kind:         rowToolResult,
			tool:         "edit_file",
			status:       tools.StatusOK,
			changedFiles: []string{name},
		},
	})
	m = updated.(model)

	// Re-open and switch to full mode: must not reuse stale cache entry
	m = testOpenFile(m, name)
	m = testSetMode(m, fileViewFull)
	got := plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(got, "package new") {
		t.Fatalf("reopening after closed mutation must render package new, got: %s", got)
	}

	// Phase 2: Diff mode mutation with equal metadata
	if err := os.WriteFile(filePath, []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(filePath, oldTime, oldTime)
	m = testSetMode(m, fileViewFull) // refreshed to old
	m = testSetMode(m, fileViewDiff)

	// Mutate while in diff mode
	if err := os.WriteFile(filePath, []byte("package new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(filePath, oldTime, oldTime)
	updated, _ = m.Update(agentRowMsg{
		runID: 1,
		row: transcriptRow{
			kind:         rowToolResult,
			tool:         "edit_file",
			status:       tools.StatusOK,
			changedFiles: []string{name},
		},
	})
	m = updated.(model)

	// Switch back to full mode: must not reuse stale cache entry
	m = testSetMode(m, fileViewFull)
	got = plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(got, "package new") {
		t.Fatalf("switching to full after diff mutation must render package new, got: %s", got)
	}

	// Phase 3: Full mode live mutation
	if err := os.WriteFile(filePath, []byte("package live\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	updated, cmd := m.Update(agentRowMsg{
		runID: 1,
		row: transcriptRow{
			kind:         rowToolResult,
			tool:         "edit_file",
			status:       tools.StatusOK,
			changedFiles: []string{name},
		},
	})
	m = updated.(model)
	if cmd == nil {
		t.Fatal("full mode mutation must yield refresh cmd")
	}
	updated, _ = m.Update(cmd())
	m = updated.(model)
	got = plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(got, "package live") {
		t.Fatalf("full mode live mutation must render package live, got: %s", got)
	}
}

func TestFileViewTransitionMatrix_RefreshResizeThemeOrdering(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	name := "order.go"
	filePath := filepath.Join(dir, name)

	if err := os.WriteFile(filePath, []byte("package v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m.activeRunID = 1
	m = testOpenFile(m, name)

	// Mutate to v2 with equal size & mtime
	fi, _ := os.Stat(filePath)
	oldTime := fi.ModTime()
	if err := os.WriteFile(filePath, []byte("package v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(filePath, oldTime, oldTime)

	// 1. Tool mutation schedules refresh
	updated, refreshCmd := m.Update(agentRowMsg{
		runID: 1,
		row: transcriptRow{
			kind:         rowToolResult,
			tool:         "edit_file",
			status:       tools.StatusOK,
			changedFiles: []string{name},
		},
	})
	m = updated.(model)
	if refreshCmd == nil {
		t.Fatal("mutation must schedule refresh")
	}

	// 2. Before refreshCmd completes, a resize event arrives (advances desiredSeq with refreshSource=false)
	updated, resizeCmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(model)
	if resizeCmd == nil {
		t.Fatal("resize must yield load cmd")
	}

	// 3. Obsolete refreshCmd returns: must be rejected
	oldRefreshMsg := refreshCmd()
	updated, _ = m.Update(oldRefreshMsg)
	m = updated.(model)

	// 4. Winning resizeCmd completes: MUST inherit requiredSourceRev and render v2
	resizeMsg := resizeCmd()
	updated, _ = m.Update(resizeMsg)
	m = updated.(model)

	got := plainRender(t, m.renderFileViewFull(120))
	if !strings.Contains(got, "package v2") {
		t.Fatalf("resize following mutation must render package v2, got: %s", got)
	}
}

func TestFileViewTransitionMatrix_DeterministicStageHooksSupersession(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	name := "hooks.go"
	filePath := filepath.Join(dir, name)
	if err := os.WriteFile(filePath, []byte("package hooks\nfunc Work() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hooks := []struct {
		name       string
		isCacheHit bool
		set        func(fn func())
	}{
		{"InsideLoad", false, func(fn func()) { fileViewInsideLoad = fn }},
		{"BeforeCacheHitFormat", true, func(fn func()) { fileViewBeforeCacheHitFormat = fn }},
		{"BeforeDiskRead", false, func(fn func()) { fileViewBeforeDiskRead = fn }},
		{"BeforeHighlight", false, func(fn func()) { fileViewBeforeHighlight = fn }},
		{"BeforeFormat", false, func(fn func()) { fileViewBeforeFormat = fn }},
		{"BeforeCacheCommit", false, func(fn func()) { fileViewBeforeCacheCommit = fn }},
	}

	for _, tc := range hooks {
		t.Run(tc.name, func(t *testing.T) {
			resetFileViewCacheForTest()
			m := filesPanelTestModel()
			m.cwd = dir

			if tc.isCacheHit {
				// Seed cache first
				m = testOpenFile(m, name)
				m = testSetMode(m, fileViewFull)
			}

			m, cmd := m.openFileView(name)
			if tc.isCacheHit {
				var loadCmd tea.Cmd
				m, loadCmd = m.startFileViewLoadCmd(m.chatColumnWidth() + 15) // width variant
				cmd = loadCmd
			}
			if cmd == nil {
				t.Fatal("load cmd")
			}
			live := m.fileView.liveSeq
			seq := m.fileView.desiredSeq

			executedHook := false
			tc.set(func() {
				executedHook = true
				live.Store(seq + 1) // supersede A with B
			})
			defer tc.set(nil)

			msg := cmd()
			if !executedHook {
				t.Fatalf("hook %s was never executed", tc.name)
			}
			loaded := msg.(fileViewLoadedMsg)
			if !errors.Is(loaded.err, errFileViewSuperseded) {
				t.Fatalf("hook %s: expected errFileViewSuperseded, got %v", tc.name, loaded.err)
			}

			// Delivery of superseded message must be a complete no-op
			updated, nextCmd := m.handleFileViewLoaded(loaded)
			if nextCmd != nil {
				t.Fatalf("hook %s: superseded message must not schedule retry", tc.name)
			}
			if updated.fileView.snapshotReady {
				t.Fatalf("hook %s: superseded message must not mark snapshotReady", tc.name)
			}
		})
	}
}

func TestFileViewTransitionMatrix_CurrentCompletionFollowedByObsoleteCompletion(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	name := "race.go"
	filePath := filepath.Join(dir, name)
	if err := os.WriteFile(filePath, []byte("package race\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir
	m, cmd1 := m.openFileView(name)
	if cmd1 == nil {
		t.Fatal("cmd1")
	}

	// Resize to advance sequence from 1 to 2
	updated, cmd2 := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(model)
	if cmd2 == nil {
		t.Fatal("cmd2")
	}

	// Sequence 2 completes first
	msg2 := cmd2()
	updated, _ = m.Update(msg2)
	m = updated.(model)
	if !m.fileView.snapshotReady || m.fileView.loadedSeq != 2 {
		t.Fatalf("seq 2 must be loaded, ready=%v seq=%d", m.fileView.snapshotReady, m.fileView.loadedSeq)
	}

	// Sequence 1 arrives late: must be a complete no-op
	msg1 := cmd1()
	updated, lateCmd := m.Update(msg1)
	m = updated.(model)
	if lateCmd != nil {
		t.Fatal("late completion must not schedule retry")
	}
	if !m.fileView.snapshotReady || m.fileView.loadedSeq != 2 {
		t.Fatalf("late completion must not degrade state: ready=%v seq=%d", m.fileView.snapshotReady, m.fileView.loadedSeq)
	}
}

func TestFileView_ChangedLineMatchingWithTabsAndControlChars(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	name := "tabmatch.go"
	content := "package main\n\nvar Field\t= 1\nvar Clean = 2\n"
	filePath := filepath.Join(dir, name)
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir
	m.transcript = append(m.transcript, transcriptRow{
		kind:         rowToolResult,
		tool:         "edit_file",
		status:       tools.StatusOK,
		changedFiles: []string{name},
		detail:       "--- a/tabmatch.go\n+++ b/tabmatch.go\n@@ -1,2 +1,3 @@\n+var Field\t= 1\n",
	})

	m = testOpenFile(m, name)
	m = testSetMode(m, fileViewFull)
	rendered := m.renderFileViewFull(80)

	// The rendered source displays 4 spaces instead of raw tab, and retains the accent gutter marker ▎
	if strings.Contains(rendered, "\t") {
		t.Fatal("rendered output must not contain raw tab characters")
	}
	if !strings.Contains(rendered, "▎") {
		t.Fatalf("added line with interior tab must retain accent gutter marker, got:\n%s", rendered)
	}
}

func TestRunDetails_KeyboardActivationStrictRowTargets(t *testing.T) {
	m := filesPanelTestModel()
	m.altScreen = true
	m.width = 80
	m.height = 30
	m.runDetailsOpen = true

	// Build a transcript with 7 files touched so files 0..3 are visible, file 4 is overflow trailer ("… more in transcript"), files 5..6 hidden
	for i := 0; i < 7; i++ {
		name := fmt.Sprintf("file_%d.go", i)
		m.transcript = append(m.transcript, transcriptRow{
			kind:         rowToolResult,
			tool:         "edit_file",
			status:       tools.StatusOK,
			changedFiles: []string{name},
		})
	}

	// 1. Visible file (file_6.go) can be activated on Enter
	m.selectedFile = "file_6.go"
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	res := updated.(model)
	if !res.fileView.active || res.fileView.path != "file_6.go" {
		t.Fatalf("visible file in run details must be activated on Enter: active=%v path=%s", res.fileView.active, res.fileView.path)
	}

	// Reset file view
	m = res.exitFileView()
	m.runDetailsOpen = true

	// 2. Hidden / overflow file (file_0.go) must NOT be activated on Enter
	m.selectedFile = "file_0.go"
	updated, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	res = updated.(model)
	if res.fileView.active {
		t.Fatalf("overflow hidden file (file_0.go) must not be activated on Enter from run details modal")
	}
}

func TestFileView_RewindReturnsAsyncCommandAndDoesNotBlockUpdate(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "target.go")
	if err := os.WriteFile(filePath, []byte("package before_rewind\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{Title: "RewindTest"})
	if err != nil {
		t.Fatal(err)
	}
	appendTestEvent(t, store, session.SessionID, sessions.EventSessionCheckpoint, map[string]any{"tool": "write_file", "files": []any{}})

	m := filesPanelTestModel()
	m.cwd = dir
	m.sessionStore = store
	m.activeSession = session
	m.activeRunID = 1

	// Open file in full view
	m = testOpenFile(m, "target.go")
	m = testSetMode(m, fileViewFull)
	if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "package before_rewind") {
		t.Fatal("expected initial view to show before_rewind")
	}

	// Overwrite on disk to simulate restored file
	if err := os.WriteFile(filePath, []byte("package after_rewind\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, summary, rewindCmd := m.handleRewindCommand("latest")
	if !strings.Contains(summary, "Rewound") {
		t.Fatalf("expected summary to indicate rewound, got %q", summary)
	}
	if rewindCmd == nil {
		t.Fatal("handleRewindCommand must return an async tea.Cmd when fileView is active in full mode")
	}

	// Model in Update must NOT have synchronously rendered the new file yet:
	// it must NOT display package after_rewind until Bubble Tea delivers fileViewLoadedMsg.
	if strings.Contains(plainRender(t, m.renderFileViewFull(80)), "package after_rewind") {
		t.Fatal("expected view to not synchronously show after_rewind before async tea.Cmd is executed")
	}

	// Now execute rewindCmd as Bubble Tea runtime would
	msg := rewindCmd()
	loadedMsg, ok := msg.(fileViewLoadedMsg)
	if !ok {
		t.Fatalf("expected fileViewLoadedMsg, got %T", msg)
	}
	updated, _ := m.Update(loadedMsg)
	m = updated.(model)

	got := plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(got, "package after_rewind") {
		t.Fatalf("rewind reload must render after_rewind, got: %s", got)
	}
}

func TestFileView_RewindPartialFailure_ActiveAndClosedView(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "partial_fail.go")
	if err := os.WriteFile(filePath, []byte("package before_partial\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{Title: "PartialFailRewind"})
	if err != nil {
		t.Fatal(err)
	}
	appendTestEvent(t, store, session.SessionID, sessions.EventSessionCheckpoint, map[string]any{"tool": "write_file", "files": []any{}})

	m := filesPanelTestModel()
	m.cwd = dir
	m.sessionStore = store
	m.activeSession = session
	m.activeRunID = 1

	// 1. Active view: open and cache
	m = testOpenFile(m, "partial_fail.go")
	m = testSetMode(m, fileViewFull)
	if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "package before_partial") {
		t.Fatal("expected before_partial")
	}

	// Restored file written to disk
	if err := os.WriteFile(filePath, []byte("package after_partial_restored\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Invalidate session ID so ApplyRewind fails after disk mutation
	m.activeSession.SessionID = "non_existent_session"

	// Invoke handleRewindCommand directly: ApplyRewind will fail!
	m, summary, refreshCmd := m.handleRewindCommand("0")
	if !strings.Contains(summary, "Rewind\n") {
		t.Fatalf("expected error summary, got: %s", summary)
	}
	if refreshCmd == nil {
		t.Fatal("expected refreshCmd on ApplyRewind error")
	}

	msg := refreshCmd()
	loaded, ok := msg.(fileViewLoadedMsg)
	if !ok {
		t.Fatalf("expected fileViewLoadedMsg, got %T", msg)
	}
	updated, _ := m.Update(loaded)
	m = updated.(model)

	got := plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(got, "package after_partial_restored") {
		t.Fatalf("expected after_partial_restored, got: %s", got)
	}

	// 2. Closed view: close view, write v3, trigger another failed rewind, reopen
	m = m.exitFileView()
	if err := os.WriteFile(filePath, []byte("package after_partial_v3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m, _, _ = m.handleRewindCommand("0")

	m = testOpenFile(m, "partial_fail.go")
	m = testSetMode(m, fileViewFull)
	got = plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(got, "package after_partial_v3") {
		t.Fatalf("expected after_partial_v3 on reopen, got: %s", got)
	}
}

func TestFileView_UnknownScopeCommandMutationClosedView(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	name := "equal_len.go"
	filePath := filepath.Join(dir, name)

	// Seed file with "package old\n" (12 bytes)
	if err := os.WriteFile(filePath, []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m.activeRunID = 1

	// Open and cache
	m = testOpenFile(m, name)
	m = testSetMode(m, fileViewFull)
	if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "package old") {
		t.Fatal("expected initial render of package old")
	}

	// Close view
	m = m.exitFileView()

	// Mutate on disk with identical size (12 bytes) and restored mtime
	fi, _ := os.Stat(filePath)
	oldModTime := fi.ModTime()
	if err := os.WriteFile(filePath, []byte("package new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(filePath, oldModTime, oldModTime)

	// Deliver an unknown-scope tool result (bash command) while view is closed
	updated, _ := m.Update(agentRowMsg{
		runID: 1,
		row: transcriptRow{
			kind:   rowToolResult,
			tool:   "bash",
			status: tools.StatusOK,
			detail: "sed -i 's/old/new/g' equal_len.go",
		},
	})
	m = updated.(model)

	// Reopen view: must NOT serve cached package old
	m = testOpenFile(m, name)
	m = testSetMode(m, fileViewFull)
	got := plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(got, "package new") {
		t.Fatalf("reopening after closed unknown-scope mutation must render package new, got: %s", got)
	}
}

func TestFileView_NoTOCTOUFileOpenInTUI(t *testing.T) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, "file_view.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}

	// Verify that inside readFileViewBoundedCancellable, os.Stat is NOT called before os.OpenFile
	ast.Inspect(node, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "readFileViewBoundedCancellable" {
			return true
		}

		var sawOpenFile bool
		var sawPreStat bool

		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if ident.Name == "os" && sel.Sel.Name == "Stat" && !sawOpenFile {
				sawPreStat = true
			}
			if ident.Name == "os" && sel.Sel.Name == "OpenFile" {
				sawOpenFile = true
			}
			return true
		})

		if sawPreStat {
			t.Fatal("TOCTOU vulnerability detected: os.Stat called before os.OpenFile in readFileViewBoundedCancellable")
		}
		if !sawOpenFile {
			t.Fatal("expected os.OpenFile with O_NONBLOCK in readFileViewBoundedCancellable")
		}
		return false
	})
}

func TestFileView_SupersededInsideDiskReadAndHighlight(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "cancel_test.go")
	if err := os.WriteFile(filePath, []byte("package cancel\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := newFileViewRenderCache(10, defaultFileViewCacheMaxBytes)
	theme := zeroTheme
	var liveSeq atomic.Uint64
	liveSeq.Store(1)

	// 1. Superseded during disk read loop
	fileViewInsideDiskRead = func() {
		liveSeq.Store(2) // supersede request 1
	}
	defer func() { fileViewInsideDiskRead = nil }()

	_, err := cache.loadAndRender(filePath, "cancel_test.go", 80, nil, "", 0, theme, 0, &liveSeq, 1)
	if !errors.Is(err, errFileViewSuperseded) {
		t.Fatalf("expected errFileViewSuperseded on inside-disk-read supersession, got: %v", err)
	}
	fileViewInsideDiskRead = nil

	// 2. Superseded before highlight
	liveSeq.Store(10)
	fileViewBeforeHighlight = func() {
		liveSeq.Store(11) // supersede request 10
	}
	defer func() { fileViewBeforeHighlight = nil }()

	_, err = cache.loadAndRender(filePath, "cancel_test.go", 80, nil, "", 0, theme, 0, &liveSeq, 10)
	if !errors.Is(err, errFileViewSuperseded) {
		t.Fatalf("expected errFileViewSuperseded on before-highlight supersession, got: %v", err)
	}
	fileViewBeforeHighlight = nil

	// 3. Superseded inside highlight (under fileViewHighlightMu)
	liveSeq.Store(20)
	fileViewInsideHighlight = func() {
		liveSeq.Store(21) // supersede request 20
	}
	defer func() { fileViewInsideHighlight = nil }()

	_, err = cache.loadAndRender(filePath, "cancel_test.go", 80, nil, "", 0, theme, 0, &liveSeq, 20)
	if !errors.Is(err, errFileViewSuperseded) {
		t.Fatalf("expected errFileViewSuperseded on inside-highlight supersession, got: %v", err)
	}
	if cache.stats().HighlightCalls != 0 {
		t.Fatalf("expected 0 HighlightCalls on superseded request, got: %d", cache.stats().HighlightCalls)
	}
	fileViewInsideHighlight = nil
}

func TestFileView_DispatchCommandRewindReturnsPromptlyWithCmd(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "prompt_test.go")
	if err := os.WriteFile(filePath, []byte("package before_dispatch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := testSessionStore(t)
	session, err := store.Create(sessions.CreateInput{Title: "DispatchRewind"})
	if err != nil {
		t.Fatal(err)
	}
	appendTestEvent(t, store, session.SessionID, sessions.EventSessionCheckpoint, map[string]any{"tool": "write_file", "files": []any{}})

	m := newModel(context.Background(), Options{SessionStore: store})
	m.cwd = dir
	m.activeSession = session
	m.activeRunID = 1

	// Open in full view
	m = testOpenFile(m, "prompt_test.go")
	m = testSetMode(m, fileViewFull)
	if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "package before_dispatch") {
		t.Fatal("expected view to display before_dispatch")
	}

	// Restored file content on disk
	if err := os.WriteFile(filePath, []byte("package after_dispatch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Type /rewind and press enter
	m.input.SetValue("/rewind latest")
	updated, cmd := m.Update(testKey(tea.KeyEnter))
	m = updated.(model)

	if cmd == nil {
		t.Fatal("expected /rewind via dispatchCommand to return a non-nil async tea.Cmd")
	}

	// Before executing cmd, full view must NOT have synchronously loaded the new content:
	if strings.Contains(plainRender(t, m.renderFileViewFull(80)), "package after_dispatch") {
		t.Fatal("expected view to not show after_dispatch before async command is delivered")
	}

	// Now execute cmd and deliver
	msg := cmd()
	loadedMsg, ok := msg.(fileViewLoadedMsg)
	if !ok {
		t.Fatalf("expected fileViewLoadedMsg from rewind tea.Cmd, got %T", msg)
	}
	updated, _ = m.Update(loadedMsg)
	m = updated.(model)

	got := plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(got, "package after_dispatch") {
		t.Fatalf("expected view to update to after_dispatch after delivery, got: %s", got)
	}
}

func TestFileView_CommandAndSweepCompletionOrders(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	name := "order_test.go"
	filePath := filepath.Join(dir, name)

	// Order 1: Command tool result arrives -> Git sweep arrives later
	if err := os.WriteFile(filePath, []byte("package v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m.activeRunID = 1

	m = testOpenFile(m, name)
	m = testSetMode(m, fileViewFull)
	if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "package v1") {
		t.Fatal("expected v1")
	}

	// Mutate on disk to v2
	if err := os.WriteFile(filePath, []byte("package v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Step 1: command tool result
	updated, cmd1 := m.Update(agentRowMsg{
		runID: 1,
		row: transcriptRow{
			kind:   rowToolResult,
			tool:   "bash",
			status: tools.StatusOK,
			detail: "echo 'package v2' > order_test.go",
		},
	})
	m = updated.(model)
	if cmd1 != nil {
		msg := cmd1()
		if loaded, ok := msg.(fileViewLoadedMsg); ok {
			updated, _ = m.Update(loaded)
			m = updated.(model)
		}
	}

	// Step 2: git sweep arrives later
	updated, cmd2 := m.Update(gitSweepMsg{
		ok:    true,
		files: []gitSweepFile{{path: name, adds: 1}},
	})
	m = updated.(model)
	if cmd2 != nil {
		msg := cmd2()
		if loaded, ok := msg.(fileViewLoadedMsg); ok {
			updated, _ = m.Update(loaded)
			m = updated.(model)
		}
	}

	got := plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(got, "package v2") {
		t.Fatalf("expected package v2 after command->sweep order, got: %s", got)
	}

	// Order 2: Git sweep arrives first -> Command tool result arrives second
	if err := os.WriteFile(filePath, []byte("package v3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	updated, cmdA := m.Update(gitSweepMsg{
		ok:    true,
		files: []gitSweepFile{{path: name, adds: 1}},
	})
	m = updated.(model)
	if cmdA != nil {
		msg := cmdA()
		if loaded, ok := msg.(fileViewLoadedMsg); ok {
			updated, _ = m.Update(loaded)
			m = updated.(model)
		}
	}

	updated, cmdB := m.Update(agentRowMsg{
		runID: 1,
		row: transcriptRow{
			kind:   rowToolResult,
			tool:   "bash",
			status: tools.StatusOK,
			detail: "echo 'package v3' > order_test.go",
		},
	})
	m = updated.(model)
	if cmdB != nil {
		msg := cmdB()
		if loaded, ok := msg.(fileViewLoadedMsg); ok {
			updated, _ = m.Update(loaded)
			m = updated.(model)
		}
	}

	got = plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(got, "package v3") {
		t.Fatalf("expected package v3 after sweep->command order, got: %s", got)
	}
}

// TestFileViewCacheByteBudget verifies the cache-wide byte cap: retained stays
// within maxBytes, eviction fires before the budget is exceeded, and the named
// default bound is pinned so a silent regression is caught.
func TestFileViewCacheByteBudget(t *testing.T) {
	if defaultFileViewCacheMaxBytes != 8<<20 {
		t.Fatalf("defaultFileViewCacheMaxBytes = %d, want %d", defaultFileViewCacheMaxBytes, 8<<20)
	}
	defaultFileViewCache.mu.Lock()
	pinned := defaultFileViewCache.maxBytes
	defaultFileViewCache.mu.Unlock()
	if pinned != defaultFileViewCacheMaxBytes {
		t.Fatalf("default cache maxBytes = %d, want %d", pinned, defaultFileViewCacheMaxBytes)
	}

	dir := t.TempDir()
	const maxBytes = 48 * 1024
	cache := newFileViewRenderCache(64, maxBytes)

	payload := strings.Repeat("abcdefghij\n", 400) // ~4 KiB per file
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("byte_cap_%d.txt", i)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := cache.loadAndRender(path, name, 80, nil, "", cache.generation(), zeroTheme, 0, nil, 0); err != nil {
			t.Fatalf("load %d: %v", i, err)
		}
	}

	cache.mu.Lock()
	retained := cache.retained
	entries := len(cache.items)
	cache.mu.Unlock()
	if retained > maxBytes {
		t.Fatalf("retained %d exceeded maxBytes %d", retained, maxBytes)
	}
	if entries == 0 {
		t.Fatal("expected at least one cached entry")
	}
	if evictions := cache.stats().Evictions; evictions == 0 {
		t.Fatal("expected byte-budget eviction to fire")
	}
}

// TestFileViewCacheRefusesOversizedEntry verifies a single entry larger than the
// byte budget is returned to the caller but never retained.
func TestFileViewCacheRefusesOversizedEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oversized.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("z", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := newFileViewRenderCache(64, 512)
	rendered, err := cache.loadAndRender(path, "oversized.txt", 80, nil, "", cache.generation(), zeroTheme, 0, nil, 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(rendered, "zzz") {
		t.Fatal("oversized entry must still be returned to the caller")
	}
	cache.mu.Lock()
	entries := len(cache.items)
	retained := cache.retained
	cache.mu.Unlock()
	if entries != 0 || retained != 0 {
		t.Fatalf("oversized entry must not be cached: entries=%d retained=%d", entries, retained)
	}
	if stats := cache.stats(); stats.SkippedOversized != 1 {
		t.Fatalf("SkippedOversized = %d, want 1", stats.SkippedOversized)
	}
}

// TestFileViewCacheEvictionDropsPathRevision verifies that LRU eviction removes
// the evicted path's revision key, so revisions cannot leak one key per file.
func TestFileViewCacheEvictionDropsPathRevision(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "rev_a.txt")
	b := filepath.Join(dir, "rev_b.txt")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("alpha\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cache := newFileViewRenderCache(1, defaultFileViewCacheMaxBytes)

	// Give a a revision, then load it so the entry and its revision coexist.
	cache.invalidatePath(a)
	if _, err := cache.loadAndRender(a, "rev_a.txt", 80, nil, "", cache.generation(), zeroTheme, cache.requiredRevision(a), nil, 0); err != nil {
		t.Fatalf("load a: %v", err)
	}
	cache.mu.Lock()
	_, hasA := cache.pathRevisions[a]
	cache.mu.Unlock()
	if !hasA {
		t.Fatal("expected revision key for a before eviction")
	}

	// Loading b with maxEntries=1 evicts a and must drop a's revision.
	if _, err := cache.loadAndRender(b, "rev_b.txt", 80, nil, "", cache.generation(), zeroTheme, 0, nil, 0); err != nil {
		t.Fatalf("load b: %v", err)
	}
	cache.mu.Lock()
	_, hasA = cache.pathRevisions[a]
	orphans := len(cache.pathRevisions)
	cache.mu.Unlock()
	if hasA {
		t.Fatal("evicted path a must not keep a revision key")
	}
	if orphans != 0 {
		t.Fatalf("expected no orphan revision keys, got %d", orphans)
	}
}

// TestFileViewCachePurgeResetsPathRevisions verifies clear() resets the revision
// map instead of bumping dead keys forever, so /clear cannot leak one key per
// abandoned tool result.
func TestFileViewCachePurgeResetsPathRevisions(t *testing.T) {
	cache := newFileViewRenderCache(8, defaultFileViewCacheMaxBytes)
	cache.invalidatePath("one.go")
	cache.invalidatePath("two.go")
	cache.mu.Lock()
	before := len(cache.pathRevisions)
	cache.mu.Unlock()
	if before != 2 {
		t.Fatalf("expected 2 revision keys, got %d", before)
	}

	cache.clear()
	cache.mu.Lock()
	after := len(cache.pathRevisions)
	cache.mu.Unlock()
	if after != 0 {
		t.Fatalf("clear() must drop revision keys, got %d", after)
	}
	if rev := cache.invalidatePath("one.go"); rev != 1 {
		t.Fatalf("revision after purge = %d, want 1", rev)
	}
}

// TestFileViewCacheFreshnessBySourceHash verifies that metadata (mtime+size) is
// only a hint: when an overwrite restores the same metadata with different
// bytes, the cached render must not be served.
func TestFileViewCacheFreshnessBySourceHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fresh.go")
	original := "package main\n// AAAA\n"
	updated := "package main\n// BBBB\n"
	if len(original) != len(updated) {
		t.Fatal("test setup: contents must have equal length")
	}
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1700000000, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	cache := newFileViewRenderCache(64, defaultFileViewCacheMaxBytes)
	first, err := cache.loadAndRender(path, "fresh.go", 80, nil, "", cache.generation(), zeroTheme, 0, nil, 0)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if !strings.Contains(first, "AAAA") {
		t.Fatalf("first render missing original content: %q", first)
	}

	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	second, err := cache.loadAndRender(path, "fresh.go", 80, nil, "", cache.generation(), zeroTheme, 0, nil, 0)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if strings.Contains(second, "AAAA") || !strings.Contains(second, "BBBB") {
		t.Fatalf("stale render served after same-metadata overwrite: %q", second)
	}
	if stats := cache.stats(); stats.HighlightCalls != 2 {
		t.Fatalf("HighlightCalls = %d, want 2 (hash mismatch must force a miss)", stats.HighlightCalls)
	}
}

// deliverCommandMessages executes a tea.Cmd tree and feeds every produced
// message back into the model through Update, unwrapping tea.BatchMsg so a
// batched dispatch (notice + reload) reaches its load completion. Commands that
// Update schedules in response are intentionally not followed.
func deliverCommandMessages(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	if cmd == nil {
		return m
	}
	pending := []tea.Cmd{cmd}
	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			continue
		}
		msg := current()
		if msg == nil {
			continue
		}
		if batch, ok := msg.(tea.BatchMsg); ok {
			pending = append(pending, batch...)
			continue
		}
		updated, _ := m.Update(msg)
		m = updated.(model)
	}
	return m
}

// TestFileViewLifecycle_OversizedReplacementEvictsObsoleteEntry verifies that an
// oversized replacement is still the displayed snapshot: the obsolete resident
// entry is removed and cannot win the next cache lookup, and the oversized
// admission is counted without being retained.
func TestFileViewLifecycle_OversizedReplacementEvictsObsoleteEntry(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	name := "oversized_lifecycle.go"
	filePath := filepath.Join(dir, name)
	if err := os.WriteFile(filePath, []byte("package original\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir
	m = testOpenFile(m, name)
	if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "package original") {
		t.Fatal("expected the seed file to be cached and displayed")
	}
	m = m.exitFileView()

	// 4000 * 78 == 312000 bytes: inside the 1 MiB read budget, but the retained
	// source plus its highlighted and rendered payload exceeds the cache budget.
	line := "var a = 123; var b = 456; var c = 789; var d = 123; var e = 456; var f = 789;\n"
	payload := strings.Repeat(line, 4000)
	if len(payload) != 312000 {
		t.Fatalf("test payload = %d bytes, want 312000", len(payload))
	}
	if err := os.WriteFile(filePath, []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}

	statsBefore := fileViewCacheStatsForTest()
	m = testOpenFile(m, name)

	displayed := plainRender(t, m.renderFileViewFull(80))
	if strings.Contains(displayed, "package original") {
		t.Fatal("the obsolete cached entry must not override the oversized replacement")
	}
	if !strings.Contains(displayed, "var a = 123") {
		t.Fatal("the oversized replacement must be the displayed snapshot")
	}
	statsAfter := fileViewCacheStatsForTest()
	if statsAfter.SkippedOversized <= statsBefore.SkippedOversized {
		t.Fatalf("SkippedOversized must increase: before=%d after=%d", statsBefore.SkippedOversized, statsAfter.SkippedOversized)
	}
	defaultFileViewCache.mu.Lock()
	_, resident := defaultFileViewCache.items[filePath]
	defaultFileViewCache.mu.Unlock()
	if resident {
		t.Fatal("an oversized replacement must not remain resident")
	}
}

// TestFileViewLifecycle_PendingRefreshPreservesViewport verifies that a pending
// full-file reload keeps the reader's scroll position, and that completion
// reconciles that position against the real body instead of leaving it at the
// loading placeholder's zero height.
func TestFileViewLifecycle_PendingRefreshPreservesViewport(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	name := "viewport.go"
	var body strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&body, "line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	m := filesPanelTestModel()
	m.cwd = dir
	m.altScreen = true
	m.width = 100
	m.height = 30
	m = testOpenFile(m, name)
	if !m.fileView.snapshotReady {
		t.Fatal("expected the initial full-file snapshot")
	}

	m.chatScrollOffset = 50
	updated, batchCmd := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(model)
	if batchCmd == nil {
		t.Fatal("resize on an open full view must schedule a reload")
	}
	if !m.fileView.loading {
		t.Fatal("reload should be pending after the resize")
	}
	if m.chatScrollOffset != 50 {
		t.Fatalf("pending reload must preserve the reading position: got %d, want 50", m.chatScrollOffset)
	}

	m = deliverCommandMessages(t, m, batchCmd)
	if m.fileView.loading {
		t.Fatal("expected the reload to complete")
	}
	if m.chatScrollOffset != 50 {
		t.Fatalf("completed reload must preserve the reading position: got %d, want 50", m.chatScrollOffset)
	}
	if !strings.Contains(plainRender(t, m.renderFileViewFull(100)), "line 199") {
		t.Fatal("expected the completed body to render the file tail")
	}
}

// TestFileViewLifecycle_BTWPreservesParentLoadOwnership verifies that a parent
// file load completing while a BTW conversation is visible is routed to the
// hidden parent, so returning restores a settled snapshot rather than a stalled
// "Loading…" view.
func TestFileViewLifecycle_BTWPreservesParentLoadOwnership(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	name := "parent_view.go"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("package parent\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newBTWTestModel(t)
	m.cwd = dir

	// Open a full view but keep its load command pending in the parent.
	m, parentLoadCmd := m.openFileView(name)
	if parentLoadCmd == nil {
		t.Fatal("expected a pending parent load command")
	}
	if !m.fileView.loading {
		t.Fatal("parent view should be loading")
	}

	// Enter BTW; the hidden parent retains ownership of its pending load.
	side, _ := m.handleBTWCommand("")
	if !side.btw.active || side.btw.parent == nil {
		t.Fatal("expected an active BTW conversation")
	}

	// The parent's load completes while BTW is visible. It must update the hidden
	// parent, not the side surface.
	updated, _ := side.Update(parentLoadCmd())
	side = updated.(model)
	if side.btw.parent == nil {
		t.Fatal("BTW lost the parent snapshot")
	}
	if side.btw.parent.fileView.loading {
		t.Fatal("parent load completion must settle the hidden parent's loading state")
	}
	if !side.btw.parent.fileView.snapshotReady {
		t.Fatal("hidden parent must integrate its snapshot before returning")
	}

	restored, _ := side.leaveBTW()
	if restored.fileView.loading {
		t.Fatal("restored parent must not be stuck loading")
	}
	got := plainRender(t, restored.renderFileViewFull(80))
	if !strings.Contains(got, "package parent") {
		t.Fatalf("restored parent must render its snapshot, got: %s", got)
	}
	if strings.Contains(got, fileViewLoadingPlaceholder) {
		t.Fatal("restored parent must not show the loading placeholder")
	}
}

// TestFileViewLifecycle_RevisionMetadataBounded verifies that per-path revision
// counters for never-cached paths stay bounded, and that retiring a revision
// cannot make an older request appear current (ABA protection).
func TestFileViewLifecycle_RevisionMetadataBounded(t *testing.T) {
	dir := t.TempDir()
	warm := filepath.Join(dir, "revision_warm.go")
	if err := os.WriteFile(warm, []byte("package warm\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := newFileViewRenderCache(defaultFileViewCacheMaxEntries, defaultFileViewCacheMaxBytes)
	oldRev := cache.invalidatePath(warm)
	if _, err := cache.loadAndRender(warm, "revision_warm.go", 80, nil, "", cache.generation(), zeroTheme, oldRev, nil, 0); err != nil {
		t.Fatalf("warm load: %v", err)
	}

	for i := 0; i < 10000; i++ {
		cache.invalidatePath(filepath.Join(dir, fmt.Sprintf("never_cached_%d.go", i)))
	}

	cache.mu.Lock()
	revisionCount := len(cache.pathRevisions)
	epochFloor := cache.revEpochFloor
	cache.mu.Unlock()
	if revisionCount > fileViewMaxPathRevisions {
		t.Fatalf("pathRevisions = %d, exceeds hard cap %d", revisionCount, fileViewMaxPathRevisions)
	}
	if revisionCount > cache.maxPathRevisions() {
		t.Fatalf("pathRevisions = %d, exceeds policy bound %d", revisionCount, cache.maxPathRevisions())
	}

	if got := cache.requiredRevision(warm); got <= oldRev {
		t.Fatalf("retired revision must not read as current: required=%d old=%d", got, oldRev)
	}
	if epochFloor <= oldRev {
		t.Fatalf("epoch floor %d must advance past retired revision %d", epochFloor, oldRev)
	}

	// A request captured before the retirement must not reuse the still-resident
	// entry; it has to re-render under the advanced authority.
	highlightBefore := cache.stats().HighlightCalls
	if _, err := cache.loadAndRender(warm, "revision_warm.go", 80, nil, "", cache.generation(), zeroTheme, oldRev, nil, 0); err != nil {
		t.Fatalf("stale load: %v", err)
	}
	if highlightAfter := cache.stats().HighlightCalls; highlightAfter != highlightBefore+1 {
		t.Fatalf("stale request must not commit a cache hit: HighlightCalls %d -> %d", highlightBefore, highlightAfter)
	}
}

// TestFileViewLifecycle_TruncationStateEquivalence verifies that two versions
// with identical size, timestamp, and retained source hash but a different
// truncation status are not interchangeable: the clipped version keeps its
// warning, and the warning disappears again when clipping stops.
func TestFileViewLifecycle_TruncationStateEquivalence(t *testing.T) {
	resetFileViewCacheForTest()

	dir := t.TempDir()
	name := "truncation_state.go"
	filePath := filepath.Join(dir, name)
	stamp := time.Unix(1700000000, 0)
	fill := strings.Repeat("x", fileViewMaxLineBytes)

	// Version A: CRLF-terminated, so the line is exactly at the limit and not clipped.
	if err := os.WriteFile(filePath, []byte(fill+"\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filePath, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	first, err := defaultFileViewCache.loadAndRender(filePath, name, 80, nil, "", defaultFileViewCache.generation(), zeroTheme, 0, nil, 0)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if strings.Contains(plainRender(t, first), "truncated") {
		t.Fatal("an unclipped line must not display a truncation warning")
	}

	// Version B: the same 4098 bytes and timestamp, but the physical line now
	// exceeds the per-line display limit while the retained line and hash do not.
	if err := os.WriteFile(filePath, []byte(fill+"Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filePath, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	second, err := defaultFileViewCache.loadAndRender(filePath, name, 80, nil, "", defaultFileViewCache.generation(), zeroTheme, 0, nil, 0)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if !strings.Contains(plainRender(t, second), "truncated") {
		t.Fatal("a clipped replacement must display its truncation warning")
	}

	// Reverse: going back to the unclipped CRLF form must drop the warning.
	if err := os.WriteFile(filePath, []byte(fill+"\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filePath, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	third, err := defaultFileViewCache.loadAndRender(filePath, name, 80, nil, "", defaultFileViewCache.generation(), zeroTheme, 0, nil, 0)
	if err != nil {
		t.Fatalf("third load: %v", err)
	}
	if strings.Contains(plainRender(t, third), "truncated") {
		t.Fatal("a previously cached warning must disappear when clipping stops")
	}
}

// TestFileViewLifecycle_VariantAccountingConsistency verifies that same-key
// variant rewrites do not charge the key twice, and that a detached or purged
// entry cannot leak bytes into the cache's retained counter.
func TestFileViewLifecycle_VariantAccountingConsistency(t *testing.T) {
	// Same-key replacement accounts for the difference, not the key again.
	entry := &fileViewCachedEntry{}
	key := "80:"
	current := strings.Repeat("a", 100)
	if delta := entry.putRender(key, current); delta != len(key)+len(current) {
		t.Fatalf("initial put delta = %d, want %d", delta, len(key)+len(current))
	}
	for i := 0; i < 10; i++ {
		next := strings.Repeat("b", 40+i)
		delta := entry.putRender(key, next)
		if want := len(next) - len(current); delta != want {
			t.Fatalf("same-key rewrite %d delta = %d, want %d", i, delta, want)
		}
		current = next
	}
	if got, want := entry.byteSize(), len(key)+len(current); got != want {
		t.Fatalf("entry byteSize = %d, want %d", got, want)
	}

	resetFileViewCacheForTest()
	dir := t.TempDir()
	path := filepath.Join(dir, "variant_accounting.go")
	if err := os.WriteFile(path, []byte("package accounting\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cache := newFileViewRenderCache(64, defaultFileViewCacheMaxBytes)
	if _, err := cache.loadAndRender(path, "variant_accounting.go", 80, nil, "", cache.generation(), zeroTheme, 0, nil, 0); err != nil {
		t.Fatalf("seed load: %v", err)
	}

	cache.mu.Lock()
	elem := cache.items[path]
	retainedBefore := cache.retained
	var residentBytes int
	if elem != nil {
		residentBytes = elem.Value.(*fileViewCachedEntry).byteSize()
	}
	cache.mu.Unlock()
	if elem == nil {
		t.Fatal("expected a resident entry to seed the accounting check")
	}
	if retainedBefore != residentBytes {
		t.Fatalf("retained = %d, want resident payload %d", retainedBefore, residentBytes)
	}

	// Evicting the entry detaches it without destroying the value the caller holds.
	cache.evictPath(path)
	cache.mu.Lock()
	afterEvict := cache.retained
	_, resident := cache.items[path]
	cache.mu.Unlock()
	if afterEvict != 0 || resident {
		t.Fatalf("eviction must release and detach the entry: retained=%d resident=%v", afterEvict, resident)
	}
	cache.commitRenderPut(elem.Value.(*fileViewCachedEntry), "80:detached", strings.Repeat("z", 512))
	cache.mu.Lock()
	polluted := cache.retained
	_, readmitted := cache.items[path]
	cache.mu.Unlock()
	if polluted != afterEvict {
		t.Fatalf("a detached entry must not charge retained: %d -> %d", afterEvict, polluted)
	}
	if readmitted {
		t.Fatal("a detached entry must not be re-admitted by commitRenderPut")
	}

	// A purge detaches every entry the same way.
	if _, err := cache.loadAndRender(path, "variant_accounting.go", 80, nil, "", cache.generation(), zeroTheme, 0, nil, 0); err != nil {
		t.Fatalf("reload: %v", err)
	}
	cache.mu.Lock()
	purged := cache.items[path].Value.(*fileViewCachedEntry)
	cache.mu.Unlock()
	cache.clear()
	cache.commitRenderPut(purged, "80:purged", strings.Repeat("y", 512))
	cache.mu.Lock()
	retainedAfterPurge := cache.retained
	purgedItems := len(cache.items)
	cache.mu.Unlock()
	if retainedAfterPurge != 0 || purgedItems != 0 {
		t.Fatalf("a purged entry must not charge retained: retained=%d items=%d", retainedAfterPurge, purgedItems)
	}
}

func TestFileViewReviewBTWInvalidation(t *testing.T) {
	for _, parentResult := range []bool{false, true} {
		scenario := "side_result_after_closing_inherited_view"
		if parentResult {
			scenario = "parent_result_with_distinct_visible_side_file"
		}
		t.Run(scenario, func(t *testing.T) {
			resetFileViewCacheForTest()
			m := newBTWTestModel(t)
			m.cwd = t.TempDir()
			write := func(name, content string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(m.cwd, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write("parent.txt", "parent before\n")
			write("side.txt", "side before\n")
			m.activeRunID = 1
			m = testOpenFile(m, "parent.txt")
			side, cmd := m.handleBTWCommand("")
			if !side.btw.active {
				t.Fatal("fork failed")
			}
			side = deliverCommandMessages(t, side, cmd)
			side = side.exitFileView()
			if parentResult {
				side = testOpenFile(side, "side.txt")
			}
			write("parent.txt", "parent after\n")
			write("side.txt", "side after\n")
			var msg tea.Msg = bashResultMsg{output: "done"}
			if parentResult {
				msg = agentRowMsg{runID: 1, row: transcriptRow{kind: rowToolResult, tool: "bash", status: tools.StatusOK}}
			}
			next, cmd := side.Update(msg)
			side = deliverCommandMessages(t, next.(model), cmd)
			if parentResult && !strings.Contains(plainRender(t, side.renderFileViewFull(80)), "side after") {
				t.Fatal("visible side did not recover after parent result")
			}
			parent, cmd := side.leaveBTW()
			parent = deliverCommandMessages(t, parent, cmd)
			if !strings.Contains(plainRender(t, parent.renderFileViewFull(80)), "parent after") {
				t.Fatal("restored parent did not recover")
			}
		})
	}
}

func TestFileViewReviewAbsolutePosition(t *testing.T) {
	for _, count := range []int{220, 60, 200} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			resetFileViewCacheForTest()
			m := filesPanelTestModel()
			m.cwd = t.TempDir()
			m.altScreen = true
			m.width = 100
			m.height = 30
			write := func(n int) {
				t.Helper()
				var body strings.Builder
				for i := 0; i < n; i++ {
					fmt.Fprintf(&body, "numbered-line-%03d\n", i)
				}
				if err := os.WriteFile(filepath.Join(m.cwd, "lines.txt"), []byte(body.String()), 0600); err != nil {
					t.Fatal(err)
				}
			}
			write(200)
			m = testOpenFile(m, "lines.txt")
			// The shell is still running; the reader scrolls before its result arrives.
			m.pending = true
			m = m.scrollChat(50)
			m = m.syncChatScroll()
			before := plainRender(t, m.View().Content)
			if !strings.Contains(before, "numbered-line-149") {
				t.Fatal("scrolled viewport must visibly include line 149 before refresh")
			}
			write(count)
			next, cmd := m.Update(bashResultMsg{output: "done"})
			m = deliverCommandMessages(t, next.(model), cmd)
			want := maxInt(0, 50+count-200)
			if m.chatScrollOffset != want {
				t.Fatalf("offset=%d want=%d", m.chatScrollOffset, want)
			}
			after := plainRender(t, m.View().Content)
			if count >= 200 {
				for i := 0; i < 200; i++ {
					line := fmt.Sprintf("numbered-line-%03d", i)
					if strings.Contains(before, line) != strings.Contains(after, line) {
						t.Fatalf("visible line changed: %s", line)
					}
				}
			} else if !strings.Contains(after, "numbered-line-059") {
				t.Fatal("shrink must clamp to tail")
			}
			next, cmd = m.Update(tea.WindowSizeMsg{Width: 90, Height: 30})
			m = deliverCommandMessages(t, next.(model), cmd)
			if m.chatScrollOffset != want {
				t.Fatalf("same height resize changed offset to %d", m.chatScrollOffset)
			}
		})
	}
}

func TestFileViewReviewMarkerReplacement(t *testing.T) {
	for _, entry := range []string{"compact", "resume", "picker"} {
		t.Run(entry, func(t *testing.T) {
			resetFileViewCacheForTest()
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", home)
			t.Setenv("XDG_CACHE_HOME", home)
			t.Setenv("XDG_STATE_HOME", home)
			t.Setenv("APPDATA", home)
			t.Setenv("LOCALAPPDATA", home)
			m := newBTWTestModel(t)
			m.cwd = t.TempDir()
			if err := os.WriteFile(filepath.Join(m.cwd, "marked.go"), []byte("var markedValue = 42\n"), 0600); err != nil {
				t.Fatal(err)
			}
			preview := "--- a/marked.go\n+++ b/marked.go\n@@ -1,1 +1,1 @@\n-old value\n+var markedValue = 42"
			_, err := m.sessionStore.AppendEvent(m.activeSession.SessionID, sessions.AppendEventInput{Type: sessions.EventToolResult, Payload: map[string]any{"changedFiles": []string{"marked.go"}, "toolCallId": "edit-review", "name": "edit_file", "status": "ok", "output": "Successfully edited marked.go (replaced 1 occurrence).", "displayPreview": preview}})
			if err != nil {
				t.Fatal(err)
			}
			m, text := m.handleResumeCommand(m.activeSession.SessionID)
			if text != "" {
				t.Fatal(text)
			}
			m = testOpenFile(m, "marked.go")
			m = testSetMode(m, fileViewFull)
			marker := func(m model) bool { return strings.Contains(plainRender(t, m.renderFileViewFull(80)), "▎") }
			if !marker(m) {
				t.Fatal("initial gutter marker missing")
			}
			var next tea.Model
			var cmd tea.Cmd
			if entry == "compact" {
				m, cmd = m.startFileViewLoadCmd(m.chatColumnWidth())
				stale := cmd()
				m.compactInFlight = true
				next, cmd = m.Update(compactResultMsg{hasSessionSnapshot: true, activeSession: m.activeSession, transcript: initialTranscript()})
				m = deliverCommandMessages(t, next.(model), cmd)
				next, _ = m.Update(stale)
				m = next.(model)
				if marker(m) || len(m.fileViewChangedLines()) != 0 {
					t.Fatal("compaction retained obsolete gutter")
				}
			} else {
				next, cmd = m.dispatchCommand(parsedCommand{kind: commandClear})
				m = deliverCommandMessages(t, next.(model), cmd)
				if marker(m) {
					t.Fatal("clear retained marker")
				}
				m, cmd = m.startFileViewLoadCmd(m.chatColumnWidth())
				stale := cmd()
				if entry == "resume" {
					next, cmd = m.dispatchCommand(parsedCommand{kind: commandResume, text: m.activeSession.SessionID})
				} else {
					m.picker = &commandPicker{kind: pickerSession, items: []pickerItem{{Value: m.activeSession.SessionID}}}
					next, cmd = m.choosePicker()
				}
				m = deliverCommandMessages(t, next.(model), cmd)
				next, _ = m.Update(stale)
				m = next.(model)
				if !marker(m) || !m.fileViewChangedLines()["var markedValue = 42"] {
					t.Fatal("same-session resume did not restore gutter")
				}
			}
		})
	}
}

// TestFileViewSpecialistCompletionRefreshesUnknownScope covers the hidden
// successful Task result: specialistCompleteMsg is the only boundary that sees
// the child's mutation, so it must conservatively reload an open full view even
// outside a git workspace.
func TestFileViewSpecialistCompletionRefreshesUnknownScope(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	name := "specialist_mut.go"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m.activeRunID = 9
	// Non-git workspace: the end-of-turn sweep cannot help here.
	m.gitFileBaseline = nil
	m.gitSweepUnavailable = true
	m = testOpenFile(m, name)
	if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "package old") {
		t.Fatal("initial snapshot must render package old")
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("package new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	updated, cmd := m.Update(specialistCompleteMsg{
		runID:          9,
		toolCallID:     "call-specialist",
		childSessionID: "sess-specialist",
		status:         specialistCompleted,
	})
	m = updated.(model)
	if cmd == nil {
		t.Fatal("specialist completion must schedule a conservative refresh")
	}
	m = deliverCommandMessages(t, m, cmd)
	got := plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(got, "package new") {
		t.Fatalf("hidden specialist mutation left a stale snapshot: %s", got)
	}
}

// TestFileViewTerminalSessionResultRefreshesUnknownScope covers tool results
// that mutate the workspace but report no ChangedFiles.
func TestFileViewTerminalSessionResultRefreshesUnknownScope(t *testing.T) {
	for _, tool := range []string{"terminal_session", "write_stdin", "swarm_collect", "swarm_status"} {
		t.Run(tool, func(t *testing.T) {
			resetFileViewCacheForTest()
			dir := t.TempDir()
			name := "terminal_mut.go"
			if err := os.WriteFile(filepath.Join(dir, name), []byte("package old\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			m := filesPanelTestModel()
			m.cwd = dir
			m.activeRunID = 11
			m.gitFileBaseline = nil
			m.gitSweepUnavailable = true
			m = testOpenFile(m, name)
			if err := os.WriteFile(filepath.Join(dir, name), []byte("package new\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			updated, cmd := m.Update(agentRowMsg{
				runID: 11,
				row: transcriptRow{
					kind:   rowToolResult,
					tool:   tool,
					status: tools.StatusOK,
				},
			})
			m = updated.(model)
			if cmd == nil {
				t.Fatalf("%s result with no changedFiles must still invalidate the open view", tool)
			}
			m = deliverCommandMessages(t, m, cmd)
			got := plainRender(t, m.renderFileViewFull(80))
			if !strings.Contains(got, "package new") {
				t.Fatalf("%s mutation left a stale snapshot: %s", tool, got)
			}
		})
	}
}

// TestFileViewTaskOutputSuppressedResultRefreshesUnknownScope covers the hidden
// TaskOutput success boundary delivered as unknownScopeMutationMsg.
func TestFileViewTaskOutputSuppressedResultRefreshesUnknownScope(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	name := "taskoutput_mut.go"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m.activeRunID = 13
	m.gitFileBaseline = nil
	m.gitSweepUnavailable = true
	m = testOpenFile(m, name)
	if !toolResultCardSuppressedInTranscript("TaskOutput", tools.StatusOK) || !toolResultMayMutateUnknownScope("TaskOutput") {
		t.Fatal("TaskOutput must be classified as a suppressed unknown-scope mutator")
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("package new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	updated, cmd := m.Update(unknownScopeMutationMsg{runID: 13})
	m = updated.(model)
	if cmd == nil {
		t.Fatal("suppressed TaskOutput completion must schedule a conservative refresh")
	}
	m = deliverCommandMessages(t, m, cmd)
	got := plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(got, "package new") {
		t.Fatalf("hidden TaskOutput mutation left a stale snapshot: %s", got)
	}
}

// TestFileViewTurnEndRefreshesInNonGitWorkspace verifies the end-of-turn
// fallback: maybeGitSweep does nothing outside git, but an open view still
// reloads so an unreported mutation cannot stay stale.
func TestFileViewTurnEndRefreshesInNonGitWorkspace(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	name := "turn_end.go"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m.activeRunID = 17
	m.pending = true
	m.gitFileBaseline = nil
	m.gitSweepUnavailable = true
	m = testOpenFile(m, name)
	if err := os.WriteFile(filepath.Join(dir, name), []byte("package new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	updated, cmd := m.Update(agentResponseMsg{
		runID: 17,
		rows:  []transcriptRow{{kind: rowAssistant, text: "done", final: true}},
	})
	m = updated.(model)
	if cmd == nil {
		t.Fatal("turn end must schedule a conservative refresh even without git")
	}
	m = deliverCommandMessages(t, m, cmd)
	got := plainRender(t, m.renderFileViewFull(80))
	if !strings.Contains(got, "package new") {
		t.Fatalf("turn-end mutation left a stale snapshot outside git: %s", got)
	}
}

// TestFileViewPendingReloadAcceptsScrollInput verifies that scrolling while an
// async reload is in flight updates the pending intent against the last
// completed body, so a Page Down is not discarded by the one-line placeholder.
func TestFileViewPendingReloadAcceptsScrollInput(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	name := "pending_scroll.go"
	var body strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&body, "line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m.altScreen = true
	m.width = 100
	m.height = 30
	m = testOpenFile(m, name)
	if !m.fileView.snapshotReady {
		t.Fatal("expected the initial full-file snapshot")
	}
	m.chatScrollOffset = 50
	// Trigger an async reload via resize, exactly like the review scenario.
	updated, batchCmd := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(model)
	if !m.fileView.loading {
		t.Fatal("reload should be pending after the resize")
	}
	before := m.fileView.preservedScrollOffset
	// Page Down while the reload is in flight: intent must move toward the tail.
	updated, _ = m.Update(testKey(tea.KeyPgDown))
	m = updated.(model)
	if m.fileView.preservedScrollOffset >= before {
		t.Fatalf("Page Down during a pending reload must lower the intent: before=%d after=%d", before, m.fileView.preservedScrollOffset)
	}
	if m.chatScrollOffset != m.fileView.preservedScrollOffset {
		t.Fatalf("syncChatScroll must hold the updated intent: offset=%d intent=%d", m.chatScrollOffset, m.fileView.preservedScrollOffset)
	}
	want := m.fileView.preservedScrollOffset
	m = deliverCommandMessages(t, m, batchCmd)
	if m.fileView.loading {
		t.Fatal("expected the reload to complete")
	}
	if m.chatScrollOffset != want && want > 0 {
		t.Fatalf("completed reload must apply the latest intent: got %d, want %d", m.chatScrollOffset, want)
	}
}

// TestFileViewPendingReloadPromptSubmissionReturnsToBottom verifies that a real
// prompt submitted while a reload is pending records the bottom intent, so
// completion cannot restore the reader's pre-submission position.
func TestFileViewPendingReloadPromptSubmissionReturnsToBottom(t *testing.T) {
	resetFileViewCacheForTest()
	dir := t.TempDir()
	name := "pending_submit.go"
	var body strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&body, "line %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	m := filesPanelTestModel()
	m.cwd = dir
	m.altScreen = true
	m.width = 100
	m.height = 30
	m = testOpenFile(m, name)
	m.chatScrollOffset = 50
	updated, batchCmd := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = updated.(model)
	if !m.fileView.loading {
		t.Fatal("reload should be pending after the resize")
	}
	m.input.SetValue("/help")
	updated, _ = m.handleSubmit()
	m = updated.(model)
	if m.fileView.preservedScrollOffset != 0 {
		t.Fatalf("prompt submission during a pending reload must record the bottom intent, got %d", m.fileView.preservedScrollOffset)
	}
	if m.chatScrollOffset != 0 {
		t.Fatalf("prompt submission must snap the viewport to the bottom, got %d", m.chatScrollOffset)
	}
	m = deliverCommandMessages(t, m, batchCmd)
	if m.chatScrollOffset != 0 {
		t.Fatalf("completed reload must respect the bottom intent, got %d", m.chatScrollOffset)
	}
}

// TestFileViewCrossSurfacePathMutationOutsideGit covers the review matrix for a
// workspace with no git sweep: a path-reporting tool result on one surface must
// reload the other surface's open full-file view immediately, without a turn-end
// fallback or a git scan.
func TestFileViewCrossSurfacePathMutationOutsideGit(t *testing.T) {
	const (
		before = "package before\n"
		after  = "package after\n"
	)
	writeFile := func(t *testing.T, dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	nonGit := func(t *testing.T, dir string) model {
		t.Helper()
		m := newBTWTestModel(t)
		m.cwd = dir
		m.width = 100
		m.height = 30
		m.gitFileBaseline = nil
		m.gitSweepUnavailable = true
		return m
	}
	for _, tool := range []string{"edit_file", "write_file", "apply_patch"} {
		t.Run("parent_"+tool+"_refreshes_side", func(t *testing.T) {
			resetFileViewCacheForTest()
			dir := t.TempDir()
			name := "side_shown.go"
			writeFile(t, dir, name, before)
			m := nonGit(t, dir)
			m.pending = true
			m.runID = 4
			m.activeRunID = 4
			side, cmd := m.handleBTWCommand("")
			if cmd != nil {
				t.Fatal("bare /btw must not start work")
			}
			if !side.btw.active || side.btw.parent == nil || side.btw.parent.activeRunID != 4 {
				t.Fatal("parent run was not preserved")
			}
			side = testOpenFile(side, name)
			if side.fileView.mode != fileViewFull {
				t.Fatal("side must open the untouched file in full mode")
			}
			if !strings.Contains(plainRender(t, side.renderFileViewFull(80)), "package before") {
				t.Fatal("side snapshot must show the pre-mutation content")
			}
			writeFile(t, dir, name, after)
			updated, cmd := side.Update(agentRowMsg{
				runID: 4,
				row: transcriptRow{
					kind:         rowToolResult,
					tool:         tool,
					status:       tools.StatusOK,
					changedFiles: []string{"other.go", name},
				},
			})
			side = updated.(model)
			if cmd == nil {
				t.Fatalf("%s on the parent must schedule a side reload without a git sweep", tool)
			}
			side = deliverCommandMessages(t, side, cmd)
			got := plainRender(t, side.renderFileViewFull(80))
			if !strings.Contains(got, "package after") || strings.Contains(got, "package before") {
				t.Fatalf("%s left the side stale outside git: %s", tool, got)
			}
		})
		t.Run("side_"+tool+"_refreshes_parent", func(t *testing.T) {
			resetFileViewCacheForTest()
			dir := t.TempDir()
			name := "parent_shown.go"
			writeFile(t, dir, name, before)
			m := nonGit(t, dir)
			m = testOpenFile(m, name)
			if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "package before") {
				t.Fatal("parent snapshot must show the pre-mutation content")
			}
			side, _ := m.handleBTWCommand("")
			if !side.btw.active || side.btw.parent == nil {
				t.Fatal("fork failed")
			}
			// Only the hidden parent keeps the file open. The inherited side load
			// is revoked here and must not be delivered.
			side = side.exitFileView()
			side.activeRunID = side.btw.sideRunIDBase
			side.runID = side.btw.sideRunIDBase
			writeFile(t, dir, name, after)
			updated, cmd := side.Update(agentRowMsg{
				runID: side.activeRunID,
				row: transcriptRow{
					kind:         rowToolResult,
					tool:         tool,
					status:       tools.StatusOK,
					changedFiles: []string{name},
				},
			})
			side = updated.(model)
			if cmd == nil {
				t.Fatalf("%s on the side must schedule a parent reload immediately", tool)
			}
			side = deliverCommandMessages(t, side, cmd)
			if side.btw.parent == nil {
				t.Fatal("hidden parent disappeared")
			}
			got := plainRender(t, side.btw.parent.renderFileViewFull(80))
			if !strings.Contains(got, "package after") || strings.Contains(got, "package before") {
				t.Fatalf("%s left the parent stale outside git: %s", tool, got)
			}
			if side.btw.parent.fileView.loading {
				t.Fatal("parent reload must have settled before returning from BTW")
			}
		})
	}
	t.Run("unrelated_path_does_not_reload_side", func(t *testing.T) {
		resetFileViewCacheForTest()
		dir := t.TempDir()
		writeFile(t, dir, "keep.go", before)
		m := nonGit(t, dir)
		m.pending = true
		m.runID = 4
		m.activeRunID = 4
		side, _ := m.handleBTWCommand("")
		side = testOpenFile(side, "keep.go")
		updated, cmd := side.Update(agentRowMsg{
			runID: 4,
			row: transcriptRow{
				kind:         rowToolResult,
				tool:         "edit_file",
				status:       tools.StatusOK,
				changedFiles: []string{"other.go"},
			},
		})
		side = updated.(model)
		if cmd != nil || side.fileView.loading || !side.fileView.snapshotReady {
			t.Fatal("a path the side is not showing must not reload its snapshot")
		}
		got := plainRender(t, side.renderFileViewFull(80))
		if !strings.Contains(got, "package before") {
			t.Fatalf("unrelated mutation disturbed the side snapshot: %s", got)
		}
	})
}

// TestFileViewCrossSurfaceMixedWriteStdinOutsideGit verifies that a mixed
// write_stdin result carrying both an exact changed path and an aggregated
// directory summary (e.g. dist/) refreshes both BTW surfaces outside a git
// workspace, whether the exact path is open on the parent or on the side.
func TestFileViewCrossSurfaceMixedWriteStdinOutsideGit(t *testing.T) {
	const (
		exactBefore = "export const exact = 'before';\n"
		exactAfter  = "export const exact = 'after';\n"
		distBefore  = "console.log('dist before');\n"
		distAfter   = "console.log('dist after');\n"
	)
	writeFile := func(t *testing.T, dir, rel, content string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	nonGit := func(t *testing.T, dir string) model {
		t.Helper()
		m := newBTWTestModel(t)
		m.cwd = dir
		m.width = 100
		m.height = 30
		m.gitFileBaseline = nil
		m.gitSweepUnavailable = true
		return m
	}
	mixedRow := func() transcriptRow {
		return transcriptRow{
			kind:         rowToolResult,
			tool:         "write_stdin",
			status:       tools.StatusOK,
			changedFiles: []string{"src/exact.ts"},
			changeSummaries: []execution.Change{
				{Path: "dist/", Kind: execution.ChangeCreated, Aggregated: true},
			},
		}
	}

	t.Run("parent_mixed_write_stdin_refreshes_visible_side_under_dist", func(t *testing.T) {
		resetFileViewCacheForTest()
		dir := t.TempDir()
		writeFile(t, dir, "src/exact.ts", exactBefore)
		writeFile(t, dir, "dist/bundle.js", distBefore)

		m := nonGit(t, dir)
		m.pending = true
		m.runID = 4
		m.activeRunID = 4
		// Parent holds the exact path; side opens a generated file under dist/.
		m = testOpenFile(m, "src/exact.ts")
		if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "before") {
			t.Fatal("parent snapshot must show initial exact file")
		}

		side, cmd := m.handleBTWCommand("")
		side = deliverCommandMessages(t, side, cmd)
		side = side.exitFileView()
		side = testOpenFile(side, "dist/bundle.js")
		if !strings.Contains(plainRender(t, side.renderFileViewFull(80)), "dist before") {
			t.Fatal("side snapshot must show initial dist/bundle.js")
		}

		writeFile(t, dir, "src/exact.ts", exactAfter)
		writeFile(t, dir, "dist/bundle.js", distAfter)

		updated, cmd := side.Update(agentRowMsg{runID: 4, row: mixedRow()})
		side = updated.(model)
		if cmd == nil {
			t.Fatal("mixed write_stdin on parent must schedule reloads for both surfaces")
		}
		side = deliverCommandMessages(t, side, cmd)

		gotSide := plainRender(t, side.renderFileViewFull(80))
		if !strings.Contains(gotSide, "dist after") || strings.Contains(gotSide, "dist before") {
			t.Fatalf("visible side under dist/ stayed stale after parent mixed write_stdin: %s", gotSide)
		}
		parent, leaveCmd := side.leaveBTW()
		parent = deliverCommandMessages(t, parent, leaveCmd)
		gotParent := plainRender(t, parent.renderFileViewFull(80))
		if !strings.Contains(gotParent, "after") || strings.Contains(gotParent, "before") {
			t.Fatalf("restored parent exact path stayed stale after parent mixed write_stdin: %s", gotParent)
		}
	})

	t.Run("side_mixed_write_stdin_refreshes_hidden_parent_under_dist", func(t *testing.T) {
		resetFileViewCacheForTest()
		dir := t.TempDir()
		writeFile(t, dir, "src/exact.ts", exactBefore)
		writeFile(t, dir, "dist/bundle.js", distBefore)

		m := nonGit(t, dir)
		// Parent holds the file under dist/; side holds the exact path.
		m = testOpenFile(m, "dist/bundle.js")
		if !strings.Contains(plainRender(t, m.renderFileViewFull(80)), "dist before") {
			t.Fatal("parent snapshot must show initial dist/bundle.js")
		}

		side, cmd := m.handleBTWCommand("")
		side = deliverCommandMessages(t, side, cmd)
		side = side.exitFileView()
		side = testOpenFile(side, "src/exact.ts")
		if !strings.Contains(plainRender(t, side.renderFileViewFull(80)), "before") {
			t.Fatal("side snapshot must show initial src/exact.ts")
		}
		side.activeRunID = side.btw.sideRunIDBase
		side.runID = side.btw.sideRunIDBase

		writeFile(t, dir, "src/exact.ts", exactAfter)
		writeFile(t, dir, "dist/bundle.js", distAfter)

		updated, cmd := side.Update(agentRowMsg{runID: side.activeRunID, row: mixedRow()})
		side = updated.(model)
		if cmd == nil {
			t.Fatal("mixed write_stdin on side must schedule reloads for both surfaces")
		}
		side = deliverCommandMessages(t, side, cmd)

		gotSide := plainRender(t, side.renderFileViewFull(80))
		if !strings.Contains(gotSide, "after") || strings.Contains(gotSide, "before") {
			t.Fatalf("side exact path stayed stale after side mixed write_stdin: %s", gotSide)
		}
		if side.btw.parent == nil {
			t.Fatal("hidden parent disappeared")
		}
		if side.btw.parent.fileView.loading {
			t.Fatal("hidden parent reload must settle before returning from BTW")
		}
		gotHiddenParent := plainRender(t, side.btw.parent.renderFileViewFull(80))
		if !strings.Contains(gotHiddenParent, "dist after") || strings.Contains(gotHiddenParent, "dist before") {
			t.Fatalf("hidden parent under dist/ stayed stale after side mixed write_stdin: %s", gotHiddenParent)
		}

		parent, leaveCmd := side.leaveBTW()
		parent = deliverCommandMessages(t, parent, leaveCmd)
		gotParent := plainRender(t, parent.renderFileViewFull(80))
		if !strings.Contains(gotParent, "dist after") || strings.Contains(gotParent, "dist before") {
			t.Fatalf("restored parent under dist/ stayed stale on return from BTW: %s", gotParent)
		}
	})
}

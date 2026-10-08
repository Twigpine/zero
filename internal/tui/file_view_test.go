package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/agent"
	"github.com/Gitlawb/zero/internal/sessions"
	"github.com/Gitlawb/zero/internal/tools"
)

// TestFileViewOpenExitRestoresScroll: opening saves the chat scroll position,
// resets it for the file body, and Esc restores it; switching files while open
// keeps the ORIGINAL saved position (not the file view's own).
func TestFileViewOpenExitRestoresScroll(t *testing.T) {
	m := filesPanelTestModel()
	m.chatScrollOffset = 12

	m = m.openFileView("web/app.js")
	if !m.fileView.active || m.fileView.mode != fileViewDiff {
		t.Fatalf("open should activate in diff mode: %+v", m.fileView)
	}
	if m.chatScrollOffset != 0 || m.fileView.parentScrollOffset != 12 {
		t.Fatalf("open should reset scroll and save the parent offset: offset=%d saved=%d", m.chatScrollOffset, m.fileView.parentScrollOffset)
	}

	m.chatScrollOffset = 5 // scrolled within the file body
	m = m.openFileView("internal/tui/sidebar.go")
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
	m = m.openFileView("web/app.js")

	updated, _ := m.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	m = updated.(model)
	if m.fileView.mode != fileViewFull {
		t.Fatal("f should switch to full mode")
	}
	updated, _ = m.Update(tea.KeyPressMsg{Code: 'd', Text: "d"})
	m = updated.(model)
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
	m = m.openFileView("internal/tui/sidebar.go")
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
	m = m.openFileView("app.js")
	m = m.setFileViewMode(fileViewFull)

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
	m = m.openFileView("internal/tui/sidebar.go")

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
	m = m.openFileView("web/app.js")

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
	m = m.openFileView("web/app.js")
	m = m.setFileViewMode(fileViewFull)
	m.chatScrollOffset = 7 // scrolled within the file body

	m = m.openFileView("web/app.js")
	if m.fileView.mode != fileViewFull {
		t.Fatal("re-opening the same file must keep full mode")
	}
	if m.chatScrollOffset != 7 {
		t.Fatalf("re-opening the same file must keep the scroll, got %d", m.chatScrollOffset)
	}
	// A DIFFERENT file still switches (and resets to diff mode as documented).
	m = m.openFileView("internal/tui/sidebar.go")
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
	m = m.openFileView("big.txt")

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
	m = m.openFileView("web/app.js")

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
	m = m.openFileView("web/app.js")

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
	m = m.openFileView("web/app.js")
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

package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Gitlawb/zero/internal/execution"
	"github.com/Gitlawb/zero/internal/tools"
)

// filesPanelTestModel is sidebarTestModel plus a couple of touched files: one
// created, one edited twice (the second touch most recent).
func filesPanelTestModel() model {
	m := sidebarTestModel()
	m.transcript = append(m.transcript,
		transcriptRow{kind: rowToolResult, tool: "write_file", id: "f1", status: tools.StatusOK,
			text:         "tool result: write_file ok Created web/app.js (10 lines).",
			detail:       "+let a = 1\n+let b = 2",
			changedFiles: []string{"web/app.js"}},
		transcriptRow{kind: rowToolResult, tool: "edit_file", id: "f2", status: tools.StatusOK,
			text:         "tool result: edit_file ok Edited internal/tui/sidebar.go.",
			detail:       "+added one\n-removed one",
			changedFiles: []string{"internal/tui/sidebar.go"}},
		transcriptRow{kind: rowToolResult, tool: "edit_file", id: "f3", status: tools.StatusOK,
			text:         "tool result: edit_file ok Edited internal/tui/sidebar.go.",
			detail:       "+two\n+three\n-gone",
			changedFiles: []string{"internal/tui/sidebar.go"}},
	)
	return m
}

func TestGeneratedTreeSummaryIsVisibleButNotClickable(t *testing.T) {
	m := sidebarTestModel()
	m.transcript = append(m.transcript, transcriptRow{
		kind: rowToolResult, status: tools.StatusOK,
		changeSummaries: []execution.Change{{Path: "node_modules/", Kind: execution.ChangeCreated, Aggregated: true}},
	})
	lines, hits := m.sidebarFileLines(34)
	if got := plainRender(t, strings.Join(lines, "\n")); !strings.Contains(got, "Σ") || !strings.Contains(got, "node_modules/") {
		t.Fatalf("generated summary missing from Files panel: %q", got)
	}
	if len(hits) != 0 {
		t.Fatalf("generated tree must not be selectable as a file: %#v", hits)
	}
}

// TestTouchedFilesAggregates: the roster is recovered from tool-result rows,
// newest-touch first, with per-file diffstat totals, edit counts, and the
// created badge from write_file's confirmation text.
func TestTouchedFilesAggregates(t *testing.T) {
	m := filesPanelTestModel()
	files := m.touchedFiles()
	if len(files) != 2 {
		t.Fatalf("expected 2 touched files, got %d: %+v", len(files), files)
	}
	// sidebar.go was touched last, so it lists first.
	if files[0].path != "internal/tui/sidebar.go" {
		t.Fatalf("most recently touched file should list first, got %q", files[0].path)
	}
	if files[0].adds != 3 || files[0].dels != 2 || files[0].edits != 2 {
		t.Errorf("sidebar.go diffstat = +%d −%d over %d edits, want +3 −2 over 2", files[0].adds, files[0].dels, files[0].edits)
	}
	if files[0].created {
		t.Error("edited file must not read as created")
	}
	if !files[1].created {
		t.Error("write_file 'Created …' result should mark the file created")
	}
	if files[0].lastRowIndex != len(m.transcript)-1 {
		t.Errorf("lastRowIndex = %d, want the final transcript row", files[0].lastRowIndex)
	}
}

// TestSidebarFileLinesRenderAndOverflow: rows carry the badge + left-truncated
// path + diffstat; beyond maxSidebarFiles the tail collapses into "+N more";
// hits index only real file rows.
func TestSidebarFileLinesRenderAndOverflow(t *testing.T) {
	m := filesPanelTestModel()
	lines, hits := m.sidebarFileLines(34)
	joined := plainRender(t, strings.Join(lines, "\n"))
	if !strings.Contains(joined, "sidebar.go") || !strings.Contains(joined, "app.js") {
		t.Fatalf("file rows missing paths:\n%s", joined)
	}
	if !strings.Contains(joined, "+3 −2") {
		t.Errorf("expected aggregated diffstat +3 −2:\n%s", joined)
	}
	if !strings.Contains(joined, "A") || !strings.Contains(joined, "M") {
		t.Errorf("expected A and M badges:\n%s", joined)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 clickable hits, got %d", len(hits))
	}
	if hits[0].path != "internal/tui/sidebar.go" {
		t.Errorf("first hit should be the newest file, got %q", hits[0].path)
	}

	// Overflow: 8 distinct files -> maxSidebarFiles rows + a "+2 more" trailer.
	over := sidebarTestModel()
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		over.transcript = append(over.transcript, transcriptRow{
			kind: rowToolResult, tool: "edit_file", id: name, status: tools.StatusOK,
			changedFiles: []string{name + ".go"},
		})
	}
	overLines, overHits := over.sidebarFileLines(34)
	if len(overHits) != maxSidebarFiles {
		t.Fatalf("expected %d clickable rows, got %d", maxSidebarFiles, len(overHits))
	}
	if got := plainRender(t, overLines[len(overLines)-1]); !strings.Contains(got, "+2 more") {
		t.Errorf("expected '+2 more' trailer, got %q", got)
	}
}

// TestSidebarFilesLivePulseRow: while a mutating tool call streams its args,
// the file it is writing shows as a pinned (unclickable) pulse row.
func TestSidebarFilesLivePulseRow(t *testing.T) {
	m := filesPanelTestModel()
	m.streamCallName = "write_file"
	m.streamCallDecoder = newStreamingDecoder()
	m.streamCallDecoder.feed(`{"path":"web/new.css","content":"body{}`)

	lines, hits := m.sidebarFileLines(34)
	if got := plainRender(t, lines[0]); !strings.Contains(got, "web/new.css") {
		t.Fatalf("live write should pin its path atop FILES, got %q", got)
	}
	for _, hit := range hits {
		if hit.path == "web/new.css" {
			t.Error("the live row must not be clickable (no result exists yet)")
		}
	}
	// A non-mutating streaming call shows no pulse row.
	m.streamCallName = "read_file"
	if m.liveEditingPath() != "" {
		t.Error("read_file must not read as a live edit")
	}
}

// TestSidebarFileSelectablesMatchRenderedRows: the hit offsets computed by
// sidebarFileSelectables must land exactly on the rendered FILES rows in
// renderContextSidebar's output — the same invariant the mouse hit-test relies
// on. Asserted against the real render, not a re-derivation of the math.
func TestSidebarFileSelectablesMatchRenderedRows(t *testing.T) {
	m := filesPanelTestModel()
	width := sidebarWidth(m.width)
	rendered := m.renderContextSidebar(width, m.height)
	hits := m.sidebarFileSelectables(width)
	if len(hits) == 0 {
		t.Fatal("expected clickable FILES rows")
	}
	for _, hit := range hits {
		if hit.lineOffset >= len(rendered) {
			t.Fatalf("hit offset %d beyond rendered sidebar (%d lines)", hit.lineOffset, len(rendered))
		}
		line := plainRender(t, rendered[hit.lineOffset])
		base := hit.path[strings.LastIndex(hit.path, "/")+1:]
		if !strings.Contains(line, base) {
			t.Errorf("hit for %q points at line %d which reads %q", hit.path, hit.lineOffset, line)
		}
	}
}

// TestRunDetailsListsTouchedFiles keeps edited files discoverable without a
// permanent column beside the conversation.
func TestRunDetailsListsTouchedFiles(t *testing.T) {
	m := filesPanelTestModel()
	m.runDetailsOpen = true
	plain := plainRender(t, m.runDetailsOverlay(m.width))
	for _, file := range m.touchedFiles() {
		if !strings.Contains(plain, file.path) {
			t.Errorf("run details missing touched file %q:\n%s", file.path, plain)
		}
	}
}

// TestRowTouchesSelectedFileTint: only tool-result rows whose changedFiles
// carry the selected path tint, and the render cache key differs across
// selection states so the tint can't be served stale.
func TestRowTouchesSelectedFileTint(t *testing.T) {
	m := filesPanelTestModel()
	row := m.transcript[len(m.transcript)-1]
	if m.rowTouchesSelectedFile(row) {
		t.Fatal("nothing selected: no row should tint")
	}
	m.selectedFile = "internal/tui/sidebar.go"
	if !m.rowTouchesSelectedFile(row) {
		t.Fatal("selected file's edit row should tint")
	}
	other := m.transcript[len(m.transcript)-3] // web/app.js
	if m.rowTouchesSelectedFile(other) {
		t.Fatal("an unrelated file's row must not tint")
	}

	rc := buildRowContext(m.transcript)
	opts := cardRenderOptions{bodyCap: cardBodyMaxLines, cwd: m.cwd}
	selectedKey, _ := m.renderRowCacheKey(row, 80, rc, opts, false)
	m.selectedFile = ""
	plainKey, _ := m.renderRowCacheKey(row, 80, rc, opts, false)
	if selectedKey == plainKey {
		t.Error("render cache key must change with the selection state")
	}
}

// TestEscClearsFileSelectionThenNothing: Esc drops an active file selection
// (before any run-level action) and is consumed doing so.
func TestEscClearsFileSelectionThenNothing(t *testing.T) {
	m := filesPanelTestModel()
	m.selectedFile = "web/app.js"
	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if got := updated.(model).selectedFile; got != "" {
		t.Fatalf("Esc should clear the file selection, still %q", got)
	}
}

func TestTruncatePathLeft(t *testing.T) {
	if got := truncatePathLeft("internal/tui/sidebar.go", 40); got != "internal/tui/sidebar.go" {
		t.Errorf("short path should pass through, got %q", got)
	}
	if got := truncatePathLeft("internal/tui/sidebar.go", 18); got != "…/tui/sidebar.go" {
		t.Errorf("want …/tui/sidebar.go, got %q", got)
	}
	if got := truncatePathLeft("internal/tui/sidebar.go", 14); got != "…/sidebar.go" {
		t.Errorf("want …/sidebar.go, got %q", got)
	}
}

// TestTouchedFilesPerFileDiffStat: a multi-file result (apply_patch spanning
// several files) must charge each file its OWN +/- counts from the per-file
// diff sections — not the whole patch's totals to every file it touched.
func TestTouchedFilesPerFileDiffStat(t *testing.T) {
	m := sidebarTestModel()
	patch := "diff --git a/web/one.js b/web/one.js\n" +
		"--- a/web/one.js\n+++ b/web/one.js\n@@ -1,1 +1,2 @@\n+added one\n+added two\n" +
		"diff --git a/web/two.js b/web/two.js\n" +
		"--- a/web/two.js\n+++ b/web/two.js\n@@ -1,2 +1,1 @@\n-gone one\n-gone two\n-gone three\n"
	m.transcript = append(m.transcript, transcriptRow{
		kind: rowToolResult, tool: "apply_patch", id: "p1", status: tools.StatusOK,
		detail:       patch,
		changedFiles: []string{"web/one.js", "web/two.js"},
	})
	byPath := map[string]touchedFile{}
	for _, f := range m.touchedFiles() {
		byPath[f.path] = f
	}
	if f := byPath["web/one.js"]; f.adds != 2 || f.dels != 0 {
		t.Errorf("web/one.js = +%d −%d, want its own +2 −0 (not the patch totals)", f.adds, f.dels)
	}
	if f := byPath["web/two.js"]; f.adds != 0 || f.dels != 3 {
		t.Errorf("web/two.js = +%d −%d, want its own +0 −3 (not the patch totals)", f.adds, f.dels)
	}
}

// TestTouchedFilesRetouchOrdersByLastTouch: touch a, then b, then a again — a
// must list first. The old "reverse first-seen order" put b first because a's
// slot kept its original position.
func TestTouchedFilesRetouchOrdersByLastTouch(t *testing.T) {
	m := sidebarTestModel()
	for _, step := range []struct{ id, path string }{
		{"r1", "a.go"}, {"r2", "b.go"}, {"r3", "a.go"},
	} {
		m.transcript = append(m.transcript, transcriptRow{
			kind: rowToolResult, tool: "edit_file", id: step.id, status: tools.StatusOK,
			changedFiles: []string{step.path},
		})
	}
	files := m.touchedFiles()
	if len(files) != 2 || files[0].path != "a.go" {
		t.Fatalf("the re-touched file must list first, got %+v", files)
	}
}

// TestSidebarFileLinesOverflowExcludesLiveRow: when the live-writing file is
// also a touched file, it renders as the pulse row and is skipped from the
// list — the "+N more" trailer must not count that skipped entry.
func TestSidebarFileLinesOverflowExcludesLiveRow(t *testing.T) {
	m := sidebarTestModel()
	for _, name := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		m.transcript = append(m.transcript, transcriptRow{
			kind: rowToolResult, tool: "edit_file", id: name, status: tools.StatusOK,
			changedFiles: []string{name + ".go"},
		})
	}
	// h.go (a touched file) is ALSO the live write: 7 listable files remain,
	// 6 render, so the trailer is +1 — not +2 (which counted the skipped live row).
	m.streamCallName = "edit_file"
	m.streamCallDecoder = newStreamingDecoder()
	m.streamCallDecoder.feed(`{"path":"h.go","content":"x`)

	lines, _ := m.sidebarFileLines(34)
	trailer := plainRender(t, lines[len(lines)-1])
	if !strings.Contains(trailer, "+1 more") {
		t.Fatalf("overflow must exclude the live row from the count, got %q", trailer)
	}
}

// TestSidebarHasContentForLiveWrite: the sidebar counts an in-flight first
// write as content, so the FILES pulse is visible before any result row lands.
func TestSidebarHasContentForLiveWrite(t *testing.T) {
	m := sidebarTestModel()
	m.plan.steps = nil // drop the helper's seeded plan: no agents/plan/files now
	if m.sidebarHasContent() {
		t.Fatal("session without agents/plan/files should have no sidebar content")
	}
	m.streamCallName = "write_file"
	m.streamCallDecoder = newStreamingDecoder()
	m.streamCallDecoder.feed(`{"path":"web/new.css","content":"body{}`)
	if !m.sidebarHasContent() {
		t.Fatal("a live in-flight write must count as sidebar content")
	}
}

// files_panel.go renders the FILES section of the right context sidebar: the
// workspace files this session has touched, newest first, with an A/M badge and
// a +added/−removed diffstat per file, plus a pulsing row for the file whose
// write is streaming right now. Like the swarm roster (sidebar.go), the touched
// set is not separate model state — it is recovered on demand from the
// transcript's tool-result rows (their changedFiles), so it survives resume for
// free and can never drift from what the chat shows.
//
// The same compact rows feed the run-details overlay; selecting a file from a
// visible transcript card opens the file drill-in (file_view.go).
package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Gitlawb/zero/internal/tools"
)

// maxSidebarFiles caps the FILES rows so the section stays a glanceable set,
// not a scrolling log; older files collapse into a "+N more" trailer.
const maxSidebarFiles = 6

// touchedFile is one workspace file the session has mutated, aggregated across
// every tool-result row that touched it.
type touchedFile struct {
	path         string
	adds         int  // total added lines across its diffs
	dels         int  // total removed lines across its diffs
	edits        int  // number of mutations
	created      bool // first touch created the file (write_file "Created …")
	failed       bool // the latest touch errored
	summarized   bool // generated tree summary; never selectable as a file
	changeKind   string
	lastRowIndex int // transcript index of the most recent result touching it
}

// touchedFiles recovers the session's touched-file roster from the transcript,
// most recently touched first. Recomputed on demand (the value-receiver model
// can't persist a registry from View), mirroring swarmSpawnedAgents; the scan is
// a single pass over the transcript per render, same order as the sidebar's
// other sections.
func (m model) touchedFiles() []touchedFile {
	var files []touchedFile
	index := map[string]int{}
	for i, row := range m.transcript {
		if row.kind != rowToolResult || (len(row.changedFiles) == 0 && len(row.changeSummaries) == 0) {
			continue
		}
		adds, dels := planDiffStat(row.detail)
		// A multi-file result (apply_patch spanning several files) must not charge
		// the WHOLE patch's totals to every file it touched — split the diff by its
		// per-file headers and attribute each file its own counts.
		var perFile map[string][2]int
		if len(row.changedFiles) > 1 {
			perFile = perFileDiffStats(row.detail)
		}
		for _, path := range row.changedFiles {
			if path == "" {
				continue
			}
			at, seen := index[path]
			if !seen {
				index[path] = len(files)
				files = append(files, touchedFile{
					path:         path,
					created:      resultRowCreatedFile(row),
					lastRowIndex: i,
				})
				at = len(files) - 1
			}
			fileAdds, fileDels := adds, dels
			if perFile != nil {
				// Headers were found: use this file's own counts. A path the split
				// couldn't attribute shows 0/0 rather than the inflated patch totals.
				counts := perFile[path]
				fileAdds, fileDels = counts[0], counts[1]
			}
			files[at].adds += fileAdds
			files[at].dels += fileDels
			files[at].edits++
			files[at].failed = row.status == tools.StatusError
			files[at].lastRowIndex = i
		}
		for _, summary := range row.changeSummaries {
			if summary.Path == "" {
				continue
			}
			at, seen := index[summary.Path]
			if !seen {
				index[summary.Path] = len(files)
				files = append(files, touchedFile{path: summary.Path, summarized: true, lastRowIndex: i})
				at = len(files) - 1
			}
			files[at].summarized = true
			files[at].changeKind = string(summary.Kind)
			files[at].edits++
			files[at].failed = row.status == tools.StatusError
			files[at].lastRowIndex = i
		}
	}
	// Most recently touched first: the file being worked on now belongs at the
	// top, like the sidebar's ACTIVITY feed. Sorted by last touch (not reversed
	// first-seen order): a file touched early and touched again last must list
	// first. Stable, so same-row files keep their changedFiles order.
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].lastRowIndex > files[j].lastRowIndex
	})
	// Merge the git sweep's discoveries (bash/subagent mutations that carry no
	// changedFiles — files_git_sweep.go) below the transcript-derived entries,
	// skipping paths a tool result already reported.
	for _, f := range m.gitTouchedFiles() {
		if _, seen := index[f.path]; seen {
			continue
		}
		files = append(files, f)
	}
	return files
}

// perFileDiffStats splits a (possibly multi-file) unified diff into per-path
// +added/−removed counts, keyed by the workspace-relative path from each
// section's "+++ b/…" header (the "--- a/…" old side when the new side is
// /dev/null, i.e. a deletion). Returns nil when the detail carries no file
// headers at all (write_file/edit_file previews may be bare hunks) — the caller
// then falls back to whole-detail totals, which are correct for a single file.
func perFileDiffStats(detail string) map[string][2]int {
	stats := map[string][2]int{}
	current, oldPath := "", ""
	normalize := func(line string, prefix string) string {
		path := strings.TrimSpace(line[4:])
		if tab := strings.IndexByte(path, '\t'); tab >= 0 {
			path = path[:tab] // unified-diff convention: optional timestamp after a tab
		}
		path = unquoteGitPath(path)
		if path == "/dev/null" {
			return ""
		}
		return strings.TrimPrefix(path, prefix)
	}
	for _, line := range strings.Split(detail, "\n") {
		switch {
		case strings.HasPrefix(line, "--- "):
			oldPath = normalize(line, "a/")
		case strings.HasPrefix(line, "+++ "):
			current = normalize(line, "b/")
			if current == "" {
				current = oldPath // deletion: charge the removed lines to the old path
			}
		case current == "":
			// Lines before the first header (or in an unattributable section).
		case strings.HasPrefix(line, "+"):
			counts := stats[current]
			counts[0]++
			stats[current] = counts
		case strings.HasPrefix(line, "-"):
			counts := stats[current]
			counts[1]++
			stats[current] = counts
		}
	}
	if len(stats) == 0 {
		return nil
	}
	return stats
}

// resultRowCreatedFile reports whether a tool-result row reads as a file
// creation ("tool result: write_file ok Created x (…)"), so the FILES row can
// badge it A rather than M. Heuristic on the confirmation text write_file
// already emits; a miss just shows M, never breaks anything.
func resultRowCreatedFile(row transcriptRow) bool {
	return strings.Contains(row.text, " Created ")
}

// liveEditingPath is the path of the file whose mutating tool call is streaming
// its arguments RIGHT NOW (the same live write streamingToolCallView previews),
// or "" when no mutation is in flight. It renders as a pulsing row pinned atop
// the FILES section.
func (m model) liveEditingPath() string {
	if m.streamCallDecoder == nil {
		return ""
	}
	switch m.streamCallName {
	case "write_file", "edit_file", "apply_patch":
		return m.streamCallDecoder.path
	}
	return ""
}

// fileHit marks a rendered FILES row (by its ABSOLUTE index inside the rendered
// sidebar) that is clickable, carrying the file it refers to.
type fileHit struct {
	lineOffset int
	path       string
}

// sidebarFilesHeader renders the FILES section header with the touched count.
func (m model) sidebarFilesHeader(width int) string {
	n := len(m.touchedFiles())
	if n == 0 {
		return sidebarHeader("FILES", width)
	}
	return sidebarHeaderWithCount("FILES", fmt.Sprintf("%d", n), zeroTheme.muted, width)
}

// sidebarFileLines renders the FILES section body: the live "writing" pulse row
// first, then the touched files newest-first, capped at maxSidebarFiles with a
// "+N more" trailer. Returns nil when the session has touched nothing (the
// caller shows a placeholder). The paired hits slice records each clickable
// row's index WITHIN the returned lines.
func (m model) sidebarFileLines(width int) ([]string, []fileHit) {
	files := m.touchedFiles()
	live := m.liveEditingPath()
	if len(files) == 0 && live == "" {
		return nil, nil
	}
	room := maxInt(4, width-3)
	var lines []string
	var hits []fileHit

	// The file being written this instant: an accent pulse, not yet clickable
	// (its result row — the diff — doesn't exist until the call completes).
	if live != "" {
		lines = append(lines, " "+zeroTheme.accent.Render("●")+" "+
			zeroTheme.ink.Render(truncatePathLeft(live, room)))
	}

	// Filter the live row out FIRST so the "+N more" trailer counts only rows
	// that could actually render — counting the skipped live entry would inflate
	// the overflow by one.
	visible := files[:0:0]
	for _, f := range files {
		if f.path != live {
			visible = append(visible, f)
		}
	}
	shown := 0
	for _, f := range visible {
		if shown >= maxSidebarFiles {
			lines = append(lines, "   "+zeroTheme.faint.Render(fmt.Sprintf("+%d more", len(visible)-shown)))
			break
		}
		shown++
		if !f.summarized {
			hits = append(hits, fileHit{lineOffset: len(lines), path: f.path})
		}
		lines = append(lines, m.renderFileRow(f, room))
	}
	return lines, hits
}

// renderFileRow renders one touched file: status badge, left-truncated path,
// and the +/− diffstat, with the selected file carrying an accent marker so the
// selection reads in the sidebar as well as in the chat tint.
func (m model) renderFileRow(f touchedFile, room int) string {
	badge := zeroTheme.muted.Render("M")
	switch {
	case f.summarized:
		badge = zeroTheme.muted.Render("Σ")
	case f.failed:
		badge = zeroTheme.red.Render("✗")
	case f.created:
		badge = zeroTheme.green.Render("A")
	}
	// Reserve the diffstat's width so the path truncates around it.
	stat := ""
	if f.adds > 0 || f.dels > 0 {
		stat = fmt.Sprintf(" +%d −%d", f.adds, f.dels)
	}
	pathRoom := maxInt(4, room-len(stat))
	pathStyle := zeroTheme.muted
	lead := " "
	if m.selectedFile == f.path {
		lead = zeroTheme.accent.Render("▸")
		pathStyle = zeroTheme.ink
	}
	line := lead + badge + " " + pathStyle.Render(truncatePathLeft(f.path, pathRoom))
	if stat != "" {
		line += zeroTheme.faintest.Render(stat)
	}
	return line
}

// sidebarFileSelectables returns the clickable FILES rows with their ABSOLUTE
// index inside the rendered sidebar. The FILES section renders after PLAN in
// renderContextSidebar: AGENTS header + body, blank + PLAN header + body, then
// blank + FILES header, then the file rows. The offset accounting mirrors
// sidebarPlanSelectables exactly, extended one section down.
func (m model) sidebarFileSelectables(width int) []fileHit {
	lines, hits := m.sidebarFileLines(width)
	if len(lines) == 0 {
		return nil
	}
	agentBody := len(m.sidebarAgentLines(width))
	if agentBody == 0 {
		agentBody = 1 // the "no agents spawned" placeholder occupies one line
	}
	planBody := len(m.sidebarPlanLines(width))
	if planBody == 0 {
		planBody = 1 // the "no active plan" placeholder occupies one line
	}
	base := 1 + agentBody + 2 + planBody + 2 // sections above + (blank + FILES header)
	for i := range hits {
		hits[i].lineOffset += base
	}
	return hits
}

// rowTouchesSelectedFile reports whether a transcript row's mutation touched
// the currently selected file, so its card renders with the selection tint.
func (m model) rowTouchesSelectedFile(row transcriptRow) bool {
	if m.selectedFile == "" || row.kind != rowToolResult {
		return false
	}
	for _, path := range row.changedFiles {
		if path == m.selectedFile {
			return true
		}
	}
	return false
}

// lastRowIndexForFile returns the transcript index of the most recent
// tool-result row that touched path, or -1.
func (m model) lastRowIndexForFile(path string) int {
	for i := len(m.transcript) - 1; i >= 0; i-- {
		row := m.transcript[i]
		if row.kind != rowToolResult {
			continue
		}
		for _, p := range row.changedFiles {
			if p == path {
				return i
			}
		}
	}
	return -1
}

func (m *model) setSelectedFile(path string) {
	if m.selectedFile == path {
		return
	}
	oldPath := m.selectedFile
	m.selectedFile = path
	for i := 0; i < m.flushed && i < len(m.transcript); i++ {
		for _, changedPath := range m.transcript[i].changedFiles {
			if changedPath == oldPath || changedPath == path {
				m.altScreenSettledWidth = 0
				return
			}
		}
	}
}

// selectFile marks path as the selected file and scrolls the transcript so its
// most recent edit card is in view; the card tint comes from the renderers
// reading selectedFile (rowTouchesSelectedFile).
func (m model) selectFile(path string) model {
	rowIndex := m.lastRowIndexForFile(path)
	m.setSelectedFile(path)
	if offset, ok := m.scrollOffsetForTranscriptRow(rowIndex); ok {
		m.chatScrollOffset = offset
		if offset == 0 {
			m.chatBodyLines = 0
		}
	}
	return m
}

// scrollOffsetForTranscriptRow computes the chatScrollOffset that places the
// given transcript row's first rendered line at the top of the viewport (with
// one line of breathing room), using the same layout metrics the scroll engine
// itself uses. ok is false outside alt-screen or when the row isn't rendered
// (e.g. skipped/collapsed).
func (m model) scrollOffsetForTranscriptRow(rowIndex int) (int, bool) {
	if rowIndex < 0 || !m.altScreen || m.height <= 0 {
		return 0, false
	}
	width := m.chatColumnWidth()
	items := m.transcriptBodyItems(width, "", false)
	metrics := measureTranscriptBodyItems(items, m.transcriptBodyHeights)
	startY := -1
	for _, span := range metrics.spans {
		if span.kind == transcriptBodyItemRow && span.rowIndex == rowIndex {
			startY = span.startY
			break
		}
	}
	if startY < 0 {
		return 0, false
	}
	frame := m.scrollableTranscriptFrame(m.pinnedTitleBar(width), m.footerView(width))
	viewport := transcriptViewportForLayout(metrics, frame, m.chatScrollOffset)
	// The offset counts lines below the fold: window.start == total-height-offset,
	// so placing startY at the window top (minus one context line) means
	// offset = total - height - (startY - 1), clamped to the scroll range.
	offset := metrics.totalLines() - frame.bodyRect.height - (startY - 1)
	return clampInt(offset, 0, viewport.maxOffset()), true
}

// truncatePathLeft fits a path into room cells by dropping leading components
// ("internal/tui/sidebar.go" → "…/tui/sidebar.go"), keeping the filename — the
// part that identifies the file — intact for as long as possible.
func truncatePathLeft(path string, room int) string {
	if room <= 0 {
		return ""
	}
	if len(path) <= room {
		return path
	}
	const ellipsis = "…/"
	parts := strings.Split(path, "/")
	for start := 1; start < len(parts); start++ {
		candidate := ellipsis + strings.Join(parts[start:], "/")
		if len(candidate) <= room {
			return candidate
		}
	}
	// Even "…/name" overflows: fall back to a right-truncated basename.
	return truncateRunes(ellipsis+parts[len(parts)-1], room)
}

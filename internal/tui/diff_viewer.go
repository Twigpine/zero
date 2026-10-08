package tui

import "strings"

// Visible unchanged lines kept at each edge of a collapsed diff section.
const diffViewerContextLines = 3

// diffViewerLine is a source diff line or a synthetic collapsed-context row.
// rawIndex keeps rendering anchored to the original diff so line-pair and
// syntax-highlighting logic remain correct after context compaction.
type diffViewerLine struct {
	text          string
	rawIndex      int
	hiddenContext int
}

// compactDiffViewerContext returns the display sequence for a unified diff.
// Long contiguous unchanged runs keep their leading and trailing context while
// one synthetic line accounts for the elided middle. Structural and changed
// rows always remain intact.
func compactDiffViewerContext(raw []string) []diffViewerLine {
	lines := make([]diffViewerLine, 0, len(raw))
	hunk := diffHunkState{}
	for index := 0; index < len(raw); {
		if parsed, ok := parseDiffHunkHeader(raw[index]); ok {
			lines = append(lines, diffViewerLine{text: raw[index], rawIndex: index})
			hunk = parsed
			index++
			continue
		}
		if !hunk.active() || !strings.HasPrefix(raw[index], " ") {
			lines = append(lines, diffViewerLine{text: raw[index], rawIndex: index})
			if hunk.active() && isDiffHunkBodyLine(raw[index]) {
				hunk.consume(raw[index])
			}
			index++
			continue
		}

		start := index
		for index < len(raw) && hunk.active() && strings.HasPrefix(raw[index], " ") {
			hunk.consume(raw[index])
			index++
		}
		count := index - start
		// Collapsing seven lines would replace one readable line with one marker,
		// leaving the display equally tall. Only collapse when it removes space.
		if count <= diffViewerContextLines*2+1 {
			for i := start; i < index; i++ {
				lines = append(lines, diffViewerLine{text: raw[i], rawIndex: i})
			}
			continue
		}

		for i := start; i < start+diffViewerContextLines; i++ {
			lines = append(lines, diffViewerLine{text: raw[i], rawIndex: i})
		}
		lines = append(lines, diffViewerLine{rawIndex: -1, hiddenContext: count - diffViewerContextLines*2})
		for i := index - diffViewerContextLines; i < index; i++ {
			lines = append(lines, diffViewerLine{text: raw[i], rawIndex: i})
		}
	}
	return lines
}

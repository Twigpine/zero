package tui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

func requireDiffViewerTrueColor(t *testing.T) {
	t.Helper()
	oldProfile := lipgloss.Writer.Profile
	lipgloss.Writer.Profile = colorprofile.TrueColor
	t.Cleanup(func() {
		lipgloss.Writer.Profile = oldProfile
	})
}

func TestDiffViewerUsesHunkHeaderForLineNumbers(t *testing.T) {
	diff := strings.Join([]string{
		"--- a/internal/tui/view.go",
		"+++ b/internal/tui/view.go",
		"@@ -40,3 +40,4 @@ func renderView() {",
		" context",
		"-old line",
		"+new line",
	}, "\n")

	body := diffCardBody(diff, 90, cardRenderOptions{bodyCap: 0})
	got := plainRender(t, strings.Join(body.lines, "\n"))
	if strings.Contains(got, "@@") || !strings.Contains(got, "  41 − old line") || !strings.Contains(got, "  41 + new line") {
		t.Fatalf("diff viewer did not apply the hunk range correctly:\n%s", got)
	}
}

func TestDiffViewerCollapsesLongUnchangedContext(t *testing.T) {
	lines := []string{
		"--- a/x.go",
		"+++ b/x.go",
		"@@ -1,12 +1,12 @@",
	}
	for i := 1; i <= 12; i++ {
		lines = append(lines, " context "+string(rune('a'+i-1)))
	}
	diff := strings.Join(lines, "\n")

	body := diffCardBody(diff, 80, cardRenderOptions{bodyCap: 0})
	got := plainRender(t, strings.Join(body.lines, "\n"))
	if !strings.Contains(got, "… 6 unchanged lines") {
		t.Fatalf("diff viewer did not collapse long unchanged context:\n%s", got)
	}
	for _, want := range []string{"context a", "context b", "context c", "context j", "context k", "context l"} {
		if !strings.Contains(got, want) {
			t.Errorf("diff viewer should preserve contextual edge %q:\n%s", want, got)
		}
	}
}

func TestDiffViewerKeepsSevenUnchangedContextLines(t *testing.T) {
	lines := []string{
		"--- a/x.go",
		"+++ b/x.go",
		"@@ -1,7 +1,7 @@",
	}
	for i := 1; i <= diffViewerContextLines*2+1; i++ {
		lines = append(lines, " context "+string(rune('a'+i-1)))
	}

	body := diffCardBody(strings.Join(lines, "\n"), 80, cardRenderOptions{bodyCap: 0})
	got := plainRender(t, strings.Join(body.lines, "\n"))
	if strings.Contains(got, "unchanged lines") {
		t.Fatalf("seven unchanged context lines should remain visible:\n%s", got)
	}
	for _, want := range []string{"context a", "context d", "context g"} {
		if !strings.Contains(got, want) {
			t.Errorf("diff viewer omitted %q:\n%s", want, got)
		}
	}
}

func TestDiffViewerKeepsLineNumbersAfterCollapsedContext(t *testing.T) {
	lines := []string{
		"--- a/x.go",
		"+++ b/x.go",
		"@@ -1,13 +1,13 @@",
	}
	for i := 1; i <= 12; i++ {
		lines = append(lines, " context "+string(rune('a'+i-1)))
	}
	lines = append(lines, "-old value", "+new value")

	body := diffCardBody(strings.Join(lines, "\n"), 80, cardRenderOptions{bodyCap: 0})
	got := plainRender(t, strings.Join(body.lines, "\n"))
	if !strings.Contains(got, "  13 − old value") || !strings.Contains(got, "  13 + new value") {
		t.Fatalf("line numbers drifted after collapsed context:\n%s", got)
	}
}

func TestDiffViewerSyntheticContextHasNoSourceIndex(t *testing.T) {
	raw := make([]string, 0, diffViewerContextLines*2+2)
	for index := 0; index < diffViewerContextLines*2+2; index++ {
		raw = append(raw, " context")
	}
	for _, line := range compactDiffViewerContext(raw) {
		if line.hiddenContext > 0 && line.rawIndex != -1 {
			t.Fatalf("synthetic collapsed context rawIndex = %d, want -1", line.rawIndex)
		}
	}
}

func TestDiffViewerNearRewriteDeletionSurvives(t *testing.T) {
	diff := strings.Join([]string{
		"--- a/notes.log",
		"+++ b/notes.log",
		"@@ -1 +1 @@",
		"-alpha bravo charlie delta",
		"+zulu yankee xray whiskey",
	}, "\n")
	body := diffCardBody(diff, 100, cardRenderOptions{bodyCap: 0})
	got := plainRender(t, strings.Join(body.lines, "\n"))
	if !strings.Contains(got, "alpha bravo charlie delta") {
		t.Fatalf("near-rewrite deletion disappeared:\n%s", got)
	}
}

func TestDiffViewerRendersStyledDeletionOnce(t *testing.T) {
	requireDiffViewerTrueColor(t)
	diff := strings.Join([]string{
		"--- a/example.go",
		"+++ b/example.go",
		"@@ -1 +1 @@",
		"-func value() string { return \"old\" }",
		"+func value() string { return \"new\" }",
	}, "\n")
	body := diffCardBody(diff, 100, cardRenderOptions{bodyCap: 0})
	got := plainRender(t, strings.Join(body.lines, "\n"))
	if count := strings.Count(got, "func value() string { return \"old\" }"); count != 1 {
		t.Fatalf("styled deleted row count = %d, want 1:\n%s", count, got)
	}
}

func TestDiffViewerDoesNotFoldPreambleContext(t *testing.T) {
	raw := []string{
		"commit deadbeef",
		"Author: Example",
		"",
	}
	for index := 1; index <= 10; index++ {
		raw = append(raw, fmt.Sprintf("    commit message line %d", index))
	}
	raw = append(raw,
		"--- a/example.go",
		"+++ b/example.go",
		"@@ -1,1 +1,1 @@",
		"-old value",
		"+new value",
	)

	compacted := compactDiffViewerContext(raw)
	for _, line := range compacted {
		if line.hiddenContext > 0 {
			t.Fatalf("preamble context was folded: %#v", compacted)
		}
	}
	if len(compacted) != len(raw) {
		t.Fatalf("compacted preamble length = %d, want %d", len(compacted), len(raw))
	}
}

func TestDiffViewerKeepsHeaderLikeHunkContent(t *testing.T) {
	diff := strings.Join([]string{
		"--- a/example.go",
		"+++ b/example.go",
		"@@ -1 +1 @@",
		"--- removed",
		"+++ added",
	}, "\n")
	body := diffCardBody(diff, 100, cardRenderOptions{bodyCap: 0})
	got := plainRender(t, strings.Join(body.lines, "\n"))
	for _, want := range []string{"-- removed", "++ added"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diff viewer lost header-like hunk content %q:\n%s", want, got)
		}
	}
	meta := diffCardMetadata(diff)
	if meta.adds != 1 || meta.dels != 1 {
		t.Fatalf("diff metadata counts = +%d -%d, want +1 -1", meta.adds, meta.dels)
	}
	if _, ok := highlightedDiffLines(strings.Split(diff, "\n"), meta)[3]; !ok {
		t.Fatal("deleted header-like hunk content was not syntax highlighted")
	}
}

func TestDiffViewerSourcePathStripsOnlyOneDiffPrefix(t *testing.T) {
	if got, want := diffViewerSourcePath("--- a/b/c.go"), "b/c.go"; got != want {
		t.Fatalf("diffViewerSourcePath = %q, want %q", got, want)
	}
}

func TestDiffViewerSyntaxHighlightsBothChangedSides(t *testing.T) {
	requireDiffViewerTrueColor(t)
	diff := strings.Join([]string{
		"--- a/example.go",
		"+++ b/example.go",
		"@@ -1 +1 @@",
		"-func greet(name string) string { return \"old\" }",
		"+func greet(name string) string { return \"new\" }",
	}, "\n")

	body := diffCardBody(diff, 100, cardRenderOptions{bodyCap: 0})
	joined := strings.Join(body.lines, "\n")
	// `func` uses Zero's accent and string literals use its green token style.
	if !strings.Contains(joined, "202;255;63") || !strings.Contains(joined, "93;209;164") {
		t.Fatalf("diff viewer did not retain Go syntax colors in changed rows:\n%s", joined)
	}
	// The exact changed string literal keeps the brighter word-diff background.
	if !strings.Contains(joined, "46;101;77") || !strings.Contains(joined, "80;45;48") {
		t.Fatalf("diff viewer lost word-level changed-span emphasis:\n%s", joined)
	}
}

func TestDiffViewerSyntaxHighlightsUnchangedHunkContext(t *testing.T) {
	requireDiffViewerTrueColor(t)
	diff := strings.Join([]string{
		"--- a/example.go",
		"+++ b/example.go",
		"@@ -1,3 +1,3 @@",
		" func greet() string {",
		"-\treturn \"old\"",
		"+\treturn \"new\"",
		" }",
	}, "\n")

	body := diffCardBody(diff, 100, cardRenderOptions{bodyCap: 0})
	for _, line := range body.lines {
		if strings.Contains(plainRender(t, line), "func greet() string {") {
			if !strings.Contains(line, "202;255;63") {
				t.Fatalf("unchanged source context was not syntax-highlighted: %q", line)
			}
			return
		}
	}
	t.Fatal("diff viewer omitted unchanged source context")
}

func TestDiffViewerPreservesSyntaxStateAcrossHunkLines(t *testing.T) {
	requireDiffViewerTrueColor(t)
	diff := strings.Join([]string{
		"--- a/example.py",
		"+++ b/example.py",
		"@@ -1,3 +1,4 @@",
		" def message():",
		"-    return \"\"\"old\"\"\"",
		"+    return \"\"\"new",
		"+continued\"\"\"",
	}, "\n")

	body := diffCardBody(diff, 100, cardRenderOptions{bodyCap: 0})
	for _, line := range body.lines {
		if strings.Contains(plainRender(t, line), "continued") {
			if !strings.Contains(line, "93;209;164") {
				t.Fatalf("multiline string lost syntax state across hunk rows: %q", line)
			}
			return
		}
	}
	t.Fatal("diff viewer omitted multiline source row")
}

func TestDiffViewerKeepsNonHunkDiffFallback(t *testing.T) {
	diff := strings.Join([]string{
		"--- a/example.go",
		"+++ b/example.go",
		"+func greet() string { return \"new\" }",
	}, "\n")

	body := diffCardBody(diff, 100, cardRenderOptions{bodyCap: 0})
	got := plainRender(t, strings.Join(body.lines, "\n"))
	if !strings.Contains(got, "func greet() string { return \"new\" }") {
		t.Fatalf("diff viewer lost content without a hunk header:\n%s", got)
	}
}

func TestDiffViewerKeepsRenameMetadataWithoutHunks(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/old-name.go b/new-name.go",
		"similarity index 100%",
		"rename from old-name.go",
		"rename to new-name.go",
	}, "\n")

	body := diffCardBody(diff, 100, cardRenderOptions{bodyCap: 0})
	got := plainRender(t, strings.Join(body.lines, "\n"))
	if !strings.Contains(got, "rename from old-name.go") || !strings.Contains(got, "rename to new-name.go") {
		t.Fatalf("rename-only diff lost its meaningful metadata:\n%s", got)
	}
}

func TestDiffViewerKeepsModeMetadataWithoutHunks(t *testing.T) {
	diff := strings.Join([]string{
		"diff --git a/tool.sh b/tool.sh",
		"old mode 100644",
		"new mode 100755",
	}, "\n")

	body := diffCardBody(diff, 100, cardRenderOptions{bodyCap: 0})
	got := plainRender(t, strings.Join(body.lines, "\n"))
	if !strings.Contains(got, "old mode 100644") || !strings.Contains(got, "new mode 100755") {
		t.Fatalf("mode-only diff lost its meaningful metadata:\n%s", got)
	}
}

func TestDiffViewerFallsBackForOversizedSourceLine(t *testing.T) {
	requireDiffViewerTrueColor(t)
	longLine := strings.Repeat("x", diffViewerHighlightMaxLineBytes)
	diff := strings.Join([]string{
		"--- a/example.go",
		"+++ b/example.go",
		"@@ -1 +1 @@",
		"+" + longLine,
	}, "\n")

	body := diffCardBody(diff, 100, cardRenderOptions{bodyCap: 0})
	joined := strings.Join(body.lines, "\n")
	if strings.Contains(joined, "202;255;63") {
		t.Fatalf("oversized diff line should use the plain diff fallback: %q", joined)
	}
	if !strings.Contains(plainRender(t, joined), strings.Repeat("x", 10)) {
		t.Fatalf("plain fallback lost oversized source line: %q", joined)
	}
}

func TestDiffViewerSyntaxHighlighterKeepsSpans(t *testing.T) {
	requireDiffViewerTrueColor(t)
	styled, ok := highlightCodeForPathWithSpans(
		[]string{"func greet(name string) string { return \"new\" }"},
		"example.go",
		200,
		zeroTheme.addLine.GetBackground(),
		[]highlightSpan{{line: 0, start: 39, end: 44, background: zeroTheme.addLineWord.GetBackground()}},
	)
	if !ok || len(styled) != 1 {
		t.Fatalf("highlightCodeForPathWithSpans = %#v, %v", styled, ok)
	}
	if !strings.Contains(styled[0], "46;101;77") {
		t.Fatalf("highlighted span lost its word-diff background: %q", styled[0])
	}
}

func TestDiffViewerHighlightsEachFileWithItsOwnPath(t *testing.T) {
	requireDiffViewerTrueColor(t)
	rawLines := []string{
		"diff --git a/example.go b/example.go",
		"--- a/example.go",
		"+++ b/example.go",
		"@@ -1 +1 @@",
		"-func old() {}",
		"+func new() {}",
		"diff --git a/example.py b/example.py",
		"--- a/example.py",
		"+++ b/example.py",
		"@@ -1 +1 @@",
		"-def old(): pass",
		"+def new(): pass",
	}

	highlighted := highlightedDiffLines(rawLines, diffCardMetadata(strings.Join(rawLines, "\n")))
	if len(highlighted) != 4 {
		t.Fatalf("highlighted line count = %d, want 4: %#v", len(highlighted), highlighted)
	}
	for _, index := range []int{4, 5, 10, 11} {
		if !strings.Contains(highlighted[index], "202;255;63") {
			t.Fatalf("source row %d lost syntax highlighting: %q", index, highlighted[index])
		}
	}
	for _, header := range []int{2, 3, 8, 9} {
		if _, ok := highlighted[header]; ok {
			t.Fatalf("file header at row %d entered syntax highlighter: %q", header, highlighted[header])
		}
	}
}

func TestDiffViewerHighlightBudgetSpansHunks(t *testing.T) {
	hunkLines := diffViewerHighlightMaxLines/2 + 1
	rawLines := []string{
		"--- a/example.go",
		"+++ b/example.go",
		fmt.Sprintf("@@ -1,%d +1,%d @@", hunkLines, hunkLines),
	}
	for i := 0; i < hunkLines; i++ {
		rawLines = append(rawLines, " context")
	}
	rawLines = append(rawLines, fmt.Sprintf("@@ -6000,%d +6000,%d @@", hunkLines, hunkLines))
	for i := 0; i < hunkLines; i++ {
		rawLines = append(rawLines, " context")
	}
	if highlighted := highlightedDiffLines(rawLines, diffCardMetadata(strings.Join(rawLines, "\n"))); highlighted != nil {
		t.Fatalf("diff exceeding the aggregate highlight budget should use the plain fallback: %d highlighted rows", len(highlighted))
	}
}
